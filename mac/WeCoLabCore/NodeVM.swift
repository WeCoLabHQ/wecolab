import CoreGraphics
import Foundation
import Virtualization

/// The node VM. The actor runs on the VM's own serial queue, so every Virtualization call
/// happens on the queue the VM was created with.
public actor NodeVM {
    private let queue = DispatchSerialQueue(label: "io.wecolab.vm")
    public nonisolated var unownedExecutor: UnownedSerialExecutor { queue.asUnownedSerialExecutor() }

    private var vm: VZVirtualMachine?
    private var config = NodeConfig()  // as started; only memoryGiB follows the app while running
    public private(set) var mode = ""  // "active" or "idle", once running

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
        mode = ""
        tick()
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
    public func tick() {
        guard let vm, vm.state == .running else { return }
        // kCGAnyInputEventType (~0): keyboard, mouse or tablet. `.null` does not measure input.
        let idle = CGEventSource.secondsSinceLastEventType(.combinedSessionState, eventType: CGEventType(rawValue: ~0)!)
        let active = idle < Double(config.idleMinutes * 60)
        (vm.memoryBalloonDevices.first as? VZVirtioTraditionalMemoryBalloonDevice)?.targetVirtualMachineMemorySize = Self.balloonTarget(active: active, config: config)
        let next = active ? "active" : "idle"
        if next != mode {
            mode = next
            try? Data(next.utf8).write(to: Paths.user.share.appending(path: "mode"), options: .atomic)
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
