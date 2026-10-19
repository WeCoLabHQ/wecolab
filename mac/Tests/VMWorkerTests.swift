import Darwin
import Foundation
import Testing
import Virtualization
@testable import WeCoLabCore


@Test func workerFramingRejectsInvalidAndOversizedRecords() throws {
    #expect(throws: Error.self) { try VMWorkerWire.decode(Data("{}".utf8)) }
    #expect(throws: Error.self) { try VMWorkerWire.decode(Data(repeating: 32, count: VMWorkerWire.maximumRecordBytes + 1)) }
    #expect(throws: Error.self) { try VMWorkerWire.decode(Data("{\"stop\":{},\"start\":{}}".utf8)) }
    #expect(throws: Error.self) { try VMWorkerWire.decode(Data("{\"memory\":-1}".utf8)) }
    let data = try JSONEncoder().encode(VMWorkerCommand.status)
    if case .status = try VMWorkerWire.decode(data) {} else { Issue.record("wrong command") }
}

@Test func workerStartAndMemoryBoundsAreOneShot() throws {
    var gate = VMWorkerGate()
    #expect(throws: Error.self) { try gate.accept(.status) }
    let start = VMWorkerStart(directoryBookmark: Data([1]), config: NodeConfig(), networkAccess: .isolated)
    try gate.accept(.start(start))
    #expect(throws: Error.self) { try gate.accept(.start(start)) }
    #expect(throws: Error.self) { try gate.accept(.memory(0)) }
    #expect(throws: Error.self) { try gate.accept(.memory(start.config.idleMemoryGiB + 1)) }
    try gate.accept(.memory(start.config.memoryGiB))
    try gate.accept(.stop)
    #expect(throws: Error.self) { try gate.accept(.memory(start.config.memoryGiB)) }
}

@Test func vmRejectsAnOrdinaryUnboundedShareDirectory() throws {
    let root = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root)
    try FileManager.default.createDirectory(at: paths.share, withIntermediateDirectories: true)
    #expect(throws: Error.self) { try NodeVM.validateShare(paths: paths) }
}

@Test func forcedBackendTeardownReapsAnUnresponsiveChild() throws {
    let process = Process()
    let ready = Pipe()
    process.executableURL = URL(fileURLWithPath: "/bin/sh")
    process.arguments = ["-c", "trap '' TERM; printf x; exec /bin/sleep 60"]
    process.standardOutput = ready
    try process.run()
    defer {
        if process.isRunning { kill(process.processIdentifier, SIGKILL); process.waitUntilExit() }
    }
    #expect(try ready.fileHandleForReading.read(upToCount: 1) == Data([120]))
    BackendProcessTermination.stop(process)
    #expect(!process.isRunning)
    #expect(process.terminationReason == .uncaughtSignal)
    #expect(process.terminationStatus == SIGKILL)
    BackendProcessTermination.stop(process)
}

@Test func newVMIdentityPersistsAndCorruptionNeverReenrollsIt() throws {
    let root = try tempDir(), paths = Paths(root: root)
    defer { try? FileManager.default.removeItem(at: root) }
    try NodeVM.prepareIdentity(paths: paths)
    let machine = try Data(contentsOf: paths.machineID)
    let mac = try String(contentsOf: paths.macAddress, encoding: .utf8)
    let efi = try Data(contentsOf: paths.efiVars)
    #expect(VZGenericMachineIdentifier(dataRepresentation: machine) != nil)
    #expect(VZMACAddress(string: mac)?.isLocallyAdministeredAddress == true)
    try NodeVM.prepareIdentity(paths: paths)
    #expect(try Data(contentsOf: paths.machineID) == machine)
    #expect(try String(contentsOf: paths.macAddress, encoding: .utf8) == mac)
    #expect(try Data(contentsOf: paths.efiVars) == efi)
    try Data("corrupt".utf8).write(to: paths.macAddress)
    #expect(throws: NodeError.self) { try NodeVM.prepareIdentity(paths: paths) }
    #expect(try String(contentsOf: paths.macAddress, encoding: .utf8) == "corrupt")
    #expect(try Data(contentsOf: paths.machineID) == machine)
}

@Test(.enabled(if: ProcessInfo.processInfo.environment["WECOLAB_SIGNED_WORKER"] != nil))
func signedWorkerReceivesHomeProfileCapabilityWithoutBootingStrictVM() throws {
    let executable = try #require(ProcessInfo.processInfo.environment["WECOLAB_SIGNED_WORKER"])
    let root = URL.applicationSupportDirectory.appending(path: "wecolab-worker-smoke-\(UUID().uuidString)", directoryHint: .isDirectory)
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root)
    let bookmark = try root.bookmarkData(options: [], includingResourceValuesForKeys: nil, relativeTo: nil)
    let request = VMWorkerCommand.start(VMWorkerStart(directoryBookmark: bookmark, config: NodeConfig(), networkAccess: .isolated))
    let process = Process(), input = Pipe(), output = Pipe()
    process.executableURL = URL(fileURLWithPath: executable)
    process.standardInput = input
    process.standardOutput = output
    try #require(fcntl(input.fileHandleForWriting.fileDescriptor, F_SETNOSIGPIPE, 1) == 0)
    try process.run()
    defer {
        if process.isRunning { kill(process.processIdentifier, SIGKILL); process.waitUntilExit() }
    }
    DispatchQueue.global().asyncAfter(deadline: .now() + 10) {
        if process.isRunning { process.terminate() }
    }
    input.fileHandleForReading.closeFile()
    output.fileHandleForWriting.closeFile()
    try input.fileHandleForWriting.write(contentsOf: JSONEncoder().encode(request) + Data([10]))
    try input.fileHandleForWriting.close()
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    let statuses = try data.split(separator: 10).map { try JSONDecoder().decode(VMWorkerStatus.self, from: Data($0)) }
    #expect(process.terminationStatus == 0)
    #expect(statuses.contains { $0.state == "error" })
    #expect(statuses.last?.state == "stopped")
    // Lock creation proves the actual sandboxed worker could use its home-directory capability.
    #expect(FileManager.default.fileExists(atPath: root.appending(path: "vm.pid").path))
    #expect(VMLock.holder(paths) == nil)
    #expect(!FileManager.default.fileExists(atPath: paths.disk.path))
}
