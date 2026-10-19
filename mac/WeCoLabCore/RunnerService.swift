import Foundation

/// All admission and lifecycle mutations are serialized here, before asynchronous preparation starts.
public actor RunnerService {
    private let locations: RunnerLocations
    private let store: ProfileStore
    private let budget: ResourceBudget
    private var running: [String: RunRecord] = [:]
    private var lastErrors: [String: String] = [:]
    private var migrationError = ""
    private var initialized = false

    private struct RunRecord {
        let generation = UUID()
        var profile: NodeProfile
        var state = "preparing"
        var mode = ""
        var message = "Preparing the VM"
        var session: VMWorkerSession?
        var preparation: Task<Void, Never>?
    }

    public init(locations: RunnerLocations, budget: ResourceBudget = .host) {
        self.locations = locations
        self.store = ProfileStore(root: locations.root)
        self.budget = budget
    }

    public func initialize() async {
        guard !initialized else { return }
        do {
            _ = try store.importLegacy()
            migrationError = ""
            let profiles = try store.profiles()
            initialized = true
            for profile in profiles where profile.autoStart {
                do { try start(profile.id) } catch { lastErrors[profile.id] = error.localizedDescription }
            }
        } catch { migrationError = error.localizedDescription }
    }

    public func handle(_ command: RunnerCommand) async -> RunnerReply {
        await initialize()
        do {
            switch command {
            case .status: break
            case .create(let profile, let confirmedConsole):
                try requireMigrated()
                try profile.config.validate()
                try budget.validate(starting: profile.config, running: [])
                let invite = try Invite(profile.config.invite)
                guard invite.console == confirmedConsole else { throw NodeError("Confirm the exact fabric Console before creating this node.") }
                try store.create(profile)
            case .configure(var profile, let confirmedConsole):
                try requireMigrated()
                let previous = try store.profile(id: profile.id)
                guard profile.hostname == previous.hostname else { throw NodeError("A node's guest identity cannot be changed by editing its settings.") }
                let paths = try store.paths(for: profile.id)
                if profile.config.invite.isEmpty { profile.config.invite = previous.config.invite }
                if profile.config.invite != previous.config.invite {
                    guard Provision.guest(paths) == .none else { throw NodeError("Reset the stopped VM before changing its fabric invite.") }
                    guard try Invite(profile.config.invite).console == confirmedConsole else { throw NodeError("Confirm the new fabric Console before changing the invite.") }
                }
                try profile.config.validate()
                try budget.validate(starting: profile.config, running: [])
                if let current = running[profile.id] {
                    guard !["preparing", "starting", "stopping"].contains(current.state) else { throw NodeError("Wait for this node's lifecycle operation to finish before changing its settings.") }
                    guard profile.config.memoryGiB <= current.profile.config.idleMemoryGiB else {
                        throw NodeError("Stop this node before raising its reservation beyond the running VM's maximum memory.")
                    }
                }
                try store.update(profile)
                if let session = running[profile.id]?.session { try session.send(.memory(profile.config.memoryGiB)) }
            case .start(let id):
                try requireMigrated()
                try start(id)
            case .stop(let id):
                _ = try store.profile(id: id)
                try stop(id)
            case .reset(let id):
                try requireStopped(id)
                let paths = try store.paths(for: id)
                try Provision.detachChannel(at: paths)
                let cleared = try Provision.reset(paths)
                var profile = try store.profile(id: id)
                profile.config = cleared
                profile.autoStart = false
                try store.update(profile)
                lastErrors[id] = nil
            case .remove(let id):
                try requireStopped(id)
                try Provision.detachChannel(at: store.paths(for: id))
                try store.remove(id: id)
                lastErrors[id] = nil
            case .stopAll:
                for id in Array(running.keys) { try stop(id) }
            }
            return RunnerReply(snapshot: try snapshot())
        } catch { return RunnerReply(error: error.localizedDescription) }
    }

    public func stopForTermination() async {
        for id in Array(running.keys) { try? stop(id) }
        // Worker EOF/SIGTERM also requests graceful shutdown. Do not let launchd kill the process
        // group before workers have had their own guest-stop window.
        for _ in 0..<140 {
            if running.isEmpty { return }
            try? await Task.sleep(for: .milliseconds(500))
        }
    }

    private func requireMigrated() throws {
        if VMLock.holder(Paths(root: locations.root)) != nil {
            throw NodeError("The original CLI-owned VM is still running. Stop it gracefully once before migrating it to the shared runner; do not reset it.")
        }
        if !migrationError.isEmpty {
            _ = try store.importLegacy()
            migrationError = ""
        }
    }

    private func requireStopped(_ id: String) throws {
        _ = try store.profile(id: id)
        guard running[id] == nil, VMLock.holder(try store.paths(for: id)) == nil else {
            throw NodeError("Stop this node before deleting or resetting its VM.")
        }
    }

    private func start(_ id: String) throws {
        if running[id] != nil { return } // concurrent app/CLI starts join the same operation
        let profile = try store.profile(id: id)
        if let blocker = profile.networkAccess.startupBlocker { throw NodeError(blocker) }
        let paths = try store.paths(for: id)
        guard VMLock.holder(paths) == nil else { throw NodeError("This profile already has a VM worker. Wait for it to stop before starting another.") }
        // A former supervisor may have exited while its workers are still shutting down.
        // Their actual maxima are unknown; admitting another VM could overcommit the host.
        for other in try store.profiles() where running[other.id] == nil {
            if VMLock.holder(try store.paths(for: other.id)) != nil {
                throw NodeError("A previous VM worker is still shutting down. Wait before starting another node.")
            }
        }
        try budget.validate(starting: profile.config, running: running.values.map { $0.profile.config })
        guard FileManager.default.isExecutableFile(atPath: locations.workerExecutable.path) else {
            throw NodeError("The VM worker is missing from this installation.")
        }
        if Provision.guest(paths) == .none { _ = try Invite(profile.config.invite) }
        // Reserve maximum resources BEFORE suspending for download/provisioning.
        let record = RunRecord(profile: profile)
        let generation = record.generation
        running[id] = record
        lastErrors[id] = nil
        let task = Task.detached { [weak self] in
            guard let self else { return }
            do {
                try await Provision.run(profile.config, at: paths, hostname: profile.hostname,
                                        confirm: { _ in true }, // create/configure already confirmed this exact Console
                                        log: { line in Task { await self.progress(id, generation: generation, line: line) } })
                try Task.checkCancellation()
                try Provision.prepareChannel(at: paths)
                try Task.checkCancellation()
                try await self.launch(id, generation: generation, paths: paths)
            } catch {
                await self.preparationFailed(id, generation: generation, error: error)
            }
        }
        running[id]?.preparation = task
    }

    private func launch(_ id: String, generation: UUID, paths: Paths) throws {
        guard var record = running[id], record.generation == generation, record.state != "stopping" else { throw CancellationError() }
        let session = VMWorkerSession(executable: locations.workerExecutable)
        Task { [weak self] in
            for await status in session.statuses { await self?.workerStatus(id, generation: generation, status) }
            if let result = session.result {
                await self?.workerExited(id, generation: generation, code: result.code, diagnostic: result.diagnostic)
            }
        }
        record.session = session
        record.preparation = nil
        record.state = "starting"
        record.message = "Starting the VM"
        running[id] = record
        do {
            try session.launch(directory: paths.root, config: record.profile.config, access: record.profile.networkAccess)
        } catch {
            if session.pid == nil {
                running[id] = nil
                try? Provision.detachChannel(at: paths)
            } else if session.isRunning {
                session.terminate()
            }
            lastErrors[id] = error.localizedDescription
            throw error
        }
    }

    private func stop(_ id: String) throws {
        guard var record = running[id] else { return }
        guard record.state != "stopping" else { return }
        record.state = "stopping"
        record.message = "Asking the guest to shut down"
        record.preparation?.cancel()
        running[id] = record
        if let session = record.session { try session.send(.stop) }
    }

    private func progress(_ id: String, generation: UUID, line: String) {
        guard running[id]?.generation == generation, running[id]?.state == "preparing" else { return }
        running[id]?.message = String(printable(line).prefix(1024))
    }

    private func preparationFailed(_ id: String, generation: UUID, error: any Error) {
        guard running[id]?.generation == generation else { return }
        // A launched session owns cleanup until its final statuses and exit have been consumed.
        if running[id]?.session?.pid != nil {
            return
        }
        let cancelled = running[id]?.state == "stopping" || error is CancellationError
        running[id] = nil
        lastErrors[id] = cancelled ? nil : error.localizedDescription
        if let paths = try? store.paths(for: id) { try? Provision.detachChannel(at: paths) }
    }

    private func workerStatus(_ id: String, generation: UUID, _ status: VMWorkerStatus) {
        guard var record = running[id], record.generation == generation, record.session?.pid == status.pid else { return }
        if record.state != "stopping" { record.state = status.state == "stopped" ? "stopping" : status.state }
        record.mode = status.mode
        record.message = String(printable(status.message).prefix(4096))
        if let memory = status.reservedMemoryGiB,
           memory >= 2 && memory <= record.profile.config.idleMemoryGiB { record.profile.config.memoryGiB = memory }
        if status.state == "error" { lastErrors[id] = record.message }
        running[id] = record
    }

    private func workerExited(_ id: String, generation: UUID, code: Int32, diagnostic: String) {
        guard let record = running[id], record.generation == generation else { return }
        running[id] = nil
        if code != 0 || (record.state != "stopping" && record.state != "stopped") {
            lastErrors[id] = !record.message.isEmpty && record.state == "error" ? record.message :
                "VM worker exited (\(code)). \(String(printable(diagnostic).suffix(2048)))"
        }
        if let paths = try? store.paths(for: id) {
            do { try Provision.detachChannel(at: paths) }
            catch { lastErrors[id] = "VM stopped, but its bounded channel could not be detached: \(error.localizedDescription)" }
        }
    }

    private func snapshot() throws -> RunnerSnapshot {
        let now = Date()
        let nodes = try store.profiles().map { stored -> NodeSnapshot in
            let paths = try store.paths(for: stored.id)
            let guest = GuestStatus.read(paths)
            let record = running[stored.id]
            let orphan = record == nil ? VMLock.holder(paths) : nil
            let modified = (try? FileManager.default.attributesOfItem(atPath: paths.share.appending(path: "nebula.json").path)[.modificationDate]) as? Date
            let fresh = modified.map { now.timeIntervalSince($0) >= -5 && now.timeIntervalSince($0) <= 90 } ?? false
            let generation: String = switch Provision.guest(paths) { case .none: "none"; case .current: "current"; case .old: "old" }
            var exposed = stored
            exposed.config.invite = "" // local control responses/logging never need the enrollment secret
            var actual = record?.profile.config
            actual?.invite = ""
            return NodeSnapshot(profile: exposed, consoleURL: (try? Invite(stored.config.invite).console) ?? "",
                                state: record?.state ?? (orphan != nil ? "stopping" : (lastErrors[stored.id] == nil ? "stopped" : "error")),
                                mode: record?.mode ?? "", message: record?.message ?? lastErrors[stored.id] ?? (orphan == nil ? "" : "A previous worker still holds this VM; waiting for its shutdown."),
                                pid: record?.session?.pid ?? orphan, runningConfig: actual, runningNetworkAccess: record?.profile.networkAccess,
                                guestGeneration: generation, meshIP: guest.meshIP, meshName: guest.meshName,
                                meshConnected: record != nil && fresh && guest.meshConnected, guestStatusFresh: record != nil && fresh,
                                join: guest.join, lastJoinLine: guest.lastLine)
        }
        return RunnerSnapshot(nodes: nodes, cpuBudget: budget.cpuCount, memoryBudgetGiB: budget.memoryGiB,
                              reservedCPUs: running.values.reduce(0) { $0 + $1.profile.config.idleCPUs },
                              reservedMemoryGiB: running.values.reduce(0) { $0 + $1.profile.config.idleMemoryGiB + ResourceBudget.workerMemoryGiB },
                              legacyPID: VMLock.holder(Paths(root: locations.root)), message: migrationError)
    }
}

/// Private pipes are capabilities: workers receive no connection to the profile-management service.
private final class VMWorkerSession: @unchecked Sendable {
    private let process = Process()
    private let input = Pipe()
    private let output = Pipe()
    private let errors = Pipe()
    private let lock = NSLock()
    private let readers = DispatchGroup()
    private var outputBuffer = Data()
    private var diagnostic = Data()
    private var exitCode: Int32?
    private var launchedPID: Int32?
    private let continuation: AsyncStream<VMWorkerStatus>.Continuation
    let statuses: AsyncStream<VMWorkerStatus>

    init(executable: URL) {
        process.executableURL = executable
        (statuses, continuation) = AsyncStream.makeStream(bufferingPolicy: .bufferingOldest(128))
    }
    // Retain identity after exit so buffered final statuses still belong to this worker.
    var pid: Int32? { launchedPID }
    var isRunning: Bool { process.isRunning }
    func terminate() { if process.isRunning { process.terminate() } }
    var result: (code: Int32, diagnostic: String)? {
        lock.lock()
        defer { lock.unlock() }
        return exitCode.map { ($0, String(decoding: diagnostic, as: UTF8.self)) }
    }

    func launch(directory: URL, config: NodeConfig, access: NodeNetworkAccess) throws {
        let bookmark: Data
        readers.enter()
        readers.enter()
        do {
            bookmark = try directory.bookmarkData(options: [], includingResourceValuesForKeys: nil, relativeTo: nil)
            process.standardInput = input
            process.standardOutput = output
            process.standardError = errors
            process.terminationHandler = { [self] child in
                lock.lock()
                exitCode = child.terminationStatus
                lock.unlock()
                // Termination does not drain pipes. Finish only after both readers reach EOF.
                readers.notify(queue: .global()) { [self] in
                    process.terminationHandler = nil
                    continuation.finish()
                }
            }
            try process.run()
            launchedPID = process.processIdentifier
        } catch {
            process.terminationHandler = nil
            readers.leave()
            readers.leave()
            continuation.finish()
            throw error
        }
        input.fileHandleForReading.closeFile()
        output.fileHandleForWriting.closeFile()
        errors.fileHandleForWriting.closeFile()
        DispatchQueue.global().async { [self] in
            defer { readers.leave() }
            while true {
                let data = output.fileHandleForReading.availableData
                if data.isEmpty { break }
                receive(data)
            }
        }
        DispatchQueue.global().async { [self] in
            defer { readers.leave() }
            while true {
                let data = errors.fileHandleForReading.availableData
                if data.isEmpty { break }
                receiveError(data)
            }
        }
        try send(.start(VMWorkerStart(directoryBookmark: bookmark, config: config, networkAccess: access)))
    }

    func send(_ command: VMWorkerCommand) throws {
        var data = try JSONEncoder().encode(command)
        data.append(0x0a)
        lock.lock()
        defer { lock.unlock() }
        guard process.isRunning else { throw NodeError("The VM worker is no longer running.") }
        try input.fileHandleForWriting.write(contentsOf: data)
    }

    private func receive(_ data: Data) {
        outputBuffer.append(data)
        var invalid = outputBuffer.count > 1024 * 1024
        while !invalid, let end = outputBuffer.firstIndex(of: 0x0a) {
            let line = outputBuffer.prefix(upTo: end)
            if let status = try? JSONDecoder().decode(VMWorkerStatus.self, from: line) {
                if case .dropped = continuation.yield(status) { invalid = true }
            } else { invalid = true }
            outputBuffer.removeSubrange(...end)
        }
        if invalid {
            outputBuffer.removeAll(keepingCapacity: false)
            receiveError(Data("Invalid, oversized or excessive VM worker response".utf8))
            if process.isRunning { process.terminate() } // SIGTERM requests guest shutdown.
        }
    }

    private func receiveError(_ data: Data) {
        lock.lock()
        diagnostic.append(data.suffix(8192))
        if diagnostic.count > 8192 { diagnostic.removeFirst(diagnostic.count - 8192) }
        lock.unlock()
    }

    deinit {
        try? input.fileHandleForWriting.close() // EOF is a controlled shutdown request.
    }
}
