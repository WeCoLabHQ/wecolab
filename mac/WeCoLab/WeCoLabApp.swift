import ServiceManagement
import SwiftUI
import WeCoLabCore

@main struct WeCoLabApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    var body: some Scene {
        Window("WeCoLab", id: "main") { MainView(model: delegate.model) }
            .defaultSize(width: 760, height: 720)
        MenuBarExtra {
            MenuView(model: delegate.model)
        } label: {
            if delegate.model.connectionError.isEmpty, delegate.model.snapshot != nil {
                Image(nsImage: menuBarMark(running: delegate.model.anyRunning))
                    .accessibilityLabel(delegate.model.anyRunning ? "WeCoLab nodes running" : "WeCoLab nodes stopped")
            } else {
                Image(systemName: "questionmark.circle").accessibilityLabel("WeCoLab runner unavailable")
            }
        }
    }
}

@MainActor final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = Model()
    func applicationWillFinishLaunching(_ notification: Notification) {
        NotificationCenter.default.addObserver(self, selector: #selector(windowChanged), name: NSWindow.didChangeOcclusionStateNotification, object: nil)
    }
    @objc private func windowChanged(_ notification: Notification) {
        guard let window = notification.object as? NSWindow, window.identifier?.rawValue == "main" else { return }
        NSApp.setActivationPolicy(window.isVisible || window.isMiniaturized ? .regular : .accessory)
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard !model.quitting else { return .terminateLater }
        model.quitting = true
        Task {
            let stopped = await model.stopForQuit()
            sender.reply(toApplicationShouldTerminate: stopped)
            if !stopped {
                model.quitting = false
                sender.activate(ignoringOtherApps: true)
                let alert = NSAlert()
                alert.alertStyle = .warning
                alert.messageText = "Could not shut down all nodes"
                alert.informativeText = model.operationError
                alert.addButton(withTitle: "Keep WeCoLab open")
                alert.runModal()
            }
        }
        return .terminateLater
    }
}

@MainActor @Observable final class Model {
    var snapshot: RunnerSnapshot?
    var selectedID: String?
    var connectionError = ""
    var operationError = ""
    var quitting = false
    private var operationInFlight = false
    var busy: Bool { quitting || operationInFlight || snapshot?.stoppingAll == true }
    private let client: RunnerClient
    private var refreshing = false
    var anyRunning: Bool { snapshot?.anyRunning ?? false }
    var selected: NodeSnapshot? { snapshot?.nodes.first { $0.id == selectedID } }
    var connected: Bool { snapshot != nil && connectionError.isEmpty }

    init() {
        let args = CommandLine.arguments
        let root: URL
        if let index = args.firstIndex(of: "--root"), index + 1 < args.count {
            root = URL(fileURLWithPath: args[index + 1], isDirectory: true)
        } else { root = RunnerLocations.defaultRoot }
        client = RunnerClient(locations: RunnerLocations(root: root))
        Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.refresh()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    func refresh() async {
        guard !refreshing, !operationInFlight, !quitting else { return }
        refreshing = true
        defer { refreshing = false }
        do { accept(try await client.send(.status)) }
        catch { connectionError = error.localizedDescription }
    }

    private func accept(_ snapshot: RunnerSnapshot) {
        self.snapshot = snapshot
        connectionError = ""
        if !snapshot.nodes.contains(where: { $0.id == selectedID }) { selectedID = snapshot.nodes.first?.id }
    }

    @discardableResult func perform(_ command: RunnerCommand) async -> Bool {
        guard !busy else { return false }
        operationInFlight = true
        operationError = ""
        defer { operationInFlight = false }
        do { accept(try await client.send(command)); return true }
        catch { operationError = error.localizedDescription; return false }
    }

    func stopForQuit() async -> Bool {
        operationError = ""
        do {
            // Finish requests already sent by this app before taking the runner's shutdown gate.
            while operationInFlight || refreshing { try await Task.sleep(for: .milliseconds(50)) }
            accept(try await client.send(.stopAll))
            guard snapshot?.anyRunning == false else {
                throw NodeError("The runner has not confirmed that every VM has stopped.")
            }
            return true
        } catch {
            operationError = error.localizedDescription
            return false
        }
    }

    func startStop(_ node: NodeSnapshot) async {
        _ = await perform(node.running ? .stop(node.id) : .start(node.id))
    }

    func confirmTrust(_ inviteText: String) -> String? {
        do {
            let invite = try Invite(inviteText.trimmingCharacters(in: .whitespacesAndNewlines))
            let alert = NSAlert()
            alert.messageText = "Join through \(invite.console)?"
            alert.informativeText = "This fabric's administrators will control this VM as root. The node gets its own disk, credentials and resource limits. Join only if you trust this Console."
            alert.addButton(withTitle: "Join this fabric")
            alert.addButton(withTitle: "Cancel")
            return alert.runModal() == .alertFirstButtonReturn ? invite.console : nil
        } catch { operationError = error.localizedDescription; return nil }
    }

    func delete(_ node: NodeSnapshot, removeProfile: Bool) async {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = removeProfile ? "Remove \(node.profile.name)?" : "Reset \(node.profile.name)'s VM?"
        alert.informativeText = "This deletes only this stopped node's local disk, identity and invite. First remove its box in its fabric Console; deleting it here does not revoke remote membership. Other nodes are not affected."
        alert.addButton(withTitle: removeProfile ? "Remove node" : "Reset VM")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        _ = await perform(removeProfile ? .remove(node.id) : .reset(node.id))
    }
}

struct MainView: View {
    @Bindable var model: Model
    @State private var adding = false
    @State private var loginStatus = SMAppService.mainApp.status
    @State private var loginError = ""

    var body: some View {
        NavigationSplitView {
            VStack(spacing: 0) {
                List(selection: $model.selectedID) {
                    ForEach(model.snapshot?.nodes ?? []) { node in
                        VStack(alignment: .leading, spacing: 4) {
                            Text(node.profile.name).font(.headline)
                            Text(model.connected ? node.state.capitalized : "Status unavailable").font(.caption).foregroundStyle(.secondary)
                        }.tag(node.id)
                    }
                }
                Button("Add node…", systemImage: "plus") { adding = true }
                    .padding().disabled(!model.connected || model.busy)
            }
            .navigationSplitViewColumnWidth(min: 180, ideal: 200)
        } detail: {
            VStack(spacing: 0) {
                if model.quitting || model.snapshot?.stoppingAll == true {
                    ProgressView("Shutting down all nodes…").padding()
                }
                if !model.connectionError.isEmpty {
                    Label("Runner unavailable: \(model.connectionError)", systemImage: "exclamationmark.triangle")
                        .font(.callout).foregroundStyle(.red).padding().frame(maxWidth: .infinity, alignment: .leading)
                }
                if let message = model.snapshot?.message, !message.isEmpty {
                    Text(message).font(.callout).padding().frame(maxWidth: .infinity, alignment: .leading)
                }
                if let pid = model.snapshot?.legacyPID {
                    Text("Your original node is running (PID \(pid)). Stop it gracefully once to migrate it; do not reset its VM.")
                        .padding().frame(maxWidth: .infinity, alignment: .leading)
                }
                if !model.operationError.isEmpty {
                    Text(model.operationError).foregroundStyle(.red).font(.callout).padding().frame(maxWidth: .infinity, alignment: .leading)
                }
                if let node = model.selected {
                    NodeDetail(model: model, node: node).id(node.id)
                } else {
                    ContentUnavailableView("Your Mac nodes", systemImage: "desktopcomputer",
                                           description: Text(model.connected ? "Add a node with an invite from a fabric. Each node runs in a separate VM." : "Connecting to the shared background runner…"))
                }
                Divider()
                VStack(alignment: .leading, spacing: 6) {
                    if let state = model.snapshot {
                        Text("Runner reservations: \(state.reservedCPUs) / \(state.cpuBudget) vCPUs · \(state.reservedMemoryGiB) / \(state.memoryBudgetGiB) GiB (including \(ResourceBudget.workerMemoryGiB) GiB overhead per VM)")
                            .font(.caption).monospacedDigit()
                    }
                    Toggle("Open WeCoLab at login", isOn: Binding(get: { loginStatus == .enabled }, set: { setLogin($0) }))
                    if loginStatus == .requiresApproval {
                        Button("Allow in Login Items…") { SMAppService.openSystemSettingsLoginItems() }
                    }
                    if !loginError.isEmpty { Text(loginError).font(.caption).foregroundStyle(.red) }
                    Text("Closing this window keeps nodes running in the menu bar. Quit WeCoLab shuts down all nodes, including those started through the CLI.")
                        .font(.caption).foregroundStyle(.secondary)
                }.padding().frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .sheet(isPresented: $adding) { AddNodeView(model: model) }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            loginStatus = SMAppService.mainApp.status
            Task { await model.refresh() }
        }
    }

    private func setLogin(_ enabled: Bool) {
        do {
            try enabled ? SMAppService.mainApp.register() : SMAppService.mainApp.unregister()
            loginError = ""
        } catch { loginError = error.localizedDescription }
        loginStatus = SMAppService.mainApp.status
    }
}

struct NodeDetail: View {
    let model: Model
    let node: NodeSnapshot
    @State private var draft: NodeProfile
    init(model: Model, node: NodeSnapshot) {
        self.model = model; self.node = node
        _draft = State(initialValue: node.profile)
    }
    private var dirty: Bool { draft != node.profile }
    private var cpuMaximum: Int { max(1, model.snapshot?.cpuBudget ?? 1) }
    private var memoryMaximum: Int { max(2, model.snapshot?.memoryBudgetGiB ?? 2) }

    var body: some View {
        Form {
            Section("Node") {
                TextField("Name", text: $draft.name)
                LabeledContent("Fabric", value: node.consoleURL.isEmpty ? "Not enrolled" : node.consoleURL)
                LabeledContent("VM", value: model.connected ? node.state.capitalized : "Unavailable (last state: \(node.state))")
                if !node.mode.isEmpty { LabeledContent("Mac activity", value: node.mode) }
                LabeledContent("Mesh", value: node.meshIP.isEmpty ? "—" : node.meshIP)
                if !node.meshIP.isEmpty && !node.guestStatusFresh {
                    Text("Guest network report is stale or the VM is stopped.").font(.caption).foregroundStyle(.secondary)
                }
                LabeledContent("Join", value: node.join)
                if !node.message.isEmpty { Text(node.message).font(.caption) }
                if let blocker = node.profile.networkAccess.startupBlocker {
                    Text(blocker).font(.callout).foregroundStyle(.orange)
                }
                if node.join != "finished", !node.lastJoinLine.isEmpty { Text(node.lastJoinLine).font(.caption.monospaced()).lineLimit(3) }
            }
            if node.guestGeneration == "none" {
                Section("Invite") {
                    SecureField(node.consoleURL.isEmpty ? "Paste a fabric invite" : "New invite (leave blank to keep current)", text: $draft.config.invite)
                    Text("An invite is used only for this VM's first join.").font(.caption).foregroundStyle(.secondary)
                }
            }
            ResourceFields(config: $draft.config, cpuMaximum: cpuMaximum, memoryMaximum: memoryMaximum)
            Section("Isolation and startup") {
                NetworkPicker(access: $draft.networkAccess)
                Toggle("Start this node when the background runner starts", isOn: $draft.autoStart)
                Text("The runner starts at login independently of the menu-bar app.").font(.caption).foregroundStyle(.secondary)
            }
            if node.needsRestart {
                Text("Saved CPU, disk, idle or network settings differ from this running VM. Stop and start this node to apply them.")
                    .font(.caption).foregroundStyle(.orange)
            }
            if let actual = node.runningConfig {
                Text("Running VM maximum: \(actual.idleCPUs) vCPUs and \(actual.idleMemoryGiB) GiB. Active reservation: \(actual.memoryGiB) GiB.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            HStack {
                Button("Apply settings") {
                    Task {
                        var confirmed: String?
                        if !draft.config.invite.isEmpty {
                            guard let console = model.confirmTrust(draft.config.invite) else { return }
                            confirmed = console
                        }
                        if await model.perform(.configure(draft, confirmedConsole: confirmed)),
                           let saved = model.snapshot?.nodes.first(where: { $0.id == node.id }) {
                            draft = saved.profile
                        }
                    }
                }.disabled(!dirty || model.busy || !model.connected || node.busy)
                Spacer()
                NodeStartStop(model: model, node: node)
            }
            Section {
                HStack {
                    Button("Reset VM…", role: .destructive) { Task { await model.delete(node, removeProfile: false) } }
                    Spacer()
                    Button("Remove node…", role: .destructive) { Task { await model.delete(node, removeProfile: true) } }
                }.disabled(node.running || model.busy || !model.connected)
            }
        }
        .formStyle(.grouped)
        .onChange(of: node.profile) { old, new in
            if draft == old || draft == new { draft = new }
        }
    }
}

struct AddNodeView: View {
    let model: Model
    @Environment(\.dismiss) private var dismiss
    @State private var profile = NodeProfile(name: "")
    var body: some View {
        VStack(spacing: 0) {
            Text("Add a separate VM node").font(.title2).padding()
            Form {
                TextField("Name", text: $profile.name, prompt: Text("My fabric or Friend's fabric"))
                SecureField("Fabric invite", text: $profile.config.invite)
                ResourceFields(config: $profile.config, cpuMaximum: max(1, model.snapshot?.cpuBudget ?? 1), memoryMaximum: max(2, model.snapshot?.memoryBudgetGiB ?? 2))
                NetworkPicker(access: $profile.networkAccess)
                Toggle("Start when the background runner starts", isOn: $profile.autoStart)
                Text("This creates a separate VM, disk and identity. It does not share another node's credentials or host folder.")
                    .font(.caption).foregroundStyle(.secondary)
                if !model.operationError.isEmpty { Text(model.operationError).foregroundStyle(.red) }
            }.formStyle(.grouped)
            HStack {
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Spacer()
                Button("Add node") {
                    guard let console = model.confirmTrust(profile.config.invite) else { return }
                    profile.config.invite = profile.config.invite.trimmingCharacters(in: .whitespacesAndNewlines)
                    Task {
                        if await model.perform(.create(profile, confirmedConsole: console)) {
                            model.selectedID = profile.id
                            dismiss()
                        }
                    }
                }.keyboardShortcut(.defaultAction)
                    .disabled(model.busy || profile.name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || profile.config.invite.isEmpty)
            }.padding()
        }.frame(width: 520, height: 640)
    }
}

struct ResourceFields: View {
    @Binding var config: NodeConfig
    let cpuMaximum: Int
    let memoryMaximum: Int
    var body: some View {
        Section("What this node contributes") {
            IntSlider(label: "vCPUs", value: $config.cpus, range: 1...max(cpuMaximum, config.cpus), unit: "")
            IntSlider(label: "Memory", value: $config.memoryGiB, range: 2...max(memoryMaximum, config.memoryGiB), unit: "GiB")
            IntSlider(label: "Disk", value: $config.diskGiB, range: 8...max(256, config.diskGiB), unit: "GiB")
            Text("Memory can change live up to the running VM's maximum. vCPUs and disk apply at the next start; disks cannot shrink. vCPUs are not dedicated physical cores.")
                .font(.caption).foregroundStyle(.secondary)
        }
        Section("When this Mac is idle") {
            IntSlider(label: "Idle after", value: $config.idleMinutes, range: 1...max(60, config.idleMinutes), unit: "min")
            IntSlider(label: "Maximum vCPUs", value: $config.idleCPUs, range: config.cpus...max(cpuMaximum, config.idleCPUs), unit: "")
            IntSlider(label: "Maximum memory", value: $config.idleMemoryGiB, range: config.memoryGiB...max(memoryMaximum, config.idleMemoryGiB), unit: "GiB")
            Text("The runner reserves these maxima across running nodes, even while you are active. Changes apply on the next start.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }
}

struct NetworkPicker: View {
    @Binding var access: NodeNetworkAccess
    var body: some View {
        Picker("Network access", selection: $access) {
            Text("Strict isolation — unavailable").tag(NodeNetworkAccess.isolated)
            Text("Trusted LAN access").tag(NodeNetworkAccess.localNetwork)
        }
        Text(access == .isolated
             ? "Startup is blocked: this build has no verified OS-enforced network firewall. App Sandbox alone does not exclude the Mac or LAN."
             : "Only for a trusted fabric: its administrators share this Mac's network. Destination filtering is defense in depth, not a guarantee against Mac, LAN or peer access.")
            .font(.caption).foregroundStyle(access == .isolated ? Color.secondary : Color.orange)
    }
}

struct IntSlider: View {
    let label: String
    @Binding var value: Int
    let range: ClosedRange<Int>
    let unit: String
    var body: some View {
        LabeledContent {
            HStack {
                Slider(value: Binding(get: { Double(value) }, set: { value = min(Int($0.rounded()), range.upperBound) }),
                       in: Double(range.lowerBound)...Double(max(range.upperBound, range.lowerBound + 1)))
                    .accessibilityLabel(label)
                Text("\(value) \(unit)").monospacedDigit().frame(width: 64, alignment: .trailing)
            }
        } label: { Text(label) }
    }
}

struct NodeStartStop: View {
    let model: Model
    let node: NodeSnapshot
    var body: some View {
        Button(node.running ? "Stop node" : "Start node") { Task { await model.startStop(node) } }
            .disabled(model.busy || !model.connected || node.state == "stopping" ||
                      (!node.running && node.profile.networkAccess.startupBlocker != nil))
    }
}

struct MenuView: View {
    let model: Model
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        if !model.connected {
            Text("Background runner unavailable")
            if !model.connectionError.isEmpty { Text(model.connectionError) }
        } else {
            let nodes = model.snapshot?.nodes ?? []
            Text("\(nodes.filter(\.running).count) of \(nodes.count) nodes running")
            if let pid = model.snapshot?.legacyPID { Text("Original node running (PID \(pid)); migration pending") }
            ForEach(nodes) { node in
                Menu("\(node.profile.name): \(node.state)") {
                    Text("Mesh: \(node.meshIP.isEmpty ? "—" : node.meshIP)")
                    Text("Join: \(node.join)")
                    if !node.message.isEmpty { Text(node.message) }
                    NodeStartStop(model: model, node: node)
                }
            }
        }
        if !model.operationError.isEmpty { Text(model.operationError) }
        Divider()
        Button("Open WeCoLab") { openWindow(id: "main"); NSApp.activate() }
        Divider()
        Button("Quit WeCoLab") { NSApp.terminate(nil) }.keyboardShortcut("q").disabled(model.quitting)
    }
}
