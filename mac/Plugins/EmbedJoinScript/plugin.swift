import Foundation
import PackagePlugin

/// Compiles ../install.sh into WeCoLabCore as `joinScriptBase64`, so the guest runs exactly the repo's script.
/// The plugin reads the file (build commands are sandboxed to the package, the plugin itself is not) and hands
/// the generated source to a tiny command that only writes it.
@main struct EmbedJoinScript: BuildToolPlugin {
    func createBuildCommands(context: PluginContext, target: Target) throws -> [Command] {
        let script = context.package.directoryURL.deletingLastPathComponent().appending(path: "install.sh")
        let source = "let joinScriptBase64 = \"\(try Data(contentsOf: script).base64EncodedString())\"\n"
        let out = context.pluginWorkDirectoryURL.appending(path: "JoinScript.swift")
        return [.buildCommand(
            displayName: "Embedding install.sh",
            executable: URL(filePath: "/bin/sh"),
            arguments: ["-c", #"printf '%s' "$0" > "$1""#, source, out.path],
            inputFiles: [script], outputFiles: [out])]
    }
}
