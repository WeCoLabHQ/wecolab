// wecolab-node: the Mac node VM, headless. Same core as the app, driven from flags.
import Foundation
import WeCoLabCore

let usage = """
    usage: wecolab-node up [--invite -] [--cpus N] [--memory GiB] [--disk GiB]
                           [--idle-minutes N] [--idle-cpus N] [--idle-memory GiB]
                           --invite - reads the invite (Console: Sites, your site, Add a Mac) from stdin
           wecolab-node status
           wecolab-node down
           wecolab-node reset      delete the guest (disk, seed, identity, share) and its spent invite, so
                                   `up --invite -` joins afresh. The new join takes the same name (mac-<name>):
                                   remove the old box in the Console first (Sites, your site, the box, Remove).
    """

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("wecolab-node: \(message)\n".utf8))
    exit(1)
}

setvbuf(stdout, nil, _IOLBF, 0)  // progress lines show up promptly when redirected to a file
let args = Array(CommandLine.arguments.dropFirst())
func flag(_ name: String) -> String? {
    guard let i = args.firstIndex(of: "--\(name)") else { return nil }
    guard i + 1 < args.count else { fail("--\(name) needs a value") }
    return args[i + 1]
}
func number(_ name: String) -> Int? {
    guard let value = flag(name) else { return nil }
    guard let n = Int(value) else { fail("--\(name) takes a whole number") }
    return n
}

switch args.first {
case "up":
    var config = NodeConfig.load()
    if let invite = flag("invite") {
        // A one-time code, so never in argv, which every process on the Mac can read.
        guard invite == "-" else { fail("--invite takes - and reads the invite from stdin, never from the command line") }
        if isatty(STDIN_FILENO) != 0 { FileHandle.standardOutput.write(Data("Invite (wcl2.…): ".utf8)) }
        config.invite = readLine()?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
    }
    config.cpus = number("cpus") ?? config.cpus
    config.memoryGiB = number("memory") ?? config.memoryGiB
    config.diskGiB = number("disk") ?? config.diskGiB
    config.idleMinutes = number("idle-minutes") ?? config.idleMinutes
    config.idleCPUs = number("idle-cpus") ?? config.idleCPUs  // after --cpus, which raises it when needed
    config.idleMemoryGiB = number("idle-memory") ?? config.idleMemoryGiB
    let vm = NodeVM()
    do {
        _ = try VMLock.acquire()  // held until this process exits
        try config.validate()
        try config.save()
        try await Provision.run(config, confirm: { invite in
            FileHandle.standardOutput.write(Data("The first start joins through \(invite.console), which gets root on the VM. Continue? [y/N] ".utf8))
            return readLine()?.lowercased().hasPrefix("y") == true
        }) { print($0) }
        try await vm.start(config)
    } catch {
        fail(error.localizedDescription)
    }
    print("VM running: \(config.cpus) CPUs and \(config.memoryGiB) GiB reserved, \(config.idleCPUs) CPUs and \(config.idleMemoryGiB) GiB when idle, \(config.diskGiB) GiB disk. Stop with: wecolab-node down")
    for sig in [SIGINT, SIGTERM] { signal(sig, SIG_IGN) }
    let signals = [SIGINT, SIGTERM].map { sig in
        let source = DispatchSource.makeSignalSource(signal: sig, queue: .main)
        source.setEventHandler { print("stopping the guest"); Task { await vm.stop() } }
        source.resume()
        return source
    }
    var lastMode = ""
    while await vm.isRunning {
        await vm.tick()
        if await vm.mode != lastMode { lastMode = await vm.mode; print("mac is \(lastMode)") }
        try? await Task.sleep(for: .seconds(5))
    }
    print("VM stopped")
    withExtendedLifetime(signals) {}  // `_ = signals` lets an optimized build free the sources early

case "status":
    let config = NodeConfig.load()
    let guest = GuestStatus.read()
    let invite = (try? Invite(config.invite)).map(\.console)
    let site = switch Provision.guest() {
    case .old: "a VM from an older WeCoLab for Mac: reset it"
    case .current: invite.map { "VM made, joins through \($0)" } ?? "VM made"
    case .none: invite.map { "joins through \($0)" } ?? "no invite"
    }
    print("config  \(config.cpus) CPUs, \(config.memoryGiB) GiB memory, \(config.diskGiB) GiB disk, \(site)")
    print("idle    after \(config.idleMinutes) min: \(config.idleCPUs) CPUs, \(config.idleMemoryGiB) GiB memory")
    print("vm      " + (VMLock.holder().map { "running (pid \($0))" } ?? "stopped"))
    print("mode    " + (readShare("mode", limit: 16) ?? "-"))
    print("mesh    " + (guest.meshIP.isEmpty ? "-" : "\(guest.meshIP) \(guest.meshName), \(guest.meshConnected ? "a steward answers" : "no steward answers")"))
    print("join    \(guest.join)")
    for line in guest.joinLog.split(separator: "\n").suffix(5) { print("        \(line)") }

case "down":
    guard let pid = VMLock.holder() else { print("not running"); exit(0) }
    kill(pid, SIGTERM)
    for _ in 0..<90 {
        if VMLock.holder() == nil { print("stopped"); exit(0) }
        sleep(1)
    }
    fail("pid \(pid) is still running after 90 s")

case "reset":
    do { _ = try Provision.reset() } catch { fail(error.localizedDescription) }
    print("guest and invite deleted; the downloaded image and the other settings are kept")

default:
    print(usage)
    exit(args.isEmpty || args.first == "help" ? 0 : 2)
}
