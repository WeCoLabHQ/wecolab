// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "WeCoLab",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "WeCoLabCore", path: "WeCoLabCore", plugins: ["EmbedJoinScript"]),
        .executableTarget(name: "wecolab-node", dependencies: ["WeCoLabCore"], path: "wecolab-node"),
        .executableTarget(name: "WeCoLab", dependencies: ["WeCoLabCore"], path: "WeCoLab"),
        .testTarget(name: "WeCoLabCoreTests", dependencies: ["WeCoLabCore"], path: "Tests"),
        // ../install.sh is compiled into WeCoLabCore, so the guest runs exactly the repo's script.
        .plugin(name: "EmbedJoinScript", capability: .buildTool(), path: "Plugins/EmbedJoinScript"),
    ]
)
