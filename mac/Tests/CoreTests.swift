import Foundation
import Testing
@testable import WeCoLabCore

func makeInvite(console: String = "https://console.fab.example") -> String {
    let json = try! JSONSerialization.data(withJSONObject: ["c": console, "t": "TOKEN"])
    return "wcl2." + json.base64EncodedString().replacingOccurrences(of: "+", with: "-")
        .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
}

func tempDir() throws -> URL {
    let dir = FileManager.default.temporaryDirectory.appending(path: "wecolab-test-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    return dir
}

@discardableResult
func run(_ tool: String, _ args: [String]) throws -> String {
    let p = Process(), out = Pipe()
    p.executableURL = URL(filePath: tool)
    p.arguments = args
    p.standardOutput = out
    try p.run()
    let data = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    #expect(p.terminationStatus == 0, "\(tool) \(args)")
    return String(decoding: data, as: UTF8.self)
}

@Test func configRoundTripsAndValidates() throws {
    var c = NodeConfig()
    #expect(c.cpus == 2 && c.memoryGiB == 4 && c.diskGiB == 20 && c.idleMinutes == 5)
    try c.validate()  // no invite needed: only the first start reads it
    #expect(c.idleCPUs == 2 && c.idleMemoryGiB == 4 && !c.startAtLogin && !c.startNodeOnLaunch)
    c.invite = makeInvite()
    c.idleCPUs = 6
    c.startNodeOnLaunch = true
    #expect(try JSONDecoder().decode(NodeConfig.self, from: JSONEncoder().encode(c)) == c)
    c.memoryGiB = 1
    #expect(throws: NodeError.self) { try c.validate() }
}

@Test func olderConfigLoadsWithIdleEqualToAlways() throws {
    let old = #"{"cpus":3,"memoryGiB":6,"diskGiB":30,"idleMinutes":10,"invite":"wcl1.x"}"#
    let c = try JSONDecoder().decode(NodeConfig.self, from: Data(old.utf8))
    #expect(c.cpus == 3 && c.memoryGiB == 6 && c.diskGiB == 30 && c.idleMinutes == 10 && c.invite == "wcl1.x")
    #expect(c.idleCPUs == 3 && c.idleMemoryGiB == 6 && !c.startAtLogin && !c.startNodeOnLaunch)
}

@Test func idleIsNeverLessThanTheReservation() throws {
    var c = NodeConfig()
    c.invite = makeInvite()
    c.idleCPUs = 8
    c.idleMemoryGiB = 12
    try c.validate()
    c.memoryGiB = 16  // raising the reservation raises idle with it
    c.cpus = 10
    #expect(c.idleMemoryGiB == 16 && c.idleCPUs == 10)
    c.memoryGiB = 4  // lowering it leaves idle alone
    #expect(c.idleMemoryGiB == 16)
    // A hand-edited config.json can still say less; validation stops it before anything boots.
    for idle in [#""idleCPUs":2,"idleMemoryGiB":8"#, #""idleCPUs":4,"idleMemoryGiB":6"#] {
        let json = #"{"cpus":4,"memoryGiB":8,"diskGiB":20,"idleMinutes":5,"invite":"\#(makeInvite())",\#(idle)}"#
        let bad = try JSONDecoder().decode(NodeConfig.self, from: Data(json.utf8))
        #expect(throws: NodeError.self) { try bad.validate() }
    }
}

@Test func balloonHoldsTheReservationWhileActive() {
    var c = NodeConfig()
    c.memoryGiB = 4
    c.idleMemoryGiB = 12
    #expect(NodeVM.balloonTarget(active: true, config: c) == 4 << 30)
    #expect(NodeVM.balloonTarget(active: false, config: c) == 12 << 30)
    c.idleMemoryGiB = 4  // idle profile equal to the reservation: the balloon never moves
    #expect(NodeVM.balloonTarget(active: true, config: c) == 4 << 30)
    #expect(NodeVM.balloonTarget(active: false, config: c) == 4 << 30)
}

@Test func modePublicationRetriesAndCachesOnlySuccessfulWrites() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    var attempts = 0
    let publication = ModePublication(url: url, write: { data, url in
        attempts += 1
        if attempts == 1 { throw NodeError("disk full") }
        try data.write(to: url, options: .atomic)
    })
    #expect(throws: NodeError.self) { try publication.publish("idle") }
    #expect(publication.mode == "")
    try publication.publish("idle")
    #expect(publication.mode == "idle")
    #expect(attempts == 2)
    #expect(try String(contentsOf: url, encoding: .utf8) == "idle")
}

@Test func modePublicationFailedActiveWriteInvalidatesIdleAndRetries() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    var failActive = true
    let publication = ModePublication(url: url, write: { data, url in
        if String(decoding: data, as: UTF8.self) == "active" && failActive { throw NodeError("disk full") }
        try data.write(to: url, options: .atomic)
    })
    try publication.publish("idle")
    #expect(throws: NodeError.self) { try publication.publish("active") }
    #expect(publication.mode == "")
    #expect(!FileManager.default.fileExists(atPath: url.path))
    failActive = false
    try publication.publish("active")
    #expect(publication.mode == "active")
    #expect(try String(contentsOf: url, encoding: .utf8) == "active")
}

@Test func modePublicationFailedActiveThenIdleRepublishes() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    var writes = 0
    let publication = ModePublication(url: url, write: { data, url in
        writes += 1
        if String(decoding: data, as: UTF8.self) == "active" { throw NodeError("disk full") }
        try data.write(to: url, options: .atomic)
    })
    try publication.publish("idle")
    #expect(throws: NodeError.self) { try publication.publish("active") }
    try publication.publish("idle")
    #expect(writes == 3)
    #expect(publication.mode == "idle")
    #expect(try String(contentsOf: url, encoding: .utf8) == "idle")
}

@Test func modePublicationIdleFailureKeepsActiveFile() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    let publication = ModePublication(url: url, write: { data, url in
        if String(decoding: data, as: UTF8.self) == "idle" { throw NodeError("disk full") }
        try data.write(to: url, options: .atomic)
    })
    try publication.publish("active")
    #expect(throws: NodeError.self) { try publication.publish("idle") }
    #expect(publication.mode == "active")
    #expect(try String(contentsOf: url, encoding: .utf8) == "active")
}

@Test func modePublicationInvalidatesIdleLeftByPreviousProcess() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    try "idle".write(to: url, atomically: true, encoding: .utf8)
    let publication = ModePublication(url: url, write: { _, _ in throw NodeError("disk full") })
    #expect(throws: NodeError.self) { try publication.publish("active") }
    #expect(publication.mode == "")
    #expect(!FileManager.default.fileExists(atPath: url.path))
}

@Test func modePublicationFailedInvalidationRequiresStop() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let url = dir.appending(path: "mode")
    let publication = ModePublication(url: url, remove: { _ in throw NodeError("cannot remove idle") })
    try publication.publish("idle")
    publication.reset()  // a restarted process has no in-memory cache
    #expect(throws: ModePublicationError.self) { try publication.publish("active") }
    #expect(publication.mode == "")
    #expect(try String(contentsOf: url, encoding: .utf8) == "idle")
}

@Test func inviteIsCheckedBeforeItReachesTheGuest() throws {
    #expect(try Invite(makeInvite()).console == "https://console.fab.example")
    #expect(try Invite(makeInvite(console: "https://console.fab.example:8443")).console == "https://console.fab.example:8443")
    // The Console is shown when asking to confirm: only https://<host>[:port], nothing that reads as another host.
    for console in ["http://console.fab.example", "https://console.fab.example/", "https://Console.fab.example",
                    "https://evil.example/\u{1b}[2K\rThe first start joins through https://console.good.example",
                    "https://console.good.example@evil.example", "https://evil.example?console.good.example",
                    "https://evil.example#x", "https://" + String(repeating: "a", count: 64) + ".example", "https://-x.example"] {
        #expect(throws: NodeError.self, "\(console)") { try Invite(makeInvite(console: console)) }
    }
    #expect(throws: NodeError.self) { try Invite("wcl2.abc'; reboot; '") }
    #expect(throws: NodeError.self) { try Invite("hello") }
    #expect(throws: NodeError.self) { try Invite(makeInvite() + "\n") }
}

@Test func hostnameIsAKubernetesNodeName() {
    #expect(guestHostname("Vinces-MacBook-Pro") == "mac-vinces-macbook-pro")
    #expect(guestHostname("Vince’s Mac mini") == "mac-vince-s-mac-mini")
    #expect(guestHostname("") == "mac-node")
    // The node is <site>-<host>, at most 63 characters, and a site name may take 32.
    #expect(32 + 1 + guestHostname(String(repeating: "a", count: 90)).count == 63)
    #expect(guestHostname(String(repeating: "a", count: 25) + "-b").count == 29)  // no trailing dash
}

/// The join wrapper as the guest runs it, with its paths moved into `dir`.
func joinWrapper(in dir: URL) throws -> String {
    let ud = Provision.userData(invite: try Invite(makeInvite()), hostname: "mac-test")
    let body = ud.components(separatedBy: "path: /usr/local/sbin/wecolab-join\n    permissions: \"0755\"\n    content: |\n")[1]
    return body.split(separator: "\n").prefix { $0.hasPrefix("      ") }.map { $0.dropFirst(6) + "\n" }.joined()
        .replacingOccurrences(of: "/var/lib/wecolab-mac/", with: dir.path + "/")
        .replacingOccurrences(of: "/opt/wecolab/", with: dir.path + "/")
        .replacingOccurrences(of: "/usr/local/sbin/", with: dir.path + "/")
        .replacingOccurrences(of: "/var/lib/wecolab/", with: dir.path + "/state/")
}

@Test func joinWrapperKeepsTheInviteOutOfArgv() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let invite = makeInvite(), wrapper = dir.appending(path: "wecolab-join")
    try invite.write(to: dir.appending(path: "invite"), atomically: true, encoding: .utf8)
    try joinWrapper(in: dir).write(to: wrapper, atomically: true, encoding: .utf8)
    // A stand-in for install.sh: it gets the invite as $1, shows every argv above it, and fails like die().
    try """
        set -euo pipefail
        echo "invite=$1"
        ps -o args= -p $$ -p $PPID
        false
        """.write(to: dir.appending(path: "join.sh"), atomically: true, encoding: .utf8)
    func attempt() throws -> String {
        let p = Process(), out = Pipe()
        p.executableURL = URL(filePath: "/bin/bash")
        p.arguments = [wrapper.path]
        p.standardOutput = out
        p.standardError = out
        try p.run()
        let log = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        p.waitUntilExit()
        #expect(p.terminationStatus == 1)
        #expect(log.hasSuffix("wecolab-join exit=1\n"), "\(log)")  // the attempt's exit is the log's last line
        #expect(!FileManager.default.fileExists(atPath: dir.appending(path: "joined").path))
        return log
    }
    let log = try attempt()
    #expect(log.contains("bash \(wrapper.path)\n"), "\(log)")  // ps did show the argv
    #expect(log.components(separatedBy: invite).count == 2 && log.contains("invite=\(invite)\n"), "\(log)")
    // No invite: install.sh would make a new fabric, so it does not run at all.
    try Data().write(to: dir.appending(path: "invite"))
    #expect(!(try attempt()).contains("invite="))
}

@Test func joinWrapperCountsAFinishedInstallAsJoined() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    // install.sh wrote its done marker, but the attempt that did never got to write joined.
    try FileManager.default.createDirectory(at: dir.appending(path: "state"), withIntermediateDirectories: true)
    try Data().write(to: dir.appending(path: "state/done"))
    try makeInvite().write(to: dir.appending(path: "invite"), atomically: true, encoding: .utf8)
    try "exit 1\n".write(to: dir.appending(path: "join.sh"), atomically: true, encoding: .utf8)  // "already belongs to a fabric"
    let wrapper = dir.appending(path: "wecolab-join")
    try joinWrapper(in: dir).write(to: wrapper, atomically: true, encoding: .utf8)
    #expect(try run("/bin/bash", [wrapper.path]).hasSuffix("wecolab-join exit=0\n"))
    #expect(FileManager.default.fileExists(atPath: dir.appending(path: "joined").path))
}

@Test func onlyThisVersionsGuestBoots() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let paths = Paths(root: dir)
    try paths.prepare()
    #expect(Provision.guest(paths) == .none)
    try Data().write(to: paths.disk)
    #expect(Provision.guest(paths) == .old)  // v1 wrote no stamp
    try Data(Provision.generation.utf8).write(to: paths.guestVersion)
    #expect(Provision.guest(paths) == .current)
    try Data("{}".utf8).write(to: paths.share.appending(path: "netbird.json"))
    #expect(Provision.guest(paths) == .old)  // v1's NetBird guest published this
}

@Test func theInviteIsReadOnlyToMakeAGuestAndResetForgetsIt() async throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let paths = Paths(root: dir), fm = FileManager.default
    var c = NodeConfig()
    c.invite = makeInvite()
    c.diskGiB = 8
    // Not confirmed: nothing is made.
    await #expect(throws: NodeError.self) { try await Provision.run(c, at: paths, confirm: { _ in false }, log: { _ in }) }
    for url in [paths.seed, paths.guestVersion, paths.disk] { #expect(!fm.fileExists(atPath: url.path)) }
    // Confirmed, over what v1 left: a new guest, with nothing of the old one's.
    try Data(count: 1 << 20).write(to: paths.image)  // stands in for the download
    try Data("{}".utf8).write(to: paths.share.appending(path: "netbird.json"))
    try Data("v1".utf8).write(to: paths.machineID)
    try await Provision.run(c, at: paths, confirm: { _ in true }, log: { _ in })
    #expect(Provision.guest(paths) == .current)
    #expect(fm.fileExists(atPath: paths.seed.path) && !fm.fileExists(atPath: paths.machineID.path))
    // Made: the invite is neither read nor confirmed again.
    c.invite = ""
    try await Provision.run(c, at: paths, confirm: { _ in Issue.record("asked again"); return false }, log: { _ in })
    // Reset deletes the guest and forgets the spent invite; the image stays.
    c.invite = makeInvite()
    try c.save(paths)
    try Data().write(to: paths.share.appending(path: "wecolab-join.log"))
    #expect(try Provision.reset(paths).invite == "" && NodeConfig.load(paths).invite == "")
    for url in [paths.seed, paths.guestVersion, paths.disk, paths.share.appending(path: "wecolab-join.log")] {
        #expect(!fm.fileExists(atPath: url.path))
    }
    #expect(fm.fileExists(atPath: paths.image.path))
    // A guest this version did not make is refused before anything is asked.
    try paths.prepare()
    try Data().write(to: paths.disk)
    await #expect(throws: NodeError.self) {
        try await Provision.run(c, at: paths, confirm: { _ in Issue.record("asked about an old guest"); return true }, log: { _ in })
    }
}

/// Parse the cloud-init write_files subset used by this seed, from the mounted filesystem.
/// Reject missing headers/entries rather than comparing how Swift renders a multiline string.
func seedFiles(_ userData: String) -> [String: (content: String, permissions: String?, encoding: String?)]? {
    guard userData.hasPrefix("#cloud-config\n"), userData.contains("\nwrite_files:\n") else { return nil }
    let sections = userData.components(separatedBy: "\n  - path: ").dropFirst()
    guard !sections.isEmpty else { return nil }
    var files: [String: (content: String, permissions: String?, encoding: String?)] = [:]
    for section in sections {
        let lines = section.components(separatedBy: "\n")
        guard let path = lines.first, path.hasPrefix("/"), !files.keys.contains(path) else { return nil }
        var content: String?, permissions: String?, encoding: String?
        var index = 1
        while index < lines.count {
            let line = lines[index]
            if line.hasPrefix("    permissions: ") {
                permissions = String(line.dropFirst("    permissions: ".count)).trimmingCharacters(in: CharacterSet(charactersIn: "\""))
            } else if line.hasPrefix("    encoding: ") {
                encoding = String(line.dropFirst("    encoding: ".count))
            } else if line.hasPrefix("    content: ") {
                let value = String(line.dropFirst("    content: ".count))
                if value == "|" {
                    var block: [String] = []
                    index += 1
                    while index < lines.count && lines[index].hasPrefix("      ") {
                        block.append(String(lines[index].dropFirst(6)))
                        index += 1
                    }
                    content = block.joined(separator: "\n") + "\n"
                    continue
                }
                content = value
            } else if line == "runcmd:" || line.hasPrefix("  - [") {
                break
            }
            index += 1
        }
        guard let content else { return nil }
        files[path] = (content, permissions, encoding)
    }
    return files
}

@Test func seedIsANoCloudISO() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let iso = dir.appending(path: "seed.iso"), mnt = dir.appending(path: "mnt")
    let invite = try Invite(makeInvite())
    try Provision.seed(invite: invite, hostname: "mac-test", to: iso)
    try run("/usr/bin/hdiutil", ["attach", "-quiet", "-readonly", "-nobrowse", "-mountpoint", mnt.path, iso.path])
    defer { _ = try? run("/usr/bin/hdiutil", ["detach", "-quiet", mnt.path]) }
    // diskutil reads the filesystem label. URLResourceValues.volumeName uses
    // the custom mountpoint name; raw ISO descriptors include NUL padding.
    let info = try run("/usr/sbin/diskutil", ["info", "-plist", mnt.path])
    let volume = try #require(PropertyListSerialization.propertyList(from: Data(info.utf8), format: nil) as? [String: Any])
    #expect((volume["VolumeName"] as? String)?.lowercased() == "cidata")
    let metadata = try String(contentsOf: mnt.appending(path: "meta-data"), encoding: .utf8)
    let meta = Dictionary(uniqueKeysWithValues: metadata.split(separator: "\n").compactMap { line -> (String, String)? in
        guard let colon = line.firstIndex(of: ":") else { return nil }
        return (String(line[..<colon]), String(line[line.index(after: colon)...]).trimmingCharacters(in: .whitespaces))
    })
    #expect(meta["instance-id"] == "wecolab-mac-test")
    #expect(meta["local-hostname"] == "mac-test")
    let userData = try String(contentsOf: mnt.appending(path: "user-data"), encoding: .utf8)
    let files = try #require(seedFiles(userData), "Mounted user-data does not parse as #cloud-config write_files (bytes: \(userData.utf8.count)); invite redacted")
    #expect(userData.contains("\nhostname: mac-test\n"))
    #expect(userData.contains("\nruncmd:\n"))
    for command in ["[systemctl, daemon-reload]", "[systemctl, enable, --now, wecolab.mount]",
                    "[systemctl, enable, --now, wecolab-share.timer]",
                    "[systemctl, enable, --now, --no-block, wecolab-join.service]"] {
        #expect(userData.components(separatedBy: "\n  - \(command)\n").count == 2, "missing cloud-init command \(command)")
    }
    let installer = try #require(files["/opt/wecolab/join.sh"])
    #expect(installer.encoding == "b64")
    #expect(installer.permissions == "0755")
    let embedded = try #require(Data(base64Encoded: installer.content))
    let repoScript = try Data(contentsOf: URL(filePath: #filePath).deletingLastPathComponent().appending(path: "../../install.sh"))
    #expect(embedded == repoScript)
    let secret = try #require(files["/var/lib/wecolab-mac/invite"])
    #expect(secret.permissions == "0600")
    #expect(secret.content == invite.code)
    let join = try #require(files["/usr/local/sbin/wecolab-join"])
    #expect(join.permissions == "0755")
    #expect(join.content.contains("set -- \"$(cat /var/lib/wecolab-mac/invite)\""))
    #expect(join.content.contains(". /opt/wecolab/join.sh"))
    #expect(join.content.contains("touch /var/lib/wecolab-mac/joined"))
    let unit = try #require(files["/etc/systemd/system/wecolab-join.service"])
    #expect(unit.content.contains("ExecStart=/usr/local/sbin/wecolab-join"))
    #expect(unit.content.contains("Environment=WECOLAB_LAPTOP=1"))
    #expect(unit.content.contains("Restart=on-failure"))
    #expect(files["/etc/systemd/system/wecolab.mount"]?.content.contains("What=wecolab") == true)
    #expect(seedFiles("hostname: mac-test\nwrite_files:\n") == nil)
    #expect(seedFiles("#cloud-config\nwrite_files:\n  - path: /opt/wecolab/join.sh\n") == nil)
    #expect(seedFiles("") == nil)  // an absent NoCloud payload cannot install or join
}

@Test func diskGrowsSparselyAndNeverShrinks() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let disk = dir.appending(path: "disk.img")
    try Data(count: 1 << 20).write(to: disk)
    try Provision.grow(disk, toGiB: 8)
    let values = try disk.resourceValues(forKeys: [.fileSizeKey, .totalFileAllocatedSizeKey])
    #expect(values.fileSize == 8 << 30)
    #expect(values.totalFileAllocatedSize! < 16 << 20)
    #expect(throws: NodeError.self) { try Provision.grow(disk, toGiB: 4) }
}

@Test func guestStatusReadsTheShare() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    #expect(GuestStatus.read(from: dir).join == "waiting for first boot")
    try #"{"ip":"10.77.1.4","name":"vince-mac","connected":true}"#
        .write(to: dir.appending(path: "nebula.json"), atomically: true, encoding: .utf8)
    // As the guest writes it: the wrapper's lines around install.sh's.
    let start = "==> wecolab-join, 2026-09-29T10:00:00Z\n\n==> Nebula\n", refused = "wecolab: the Console refused the invite\n"
    let log = dir.appending(path: "wecolab-join.log")
    try start.write(to: log, atomically: true, encoding: .utf8)
    let s = GuestStatus.read(from: dir)
    #expect(s.meshIP == "10.77.1.4" && s.meshName == "vince-mac" && s.meshConnected && s.join == "running" && s.lastLine == "==> Nebula")
    try (start + refused + "wecolab-join exit=1\n").write(to: log, atomically: true, encoding: .utf8)
    #expect(GuestStatus.read(from: dir).join == "failed (exit 1), will retry")
    #expect(GuestStatus.read(from: dir).lastLine == "wecolab: the Console refused the invite")  // the error, not the wrapper's line
    try (start + refused + "wecolab-join exit=1\n" + start).write(to: log, atomically: true, encoding: .utf8)
    #expect(GuestStatus.read(from: dir).join == "retrying")
    try (start + refused + "wecolab-join exit=1\n" + start + "wecolab-join exit=0\n").write(to: log, atomically: true, encoding: .utf8)
    #expect(GuestStatus.read(from: dir).join == "finished")
}

@Test func shareIsReadWithoutFollowingTheGuest() throws {
    let dir = try tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let secret = dir.appending(path: "disk.img")
    try "not the guest's".write(to: secret, atomically: true, encoding: .utf8)
    // A symlink to a file on the Mac, a FIFO that would block, a directory: none is read.
    try FileManager.default.createSymbolicLink(at: dir.appending(path: "wecolab-join.log"), withDestinationURL: secret)
    #expect(mkfifo(dir.appending(path: "nebula.json").path, 0o600) == 0)
    try FileManager.default.createDirectory(at: dir.appending(path: "mode"), withIntermediateDirectories: false)
    #expect(readShare("wecolab-join.log", in: dir, limit: 1024) == nil)
    #expect(readShare("nebula.json", in: dir, limit: 1024) == nil)
    #expect(readShare("mode", in: dir, limit: 16) == nil)
    #expect(GuestStatus.read(from: dir) == GuestStatus())
    // A file without end: only its tail.
    try (String(repeating: "x", count: 1 << 20) + "wecolab-join exit=0\n").write(to: dir.appending(path: "big"), atomically: true, encoding: .utf8)
    #expect(readShare("big", in: dir, limit: 64) == String(repeating: "x", count: 44) + "wecolab-join exit=0\n")
    // Nothing that a terminal would act on: `wecolab-node status` prints these.
    try "\u{1b}[2K\rok\u{202e}\n".write(to: dir.appending(path: "text"), atomically: true, encoding: .utf8)
    #expect(readShare("text", in: dir, limit: 64) == "[2Kok\n")
    try #"{"ip":"10.77.1.4","name":"\u001b[2Kvince"}"#.write(to: dir.appending(path: "nebula.json"), atomically: true, encoding: .utf8)
    #expect(GuestStatus.read(from: dir).meshName == "[2Kvince")  // a JSON escape too
}
