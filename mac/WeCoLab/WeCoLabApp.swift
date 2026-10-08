import ServiceManagement
import SwiftUI
import WeCoLabCore

@main struct WeCoLabApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate

    var body: some Scene {
        Window("WeCoLab", id: "main") { MainView(model: delegate.model) }
            .windowResizability(.contentSize)
        MenuBarExtra {
            MenuView(model: delegate.model)
        } label: {
            Image(nsImage: menuBarMark(running: delegate.model.running))
        }
    }
}

/// A menu bar app (LSUIElement), in the Dock only while its main window is open. Closing that window
/// leaves the node running; quitting stops the guest cleanly first: pulling the plug risks its disk.
@MainActor final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = Model()
    private var sigterm: DispatchSourceSignal?

    func applicationWillFinishLaunching(_ notification: Notification) {
        NotificationCenter.default.addObserver(self, selector: #selector(windowChanged), name: NSWindow.didChangeOcclusionStateNotification, object: nil)
        // `wecolab-node down` sends SIGTERM to whoever holds the VM: quit as from the menu, guest first.
        signal(SIGTERM, SIG_IGN)
        sigterm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        sigterm?.setEventHandler { MainActor.assumeIsolated { NSApp.terminate(nil) } }
        sigterm?.resume()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        // Only a guest that exists starts unattended: the first start asks about the invite's Console.
        guard model.config.startNodeOnLaunch, Provision.guest() == .current else { return }
        // Opened at login, say: the node starts and WeCoLab stays in the menu bar, without its window.
        NSApp.windows.first { $0.identifier?.rawValue == "main" }?.close()
        Task { await model.start() }
    }

    /// SwiftUI names the window after its scene id. onAppear/onDisappear do not follow a Window scene
    /// being closed and reopened; its visibility does.
    @objc func windowChanged(_ notification: Notification) {
        guard let window = notification.object as? NSWindow, window.identifier?.rawValue == "main" else { return }
        NSApp.setActivationPolicy(window.isVisible || window.isMiniaturized ? .regular : .accessory)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard model.running else { return .terminateNow }
        Task {
            await model.stop()
            sender.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }
}

@MainActor @Observable final class Model {
    var config = NodeConfig.load() {
        // Settings outlive the app; start() saves again and reports errors. Views write back unchanged values.
        didSet { if config != oldValue { try? config.save() } }
    }
    var vmState = "stopped"
    var mode = ""
    var guest = GuestStatus()
    var disk = Provision.guest()
    var message = ""
    var busy = false
    var running: Bool { lock != nil }

    private let vm = NodeVM()
    private var lock: Int32?
    private var modeError = false

    init() {
        Task {
            while true {
                await refresh()
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }

    func refresh() async {
        await vm.reserve(memoryGiB: config.memoryGiB)  // live: the balloon follows the Memory slider
        do {
            try await vm.tick()
            let running = await vm.isRunning
            if modeError && running { message = ""; modeError = false }
        } catch {
            message = "Mode publication failed: \(error.localizedDescription)"
            modeError = true
        }
        vmState = await vm.state
        mode = await vm.mode
        (guest, disk) = await Task.detached { (GuestStatus.read(), Provision.guest()) }.value  // the guest's files, off the main actor
        if let lock, !(await vm.isRunning), !busy {  // the guest powered itself off
            VMLock.release(lock)
            self.lock = nil
        }
    }

    func start() async {
        busy = true
        defer { busy = false }
        config.invite = config.invite.trimmingCharacters(in: .whitespacesAndNewlines)
        do {
            lock = try VMLock.acquire()
            try config.validate()
            try config.save()
            try await Provision.run(config, confirm: { invite in await self.confirmJoin(invite) }) { line in Task { @MainActor in self.message = line } }
            message = "booting"
            try await vm.start(config)
            message = ""
        } catch {
            message = error.localizedDescription
            if !(await vm.isRunning) {
                if let lock { VMLock.release(lock) }
                lock = nil
            }
        }
        await refresh()
    }

    func stop() async {
        busy = true
        message = "asking the guest to shut down"
        await vm.stop()
        if let lock { VMLock.release(lock) }
        lock = nil
        message = ""
        busy = false
        await refresh()
    }

    /// The first start gives whoever runs the invite's Console root on a VM on this Mac's network.
    private func confirmJoin(_ invite: Invite) -> Bool {
        let alert = NSAlert()
        alert.messageText = "Join through \(URL(string: invite.console)?.host() ?? invite.console)?"
        alert.informativeText = "The Console at \(invite.console) will install and run software as root in this Mac's VM, on your network. Join only if this invite came from your site's Console (Sites, your site, Add a Mac)."
        alert.addButton(withTitle: "Join")
        alert.addButton(withTitle: "Cancel")
        NSApp.activate()
        return alert.runModal() == .alertFirstButtonReturn
    }

    func reset() {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "Reset this Mac's VM?"
        alert.informativeText = "This deletes the VM (its disk, identity and share) and its spent invite. First remove its box in the Console (Sites, your site, the box, Remove): joining again takes the same name. Then add this Mac again with a new invite."
        alert.addButton(withTitle: "Reset")
        alert.addButton(withTitle: "Cancel")
        NSApp.activate()
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        do {
            config = try Provision.reset()
            message = ""
        } catch {
            message = error.localizedDescription
        }
        Task { await refresh() }
    }
}

struct MainView: View {
    @Bindable var model: Model
    @State private var loginStatus = SMAppService.mainApp.status
    @State private var loginError = ""
    private let maxCPUs = ProcessInfo.processInfo.activeProcessorCount
    private let maxMemory = max(2, Int(ProcessInfo.processInfo.physicalMemory >> 30) - 4)
    private let maxIdleMemory = max(2, Int(ProcessInfo.processInfo.physicalMemory >> 30) - 2)

    var body: some View {
        Form {
            Section("Invite") {
                switch model.disk {
                case .none:
                    TextField("Invite", text: $model.config.invite, prompt: Text("wcl2.…"), axis: .vertical)
                        .labelsHidden()
                        .lineLimit(3...5)
                        .font(.system(.caption, design: .monospaced))
                    Text("In the Console, open Sites, choose Add a Mac on your site, and paste the invite here. The first start asks you to confirm the Console it joins through.")
                        .font(.caption).foregroundStyle(.secondary)
                case .current:
                    Text("This Mac's VM was made with its invite, which is used only once. Reset deletes the VM, so this Mac can be added again.")
                        .font(.caption).foregroundStyle(.secondary)
                case .old:
                    Text("This VM was made by an older WeCoLab for Mac and does not start here. Remove its box in the Console, reset, then add this Mac again.")
                        .font(.caption)
                }
                if model.disk != .none {
                    Button("Reset…") { model.reset() }.disabled(model.running || model.busy)
                }
            }
            Section {
                IntSlider(label: "CPUs", value: $model.config.cpus, range: 1...maxCPUs, unit: "")
                IntSlider(label: "Memory", value: $model.config.memoryGiB, range: 2...maxMemory, unit: "GiB")
                IntSlider(label: "Disk", value: $model.config.diskGiB, range: 8...256, unit: "GiB")
            } header: {
                Text("What this Mac contributes")
            } footer: {
                Text("Reserved for the node all the time while WeCoLab runs. Memory changes apply within seconds while the node runs, up to the size it started with; CPUs and disk at the next start. The disk can grow but not shrink.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section {
                IntSlider(label: "Idle after", value: $model.config.idleMinutes, range: 1...60, unit: "min")
                IntSlider(label: "Idle CPUs", value: $model.config.idleCPUs,
                          range: model.config.cpus...max(model.config.cpus, maxCPUs), unit: "")
                IntSlider(label: "Idle memory", value: $model.config.idleMemoryGiB,
                          range: model.config.memoryGiB...max(model.config.memoryGiB, maxIdleMemory), unit: "GiB")
            } header: {
                Text("When idle")
            } footer: {
                Text("After this long without keyboard, mouse or trackpad input the node may use up to these; never less than what this Mac contributes. When you are back it hands the extra memory back to macOS and its apps move off this Mac. Changes here apply at the next start of the node.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section("Startup") {
                Toggle("Start WeCoLab at login", isOn: Binding(get: { model.config.startAtLogin }, set: { setStartAtLogin($0) }))
                Text(loginText).font(.caption).foregroundStyle(.secondary)
                if loginStatus == .requiresApproval {
                    Button("Open Login Items") { SMAppService.openSystemSettingsLoginItems() }
                }
                if !loginError.isEmpty { Text(loginError).font(.caption) }
                Toggle("Start the node when WeCoLab opens", isOn: $model.config.startNodeOnLaunch)
            }
            Section("Status") {
                LabeledContent("VM", value: model.vmState + (model.mode.isEmpty ? "" : ", you are \(model.mode)"))
                LabeledContent("Mesh address", value: model.guest.meshIP.isEmpty ? "—" : model.guest.meshIP)
                LabeledContent("Join", value: model.guest.join)
                if !model.guest.lastLine.isEmpty, model.guest.join != "finished" {
                    Text(model.guest.lastLine).font(.system(.caption, design: .monospaced)).foregroundStyle(.secondary).lineLimit(2)
                }
                if !model.message.isEmpty { Text(model.message).font(.caption) }
            }
            HStack {
                Spacer()
                StartStopButton(model: model).keyboardShortcut(.defaultAction)
            }
        }
        .formStyle(.grouped)
        .frame(width: 480)
        // Back from System Settings: show what was approved or removed there.
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            loginStatus = SMAppService.mainApp.status
        }
    }

    private func setStartAtLogin(_ on: Bool) {
        model.config.startAtLogin = on
        do {
            try on ? SMAppService.mainApp.register() : SMAppService.mainApp.unregister()
            loginError = ""
        } catch {
            loginError = error.localizedDescription
        }
        loginStatus = SMAppService.mainApp.status
    }

    private var loginText: String {
        switch loginStatus {
        case .enabled: "WeCoLab opens when you log in."
        case .requiresApproval: "Waiting for your approval in System Settings, General, Login Items."
        case .notFound: "macOS cannot find WeCoLab as a login item."
        default: "WeCoLab does not open at login."
        }
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
                Text("\(value) \(unit)").monospacedDigit().frame(width: 64, alignment: .trailing)
            }
        } label: { Text(label) }
    }
}

struct StartStopButton: View {
    let model: Model
    var body: some View {
        Button(model.running ? "Stop node" : "Start node") {
            Task { model.running ? await model.stop() : await model.start() }
        }
        .disabled(model.busy)
    }
}

struct MenuView: View {
    let model: Model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text("VM: \(model.vmState)")
        if !model.mode.isEmpty { Text("Mode: \(model.mode)") }
        Text("Mesh: \(model.guest.meshIP.isEmpty ? "—" : model.guest.meshIP)")
        Text("Join: \(model.guest.join)")
        if !model.message.isEmpty { Text(model.message) }  // progress or error, also when the window is closed
        Divider()
        StartStopButton(model: model)
        Button("Open WeCoLab") {
            openWindow(id: "main")
            NSApp.activate()
        }
        Divider()
        Button("Quit") { NSApp.terminate(nil) }.keyboardShortcut("q")
    }
}
