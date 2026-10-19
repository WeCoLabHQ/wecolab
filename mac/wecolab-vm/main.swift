import Darwin
import Foundation
import WeCoLabCore

private actor Worker {
    private var gate = VMWorkerGate()
    private var scopedDirectory: URL?
    private var lock: Int32?
    private var vm: NodeVM?
    private var stopping = false
    private var finished = false
    private var lastError = ""
    private var reservedMemoryGiB: Int?
    private var reportsEnabled = true
    private var shutdownTask: Task<Void, Never>?
    private var failedToStop = false

    private func report(_ state: String, mode: String = "", message: String = "") {
        guard reportsEnabled else { return }
        let status = VMWorkerStatus(state: state, mode: mode, message: message, pid: getpid(), reservedMemoryGiB: reservedMemoryGiB)
        guard let data = try? JSONEncoder().encode(status) else { return }
        do {
            try FileHandle.standardOutput.write(contentsOf: data + Data([10]))
        } catch {
            reportsEnabled = false
            stopping = true
            // A broken controller must not strand its VM; never report this failure.
            Task { await self.shutdown() }
        }
    }

    func command(_ record: Data) async {
        if stopping { await shutdown(); return }
        do {
            let command = try VMWorkerWire.decode(record)
            try gate.accept(command)
            switch command {
            case .start(let request):
                reservedMemoryGiB = request.config.memoryGiB
                report("starting")
                if stopping { await shutdown(); return }
                var stale = false
                let url = try URL(resolvingBookmarkData: request.directoryBookmark, options: [], relativeTo: nil, bookmarkDataIsStale: &stale)
                // Sandbox home remapping can mark a fresh bookmark stale. These are one-shot
                // capabilities, not persisted bookmarks; actual security-scoped access is required.
                guard url.isFileURL, url.startAccessingSecurityScopedResource() else {
                    throw NodeError("profile directory capability unavailable")
                }
                scopedDirectory = url
                var details = stat()
                guard lstat(url.path, &details) == 0, details.st_mode & S_IFMT == S_IFDIR else {
                    throw NodeError("profile capability is not a directory")
                }
                let paths = Paths(root: url)
                lock = try VMLock.acquire(paths)
                // The executable is strictly a sibling of this worker, not a profile-supplied path.
                guard let ownExecutable = Bundle.main.executableURL else {
                    throw NodeError("cannot locate bundled network backend")
                }
                let executable = ownExecutable.deletingLastPathComponent().appending(path: "wecolab-network")
                let node = NodeVM(paths: paths, networkAccess: request.networkAccess, networkExecutable: executable)
                vm = node
                try await node.start(request.config)
                if stopping { await shutdown(); return }
                report(await node.state, mode: await node.mode)
            case .memory(let memory):
                guard let vm else { throw NodeError("VM has not started") }
                try await vm.reserve(memoryGiB: memory)
                reservedMemoryGiB = memory
                try await vm.tick()
                report(await vm.state, mode: await vm.mode)
            case .status:
                await status()
            case .stop:
                await shutdown()
            }
        } catch {
            lastError = error.localizedDescription
            report("error", message: lastError)
            await shutdown()
        }
    }

    func status() async {
        guard !stopping, !finished, let vm else { return }
        if !(await vm.networkIsHealthy) {
            lastError = "network backend exited"
            report("error", message: lastError)
            await shutdown()
            return
        }
        do { try await vm.tick() } catch {
            lastError = error.localizedDescription
            report("error", message: lastError)
            await shutdown()
            return
        }
        let state = await vm.state
        report(state, mode: await vm.mode, message: lastError)
        if state == "stopped" || state == "error" { await shutdown() }
    }

    func shutdown() async {
        if let shutdownTask { await shutdownTask.value; return }
        let task = Task { await self.finishShutdown() }
        shutdownTask = task
        await task.value
    }

    private func finishShutdown() async {
        guard !finished else { return }
        stopping = true
        if let vm {
            await vm.stop()
            guard await vm.state == "stopped" else {
                failedToStop = true
                report("error", message: "VM did not stop; retaining ownership")
                return
            }
        }
        if let lock { VMLock.release(lock); self.lock = nil }
        scopedDirectory?.stopAccessingSecurityScopedResource()
        scopedDirectory = nil
        reservedMemoryGiB = nil
        finished = true
        report("stopped", message: lastError)
    }
    /// Forced worker exit must reap the helper even when Virtualization refuses to stop.
    /// Ownership lock remains held until actual stop or this process exits.
    func forceExitCleanup() async {
        await vm?.forceTerminateBackend()
    }

    var isFinished: Bool { finished }
    var requiresForcedExit: Bool { failedToStop }
}

/// Reads bounded newline-delimited records; an incomplete, oversized or invalid frame fails
/// closed. poll's timeout also drives the 5-second status/balloon publication cadence.
@main
private struct VMWorkerMain {
    static func main() async {
        let worker = Worker()
        signal(SIGPIPE, SIG_IGN)
        signal(SIGTERM, SIG_IGN)
        let termination = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .global())
        termination.setEventHandler {
            Task {
                await worker.shutdown()
                await worker.forceExitCleanup()
                exit(0) // The process exit releases ownership only after its VM is gone.
            }
        }
        termination.resume()
        var pending = Data()
        var lastStatus = Date()
        while !(await worker.isFinished), !(await worker.requiresForcedExit) {
            var input = pollfd(fd: STDIN_FILENO, events: Int16(POLLIN | POLLHUP), revents: 0)
            let result = poll(&input, 1, 1_000)
            if result < 0 {
                if errno == EINTR { continue }
                await worker.shutdown()
                break
            }
            if result > 0 {
                var bytes = [UInt8](repeating: 0, count: 8192)
                let count = read(STDIN_FILENO, &bytes, bytes.count)
                if count <= 0 { await worker.shutdown(); break }
                for byte in bytes.prefix(Int(count)) {
                    if byte == 10 {
                        await worker.command(pending)
                        pending.removeAll(keepingCapacity: true)
                        if await worker.isFinished { break }
                    } else {
                        pending.append(byte)
                        if pending.count > VMWorkerWire.maximumRecordBytes {
                            await worker.command(pending)
                            await worker.shutdown()
                            break
                        }
                    }
                }
            }
            if Date().timeIntervalSince(lastStatus) >= 5 {
                await worker.status()
                lastStatus = Date()
            }
        }
        await worker.forceExitCleanup()
        termination.cancel()
    }
}
