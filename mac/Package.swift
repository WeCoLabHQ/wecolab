// swift-tools-version: 6.0
import Foundation
import PackageDescription

let package = Package(
    name: "WeCoLab",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "WeCoLabCore", path: "WeCoLabCore", plugins: ["EmbedJoinScript"]),
        .executableTarget(name: "wecolab-node", dependencies: ["WeCoLabCore"], path: "wecolab-node"),
        .executableTarget(name: "wecolab-runner", dependencies: ["WeCoLabCore"], path: "wecolab-runner"),
        .executableTarget(
            name: "wecolab-vm", dependencies: ["WeCoLabCore"], path: "wecolab-vm",
            exclude: ["Info.plist"],
            // Standalone sandbox executables need bundle identity in their signed Mach-O.
            linkerSettings: [.unsafeFlags([
                "-Xlinker", "-sectcreate", "-Xlinker", "__TEXT", "-Xlinker", "__info_plist",
                "-Xlinker", URL(fileURLWithPath: #filePath).deletingLastPathComponent()
                    .appending(path: "wecolab-vm/Info.plist").path
            ])]
        ),
        .executableTarget(name: "WeCoLab", dependencies: ["WeCoLabCore"], path: "WeCoLab"),
        .testTarget(name: "WeCoLabCoreTests", dependencies: ["WeCoLabCore"], path: "Tests"),
        // ../install.sh is compiled into WeCoLabCore, so the guest runs exactly the repo's script.
        .plugin(name: "EmbedJoinScript", capability: .buildTool(), path: "Plugins/EmbedJoinScript"),
    ]
)
