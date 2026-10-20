import Foundation
import PackagePlugin

/// Embeds the repo's installer and source identity in WeCoLabCore. Build commands cannot
/// access the parent checkout under SwiftPM's sandbox; the plugin process can. It reads
/// metadata on each invocation so Git HEAD, index, tracked and untracked changes refresh
/// the generated command rather than depending on Git file timestamps.
@main struct EmbedJoinScript: BuildToolPlugin {
    func createBuildCommands(context: PluginContext, target: Target) throws -> [Command] {
        let repository = context.package.directoryURL.deletingLastPathComponent()
        let script = repository.appending(path: "install.sh")
        let installer = "let joinScriptBase64 = \"\(try Data(contentsOf: script).base64EncodedString())\"\n"
        let joinOut = context.pluginWorkDirectoryURL.appending(path: "JoinScript.swift")
        let buildOut = context.pluginWorkDirectoryURL.appending(path: "BuildInfo.swift")
        let identity = try buildInfoSource(repository: repository)
        return [
            .buildCommand(
                displayName: "Embedding install.sh",
                executable: URL(filePath: "/bin/sh"),
                arguments: ["-c", #"printf '%s' "$0" > "$1""#, installer, joinOut.path],
                inputFiles: [script], outputFiles: [joinOut]),
            .buildCommand(
                displayName: "Embedding product and source identity",
                executable: URL(filePath: "/bin/sh"),
                arguments: ["-c", #"printf '%s' "$0" > "$1""#, identity, buildOut.path],
                inputFiles: [repository.appending(path: "hack/build-info.py"), repository.appending(path: "VERSION")],
                outputFiles: [buildOut]),
        ]
    }

    private func buildInfoSource(repository: URL) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = ["python3", repository.appending(path: "hack/build-info.py").path,
                             "--root", repository.path, "--format", "swift"]
        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr
        try process.run()
        let source = stdout.fileHandleForReading.readDataToEndOfFile()
        let diagnostics = stderr.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        guard process.terminationStatus == 0 else {
            throw BuildInfoError.generation(String(decoding: diagnostics, as: UTF8.self))
        }
        return String(decoding: source, as: UTF8.self)
    }

    private enum BuildInfoError: Error {
        case generation(String)
    }
}
