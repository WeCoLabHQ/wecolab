import Foundation

public enum NodeNetworkAccess: String, Codable, CaseIterable, Sendable {
    case isolated, localNetwork

    /// User-space destination checks are not a kernel-enforced network security boundary.
    /// Do not enable this policy merely because a certificate or extension is present.
    public var startupBlocker: String? {
        guard self == .isolated else { return nil }
        return "Strict Mac/LAN isolation is unavailable in this build. No verified OS-enforced network firewall is implemented or installed. This node cannot start; trusted LAN mode is not a substitute for an untrusted fabric."
    }
}

public struct NodeProfile: Codable, Equatable, Sendable, Identifiable {
    public var id: String
    public var name: String
    public var hostname: String
    public var config: NodeConfig
    public var networkAccess: NodeNetworkAccess
    public var autoStart: Bool

    public init(id: String = UUID().uuidString.lowercased(), name: String, hostname: String? = nil,
                config: NodeConfig = NodeConfig(), networkAccess: NodeNetworkAccess = .isolated, autoStart: Bool = false) {
        self.id = id
        self.name = name
        self.hostname = hostname ?? "mac-" + id.prefix(12)
        self.config = config
        self.networkAccess = networkAccess
        self.autoStart = autoStart
    }
}

public struct ResourceBudget: Sendable {
    /// Admission also leaves room for Virtualization.framework and the bounded network helper.
    public static let workerMemoryGiB = 1
    public var cpuCount: Int
    public var memoryGiB: Int
    public init(cpuCount: Int, memoryGiB: Int) { self.cpuCount = cpuCount; self.memoryGiB = memoryGiB }
    public static var host: ResourceBudget {
        let memory = ProcessInfo.processInfo.physicalMemory / (1 << 30)
        return ResourceBudget(cpuCount: max(1, ProcessInfo.processInfo.activeProcessorCount - 1),
                              memoryGiB: max(2, Int(memory > 4 ? memory - 4 : 2)))
    }
    public func validate(starting: NodeConfig, running: [NodeConfig]) throws {
        try starting.validate()
        guard cpuCount > 0, memoryGiB > 0 else { throw NodeError("no VM resources available") }
        var cpus = starting.idleCPUs, memory = starting.idleMemoryGiB + Self.workerMemoryGiB
        for config in running {
            try config.validate()
            let (nextCPU, cpuOverflow) = cpus.addingReportingOverflow(config.idleCPUs)
            let (nextMemory, memoryOverflow) = memory.addingReportingOverflow(config.idleMemoryGiB + Self.workerMemoryGiB)
            guard !cpuOverflow, !memoryOverflow else { throw NodeError("VM resource total overflow") }
            cpus = nextCPU; memory = nextMemory
        }
        guard cpus <= cpuCount, memory <= memoryGiB else {
            throw NodeError("running VMs exceed the host's available maximum CPU or memory")
        }
    }
}

/// Owned by the serialized supervisor. File names are derived exclusively from checked UUIDs.
public final class ProfileStore {
    public let root: URL
    private var directory: URL { root.appending(path: "profiles", directoryHint: .isDirectory) }
    private var journal: URL { root.appending(path: "legacy-import.json") }
    private let fm = FileManager.default
    public init(root: URL) { self.root = root.standardizedFileURL }

    private func checked(_ url: URL, directory: Bool? = nil) throws -> Bool {
        var info = stat()
        if lstat(url.path, &info) != 0 {
            if errno == ENOENT { return false }
            throw NodeError("cannot inspect \(url.path)")
        }
        let kind = info.st_mode & S_IFMT
        guard kind != S_IFLNK, directory == nil || (directory! ? kind == S_IFDIR : kind == S_IFREG) else {
            throw NodeError("unexpected or symbolic-link registry path: \(url.path)")
        }
        return true
    }
    private func base() throws {
        if try !checked(root, directory: true) {
            try fm.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        }
        if try !checked(directory, directory: true) {
            try fm.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        }
    }
    private func validID(_ id: String) throws {
        guard let uuid = UUID(uuidString: id), uuid.uuidString.lowercased() == id else { throw NodeError("invalid profile ID") }
    }
    private func validate(_ profile: NodeProfile) throws {
        try validID(profile.id)
        try profile.config.validate()
        guard !profile.name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
              profile.name == profile.name.trimmingCharacters(in: .whitespacesAndNewlines),
              profile.name.count <= 80, !profile.name.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
            throw NodeError("invalid profile name")
        }
        guard profile.hostname.count <= 30, profile.hostname.wholeMatch(of: /[a-z0-9]([-a-z0-9]*[a-z0-9])?/) != nil else {
            throw NodeError("invalid guest hostname")
        }
    }
    private func file(_ id: String) throws -> URL {
        try validID(id)
        return directory.appending(path: id, directoryHint: .isDirectory).appending(path: "profile.json")
    }
    private func read(_ id: String) throws -> NodeProfile {
        let url = try file(id)
        guard try checked(url.deletingLastPathComponent(), directory: true), try checked(url, directory: false) else {
            throw NodeError("unknown profile \(id)")
        }
        let profile = try JSONDecoder().decode(NodeProfile.self, from: Data(contentsOf: url))
        try validate(profile)
        guard profile.id == id else { throw NodeError("profile ID mismatch") }
        return profile
    }
    public func profiles() throws -> [NodeProfile] {
        try base()
        return try fm.contentsOfDirectory(atPath: directory.path).sorted().map { try read($0) }
    }
    public func profile(id: String) throws -> NodeProfile { try base(); return try read(id) }
    public func paths(for id: String) throws -> Paths {
        _ = try profile(id: id)
        return Paths(root: directory.appending(path: id, directoryHint: .isDirectory))
    }
    private func unique(_ profile: NodeProfile) throws {
        let others = try profiles().filter { $0.id != profile.id }
        guard !others.contains(where: { $0.name.caseInsensitiveCompare(profile.name) == .orderedSame || $0.hostname == profile.hostname }) else {
            throw NodeError("profile name or hostname already exists")
        }
    }
    private func persist(_ profile: NodeProfile) throws {
        let url = try file(profile.id)
        try JSONEncoder().encode(profile).write(to: url, options: .atomic)
        try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
    public func create(_ profile: NodeProfile) throws {
        try validate(profile)
        try base()
        try unique(profile)
        let dir = try file(profile.id).deletingLastPathComponent()
        guard try !checked(dir) else { throw NodeError("profile already exists") }
        try fm.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        do { try persist(profile) } catch { try? fm.removeItem(at: dir); throw error }
    }
    public func update(_ profile: NodeProfile) throws {
        try validate(profile)
        let existing = try self.profile(id: profile.id)
        guard profile.hostname == existing.hostname else { throw NodeError("guest hostname cannot change after creation") }
        try unique(profile)
        try persist(profile)
    }
    private func legacyHostname(_ legacy: Paths) throws -> String {
        guard try checked(legacy.seed, directory: false) else {
            for identity in [legacy.disk, legacy.machineID, legacy.macAddress, legacy.efiVars] {
                guard try !checked(identity) else { throw NodeError("legacy guest identity has no seed to recover its hostname") }
            }
            return guestHostname()
        }
        let mount = fm.temporaryDirectory.appending(path: "wecolab-seed-read-\(UUID().uuidString)")
        try fm.createDirectory(at: mount, withIntermediateDirectories: false)
        func hdiutil(_ arguments: [String]) throws {
            let process = Process()
            process.executableURL = URL(filePath: "/usr/bin/hdiutil")
            process.arguments = arguments
            process.standardOutput = FileHandle.nullDevice
            process.standardError = FileHandle.nullDevice
            try process.run()
            process.waitUntilExit()
            guard process.terminationStatus == 0 else { throw NodeError("cannot inspect legacy guest hostname") }
        }
        do { try hdiutil(["attach", "-readonly", "-quiet", "-nobrowse", "-mountpoint", mount.path, legacy.seed.path]) }
        catch { try? fm.removeItem(at: mount); throw error }
        var attached = true
        defer {
            if attached { attached = (try? hdiutil(["detach", "-quiet", mount.path])) == nil }
            if !attached { try? fm.removeItem(at: mount) }
        }
        let metadata = try String(contentsOf: mount.appending(path: "meta-data"), encoding: .utf8)
        guard let line = metadata.split(separator: "\n").first(where: { $0.hasPrefix("local-hostname: ") }) else {
            throw NodeError("legacy guest seed lacks its hostname")
        }
        let hostname = String(line.dropFirst("local-hostname: ".count))
        guard hostname.count <= 30, hostname.wholeMatch(of: /[a-z0-9]([-a-z0-9]*[a-z0-9])?/) != nil else {
            throw NodeError("legacy guest hostname is invalid")
        }
        try hdiutil(["detach", "-quiet", mount.path])
        attached = false
        return hostname
    }

    public func remove(id: String) throws {
        let paths = try paths(for: id)
        let lock = try VMLock.acquire(paths)
        defer { VMLock.release(lock) }
        try fm.removeItem(at: paths.root)
    }

    /// The journal is written before any move; each move is idempotent on retry and the
    /// profile record is published only after all original files have arrived.
    public func importLegacy() throws -> NodeProfile? {
        try base()
        let legacy = Paths(root: root)
        let pending = try checked(journal, directory: false)
        let oldConfig = try checked(legacy.config, directory: false)
        guard pending || oldConfig else { return nil }
        let lock = try VMLock.acquire(legacy)
        defer { VMLock.release(lock) }
        struct LegacyConfig: Decodable { let startNodeOnLaunch: Bool? }
        struct Journal: Codable { let profile: NodeProfile }
        let record: Journal
        if pending {
            record = try JSONDecoder().decode(Journal.self, from: Data(contentsOf: journal))
            try validate(record.profile)
        } else {
            let config = try NodeConfig.load(legacy)
            let autoStart = (try JSONDecoder().decode(LegacyConfig.self, from: Data(contentsOf: legacy.config))).startNodeOnLaunch ?? false
            let profile = NodeProfile(name: "Mac node", hostname: try legacyHostname(legacy), config: config,
                                      networkAccess: .localNetwork, autoStart: autoStart)
            try unique(profile)
            record = Journal(profile: profile)
            try JSONEncoder().encode(record).write(to: journal, options: .atomic)
            try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: journal.path)
        }
        let profile = record.profile
        let destination = Paths(root: try file(profile.id).deletingLastPathComponent())
        if try !checked(destination.root, directory: true) {
            try fm.createDirectory(at: destination.root, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        }
        let names = ["config.json", "disk.img", "seed.iso", "efi-vars", "machine-id", "mac-address",
                     "console.log", "guest-version", "share", Provision.imageName, "channel.dmg"]
        for name in names {
            let source = root.appending(path: name), target = destination.root.appending(path: name)
            let sourceExists = try checked(source)
            let targetExists = try checked(target)
            guard !(sourceExists && targetExists) else { throw NodeError("ambiguous legacy file \(name)") }
            if sourceExists {
                if name == "share" {
                    guard try checked(source, directory: true) else { throw NodeError("missing legacy share") }
                    for entry in try fm.contentsOfDirectory(atPath: source.path) {
                        _ = try checked(source.appending(path: entry))
                    }
                } else { _ = try checked(source, directory: false) }
                try fm.moveItem(at: source, to: target)
            }
        }
        guard try checked(destination.config, directory: false) else { throw NodeError("legacy configuration missing") }
        guard try NodeConfig.load(destination) == profile.config else { throw NodeError("legacy configuration changed during import") }
        if try !checked(destination.root.appending(path: "profile.json")) { try persist(profile) }
        else { guard try read(profile.id) == profile else { throw NodeError("legacy import conflicts with profile") } }
        try fm.removeItem(at: journal)
        return profile
    }
}
