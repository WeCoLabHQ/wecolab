import CryptoKit
import Foundation

/// First run: the Debian cloud image becomes the guest disk and a NoCloud seed carries the join.
public enum Provision {
    static let images = URL(string: "https://cloud.debian.org/images/cloud/trixie/latest/")!
    static let imageName = "debian-13-genericcloud-arm64.raw"  // raw, not qcow2: Virtualization.framework reads raw images
    static let session = URLSession(configuration: .ephemeral)  // no HTTP cache or cookies left in ~/Library
    /// Written to guest-version when this version provisions a guest; bump it when an old guest must not boot.
    static let generation = "2\n"

    /// The guest on disk: none yet, this version's, or one it must not boot (v1's NetBird guest, which
    /// has no stamp and published netbird.json).
    public enum Guest: Sendable { case none, current, old }

    public static func guest(_ paths: Paths) -> Guest {
        guard FileManager.default.fileExists(atPath: paths.disk.path) else { return .none }
        let stamped = (try? String(contentsOf: paths.guestVersion, encoding: .utf8)) == generation
        return stamped && !FileManager.default.fileExists(atPath: paths.share.appending(path: "netbird.json").path) ? .current : .old
    }

    /// Makes sure disk and seed exist, then grows the disk to the configured size. Only the first start
    /// reads the invite, and only after `confirm`: whoever runs its Console gets root on the VM.
    public static func run(_ config: NodeConfig, at paths: Paths, hostname: String, confirm: @Sendable (Invite) async -> Bool,
                           log: @Sendable (String) -> Void) async throws {
        let fm = FileManager.default
        try config.validate()
        try paths.validateFiles()
        try paths.prepare()
        switch guest(paths) {
        case .old:
            throw NodeError("this VM was made by an older WeCoLab for Mac and does not start here; Reset it, then add this Mac again")
        case .none:
            let invite = try Invite(config.invite)
            guard await confirm(invite) else { throw NodeError("not started: the invite's Console was not confirmed") }
            // An interrupted first boot may leave identity behind: never silently replace it.
            guard ![paths.machineID, paths.macAddress, paths.efiVars, paths.seed, paths.guestVersion].contains(where: { fm.fileExists(atPath: $0.path) }),
                  try (!fm.fileExists(atPath: paths.share.path) ||
                    fm.contentsOfDirectory(atPath: paths.share.path).isEmpty) else {
                throw NodeError("existing guest identity or shared state needs an explicit reset")
            }
            let available = try paths.root.resourceValues(forKeys: [.volumeAvailableCapacityKey]).volumeAvailableCapacity ?? 0
            let baseSize = (try? fm.attributesOfItem(atPath: paths.image.path)[.size] as? NSNumber)?.int64Value ?? 3 << 30
            guard Int64(available) >= baseSize + (1 << 30) else { throw NodeError("not enough free disk space to provision a guest") }
            if !fm.fileExists(atPath: paths.image.path) { try await download(to: paths.image, log: log) }
            try seed(invite: invite, hostname: hostname, to: paths.seed)
            try Data(generation.utf8).write(to: paths.guestVersion)  // before the disk: a disk is never unstamped by accident
            try fm.copyItem(at: paths.image, to: paths.disk)  // an APFS clone: instant, shares blocks with the image
            log("first boot will join through \(invite.console) as \(hostname)")
        case .current:
            break
        }
        try grow(paths.disk, toGiB: config.diskGiB)
    }

    /// Deletes the guest (disk, seed, identity, share) and the spent invite; the downloaded image stays.
    /// Joining again takes the same box name, so the old box has to be removed in the Console first.
    /// The caller persists the returned configuration in ProfileStore for registered profiles.
    public static func reset(_ paths: Paths) throws -> NodeConfig {
        try paths.validateFiles()
        let lock = try VMLock.acquire(paths)  // refuses while a VM runs, here or in the CLI
        defer { VMLock.release(lock) }
        let profileFile = paths.root.appending(path: "profile.json")
        var info = stat()
        let hasProfile = lstat(profileFile.path, &info) == 0
        if !hasProfile && errno != ENOENT { throw NodeError("cannot inspect profile record") }
        if hasProfile && info.st_mode & S_IFMT != S_IFREG { throw NodeError("invalid profile record") }
        let config: NodeConfig
        if hasProfile {
            let registry = ProfileStore(root: paths.root.deletingLastPathComponent().deletingLastPathComponent())
            config = try registry.profile(id: paths.root.lastPathComponent).config
        } else {
            config = try NodeConfig.load(paths)
        }
        try removeGuest(paths)
        var cleared = config
        cleared.invite = ""
        if hasProfile {
            if FileManager.default.fileExists(atPath: paths.config.path) {
                try FileManager.default.removeItem(at: paths.config)
            }
        } else {
            try cleared.save(paths)
        }
        return cleared
    }

    /// Everything that is one guest's; the image, the settings and the lock stay.
    static func removeGuest(_ paths: Paths) throws {
        try detachChannel(at: paths)
        for url in [paths.disk, paths.guestVersion, paths.seed, paths.efiVars, paths.machineID, paths.macAddress, paths.console, paths.share, paths.channelImage, paths.root.appending(path: "share-legacy")] {
            do { try FileManager.default.removeItem(at: url) } catch CocoaError.fileNoSuchFile {}
        }
    }

    private static func command(_ args: [String]) throws -> Data {
        let process = Process(), output = Pipe()
        process.executableURL = URL(filePath: "/usr/bin/hdiutil")
        process.arguments = args
        process.standardOutput = output
        // `info -plist` must retain clean stdout. Other subcommands return no machine data,
        // so capture their diagnostics in the same drained pipe rather than hide the cause.
        if args.first == "info" { process.standardError = FileHandle.standardError }
        else { process.standardError = output }
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        guard process.terminationStatus == 0 else {
            let detail = printable(String(decoding: data.suffix(4096), as: UTF8.self))
            throw NodeError("hdiutil \(args.first ?? "") failed (\(process.terminationStatus)): \(detail)")
        }
        return data
    }

    /// Check the actual hdiutil image-to-mount mapping; an ordinary directory is not a quota.
    private static func mountedImage(at paths: Paths) throws -> Bool {
        let data = try command(["info", "-plist"])
        guard let plist = try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
              let images = plist["images"] as? [[String: Any]] else { throw NodeError("cannot verify channel mounts") }
        var matched = false
        for image in images {
            let path = image["image-path"] as? String
            for entity in image["system-entities"] as? [[String: Any]] ?? [] {
                guard let mount = entity["mount-point"] as? String else { continue }
                let mountedPath = URL(fileURLWithPath: mount).resolvingSymlinksInPath().path
                guard mountedPath == paths.share.resolvingSymlinksInPath().path else { continue }
                guard let path, URL(fileURLWithPath: path).resolvingSymlinksInPath().path == paths.channelImage.resolvingSymlinksInPath().path else {
                    throw NodeError("share is mounted from a different image")
                }
                matched = true
            }
        }
        return matched
    }

    public static func prepareChannel(at paths: Paths) throws {
        let fm = FileManager.default
        try paths.prepare()
        let stage = paths.root.appending(path: "share-legacy")
        try paths.validateFiles()
        func kind(_ url: URL) throws -> mode_t? {
            var info = stat()
            if lstat(url.path, &info) != 0 {
                if errno == ENOENT { return nil }
                throw NodeError("cannot inspect \(url.path)")
            }
            guard info.st_mode & S_IFMT != S_IFLNK else { throw NodeError("symbolic-link channel path") }
            return info.st_mode & S_IFMT
        }
        guard try kind(paths.root) == S_IFDIR, try kind(paths.channelImage).map({ $0 == S_IFREG }) ?? true,
              try kind(paths.share).map({ $0 == S_IFDIR }) ?? true,
              try kind(stage).map({ $0 == S_IFDIR }) ?? true else { throw NodeError("invalid channel paths") }
        if try mountedImage(at: paths) {
            guard try kind(paths.channelImage) == S_IFREG else { throw NodeError("mounted channel image missing") }
        } else {
            if try kind(stage) == nil, try kind(paths.share) != nil {
                try fm.moveItem(at: paths.share, to: stage)
            }
            if try kind(paths.share) == nil {
                try fm.createDirectory(at: paths.share, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            }
            if try kind(paths.channelImage) == nil {
                _ = try command(["create", "-quiet", "-size", "16m", "-fs", "HFS+", "-volname", "wecolab-channel",
                                 "-layout", "NONE", "-type", "UDIF", paths.channelImage.path])
                try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: paths.channelImage.path)
            }
            guard ((try fm.attributesOfItem(atPath: paths.channelImage.path)[.size] as? NSNumber)?.int64Value ?? 0) <= 20 << 20 else {
                throw NodeError("channel image exceeds quota")
            }
            _ = try command(["attach", "-quiet", "-nobrowse", "-mountpoint", paths.share.path, paths.channelImage.path])
            guard try mountedImage(at: paths) else { throw NodeError("channel failed to mount") }
        }
        var volume = statfs()
        guard statfs(paths.share.path, &volume) == 0,
              withUnsafePointer(to: &volume.f_fstypename, {
                  $0.withMemoryRebound(to: CChar.self, capacity: 16) { String(cString: $0) }
              }) == "hfs" else { throw NodeError("channel is not a mounted HFS+ volume") }
        let (capacity, overflow) = UInt64(volume.f_blocks).multipliedReportingOverflow(by: UInt64(volume.f_bsize))
        guard !overflow, capacity >= 14 << 20, capacity <= 16 << 20 else {
            throw NodeError("channel filesystem has an invalid quota")
        }
        if try kind(stage) != nil {
            for name in try fm.contentsOfDirectory(atPath: stage.path) {
                guard ["mode", "wecolab-join.log", "nebula.json"].contains(name) else { throw NodeError("unexpected legacy share entry") }
                let source = stage.appending(path: name), destination = paths.share.appending(path: name)
                guard try kind(source) == S_IFREG else { throw NodeError("invalid legacy share entry") }
                let size = try fm.attributesOfItem(atPath: source.path)[.size] as? NSNumber
                guard let size, size.int64Value <= 1 << 20 else { throw NodeError("legacy share entry exceeds quota") }
                let content = try Data(contentsOf: source)
                guard content.count <= 1 << 20 else { throw NodeError("legacy share entry exceeds quota") }
                if try kind(destination) == nil {
                    try content.write(to: destination, options: .atomic)
                } else {
                    guard try kind(destination) == S_IFREG, try Data(contentsOf: destination) == content else {
                        throw NodeError("conflicting channel migration entry")
                    }
                }
            }
            try fm.removeItem(at: stage)
        }
    }

    public static func detachChannel(at paths: Paths) throws {
        if try mountedImage(at: paths) {
            _ = try command(["detach", "-quiet", paths.share.path])
            guard try !mountedImage(at: paths) else { throw NodeError("channel remained mounted") }
        }
    }

    static func download(to image: URL, log: @Sendable (String) -> Void) async throws {
        let (sums, _) = try await session.data(from: images.appending(path: "SHA512SUMS"))
        guard let line = String(decoding: sums, as: UTF8.self).split(separator: "\n").first(where: { $0.hasSuffix("  " + imageName) }) else {
            throw NodeError("SHA512SUMS on cloud.debian.org does not list \(imageName)")
        }
        // ponytail: no progress reporting; a URLSessionDownloadDelegate adds it if the 3 GiB wait needs a bar.
        log("downloading \(imageName) (3 GiB) from cloud.debian.org")
        let (tmp, response) = try await session.download(from: images.appending(path: imageName))
        defer { try? FileManager.default.removeItem(at: tmp) }
        guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw NodeError("download failed: \(response)") }
        log("verifying SHA512")
        guard try sha512(tmp) == line.prefix(128).lowercased() else {
            throw NodeError("\(imageName) does not match the published SHA512SUMS; not using it")
        }
        try FileManager.default.moveItem(at: tmp, to: image)
    }

    static func sha512(_ url: URL) throws -> String {
        let file = try FileHandle(forReadingFrom: url)
        defer { try? file.close() }
        var hash = SHA512()
        while let chunk = try file.read(upToCount: 8 << 20), !chunk.isEmpty { hash.update(data: chunk) }
        return hash.finalize().map { String(format: "%02x", $0) }.joined()
    }

    /// Sparse resize: extends the file without writing blocks; cloud-init grows the root partition at boot.
    static func grow(_ url: URL, toGiB gib: Int) throws {
        guard gib >= 8, gib <= 4096 else { throw NodeError("disk size exceeds supported limit") }
        let file = try FileHandle(forWritingTo: url)
        defer { try? file.close() }
        let want = UInt64(gib) << 30, have = try file.seekToEnd()
        guard want >= have else { throw NodeError("the disk is already \(have >> 30) GiB; it can grow but not shrink") }
        if want > have {
            let available = try url.deletingLastPathComponent().resourceValues(forKeys: [.volumeAvailableCapacityKey]).volumeAvailableCapacity ?? 0
            guard available >= 1 << 30 else { throw NodeError("not enough free disk space to grow a guest") }
            try file.truncate(atOffset: want)
        }
    }

    /// A NoCloud seed: an ISO labelled "cidata" holding meta-data and user-data.
    static func seed(invite: Invite, hostname: String, to iso: URL) throws {
        let dir = FileManager.default.temporaryDirectory.appending(path: "wecolab-seed-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        try "instance-id: wecolab-\(hostname)\nlocal-hostname: \(hostname)\n".write(to: dir.appending(path: "meta-data"), atomically: true, encoding: .utf8)
        try userData(invite: invite, hostname: hostname).write(to: dir.appending(path: "user-data"), atomically: true, encoding: .utf8)
        try? FileManager.default.removeItem(at: iso)
        let hdiutil = Process()
        hdiutil.executableURL = URL(filePath: "/usr/bin/hdiutil")
        hdiutil.arguments = ["makehybrid", "-quiet", "-iso", "-joliet", "-default-volume-name", "cidata", "-o", iso.path, dir.path]
        try hdiutil.run()
        hdiutil.waitUntilExit()
        guard hdiutil.terminationStatus == 0 else { throw NodeError("hdiutil makehybrid failed (\(hdiutil.terminationStatus))") }
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: iso.path)  // it holds the invite
    }

    /// First boot: set the hostname, mount the share, install install.sh, the host channel and the join.
    /// The join is a unit that retries until it succeeds, each time running install.sh again, which skips
    /// what an earlier attempt finished; so a network blip, a busy apt or a Stop in the middle is not the
    /// end of it. It joins as a laptop.
    static func userData(invite: Invite, hostname: String) -> String {
        """
        #cloud-config
        hostname: \(hostname)
        write_files:
          - path: /opt/wecolab/join.sh
            permissions: "0755"
            encoding: b64
            content: \(joinScriptBase64)
          - path: /var/lib/wecolab-mac/invite
            permissions: "0600"
            content: \(invite.code)
          - path: /etc/systemd/system/wecolab.mount
            content: |
              # The host channel, mounted at boot, before the node agent's pod bind-mounts /wecolab. A unit
              # rather than cloud-init's mounts:, which drops a device it cannot find under /dev.
              [Unit]
              Description=WeCoLab host channel (virtio-fs tag wecolab)
              Before=k3s-agent.service
              [Mount]
              What=wecolab
              Where=/wecolab
              Type=virtiofs
              [Install]
              WantedBy=multi-user.target
          - path: /usr/local/sbin/wecolab-join
            permissions: "0755"
            content: |
              #!/bin/bash
              # One attempt at the join; wecolab-join.service retries until one succeeds.
              echo "==> wecolab-join, $(date -u +%FT%TZ)"
              # A Stop while apt was installing leaves dpkg interrupted, and then every apt-get fails.
              DEBIAN_FRONTEND=noninteractive dpkg --configure -a || true
              # install.sh takes the invite as $1. Sourced in a subshell, $1 is set without an exec, so the
              # invite is in no process's argv, which anything in the guest can read. Without an invite
              # install.sh would make a new fabric. If install.sh finished (its done marker) but this script
              # never wrote joined, the join counts: a second one dies "already belongs to a fabric", forever.
              ( [ ! -f /var/lib/wecolab/done ] || exit 0; set -- "$(cat /var/lib/wecolab-mac/invite)"; [ -n "$1" ] || exit 1; . /opt/wecolab/join.sh )
              rc=$?
              echo "wecolab-join exit=$rc"
              [ "$rc" != 0 ] || touch /var/lib/wecolab-mac/joined
              /usr/local/sbin/wecolab-share >/dev/null 2>&1  # the attempt's exit is this log's last line
              exit "$rc"
          - path: /etc/systemd/system/wecolab-join.service
            content: |
              [Unit]
              Description=WeCoLab: join this Mac's VM to its site, until it has
              Wants=network-online.target
              After=network-online.target
              ConditionPathExists=!/var/lib/wecolab-mac/joined
              StartLimitIntervalSec=0
              [Service]
              Type=oneshot
              Environment=WECOLAB_LAPTOP=1
              ExecStart=/usr/local/sbin/wecolab-join
              StandardOutput=append:/var/log/wecolab-join.log
              StandardError=inherit
              Restart=on-failure
              RestartSec=15
              RestartSteps=6
              RestartMaxDelaySec=10min
              [Install]
              WantedBy=multi-user.target
          - path: /usr/local/sbin/wecolab-share
            permissions: "0755"
            content: |
              #!/bin/sh
              # Host channel (virtio-fs tag "wecolab"): log the Mac's mode, publish the join log and Nebula status.
              mountpoint -q /wecolab || exit 0
              echo "mac mode: $(cat /wecolab/mode 2>/dev/null || echo unknown)"
              [ -f /var/log/wecolab-join.log ] && tail -c 65536 /var/log/wecolab-join.log > /wecolab/.join.log && mv /wecolab/.join.log /wecolab/wecolab-join.log
              ip=$(ip -o -4 addr show dev nebula1 2>/dev/null | awk '{print $4}')
              if [ -n "$ip" ]; then
                ok=false
                for s in $(cat /etc/nebula/stewards); do curl -s -m 3 -o /dev/null "http://$s:8094/" && { ok=true; break; }; done
                jq -n --arg ip "${ip%/*}" --arg name "$(cat /etc/nebula/name)" --argjson connected "$ok" '{ip:$ip, name:$name, connected:$connected}' > /wecolab/.nebula.json && mv /wecolab/.nebula.json /wecolab/nebula.json
              fi
              exit 0
          - path: /etc/systemd/system/wecolab-share.service
            content: |
              [Unit]
              Description=WeCoLab host channel
              [Service]
              Type=oneshot
              ExecStart=/usr/local/sbin/wecolab-share
          - path: /etc/systemd/system/wecolab-share.timer
            content: |
              [Unit]
              Description=WeCoLab host channel, every 30 s
              [Timer]
              OnBootSec=5
              OnUnitActiveSec=30
              AccuracySec=1
              [Install]
              WantedBy=timers.target
        runcmd:
          - [systemctl, daemon-reload]
          - [systemctl, enable, --now, wecolab.mount]
          - [systemctl, enable, --now, wecolab-share.timer]
          - [systemctl, enable, --now, --no-block, wecolab-join.service]

        """
    }
}
