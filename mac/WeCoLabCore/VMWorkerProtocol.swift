import Foundation

public struct VMWorkerStart: Codable, Sendable {
    public var directoryBookmark: Data
    public var config: NodeConfig
    public var networkAccess: NodeNetworkAccess

    public init(directoryBookmark: Data, config: NodeConfig, networkAccess: NodeNetworkAccess) {
        self.directoryBookmark = directoryBookmark
        self.config = config
        self.networkAccess = networkAccess
    }
}

public enum VMWorkerCommand: Codable, Sendable {
    case start(VMWorkerStart)
    case memory(Int)
    case stop
    case status
}

public struct VMWorkerStatus: Codable, Sendable {
    public var state: String
    public var mode: String
    public var message: String
    public var pid: Int32
    public var reservedMemoryGiB: Int?

    public init(state: String, mode: String = "", message: String = "", pid: Int32, reservedMemoryGiB: Int? = nil) {
        self.state = state
        self.mode = mode
        self.message = message
        self.pid = pid
        self.reservedMemoryGiB = reservedMemoryGiB
    }
}

/// A record is a single compact JSON object (without its terminating newline).
public enum VMWorkerWire {
    public static let maximumRecordBytes = 1 << 20

    public static func decode(_ record: Data) throws -> VMWorkerCommand {
        guard !record.isEmpty, record.count <= maximumRecordBytes,
              let object = try JSONSerialization.jsonObject(with: record) as? [String: Any],
              object.count == 1, let key = object.keys.first,
              ["start", "memory", "stop", "status"].contains(key) else {
            throw NodeError("invalid worker command")
        }
        return try JSONDecoder().decode(VMWorkerCommand.self, from: record)
    }
}

/// Independent of the supervisor's resource checks: even a compromised controller cannot
/// replace a running profile capability or grow a VM beyond its original maximum.
public struct VMWorkerGate: Sendable {
    private var maximumMemoryGiB: Int?
    private var stopped = false

    public init() {}

    public mutating func accept(_ command: VMWorkerCommand) throws {
        switch command {
        case .start(let start):
            guard maximumMemoryGiB == nil, !stopped, !start.directoryBookmark.isEmpty else {
                throw NodeError("worker accepts only one profile")
            }
            try start.config.validate()
            maximumMemoryGiB = start.config.idleMemoryGiB
        case .memory(let memory):
            guard let maximumMemoryGiB, !stopped, memory >= 2, memory <= maximumMemoryGiB else {
                throw NodeError("memory exceeds the running VM maximum")
            }
        case .stop:
            guard maximumMemoryGiB != nil, !stopped else { throw NodeError("worker is not running") }
            stopped = true
        case .status:
            guard maximumMemoryGiB != nil, !stopped else { throw NodeError("worker has not started") }
        }
    }
}
