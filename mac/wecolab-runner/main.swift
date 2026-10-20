import Foundation
import OSLog
import WeCoLabCore

private final class ReplyClosure: @unchecked Sendable {
    let call: (Data) -> Void
    init(_ call: @escaping (Data) -> Void) { self.call = call }
}

private final class ControlEndpoint: NSObject, RunnerXPCProtocol {
    let service: RunnerService
    init(service: RunnerService) { self.service = service }
    func request(_ data: Data, withReply reply: @escaping (Data) -> Void) {
        let completion = ReplyClosure(reply)
        guard data.count <= 1024 * 1024 else {
            completion.call(Self.encode(RunnerReply(error: "Control request exceeds 1 MiB.")))
            return
        }
        do {
            let command = try JSONDecoder().decode(RunnerCommand.self, from: data)
            Task { @Sendable [service, completion, command] in
                completion.call(ControlEndpoint.encode(await service.handle(command)))
            }
        } catch { completion.call(Self.encode(RunnerReply(error: "Invalid runner control request."))) }
    }
    private static func encode(_ reply: RunnerReply) -> Data {
        // All response values are finite integers and strings; encode failures are still explicit.
        (try? JSONEncoder().encode(reply)) ?? Data(#"{"error":"Cannot encode runner state."}"#.utf8)
    }
}

private final class ControlListener: NSObject, NSXPCListenerDelegate {
    let service: RunnerService
    init(service: RunnerService) { self.service = service }
    func listener(_ listener: NSXPCListener, shouldAcceptNewConnection connection: NSXPCConnection) -> Bool {
        guard connection.effectiveUserIdentifier == getuid() else { return false }
        connection.exportedInterface = NSXPCInterface(with: RunnerXPCProtocol.self)
        connection.exportedObject = ControlEndpoint(service: service)
        connection.resume()
        return true
    }
}

private func holdRunnerLock(_ root: URL) throws -> Int32 {
    let file = root.appending(path: "runner.lock")
    let fd = open(file.path, O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0o600)
    guard fd >= 0 else { throw NodeError("Cannot open the runner ownership lock.") }
    var st = stat()
    guard fstat(fd, &st) == 0, st.st_uid == getuid(), st.st_mode & S_IFMT == S_IFREG,
          flock(fd, LOCK_EX | LOCK_NB) == 0 else {
        close(fd)
        throw NodeError("Another runner owns this state directory, or its lock is unsafe.")
    }
    return fd
}

let args = Array(CommandLine.arguments.dropFirst())
if args == ["--version"] {
    print(BuildInfo.display)
    exit(EXIT_SUCCESS)
}

// A worker may close its command pipe while a stop or memory request is being written.
signal(SIGPIPE, SIG_IGN)

// Only service startup below may touch the user's runner state.
guard args.count == 4, args[0] == "--root", args[2] == "--service" else {
    FileHandle.standardError.write(Data("wecolab-runner is managed by the WeCoLab launch agent.\n".utf8))
    exit(EXIT_FAILURE)
}
let locations = RunnerLocations(root: URL(fileURLWithPath: args[1], isDirectory: true))
guard args[3] == locations.serviceName else { exit(EXIT_FAILURE) }
let ownership: Int32
do { ownership = try holdRunnerLock(locations.root) }
catch {
    Logger(subsystem: "io.wecolab.mac", category: "runner").error("Runner startup failed: \(error.localizedDescription, privacy: .public)")
    exit(EXIT_FAILURE)
}
let service = RunnerService(locations: locations)
private let delegate = ControlListener(service: service)
let listener = NSXPCListener(machServiceName: locations.serviceName)
listener.delegate = delegate
listener.resume()
Task { await service.initialize() }
let signals = [SIGTERM, SIGINT].map { value in
    signal(value, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: value, queue: .main)
    source.setEventHandler {
        Task {
            await service.stopForTermination()
            close(ownership)
            exit(EXIT_SUCCESS)
        }
    }
    source.resume()
    return source
}
dispatchMain()
