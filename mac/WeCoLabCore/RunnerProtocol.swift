import Foundation

/// Local control messages. Invites travel only over authenticated local IPC, never argv or logs.
public enum RunnerCommand: Codable, Sendable {
    case status
    case create(NodeProfile, confirmedConsole: String)
    case configure(NodeProfile, confirmedConsole: String?)
    case start(String)
    case stop(String)
    case reset(String)
    case remove(String)
    case stopAll
}

public struct NodeSnapshot: Codable, Sendable, Identifiable {
    public var profile: NodeProfile
    public var consoleURL: String
    public var state: String
    public var mode: String
    public var message: String
    public var pid: Int32?
    public var runningConfig: NodeConfig?
    public var runningNetworkAccess: NodeNetworkAccess?
    public var guestGeneration: String
    public var meshIP: String
    public var meshName: String
    public var meshConnected: Bool
    public var guestStatusFresh: Bool
    public var join: String
    public var lastJoinLine: String

    public var id: String { profile.id }
    public var running: Bool { pid != nil || ["preparing", "starting", "running", "stopping"].contains(state) }
    public var busy: Bool { ["preparing", "starting", "stopping"].contains(state) }
    public var needsRestart: Bool {
        guard let actual = runningConfig else { return false }
        let desired = profile.config
        return actual.cpus != desired.cpus || actual.diskGiB != desired.diskGiB ||
            actual.idleCPUs != desired.idleCPUs || actual.idleMemoryGiB != desired.idleMemoryGiB ||
            actual.idleMinutes != desired.idleMinutes || runningNetworkAccess != profile.networkAccess
    }
}

public struct RunnerSnapshot: Codable, Sendable {
    public var nodes: [NodeSnapshot]
    public var cpuBudget: Int
    public var memoryBudgetGiB: Int
    public var reservedCPUs: Int
    public var reservedMemoryGiB: Int
    public var stoppingAll: Bool
    public var legacyPID: Int32?
    public var message: String

    public var anyRunning: Bool { legacyPID != nil || nodes.contains(where: \.running) }
}

public struct RunnerReply: Codable, Sendable {
    public var snapshot: RunnerSnapshot?
    public var error: String?
    public init(snapshot: RunnerSnapshot? = nil, error: String? = nil) {
        self.snapshot = snapshot
        self.error = error
    }
}

@objc public protocol RunnerXPCProtocol {
    func request(_ data: Data, withReply reply: @escaping (Data) -> Void)
}
