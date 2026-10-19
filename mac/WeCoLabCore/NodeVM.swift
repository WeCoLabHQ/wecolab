import CoreGraphics
import Darwin
import Foundation
import Virtualization

/// Publication state belongs to the share, not to the requested input state. An absent mode is
/// non-idle to the node agent. The writer/remover seams allow filesystem failures to be tested
/// without starting a VM or modifying a user's share.
enum ModePublicationError: Error {
    case unsafeIdle(Error)
}

final class ModePublication {
    private let url: URL
    private let write: (Data, URL) throws -> Void
    private let remove: (URL) throws -> Void
    private(set) var mode = ""

    init(url: URL, write: @escaping (Data, URL) throws -> Void = { try $0.write(to: $1, options: .atomic) },
         remove: @escaping (URL) throws -> Void = { try FileManager.default.removeItem(at: $0) }) {
        self.url = url
        self.write = write
        self.remove = remove
    }

    func reset() { mode = "" }

    func publish(_ next: String) throws {
        guard next != mode else { return }
        if next == "active" {
            // A previous process may have left idle behind. Attempt removal even if a
            // permissions error would make a file-existence check report false.
            mode = ""
            do {
                try remove(url)
            } catch {
                if let cocoa = error as? CocoaError, cocoa.code == .fileNoSuchFile {
                    // Missing mode is the guest agent's existing non-idle backstop.
                } else {
                    throw ModePublicationError.unsafeIdle(error)
                }
            }
        }
        try write(Data(next.utf8), url)
        mode = next
    }
}

/// Consume console output without enqueuing one actor task per guest write.
private final class BoundedCapture: @unchecked Sendable {
    private let lock = NSLock()
    private let capacity: Int
    private var tail = Data()

    init(capacity: Int) { self.capacity = capacity }

    func append(_ data: Data) {
        lock.lock()
        tail.append(data.suffix(capacity))
        if tail.count > capacity { tail.removeFirst(tail.count - capacity) }
        lock.unlock()
    }

    func snapshot() -> Data {
        lock.lock()
        defer { lock.unlock() }
        return tail
    }
}

/// Process termination is independent of datagram socket EOF: SOCK_DGRAM peers do not
/// provide a reliable notification when the other end closes.
enum BackendProcessTermination {
    static func stop(_ process: Process) {
        guard process.isRunning else { process.waitUntilExit(); return }
        process.terminate()
        let deadline = Date().addingTimeInterval(1)
        while process.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.01)
        }
        if process.isRunning { kill(process.processIdentifier, SIGKILL) }
        process.waitUntilExit()
    }
}

/// The node VM. The actor runs on the VM's own serial queue, so every Virtualization call
/// happens on the queue the VM was created with.
public actor NodeVM {
    private let queue = DispatchSerialQueue(label: "io.wecolab.vm")
    public nonisolated var unownedExecutor: UnownedSerialExecutor { queue.asUnownedSerialExecutor() }

    private var vm: VZVirtualMachine?
    private var config = NodeConfig()
    private let paths: Paths
    private let networkAccess: NodeNetworkAccess
    private let networkExecutable: URL
    private let publication: ModePublication
    private var backend: Process?
    private var networkSocket: FileHandle?
    private var serialPipe: Pipe?
    private let serialCapture = BoundedCapture(capacity: 1 << 20)
    private var backendErrorPipe: Pipe?
    private let backendErrorCapture = BoundedCapture(capacity: 64 << 10)
    private var backendFailed = false
    public var mode: String { publication.mode }

    public init(paths: Paths, networkAccess: NodeNetworkAccess, networkExecutable: URL) {
        self.paths = paths
        self.networkAccess = networkAccess
        self.networkExecutable = networkExecutable
        publication = ModePublication(url: paths.share.appending(path: "mode"))
    }

    public var state: String {
        guard let vm else { return "stopped" }
        switch vm.state {
        case .starting: return "starting"
        case .running: return "running"
        case .stopping: return "stopping"
        case .error: return "error"
        case .stopped: return "stopped"
        default: return "busy"
        }
    }

    public var isRunning: Bool { vm.map { [.starting, .running, .stopping].contains($0.state) } ?? false }

    public func start(_ config: NodeConfig) async throws {
        guard vm == nil, backend == nil else { throw NodeError("VM is already starting or running") }
        if let blocker = networkAccess.startupBlocker { throw NodeError(blocker) }
        try config.validate()
        try Self.validateShare(paths: paths)
        backendFailed = false
        do {
            try Self.prepareIdentity(paths: paths)
            let mac = try Self.macAddress(at: paths.macAddress)
            let socket = try startBackend(mac: mac)
            let serial = Pipe()
            serial.fileHandleForReading.readabilityHandler = { [serialCapture] handle in
                let chunk = handle.availableData
                guard !chunk.isEmpty else { handle.readabilityHandler = nil; return }
                serialCapture.append(chunk)
            }
            serialPipe = serial
            let configuration = try Self.configuration(config, paths: paths, socket: socket, serial: serial)
            let vm = VZVirtualMachine(configuration: configuration, queue: queue)
            self.vm = vm // Retain even if start fails until confirmed stopped.
            try await withCheckedThrowingContinuation { (done: CheckedContinuation<Void, Error>) in
                vm.start { done.resume(with: $0) }
            }
            self.config = config
            publication.reset()
            guard !backendFailed, backend?.isRunning == true else {
                throw NodeError("network backend exited while starting")
            }
            try await tick()
        } catch {
            do { try await stopImmediately() } catch { throw error }
            terminateBackend()
            throw error
        }
    }

    /// Requests a guest shutdown, then forces a stop after one minute. Never drop the VM
    /// reference or its ownership lock while the hypervisor still reports it running.
    public func stop() async {
        guard let vm else { terminateBackend(); return }
        if vm.canRequestStop { try? vm.requestStop() }
        for _ in 0..<60 where isRunning { try? await Task.sleep(for: .seconds(1)) }
        if vm.state == .running || vm.state == .starting || vm.state == .stopping {
            if vm.canStop {
                await withCheckedContinuation { (done: CheckedContinuation<Void, Never>) in vm.stop { _ in done.resume() } }
            }
        }
        if vm.state == .stopped || vm.state == .error {
            self.vm = nil
            terminateBackend()
        }
    }

    private func stopImmediately() async throws {
        guard let vm else { return }
        guard vm.canStop else {
            if vm.state == .stopped || vm.state == .error { self.vm = nil; return }
            throw NodeError("VM cannot be stopped; retaining its lock")
        }
        try await withCheckedThrowingContinuation { (done: CheckedContinuation<Void, Error>) in
            vm.stop { error in
                if let error { done.resume(throwing: error) }
                else { done.resume(returning: ()) }
            }
        }
        guard vm.state == .stopped || vm.state == .error else {
            throw NodeError("VM has not stopped; retaining its lock")
        }
        self.vm = nil
    }

    /// The reserved memory can change while the guest runs, up to the size the VM started with
    /// (the balloon cannot go above it); the balloon follows on the next tick.
    public func reserve(memoryGiB: Int) throws {
        guard vm?.state == .running, memoryGiB >= 2, memoryGiB <= config.idleMemoryGiB else {
            throw NodeError("memory exceeds the running VM maximum")
        }
        config.memoryGiB = memoryGiB
    }

    /// The VM is sized to the idle profile. While the person is active the balloon holds the guest to
    /// the reserved memory; when idle it is released. CPU needs no such step: while the person is active
    /// the idle taint evicts every pod but the node's own, so the guest has nothing to run. The VM
    /// process is never put in the background state (PRIO_DARWIN_BG): that also throttles its network
    /// and disk (setpriority(2)), which kept the guest's Nebula tunnels from holding, so the site could
    /// not reach the node.
    /// Applied on every tick, not only on change: a target set before the guest's balloon driver
    /// loads is lost, and the guest must see it again once it can act on it.
    public func tick() async throws {
        guard let vm, vm.state == .running else { return }
        // kCGAnyInputEventType (~0): keyboard, mouse or tablet. `.null` does not measure input.
        let idle = CGEventSource.secondsSinceLastEventType(.combinedSessionState, eventType: CGEventType(rawValue: ~0)!)
        let active = idle < Double(config.idleMinutes * 60)
        (vm.memoryBalloonDevices.first as? VZVirtioTraditionalMemoryBalloonDevice)?.targetVirtualMachineMemorySize = Self.balloonTarget(active: active, config: config)
        do {
            try publication.publish(active ? "active" : "idle")
        } catch ModePublicationError.unsafeIdle(let error) {
            try await stopImmediately()
            throw error
        }
    }

    /// Bytes the guest keeps: the reservation while the person is active, the whole VM when idle.
    static func balloonTarget(active: Bool, config: NodeConfig) -> UInt64 {
        UInt64(active ? config.memoryGiB : config.idleMemoryGiB) << 30
    }

    static func configuration(_ c: NodeConfig, paths: Paths, socket: FileHandle, serial: Pipe) throws -> VZVirtualMachineConfiguration {
        let conf = VZVirtualMachineConfiguration()
        conf.cpuCount = c.idleCPUs
        conf.memorySize = UInt64(c.idleMemoryGiB) << 30

        let platform = VZGenericPlatformConfiguration()
        platform.machineIdentifier = try existing(paths.machineID, VZGenericMachineIdentifier.init(dataRepresentation:))
        conf.platform = platform
        let boot = VZEFIBootLoader()
        guard FileManager.default.fileExists(atPath: paths.efiVars.path) else { throw NodeError("missing VM EFI variables") }
        boot.variableStore = VZEFIVariableStore(url: paths.efiVars)
        conf.bootLoader = boot

        var disks = [VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(
            url: paths.disk, readOnly: false, cachingMode: .cached, synchronizationMode: .full))]
        if FileManager.default.fileExists(atPath: paths.seed.path) {
            disks.append(VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(url: paths.seed, readOnly: true)))
        }
        conf.storageDevices = disks

        let net = VZVirtioNetworkDeviceConfiguration()
        let attachment = VZFileHandleNetworkDeviceAttachment(fileHandle: socket)
        attachment.maximumTransmissionUnit = 1500
        net.attachment = attachment
        net.macAddress = try macAddress(at: paths.macAddress)
        conf.networkDevices = [net]

        try validateShare(paths: paths)
        let share = VZVirtioFileSystemDeviceConfiguration(tag: "wecolab")
        share.share = VZSingleDirectoryShare(directory: VZSharedDirectory(url: paths.share, readOnly: false))
        conf.directorySharingDevices = [share]

        conf.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        conf.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        let console = VZVirtioConsoleDeviceSerialPortConfiguration()
        console.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: nil, fileHandleForWriting: serial.fileHandleForWriting)
        conf.serialPorts = [console]

        try conf.validate()
        return conf
    }

    /// The worker owns the profile lock before creating identities. Never replace an existing identity.
    static func prepareIdentity(paths: Paths) throws {
        try paths.validateFiles()
        let fm = FileManager.default
        if !fm.fileExists(atPath: paths.macAddress.path) {
            try Data(VZMACAddress.randomLocallyAdministered().string.utf8).write(to: paths.macAddress, options: .withoutOverwriting)
        }
        _ = try macAddress(at: paths.macAddress)
        if !fm.fileExists(atPath: paths.machineID.path) {
            try VZGenericMachineIdentifier().dataRepresentation.write(to: paths.machineID, options: .withoutOverwriting)
        }
        _ = try existing(paths.machineID, VZGenericMachineIdentifier.init(dataRepresentation:))
        if !fm.fileExists(atPath: paths.efiVars.path) {
            _ = try VZEFIVariableStore(creatingVariableStoreAt: paths.efiVars)
        }
    }

    private static func existing<T>(_ url: URL, _ decode: (Data) -> T?) throws -> T {
        guard let value = decode(try Data(contentsOf: url)) else {
            throw NodeError("invalid VM identity: \(url.lastPathComponent)")
        }
        return value
    }

    private static func macAddress(at url: URL) throws -> VZMACAddress {
        try existing(url, { VZMACAddress(string: String(decoding: $0, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)) })
    }

    /// The guest-writable virtio-fs export must be the supervisor's mounted 16 MiB HFS+ image.
    /// A plain directory here gives a compromised guest an unbounded host filesystem.
    static func validateShare(paths: Paths) throws {
        var fs = statfs()
        guard statfs(paths.share.path, &fs) == 0 else { throw NodeError("bounded channel is not mounted") }
        let mountSize = MemoryLayout.size(ofValue: fs.f_mntonname)
        let typeSize = MemoryLayout.size(ofValue: fs.f_fstypename)
        let mountedAt = withUnsafePointer(to: &fs.f_mntonname) {
            $0.withMemoryRebound(to: CChar.self, capacity: mountSize) { String(cString: $0) }
        }
        let fileSystem = withUnsafePointer(to: &fs.f_fstypename) {
            $0.withMemoryRebound(to: CChar.self, capacity: typeSize) { String(cString: $0) }
        }
        let maximum = UInt64(16 << 20)
        guard URL(fileURLWithPath: mountedAt).resolvingSymlinksInPath() == paths.share.resolvingSymlinksInPath(),
              fileSystem.lowercased() == "hfs",
              fs.f_bsize > 0, fs.f_blocks <= maximum / UInt64(fs.f_bsize) else {
            throw NodeError("bounded channel image is missing or exceeds 16 MiB")
        }
    }

    private func startBackend(mac: VZMACAddress) throws -> FileHandle {
        var fds: [Int32] = [-1, -1]
        guard socketpair(AF_UNIX, SOCK_DGRAM, 0, &fds) == 0 else { throw NodeError("cannot create network socketpair") }
        let socket = FileHandle(fileDescriptor: fds[0], closeOnDealloc: true)
        let childSocket = FileHandle(fileDescriptor: fds[1], closeOnDealloc: true)
        for fd in fds {
            var sendSize: Int32 = 256 << 10
            var receiveSize: Int32 = 1 << 20
            guard setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &sendSize, socklen_t(MemoryLayout<Int32>.size)) == 0,
                  setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &receiveSize, socklen_t(MemoryLayout<Int32>.size)) == 0 else {
                throw NodeError("cannot size network socket buffers")
            }
        }
        let process = Process()
        process.executableURL = networkExecutable
        process.arguments = ["--mac", mac.string, "--access", networkAccess.rawValue, "--parent-pid", String(getpid())]
        process.standardInput = childSocket
        let ready = Pipe()
        let diagnostics = Pipe()
        process.standardOutput = ready
        process.standardError = diagnostics
        diagnostics.fileHandleForReading.readabilityHandler = { [backendErrorCapture] handle in
            let chunk = handle.availableData
            guard !chunk.isEmpty else { handle.readabilityHandler = nil; return }
            backendErrorCapture.append(chunk)
        }
        backendErrorPipe = diagnostics
        process.terminationHandler = { [weak self] _ in Task { await self?.backendExited() } }
        try process.run()
        backend = process
        childSocket.closeFile()
        // READY is the only permitted stdout protocol. Bound both the read and its wait.
        var descriptor = pollfd(fd: ready.fileHandleForReading.fileDescriptor, events: Int16(POLLIN | POLLHUP), revents: 0)
        guard poll(&descriptor, 1, 10_000) > 0 else { throw NodeError("network backend did not become ready") }
        var bytes = [UInt8](repeating: 0, count: 64)
        let count = read(descriptor.fd, &bytes, bytes.count)
        guard count == 6, bytes.prefix(Int(count)).elementsEqual(Array("READY\n".utf8)), process.isRunning else {
            throw NodeError("network backend rejected startup")
        }
        networkSocket = socket
        ready.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let unexpected = handle.availableData
            handle.readabilityHandler = nil
            if !unexpected.isEmpty { Task { await self?.backendProtocolFailed() } }
        }
        return socket
    }

    private func backendProtocolFailed() async {
        backendFailed = true
        if vm != nil { try? await stopImmediately() }
        if vm == nil { terminateBackend() }
    }

    private func backendExited() async {
        guard backend != nil else { return }
        backendFailed = true
        if vm != nil { try? await stopImmediately() }
        if vm == nil { terminateBackend() }
    }

    /// Invoked before any intentional worker exit, including a VM that refused to stop.
    /// Does not touch the VM reference or release its ownership lock.
    public func forceTerminateBackend() {
        terminateBackend()
    }

    private func terminateBackend() {
        serialPipe?.fileHandleForReading.readabilityHandler = nil
        backendErrorPipe?.fileHandleForReading.readabilityHandler = nil
        let serialTail = serialCapture.snapshot()
        if !serialTail.isEmpty { try? serialTail.write(to: paths.console, options: .atomic) }
        let backendTail = backendErrorCapture.snapshot()
        if !backendTail.isEmpty {
            try? backendTail.write(to: paths.root.appending(path: "network.log"), options: .atomic)
        }
        serialPipe = nil
        backendErrorPipe = nil
        networkSocket = nil
        if let backend { BackendProcessTermination.stop(backend) }
        backend = nil
    }

    public var networkIsHealthy: Bool { !backendFailed && backend?.isRunning == true }
}
