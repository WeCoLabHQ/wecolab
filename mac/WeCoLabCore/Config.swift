import Foundation
import SystemConfiguration

/// What this Mac contributes to its Site, and the invite it joins with.
/// Saved as JSON readable only by the user, because the invite is a one-time code.
public struct NodeConfig: Codable, Equatable, Sendable {
    /// Reserved for the node all the time. Raising it raises the idle profile with it: idle is never less.
    public var cpus = 2 { didSet { idleCPUs = max(idleCPUs, cpus) } }
    public var memoryGiB = 4 { didSet { idleMemoryGiB = max(idleMemoryGiB, memoryGiB) } }
    public var diskGiB = 20
    /// Up to this much once nobody has touched the Mac for idleMinutes. The VM is sized to it.
    public var idleMinutes = 5
    public var idleCPUs = 2
    public var idleMemoryGiB = 4
    public var startAtLogin = false
    public var startNodeOnLaunch = false
    public var invite = ""

    public init() {}

    /// A config.json from before the idle profile has none: idle is then what is always reserved.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        cpus = try c.decode(Int.self, forKey: .cpus)
        memoryGiB = try c.decode(Int.self, forKey: .memoryGiB)
        diskGiB = try c.decode(Int.self, forKey: .diskGiB)
        idleMinutes = try c.decode(Int.self, forKey: .idleMinutes)
        idleCPUs = try c.decodeIfPresent(Int.self, forKey: .idleCPUs) ?? cpus
        idleMemoryGiB = try c.decodeIfPresent(Int.self, forKey: .idleMemoryGiB) ?? memoryGiB
        startAtLogin = try c.decodeIfPresent(Bool.self, forKey: .startAtLogin) ?? false
        startNodeOnLaunch = try c.decodeIfPresent(Bool.self, forKey: .startNodeOnLaunch) ?? false
        invite = try c.decode(String.self, forKey: .invite)
    }

    public static func load(_ paths: Paths = .user) -> NodeConfig {
        (try? JSONDecoder().decode(NodeConfig.self, from: Data(contentsOf: paths.config))) ?? NodeConfig()
    }

    public func save(_ paths: Paths = .user) throws {
        try paths.prepare()
        try JSONEncoder().encode(self).write(to: paths.config, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: paths.config.path)
    }

    /// Rejects, up front, what would otherwise fail inside the guest. The invite is not checked here:
    /// only the first start uses it (Provision.run).
    public func validate() throws {
        guard cpus >= 1, memoryGiB >= 2, diskGiB >= 8, idleMinutes >= 1 else {
            throw NodeError("a node needs at least 1 CPU, 2 GiB memory, 8 GiB disk and 1 idle minute")
        }
        guard idleCPUs >= cpus, idleMemoryGiB >= memoryGiB else {
            throw NodeError("when idle the node gets at least what is always reserved: \(cpus) CPUs and \(memoryGiB) GiB memory")
        }
    }
}

/// A Mac's invite: the Console to join through and a one-time code. What the code admits (the site, as a
/// laptop, until when) only the Console knows; it refuses a code that is used, expired or not a Mac's.
public struct Invite: Sendable {
    /// https://<host>[:port] and nothing else: it is shown when asking to confirm, so it can carry no
    /// userinfo, path or control characters that would make it read as another host.
    public let console: String
    /// The whole line, checked: the only form in which an invite reaches the guest's user-data.
    let code: String

    public init(_ text: String) throws {
        // The invite ends up in the guest's user-data, so only its own alphabet is accepted.
        guard text.wholeMatch(of: /wcl2\.[A-Za-z0-9_-]+/) != nil else {
            throw NodeError("an invite is one line starting with wcl2. (in the Console: Sites, your site, Add a Mac)")
        }
        struct Payload: Decodable { let c: String, t: String }
        var b64 = text.dropFirst(5).replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        b64 += String(repeating: "=", count: (4 - b64.count % 4) % 4)
        guard let data = Data(base64Encoded: b64), let p = try? JSONDecoder().decode(Payload.self, from: data), !p.t.isEmpty else {
            throw NodeError("the invite does not decode; copy the whole line")
        }
        // The host as validate.DNSName has it: lowercase labels of at most 63, at most 253 in all.
        guard let m = p.c.wholeMatch(of: /https:\/\/([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)(:[0-9]{1,5})?/),
              m.1.count <= 253, m.1.split(separator: ".").allSatisfy({ $0.count <= 63 }) else {
            throw NodeError("the invite's Console is not https://<host>; this invite was not made by a Console")
        }
        console = p.c
        code = text
    }
}

public struct NodeError: LocalizedError {
    public let errorDescription: String?
    public init(_ message: String) { errorDescription = message }
}

/// Everything lives in ~/Library/Application Support/WeCoLab (`user`); tests use a directory of their own.
public struct Paths: Sendable {
    let root: URL
    public static let user = Paths(root: URL.applicationSupportDirectory.appending(path: "WeCoLab", directoryHint: .isDirectory))

    public var config: URL { root.appending(path: "config.json") }
    public var image: URL { root.appending(path: "debian-13-genericcloud-arm64.raw") }  // verified download, kept for resets
    public var disk: URL { root.appending(path: "disk.img") }
    public var seed: URL { root.appending(path: "seed.iso") }
    public var efiVars: URL { root.appending(path: "efi-vars") }
    public var machineID: URL { root.appending(path: "machine-id") }
    public var macAddress: URL { root.appending(path: "mac-address") }
    public var console: URL { root.appending(path: "console.log") }
    public var lock: URL { root.appending(path: "vm.pid") }
    /// Which WeCoLab for Mac made disk.img. v1 used these same paths for a NetBird guest.
    public var guestVersion: URL { root.appending(path: "guest-version") }
    /// Shared with the guest over virtio-fs, mounted there at /wecolab.
    public var share: URL { root.appending(path: "share", directoryHint: .isDirectory) }

    func prepare() throws {
        try FileManager.default.createDirectory(at: share, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    }
}

/// "mac-" and the Mac's short name, at most 30 characters: the node is named <site>-<host>, at most 63,
/// and a site name may take 32.
public func guestHostname(_ shortName: String = SCDynamicStoreCopyLocalHostName(nil) as String? ?? "") -> String {
    let name = String(shortName.lowercased().map { ("a"..."z").contains($0) || ("0"..."9").contains($0) ? $0 : "-" })
    let trimmed = name.prefix(26).trimmingCharacters(in: CharacterSet(charactersIn: "-"))
    return "mac-" + (trimmed.isEmpty ? "node" : trimmed)
}

/// One VM per user: a lock file that holds the running process's PID and is released when that process exits.
public enum VMLock {
    public static func acquire(_ paths: Paths = .user) throws -> Int32 {
        try paths.prepare()
        let fd = open(paths.lock.path, O_RDWR | O_CREAT, 0o600)
        guard fd >= 0 else { throw NodeError("cannot open \(paths.lock.path)") }
        guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
            close(fd)
            throw NodeError("the VM is already running (pid \(holder(paths).map(String.init) ?? "?"))")
        }
        let pid = Data("\(getpid())\n".utf8)
        ftruncate(fd, 0)
        _ = pid.withUnsafeBytes { pwrite(fd, $0.baseAddress, $0.count, 0) }
        return fd
    }

    public static func release(_ fd: Int32) { close(fd) }

    /// The PID of the process running the VM, or nil when none is.
    public static func holder(_ paths: Paths = .user) -> pid_t? {
        let fd = open(paths.lock.path, O_RDONLY)
        guard fd >= 0 else { return nil }
        defer { close(fd) }
        if flock(fd, LOCK_SH | LOCK_NB) == 0 { return nil }
        return (try? String(contentsOf: paths.lock, encoding: .utf8)).flatMap { pid_t($0.trimmingCharacters(in: .whitespacesAndNewlines)) }
    }
}

/// Reads a file in the share. The guest owns the share, so the name may be a symlink to anything on the
/// Mac (disk.img, say), a FIFO, or a file that keeps growing: only a regular file is read, never through a
/// symlink, and only its last `limit` bytes, as printable text.
public func readShare(_ name: String, in dir: URL = Paths.user.share, limit: Int) -> String? {
    let fd = open(dir.appending(path: name).path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
    guard fd >= 0 else { return nil }
    defer { close(fd) }
    var st = stat()
    guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return nil }
    let start = max(0, Int(st.st_size) - limit)
    var buf = [UInt8](repeating: 0, count: Int(st.st_size) - start)
    let n = pread(fd, &buf, buf.count, off_t(start))
    return n < 0 ? nil : printable(String(decoding: buf.prefix(n), as: UTF8.self))
}

/// The guest's text without control or format characters, newlines aside: in a terminal an escape
/// sequence could rewrite what is shown, a bidi override reorder it.
func printable(_ text: String) -> String {
    String(text.unicodeScalars.filter { $0 == "\n" || !CharacterSet.controlCharacters.contains($0) }.map(Character.init))
}

/// What the guest last published into the share (it does so every 30 s).
public struct GuestStatus: Equatable, Sendable {
    public var meshIP = ""
    public var meshName = ""
    public var meshConnected = false  // a steward's certificate service answers over Nebula
    public var join = "waiting for first boot"
    public var joinLog = ""

    public init() {}

    /// install.sh's last line, not the wrapper's: while a join waits to retry, the error that stopped it.
    public var lastLine: String {
        joinLog.split(separator: "\n").last { !$0.hasPrefix("wecolab-join exit=") && !$0.hasPrefix("==> wecolab-join") }.map(String.init) ?? ""
    }

    public static func read(from dir: URL = Paths.user.share) -> GuestStatus {
        var s = GuestStatus()
        if let text = readShare("nebula.json", in: dir, limit: 4096),
           let nb = try? JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any] {
            s.meshIP = printable(nb["ip"] as? String ?? "")  // JSON escapes can spell what readShare removed
            s.meshName = printable(nb["name"] as? String ?? "")
            s.meshConnected = nb["connected"] as? Bool ?? false
        }
        if let log = readShare("wecolab-join.log", in: dir, limit: 16 << 10) {
            s.joinLog = log
            // The guest retries a failed join; only the last attempt counts.
            if let exit = log.matches(of: /wecolab-join exit=(\d+)/).last {
                let after = log[exit.range.upperBound...].trimmingCharacters(in: .whitespacesAndNewlines)
                s.join = exit.1 == "0" ? "finished" : after.isEmpty ? "failed (exit \(exit.1)), will retry" : "retrying"
            } else {
                s.join = "running"
            }
        }
        return s
    }
}
