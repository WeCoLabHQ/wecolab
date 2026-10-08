import CoreGraphics
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

/// The node VM. The actor runs on the VM's own serial queue, so every Virtualization call
/// happens on the queue the VM was created with.
public actor NodeVM {
    private let queue = DispatchSerialQueue(label: "io.wecolab.vm")
    public nonisolated var unownedExecutor: UnownedSerialExecutor { queue.asUnownedSerialExecutor() }

    private var vm: VZVirtualMachine?
    private var config = NodeConfig()  // as started; only memoryGiB follows the app while running
    private let publication = ModePublication(url: Paths.user.share.appending(path: "mode"))
    public var mode: String { publication.mode }

    public init() {}

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
        let vm = VZVirtualMachine(configuration: try Self.configuration(config), queue: queue)
        try await withCheckedThrowingContinuation { (done: CheckedContinuation<Void, Error>) in vm.start { done.resume(with: $0) } }
        self.vm = vm
        self.config = config
        publication.reset()
        do {
            try await tick()
        } catch {
            try await stopImmediately()  // retain a running VM if the hypervisor refuses to stop it
            throw error
        }
    }

    /// Asks the guest to power off, and pulls the plug only if it has not after a minute.
    public func stop() async {
        guard let vm else { return }
        if vm.canRequestStop { try? vm.requestStop() }
        for _ in 0..<60 where isRunning { try? await Task.sleep(for: .seconds(1)) }
        if vm.canStop {
            await withCheckedContinuation { (done: CheckedContinuation<Void, Never>) in vm.stop { _ in done.resume() } }
        }
        self.vm = nil
    }

    /// A stale idle file is unsafe when invalidation fails. Stop rather than leave the node
    /// workload-eligible while reporting that the Mac is active.
    private func stopImmediately() async throws {
        guard let vm else { return }
        guard vm.canStop else {
            if vm.state == .stopped || vm.state == .error { self.vm = nil; return }
            throw NodeError("mode invalidation failed and the VM cannot be stopped; keeping its lock")
        }
        try await withCheckedThrowingContinuation { (done: CheckedContinuation<Void, Error>) in
            vm.stop { error in
                if let error { done.resume(throwing: error) }
                else { done.resume(returning: ()) }
            }
        }
        self.vm = nil
    }

    /// The reserved memory can change while the guest runs, up to the size the VM started with
    /// (the balloon cannot go above it); the balloon follows on the next tick.
    public func reserve(memoryGiB: Int) { config.memoryGiB = min(memoryGiB, config.idleMemoryGiB) }

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

    static func configuration(_ c: NodeConfig) throws -> VZVirtualMachineConfiguration {
        let conf = VZVirtualMachineConfiguration()
        conf.cpuCount = c.idleCPUs
        conf.memorySize = UInt64(c.idleMemoryGiB) << 30

        let platform = VZGenericPlatformConfiguration()
        platform.machineIdentifier = try persisted(Paths.user.machineID, VZGenericMachineIdentifier.init(dataRepresentation:)) {
            VZGenericMachineIdentifier().dataRepresentation
        }
        conf.platform = platform
        let boot = VZEFIBootLoader()
        boot.variableStore = FileManager.default.fileExists(atPath: Paths.user.efiVars.path)
            ? VZEFIVariableStore(url: Paths.user.efiVars)
            : try VZEFIVariableStore(creatingVariableStoreAt: Paths.user.efiVars)
        conf.bootLoader = boot

        var disks = [VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(
            url: Paths.user.disk, readOnly: false, cachingMode: .cached, synchronizationMode: .full))]
        if FileManager.default.fileExists(atPath: Paths.user.seed.path) {
            disks.append(VZVirtioBlockDeviceConfiguration(attachment: try VZDiskImageStorageDeviceAttachment(url: Paths.user.seed, readOnly: true)))
        }
        conf.storageDevices = disks

        // A fixed MAC: cloud-init binds the guest's network config to the first one it saw.
        let net = VZVirtioNetworkDeviceConfiguration()
        net.attachment = VZNATNetworkDeviceAttachment()
        net.macAddress = try persisted(Paths.user.macAddress, { VZMACAddress(string: String(decoding: $0, as: UTF8.self)) }) {
            Data(VZMACAddress.randomLocallyAdministered().string.utf8)
        }
        conf.networkDevices = [net]

        let share = VZVirtioFileSystemDeviceConfiguration(tag: "wecolab")
        share.share = VZSingleDirectoryShare(directory: VZSharedDirectory(url: Paths.user.share, readOnly: false))
        conf.directorySharingDevices = [share]

        conf.memoryBalloonDevices = [VZVirtioTraditionalMemoryBalloonDeviceConfiguration()]
        conf.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        let console = VZVirtioConsoleDeviceSerialPortConfiguration()
        console.attachment = try VZFileSerialPortAttachment(url: Paths.user.console, append: false)
        conf.serialPorts = [console]

        try conf.validate()
        return conf
    }

    /// Reads a value saved on an earlier boot, or makes and saves one.
    static func persisted<T>(_ url: URL, _ decode: (Data) -> T?, make: () -> Data) throws -> T {
        if let data = try? Data(contentsOf: url), let value = decode(data) { return value }
        let data = make()
        try data.write(to: url)
        guard let value = decode(data) else { throw NodeError("cannot decode \(url.lastPathComponent)") }
        return value
    }

}
