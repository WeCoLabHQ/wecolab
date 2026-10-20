import Foundation
import WeCoLabCore

let usage = """
usage: wecolab-node list
       wecolab-node status [--node NAME-OR-ID] [--json]
       wecolab-node version [--json] | --version [--json]
       wecolab-node add --name NAME --invite - [resource options] [--network isolated|localNetwork] [--yes]
       wecolab-node up [--node NAME-OR-ID] [resource options]
       wecolab-node down [--node NAME-OR-ID]
       wecolab-node configure [--node NAME-OR-ID] [resource options] [--auto-start true|false]
       wecolab-node reset|remove --node NAME-OR-ID [--yes]

resource options: --cpus N --memory GiB --disk GiB --idle-minutes N --idle-cpus N --idle-memory GiB
--invite - reads a one-time invite from stdin; never put it in argv.
--network isolated is the default and cannot start: this build has no verified OS network firewall.
localNetwork is only for trusted fabrics; it does not guarantee Mac/LAN exclusion.
App and CLI share one runner. Closing the app window or exiting the CLI leaves nodes running; Quit WeCoLab stops all nodes.
With multiple nodes, mutation commands require --node. reset/remove never revoke remote membership.
--root PATH uses a separate state directory and launch agent for development or disposable smoke runs.
"""

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("wecolab-node: \(message)\n".utf8))
    exit(EXIT_FAILURE)
}

struct Arguments {
    let command: String
    let values: [String: String]
    let yes: Bool
    let json: Bool
    init(_ args: [String]) throws {
        command = args.first ?? "help"
        var values: [String: String] = [:], yes = false, json = false
        let allowed: Set<String> = ["node", "name", "invite", "cpus", "memory", "disk", "idle-minutes", "idle-cpus", "idle-memory", "network", "auto-start", "root"]
        var index = 1
        while index < args.count {
            let flag = args[index]
            if flag == "--yes" { guard !yes else { throw NodeError("duplicate --yes") }; yes = true; index += 1; continue }
            if flag == "--json" { guard !json else { throw NodeError("duplicate --json") }; json = true; index += 1; continue }
            guard flag.hasPrefix("--"), allowed.contains(String(flag.dropFirst(2))), index + 1 < args.count else {
                throw NodeError("unknown option or missing value: \(flag)")
            }
            let name = String(flag.dropFirst(2))
            guard values[name] == nil else { throw NodeError("duplicate --\(name)") }
            values[name] = args[index + 1]
            index += 2
        }
        self.values = values; self.yes = yes; self.json = json
    }
    func number(_ key: String) throws -> Int? {
        guard let text = values[key] else { return nil }
        guard let value = Int(text) else { throw NodeError("--\(key) needs a whole number") }
        return value
    }
    func apply(to profile: inout NodeProfile) throws {
        if let name = values["name"] { profile.name = name }
        if let value = try number("cpus") { profile.config.cpus = value }
        if let value = try number("memory") { profile.config.memoryGiB = value }
        if let value = try number("disk") { profile.config.diskGiB = value }
        if let value = try number("idle-minutes") { profile.config.idleMinutes = value }
        if let value = try number("idle-cpus") { profile.config.idleCPUs = value }
        if let value = try number("idle-memory") { profile.config.idleMemoryGiB = value }
        if let text = values["network"] {
            guard let value = NodeNetworkAccess(rawValue: text) else { throw NodeError("--network must be isolated or localNetwork") }
            profile.networkAccess = value
        }
        if let text = values["auto-start"] {
            guard text == "true" || text == "false" else { throw NodeError("--auto-start must be true or false") }
            profile.autoStart = text == "true"
        }
        try profile.config.validate()
    }
}

func selectNode(_ snapshot: RunnerSnapshot, selector: String?) throws -> NodeSnapshot {
    if let selector {
        let matches = snapshot.nodes.filter { $0.id == selector.lowercased() || $0.profile.name.caseInsensitiveCompare(selector) == .orderedSame }
        guard matches.count == 1, let node = matches.first else { throw NodeError("No unique node matches \(selector). Run wecolab-node list.") }
        return node
    }
    guard snapshot.nodes.count == 1, let node = snapshot.nodes.first else {
        if snapshot.legacyPID != nil { throw NodeError(snapshot.message) }
        throw NodeError(snapshot.nodes.isEmpty ? "No nodes yet. Add a node with an invite first." : "More than one node exists; select one with --node NAME-OR-ID.")
    }
    return node
}

func confirm(_ question: String, yes: Bool) throws {
    if yes { return }
    guard isatty(STDIN_FILENO) != 0 else { throw NodeError("\(question) Run interactively or explicitly pass --yes.") }
    print(question + " [y/N] ", terminator: "")
    guard readLine()?.lowercased() == "y" else { throw NodeError("cancelled") }
}

func warnLocalNetwork(requiresConfirmation: Bool, yes: Bool) throws {
    let warning = "WARNING: Trusted LAN mode lets this fabric share the Mac's network. Filtering does not guarantee exclusion of the Mac, LAN devices or peer VMs. Do not use it for an untrusted fabric."
    FileHandle.standardError.write(Data((warning + "\n").utf8))
    if requiresConfirmation { try confirm("Enable trusted LAN networking for this fabric?", yes: yes) }
}

func readInvite(_ options: Arguments) throws -> String {
    guard options.values["invite"] == "-" else { throw NodeError("use --invite - to read the invite from stdin, never the command line") }
    if isatty(STDIN_FILENO) != 0 { print("Invite (wcl2.…): ", terminator: "") }
    guard let text = readLine() else { throw NodeError("no invite on stdin") }
    let code = text.trimmingCharacters(in: .whitespacesAndNewlines)
    let invite = try Invite(code)
    try confirm("Join \(invite.console)? Its administrators will control this VM as root.", yes: options.yes)
    return code
}

func show(_ node: NodeSnapshot) {
    print("node    \(node.profile.name) (\(node.id))")
    print("fabric  \(node.consoleURL.isEmpty ? "not enrolled" : node.consoleURL)")
    print("vm      \(node.state)" + (node.pid.map { " (pid \($0))" } ?? ""))
    print("network \(node.profile.networkAccess.rawValue)")
    let c = node.profile.config
    print("config  \(c.cpus) vCPUs, \(c.memoryGiB) GiB memory, \(c.diskGiB) GiB disk")
    print("idle    after \(c.idleMinutes) min: up to \(c.idleCPUs) vCPUs, \(c.idleMemoryGiB) GiB memory")
    if let actual = node.runningConfig {
        print("running \(actual.idleCPUs) maximum vCPUs, \(actual.idleMemoryGiB) GiB maximum memory; \(actual.memoryGiB) GiB active reservation")
    }
    if node.needsRestart { print("pending settings require the next VM start") }
    print("mode    \(node.mode.isEmpty ? "-" : node.mode)")
    print("mesh    \(node.meshIP.isEmpty ? "-" : node.meshIP) \(node.meshName)" + (node.guestStatusFresh ? (node.meshConnected ? ", a steward answers" : ", no steward answers") : ", guest report stale or VM stopped"))
    print("join    \(node.join)")
    if !node.message.isEmpty { print("status  \(node.message)") }
}

setvbuf(stdout, nil, _IOLBF, 0)
do {
    let args = Array(CommandLine.arguments.dropFirst())
    if args.first == "--version" {
        guard args == ["--version"] || args == ["--version", "--json"] else { throw NodeError(usage) }
        print(args.contains("--json") ? BuildInfo.json : BuildInfo.display)
        exit(EXIT_SUCCESS)
    }
    let options = try Arguments(args)
    if options.command == "help" { print(usage); exit(EXIT_SUCCESS) }
    if options.command == "version" {
        guard options.values.isEmpty, !options.yes else { throw NodeError(usage) }
        print(options.json ? BuildInfo.json : BuildInfo.display)
        exit(EXIT_SUCCESS)
    }
    let commands: Set<String> = ["list", "status", "add", "up", "down", "configure", "reset", "remove"]
    guard commands.contains(options.command) else { throw NodeError(usage) }
    let locations = RunnerLocations(root: options.values["root"].map { URL(fileURLWithPath: $0, isDirectory: true) } ?? RunnerLocations.defaultRoot)
    let client = RunnerClient(locations: locations)
    var snapshot = try await client.send(.status)
    switch options.command {
    case "list", "status":
        let nodes = options.values["node"] == nil ? snapshot.nodes : [try selectNode(snapshot, selector: options.values["node"])]
        if options.json {
            snapshot.nodes = nodes
            let encoder = JSONEncoder(); encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
            print(String(decoding: try encoder.encode(snapshot), as: UTF8.self))
        } else {
            if let pid = snapshot.legacyPID { print("vm      running under the original app/CLI (pid \(pid)); migration requires one graceful stop") }
            if !snapshot.message.isEmpty { print(snapshot.message) }
            if nodes.isEmpty && snapshot.legacyPID == nil { print("No nodes configured.") }
            for node in nodes { show(node) }
            print("budget  \(snapshot.reservedCPUs)/\(snapshot.cpuBudget) maximum vCPUs, \(snapshot.reservedMemoryGiB)/\(snapshot.memoryBudgetGiB) GiB maximum memory reserved")
        }
    case "add":
        guard let name = options.values["name"], !name.isEmpty else { throw NodeError("add needs --name") }
        var profile = NodeProfile(name: name)
        try options.apply(to: &profile)
        if profile.networkAccess == .localNetwork {
            try warnLocalNetwork(requiresConfirmation: true, yes: options.yes)
        }
        profile.config.invite = try readInvite(options)
        snapshot = try await client.send(.create(profile, confirmedConsole: Invite(profile.config.invite).console))
        show(try selectNode(snapshot, selector: profile.id))
    case "up", "configure":
        var node = try selectNode(snapshot, selector: options.values["node"])
        var changed = node.profile
        try options.apply(to: &changed)
        if changed.networkAccess == .localNetwork,
           options.command == "up" || changed.networkAccess != node.profile.networkAccess {
            try warnLocalNetwork(requiresConfirmation: changed.networkAccess != node.profile.networkAccess, yes: options.yes)
        }
        var confirmed: String?
        if options.values["invite"] != nil {
            changed.config.invite = try readInvite(options)
            confirmed = try Invite(changed.config.invite).console
        }
        if changed != node.profile { snapshot = try await client.send(.configure(changed, confirmedConsole: confirmed)) }
        if options.command == "up" {
            snapshot = try await client.send(.start(node.id))
            let deadline = Date().addingTimeInterval(900)
            var lastMessage = ""
            while let current = snapshot.nodes.first(where: { $0.id == node.id }), current.busy {
                if !current.message.isEmpty && current.message != lastMessage { print(current.message); lastMessage = current.message }
                guard Date() < deadline else { throw NodeError("Node preparation continues in the background; use status to inspect it.") }
                try await Task.sleep(for: .seconds(1))
                snapshot = try await client.send(.status)
            }
        }
        node = try selectNode(snapshot, selector: node.id)
        show(node)
        if options.command == "up" && node.state != "running" { throw NodeError(node.message.isEmpty ? "Node did not reach running state." : node.message) }
    case "down":
        let node = try selectNode(snapshot, selector: options.values["node"])
        snapshot = try await client.send(.stop(node.id))
        let deadline = Date().addingTimeInterval(90)
        while snapshot.nodes.contains(where: { $0.id == node.id && $0.running }) {
            guard Date() < deadline else { throw NodeError("The VM still owns its disk after 90 seconds; inspect status before trying again.") }
            try await Task.sleep(for: .seconds(1))
            snapshot = try await client.send(.status)
        }
        show(try selectNode(snapshot, selector: node.id))
    case "reset", "remove":
        let node = try selectNode(snapshot, selector: options.values["node"])
        guard !node.running else { throw NodeError("Stop this node first.") }
        try confirm("Delete \(node.profile.name)'s local VM and identity? Remove its box in its fabric Console first; this cannot revoke remote membership.", yes: options.yes)
        snapshot = try await client.send(options.command == "reset" ? .reset(node.id) : .remove(node.id))
        print("\(options.command == "reset" ? "Reset" : "Removed") \(node.profile.name). Other nodes were not changed.")
    default: break
    }
} catch { fail(error.localizedDescription) }
