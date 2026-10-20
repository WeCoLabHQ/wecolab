import Foundation

private struct BuildIdentity: Encodable {
    let productVersion: String
    let buildNumber: Int
    let sourceRevision: String
    let dirty: Bool
}

public extension BuildInfo {
    static var display: String {
        "WeCoLab \(productVersion) (build \(buildNumber), \(sourceRevision)\(dirty ? ", dirty" : ""))"
    }

    static var json: String {
        let info = BuildIdentity(productVersion: productVersion, buildNumber: buildNumber,
                                 sourceRevision: sourceRevision, dirty: dirty)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        // This fixed collection of primitive values is always encodable.
        return String(decoding: try! encoder.encode(info), as: UTF8.self)
    }
}
