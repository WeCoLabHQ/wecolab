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

    public static func guest(_ paths: Paths = .user) -> Guest {
        guard FileManager.default.fileExists(atPath: paths.disk.path) else { return .none }
        let stamped = (try? String(contentsOf: paths.guestVersion, encoding: .utf8)) == generation
        return stamped && !FileManager.default.fileExists(atPath: paths.share.appending(path: "netbird.json").path) ? .current : .old
    }

    /// Makes sure disk and seed exist, then grows the disk to the configured size. Only the first start
    /// reads the invite, and only after `confirm`: whoever runs its Console gets root on the VM.
    public static func run(_ config: NodeConfig, at paths: Paths = .user, confirm: @Sendable (Invite) async -> Bool,
                           log: @Sendable (String) -> Void) async throws {
        let fm = FileManager.default
        try paths.prepare()
        switch guest(paths) {
        case .old:
            throw NodeError("this VM was made by an older WeCoLab for Mac and does not start here; Reset it, then add this Mac again")
        case .none:
            let invite = try Invite(config.invite)
            guard await confirm(invite) else { throw NodeError("not started: the invite's Console was not confirmed") }
            // What v1 or an earlier start left (identity, EFI variables, share files) is not this guest's.
            try removeGuest(paths)
            try paths.prepare()
            if !fm.fileExists(atPath: paths.image.path) { try await download(to: paths.image, log: log) }
            try seed(invite: invite, hostname: guestHostname(), to: paths.seed)
            try Data(generation.utf8).write(to: paths.guestVersion)  // before the disk: a disk is never unstamped by accident
            try fm.copyItem(at: paths.image, to: paths.disk)  // an APFS clone: instant, shares blocks with the image
            log("first boot will join through \(invite.console) as \(guestHostname())")
        case .current:
            break
        }
        try grow(paths.disk, toGiB: config.diskGiB)
    }

    /// Deletes the guest (disk, seed, identity, share) and the spent invite; the downloaded image stays.
    /// Joining again takes the same box name, so the old box has to be removed in the Console first.
    public static func reset(_ paths: Paths = .user) throws -> NodeConfig {
        let lock = try VMLock.acquire(paths)  // refuses while a VM runs, here or in the CLI
        defer { VMLock.release(lock) }
        try removeGuest(paths)
        var config = NodeConfig.load(paths)
        config.invite = ""
        try config.save(paths)
        return config
    }

    /// Everything that is one guest's; the image, the settings and the lock stay.
    static func removeGuest(_ paths: Paths) throws {
        for url in [paths.disk, paths.guestVersion, paths.seed, paths.efiVars, paths.machineID, paths.macAddress, paths.console, paths.share] {
            do { try FileManager.default.removeItem(at: url) } catch CocoaError.fileNoSuchFile {}
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
        let file = try FileHandle(forWritingTo: url)
        defer { try? file.close() }
        let want = UInt64(gib) << 30, have = try file.seekToEnd()
        guard want >= have else { throw NodeError("the disk is already \(have >> 30) GiB; it can grow but not shrink") }
        if want > have { try file.truncate(atOffset: want) }
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
