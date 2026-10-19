import CryptoKit
import Foundation

public struct RunnerLocations: Sendable {
    public static var defaultRoot: URL {
        URL.applicationSupportDirectory.appending(path: "WeCoLab", directoryHint: .isDirectory)
    }
    public let root: URL
    public init(root: URL = RunnerLocations.defaultRoot) { self.root = root.standardizedFileURL }
    public var serviceName: String {
        if root == Self.defaultRoot.standardizedFileURL { return "io.wecolab.runner" }
        let suffix = SHA256.hash(data: Data(root.path.utf8)).prefix(8).map { String(format: "%02x", $0) }.joined()
        return "io.wecolab.runner.\(suffix)"
    }
    public var launchAgent: URL {
        FileManager.default.homeDirectoryForCurrentUser.appending(path: "Library/LaunchAgents/\(serviceName).plist")
    }
    public var workerExecutable: URL { Self.siblingExecutable("wecolab-vm") }
    public var runnerExecutable: URL { Self.siblingExecutable("wecolab-runner") }

    public static func siblingExecutable(_ name: String) -> URL {
        let executable = Bundle.main.executableURL ?? URL(fileURLWithPath: CommandLine.arguments[0])
        return executable.deletingLastPathComponent().appending(path: name)
    }
}

/// Both clients use the same user-scoped Mach service; only the service owns VM processes.
public final class RunnerClient: @unchecked Sendable {
    public let locations: RunnerLocations
    public init(locations: RunnerLocations = RunnerLocations()) { self.locations = locations }

    public func send(_ command: RunnerCommand) async throws -> RunnerSnapshot {
        try await RunnerInstallation.shared.ensure(locations)
        let request = try JSONEncoder().encode(command)
        // stopAll acknowledges worker exit, including the guest's 60-second graceful-stop window.
        let timeout: Double = if case .stopAll = command { 90 } else { 20 }
        let data: Data = try await withCheckedThrowingContinuation { continuation in
            let connection = NSXPCConnection(machServiceName: locations.serviceName)
            connection.remoteObjectInterface = NSXPCInterface(with: RunnerXPCProtocol.self)
            let pending = PendingRunnerReply(connection: connection, continuation: continuation)
            connection.interruptionHandler = { pending.fail("The background runner disconnected; VM state is unavailable.") }
            connection.invalidationHandler = { pending.fail("Cannot connect to the background runner. Check macOS Login Items and background permissions.") }
            connection.resume()
            guard let remote = connection.remoteObjectProxyWithErrorHandler({ error in
                pending.fail("Background runner: \(error.localizedDescription)")
            }) as? RunnerXPCProtocol else {
                pending.fail("The background runner has an incompatible control interface.")
                return
            }
            remote.request(request) { pending.finish(.success($0)) }
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                pending.fail("The background runner did not answer; VM state is unavailable.")
            }
        }
        guard data.count <= 4 * 1024 * 1024 else { throw NodeError("The background runner sent an oversized response.") }
        let reply = try JSONDecoder().decode(RunnerReply.self, from: data)
        if let error = reply.error { throw NodeError(error) }
        guard let snapshot = reply.snapshot else { throw NodeError("The background runner returned no state.") }
        return snapshot
    }
}

private final class PendingRunnerReply: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Data, any Error>?
    private let connection: NSXPCConnection
    init(connection: NSXPCConnection, continuation: CheckedContinuation<Data, any Error>) {
        self.connection = connection
        self.continuation = continuation
    }
    func fail(_ message: String) { finish(.failure(NodeError(message))) }
    func finish(_ result: Result<Data, any Error>) {
        lock.lock()
        let continuation = self.continuation
        self.continuation = nil
        lock.unlock()
        guard let continuation else { return }
        connection.interruptionHandler = nil
        connection.invalidationHandler = nil
        connection.invalidate()
        continuation.resume(with: result)
    }
}

/// LaunchAgent registration is serialized across this client's concurrent requests. launchd and the
/// runner's own lock serialize independent clients. Never replace a live runner from another build.
private actor RunnerInstallation {
    static let shared = RunnerInstallation()
    private var prepared: Set<String> = []

    func ensure(_ locations: RunnerLocations) throws {
        guard !prepared.contains(locations.serviceName) else { return }
        let service = "gui/\(getuid())/\(locations.serviceName)"
        // App and CLI may be installed in different locations. A registered service owns the
        // state; connecting another client must neither replace it nor demand matching paths.
        if try Self.tool("/bin/launchctl", ["print", service]).status == 0 {
            prepared.insert(locations.serviceName)
            return
        }
        let fm = FileManager.default
        let runner = locations.runnerExecutable
        guard fm.isExecutableFile(atPath: runner.path), fm.isExecutableFile(atPath: locations.workerExecutable.path) else {
            throw NodeError("The shared runner is not installed beside this client. Install the complete WeCoLab app or use make sign.")
        }
        if let values = try? locations.root.resourceValues(forKeys: [.isSymbolicLinkKey]), values.isSymbolicLink == true {
            throw NodeError("The WeCoLab state directory must not be a symbolic link.")
        }
        try fm.createDirectory(at: locations.root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: locations.root.path)
        let plist: [String: Any] = [
            "Label": locations.serviceName,
            "ProgramArguments": [runner.path, "--root", locations.root.path, "--service", locations.serviceName],
            "MachServices": [locations.serviceName: true],
            "RunAtLoad": true,
            "ProcessType": "Interactive",
            "ThrottleInterval": 10,
            "ExitTimeOut": 75,
            "AssociatedBundleIdentifiers": ["io.wecolab.mac"],
            "StandardOutPath": "/dev/null",
            "StandardErrorPath": "/dev/null"
        ]
        let encoded = try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
        try fm.createDirectory(at: locations.launchAgent.deletingLastPathComponent(), withIntermediateDirectories: true)
        try encoded.write(to: locations.launchAgent, options: .atomic)
        try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: locations.launchAgent.path)
        let loaded = try Self.tool("/bin/launchctl", ["print", service]).status == 0
        if !loaded {
            let bootstrap = try Self.tool("/bin/launchctl", ["bootstrap", "gui/\(getuid())", locations.launchAgent.path])
            if bootstrap.status != 0, try Self.tool("/bin/launchctl", ["print", service]).status != 0 {
                throw NodeError("Cannot register the background runner: \(bootstrap.output.trimmingCharacters(in: .whitespacesAndNewlines))")
            }
        }
        prepared.insert(locations.serviceName)
    }

    private static func tool(_ executable: String, _ arguments: [String]) throws -> (status: Int32, output: String) {
        let process = Process(), output = Pipe()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.standardOutput = output
        process.standardError = output
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        return (process.terminationStatus, String(decoding: data, as: UTF8.self))
    }
}
