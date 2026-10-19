import Foundation
import Testing
@testable import WeCoLabCore

@Test func runnerRequiresExactFabricConfirmationAndNeverReturnsInvite() async throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let service = RunnerService(locations: RunnerLocations(root: root), budget: ResourceBudget(cpuCount: 8, memoryGiB: 16))
    var profile = NodeProfile(name: "Friend")
    profile.config.invite = makeInvite()
    let rejected = await service.handle(.create(profile, confirmedConsole: "https://different.example"))
    #expect(rejected.error != nil)
    #expect(try ProfileStore(root: root).profiles().isEmpty)

    let created = await service.handle(.create(profile, confirmedConsole: "https://console.fab.example"))
    #expect(created.error == nil)
    let snapshot = try #require(created.snapshot)
    let exposed = try #require(snapshot.nodes.first)
    #expect(exposed.profile.id == profile.id)
    #expect(exposed.consoleURL == "https://console.fab.example")
    #expect(exposed.profile.config.invite.isEmpty)
    #expect(exposed.pid == nil)
    #expect(snapshot.reservedCPUs == 0)
    #expect(snapshot.reservedMemoryGiB == 0)
    #expect(try ProfileStore(root: root).profile(id: profile.id).config.invite == profile.config.invite)
    let encoded = try JSONEncoder().encode(snapshot)
    #expect(!String(decoding: encoded, as: UTF8.self).contains(profile.config.invite))
}

@Test func runnerConfigurationPreservesRedactedSecretAndRejectsIdentityChanges() async throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let service = RunnerService(locations: RunnerLocations(root: root), budget: ResourceBudget(cpuCount: 8, memoryGiB: 16))
    var profile = NodeProfile(name: "Original")
    profile.config.invite = makeInvite()
    let created = await service.handle(.create(profile, confirmedConsole: "https://console.fab.example"))
    var editable = try #require(created.snapshot?.nodes.first?.profile)
    editable.name = "Renamed"
    let updated = await service.handle(.configure(editable, confirmedConsole: nil))
    #expect(updated.error == nil)
    let saved = try ProfileStore(root: root).profile(id: profile.id)
    #expect(saved.name == "Renamed")
    #expect(saved.config.invite == profile.config.invite)
    editable.hostname = "replacement"
    let rejected = await service.handle(.configure(editable, confirmedConsole: nil))
    #expect(rejected.error != nil)
    #expect(try ProfileStore(root: root).profile(id: profile.id) == saved)
}

@Test func runnerResetAndRemovalRespectActiveWorkerOwnership() async throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let service = RunnerService(locations: RunnerLocations(root: root), budget: ResourceBudget(cpuCount: 8, memoryGiB: 16))
    var profile = NodeProfile(name: "Owned")
    profile.config.invite = makeInvite()
    let created = await service.handle(.create(profile, confirmedConsole: "https://console.fab.example"))
    #expect(created.error == nil)
    let store = ProfileStore(root: root), paths = try store.paths(for: profile.id)
    try Data("preserve disk".utf8).write(to: paths.disk)
    let ownership = try VMLock.acquire(paths)
    defer { VMLock.release(ownership) }
    let reset = await service.handle(.reset(profile.id))
    let removed = await service.handle(.remove(profile.id))
    #expect(reset.error != nil)
    #expect(removed.error != nil)
    #expect(try Data(contentsOf: paths.disk) == Data("preserve disk".utf8))
    #expect(try store.profile(id: profile.id).config.invite == profile.config.invite)
}

@Test func runnerRejectsAnUnstartableResourceMaximumWithoutCreatingProfile() async throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let service = RunnerService(locations: RunnerLocations(root: root), budget: ResourceBudget(cpuCount: 4, memoryGiB: 8))
    var profile = NodeProfile(name: "Too much")
    profile.config.invite = makeInvite()
    profile.config.idleCPUs = 5
    let reply = await service.handle(.create(profile, confirmedConsole: "https://console.fab.example"))
    #expect(reply.error != nil)
    #expect(try ProfileStore(root: root).profiles().isEmpty)
}

@Test func strictNodeCannotReserveResourcesOrProvisionWithoutOSProtection() async throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let service = RunnerService(locations: RunnerLocations(root: root), budget: ResourceBudget(cpuCount: 8, memoryGiB: 16))
    var profile = NodeProfile(name: "Untrusted fabric")
    profile.config.invite = makeInvite()
    let created = await service.handle(.create(profile, confirmedConsole: "https://console.fab.example"))
    #expect(created.error == nil)
    let start = await service.handle(.start(profile.id))
    #expect(start.error != nil)
    let state = try #require(await service.handle(.status).snapshot)
    #expect(state.reservedCPUs == 0)
    #expect(state.reservedMemoryGiB == 0)
    let paths = try ProfileStore(root: root).paths(for: profile.id)
    #expect(!FileManager.default.fileExists(atPath: paths.disk.path))
    #expect(!FileManager.default.fileExists(atPath: paths.seed.path))
    #expect(!FileManager.default.fileExists(atPath: paths.channelImage.path))
    #expect(VMLock.holder(paths) == nil)
}
