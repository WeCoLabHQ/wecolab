import Foundation
import Testing
@testable import WeCoLabCore

@Test func profilesKeepDistinctDirectoriesAndStableHostnames() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = ProfileStore(root: root)
    let a = NodeProfile(name: "Alpha")
    let b = NodeProfile(name: "Beta")
    try store.create(a)
    try store.create(b)
    #expect(a.hostname != b.hostname)
    #expect(try store.profiles().count == 2)
    let first = try store.paths(for: a.id), second = try store.paths(for: b.id)
    try Data("first".utf8).write(to: first.disk)
    try Data("second".utf8).write(to: second.disk)
    try store.remove(id: a.id)
    #expect(try Data(contentsOf: second.disk) == Data("second".utf8))
    #expect(try store.profile(id: b.id).hostname == b.hostname)
    var changed = b
    changed.hostname = "mac-replaced"
    #expect(throws: NodeError.self) { try store.update(changed) }
    #expect(!FileManager.default.fileExists(atPath: first.root.path))
}

@Test func profilesRejectInvalidIdsLabelsCorruptionAndSymlinkEscape() throws {
    let root = try tempDir(), outside = try tempDir()
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: outside) }
    let store = ProfileStore(root: root)
    #expect(throws: NodeError.self) { try store.paths(for: "../elsewhere") }
    #expect(throws: NodeError.self) { try store.paths(for: UUID().uuidString.uppercased()) }
    let one = NodeProfile(name: "Name")
    try store.create(one)
    #expect(throws: NodeError.self) { try store.create(NodeProfile(name: "nAmE")) }
    let target = try store.paths(for: one.id)
    try FileManager.default.removeItem(at: target.root)
    try FileManager.default.createSymbolicLink(at: target.root, withDestinationURL: outside)
    #expect(throws: NodeError.self) { try store.profile(id: one.id) }
    #expect(throws: NodeError.self) { try store.remove(id: one.id) }
    #expect(FileManager.default.fileExists(atPath: outside.path))
}

@Test func corruptProfileNeverBecomesDefaultConfig() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = ProfileStore(root: root), p = NodeProfile(name: "Keep")
    try store.create(p)
    let paths = try store.paths(for: p.id)
    try Data("not json".utf8).write(to: paths.root.appending(path: "profile.json"), options: .atomic)
    #expect(throws: Error.self) { try store.profiles() }
    #expect(throws: Error.self) { try store.update(p) }
}

@Test func legacyMigrationRetriesWithoutLosingIdentityOrData() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root), store = ProfileStore(root: root)
    var config = NodeConfig()
    config.invite = makeInvite()
    try config.save(paths)
    var legacy = try JSONEncoder().encode(config)
    legacy.removeLast()
    legacy.append(contentsOf: Data(#","startNodeOnLaunch":true,"startAtLogin":true}"#.utf8))
    try legacy.write(to: paths.config)
    try Data("identity".utf8).write(to: paths.machineID)
    try Data("sparse disk".utf8).write(to: paths.disk)
    try Provision.seed(invite: Invite(makeInvite()), hostname: "mac-original", to: paths.seed)
    try Data(Provision.generation.utf8).write(to: paths.guestVersion)
    try paths.prepare()
    try FileManager.default.createDirectory(at: paths.share, withIntermediateDirectories: false)
    try Data("joined".utf8).write(to: paths.share.appending(path: "wecolab-join.log"))
    let migrated = try #require(try store.importLegacy())
    let moved = try store.paths(for: migrated.id)
    #expect(migrated.networkAccess == .localNetwork)
    #expect(migrated.hostname == "mac-original")
    #expect(migrated.autoStart)
    #expect(migrated.config.invite == config.invite)
    #expect(try Data(contentsOf: moved.disk) == Data("sparse disk".utf8))
    #expect(try Data(contentsOf: moved.machineID) == Data("identity".utf8))
    #expect(try Data(contentsOf: moved.share.appending(path: "wecolab-join.log")) == Data("joined".utf8))
    #expect(Provision.guest(moved) == .current)
    #expect(try store.importLegacy() == nil)
}

@Test func legacyMigrationRefusesRunningVMLock() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root)
    try NodeConfig().save(paths)
    try Data("disk".utf8).write(to: paths.disk)
    let lock = try VMLock.acquire(paths)
    defer { VMLock.release(lock) }
    #expect(throws: NodeError.self) { try ProfileStore(root: root).importLegacy() }
    #expect(FileManager.default.fileExists(atPath: paths.disk.path))
}

@Test func budgetUsesMaximumIdleResourcesAndExactBoundary() throws {
    let budget = ResourceBudget(cpuCount: 8, memoryGiB: 18) // 16 GiB guest maxima + 1 GiB per worker
    var running = NodeConfig(), starting = NodeConfig()
    running.idleCPUs = 5
    running.idleMemoryGiB = 10
    starting.idleCPUs = 3
    starting.idleMemoryGiB = 6
    try budget.validate(starting: starting, running: [running])

    starting.idleCPUs = 4
    #expect(throws: NodeError.self) { try budget.validate(starting: starting, running: [running]) }
    starting.idleCPUs = 3
    starting.idleMemoryGiB = 7
    #expect(throws: NodeError.self) { try budget.validate(starting: starting, running: [running]) }
}
@Test func legacyImportFailureKeepsDiskAndRetriesWithSameProfileID() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root), store = ProfileStore(root: root)
    let config = NodeConfig()
    try config.save(paths)
    try Data("disk bytes".utf8).write(to: paths.disk)
    try Provision.seed(invite: Invite(makeInvite()), hostname: "mac-retry", to: paths.seed)
    try paths.prepare()
    let bad = paths.share.appending(path: "mode")
    try FileManager.default.createDirectory(at: paths.share, withIntermediateDirectories: false)
    try FileManager.default.createSymbolicLink(at: bad, withDestinationURL: paths.disk)
    #expect(throws: NodeError.self) { try store.importLegacy() }
    let journal = root.appending(path: "legacy-import.json")
    #expect(FileManager.default.fileExists(atPath: journal.path))
    let stored = try #require(JSONSerialization.jsonObject(with: Data(contentsOf: journal)) as? [String: Any])
    let details = try #require(stored["profile"] as? [String: Any])
    let reservedID = try #require(details["id"] as? String)
    #expect(try Data(contentsOf: root.appending(path: "profiles/\(reservedID)/disk.img")) == Data("disk bytes".utf8))
    try FileManager.default.removeItem(at: bad)
    let imported = try #require(try store.importLegacy())
    #expect(imported.id == reservedID)
    #expect(try Data(contentsOf: store.paths(for: imported.id).disk) == Data("disk bytes".utf8))
}

@Test func channelRejectsSymlinkInsteadOfUsingUnboundedDirectory() throws {
    let root = try tempDir(), outside = try tempDir()
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: outside) }
    let paths = Paths(root: root)
    try FileManager.default.createSymbolicLink(at: paths.share, withDestinationURL: outside)
    #expect(throws: NodeError.self) { try Provision.prepareChannel(at: paths) }
    #expect(!FileManager.default.fileExists(atPath: paths.channelImage.path))
}

@Test func channelMountIsFixedSizeAndPreservesLegacyStatus() throws {
    let root = try tempDir(), paths = Paths(root: root)
    defer {
        do {
            try Provision.detachChannel(at: paths)
            try FileManager.default.removeItem(at: root)
        } catch { Issue.record("channel test cleanup: \(error)") }
    }
    try FileManager.default.createDirectory(at: paths.share, withIntermediateDirectories: false)
    try Data("joined".utf8).write(to: paths.share.appending(path: "wecolab-join.log"))
    try Provision.prepareChannel(at: paths)
    try NodeVM.validateShare(paths: paths)
    #expect(try Data(contentsOf: paths.share.appending(path: "wecolab-join.log")) == Data("joined".utf8))
    var volume = statfs()
    #expect(statfs(paths.share.path, &volume) == 0)
    #expect(UInt64(volume.f_blocks) * UInt64(volume.f_bsize) <= 16 << 20)
    try Provision.detachChannel(at: paths)
    #expect(!FileManager.default.fileExists(atPath: paths.share.appending(path: "wecolab-join.log").path))
    try Provision.prepareChannel(at: paths)
    #expect(try Data(contentsOf: paths.share.appending(path: "wecolab-join.log")) == Data("joined".utf8))
}

@Test func provisioningRejectsDiskSymlinkWithoutTouchingSibling() async throws {
    let root = try tempDir(), sibling = try tempDir()
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: sibling) }
    let paths = Paths(root: root), otherDisk = sibling.appending(path: "disk.img")
    try Data("sibling".utf8).write(to: otherDisk)
    try FileManager.default.createSymbolicLink(at: paths.disk, withDestinationURL: otherDisk)
    var config = NodeConfig()
    config.invite = makeInvite()
    await #expect(throws: NodeError.self) {
        try await Provision.run(config, at: paths, hostname: "mac-test", confirm: { _ in true }, log: { _ in })
    }
    #expect(try Data(contentsOf: otherDisk) == Data("sibling".utf8))
}

@Test func resetReadsCanonicalProfileConfigNotLegacyCopy() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let store = ProfileStore(root: root)
    var profile = NodeProfile(name: "New")
    profile.config.invite = makeInvite()
    try store.create(profile)
    let paths = try store.paths(for: profile.id)
    #expect(!FileManager.default.fileExists(atPath: paths.config.path))
    try Data("disk".utf8).write(to: paths.disk)
    let cleared = try Provision.reset(paths)
    #expect(cleared.invite == "")
    #expect(!FileManager.default.fileExists(atPath: paths.disk.path))
    profile.config = cleared
    try store.update(profile)
    #expect(try store.profile(id: profile.id).config.invite == "")
}

@Test func legacyMigrationKeepsSparseDiskAllocation() throws {
    let root = try tempDir()
    defer { try? FileManager.default.removeItem(at: root) }
    let paths = Paths(root: root), store = ProfileStore(root: root)
    try NodeConfig().save(paths)
    try Provision.seed(invite: Invite(makeInvite()), hostname: "mac-sparse", to: paths.seed)
    _ = FileManager.default.createFile(atPath: paths.disk.path, contents: nil)
    let disk = try FileHandle(forWritingTo: paths.disk)
    try disk.truncate(atOffset: 8 << 30)
    try disk.close()
    let before = try paths.disk.resourceValues(forKeys: [.fileSizeKey, .totalFileAllocatedSizeKey])
    let migrated = try #require(try store.importLegacy())
    let after = try store.paths(for: migrated.id).disk.resourceValues(forKeys: [.fileSizeKey, .totalFileAllocatedSizeKey])
    #expect(after.fileSize == before.fileSize)
    #expect(after.totalFileAllocatedSize == before.totalFileAllocatedSize)
    #expect(migrated.hostname == "mac-sparse")
}

@Test func resetDropsFailedChannelMigrationWithoutFollowingLinks() throws {
    let root = try tempDir(), outside = try tempDir()
    defer { try? FileManager.default.removeItem(at: root); try? FileManager.default.removeItem(at: outside) }
    let paths = Paths(root: root), stage = root.appending(path: "share-legacy")
    try NodeConfig().save(paths)
    try FileManager.default.createDirectory(at: stage, withIntermediateDirectories: false)
    try Data("stale guest state".utf8).write(to: stage.appending(path: "unexpected-entry"))
    _ = try Provision.reset(paths)
    #expect(!FileManager.default.fileExists(atPath: stage.path))

    let protected = outside.appending(path: "keep")
    try Data("sibling data".utf8).write(to: protected)
    try FileManager.default.createSymbolicLink(at: stage, withDestinationURL: outside)
    _ = try Provision.reset(paths)
    #expect(!FileManager.default.fileExists(atPath: stage.path))
    #expect(try Data(contentsOf: protected) == Data("sibling data".utf8))
}
