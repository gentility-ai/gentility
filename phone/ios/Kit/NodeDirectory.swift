import Foundation

/// Where `phonenode` keeps the node's identity and state: Application
/// Support/phonenode, "complete until first user authentication" (v1 spec
/// 3.2), kept out of backups so the identity never lands on another phone.
public struct NodeDirectory: Sendable {
    public let url: URL

    public init(url: URL) {
        self.url = url
    }

    public static func standard() throws -> NodeDirectory {
        let support = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        return NodeDirectory(url: support.appendingPathComponent("phonenode", isDirectory: true))
    }

    public var path: String { url.path }

    /// Made if missing; the class set on it and on everything Go has written
    /// in it, since a file created before keeps the class it was made with.
    public func prepare() throws {
        let files = FileManager.default
        let protection: [FileAttributeKey: Any] = [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication]
        if !files.fileExists(atPath: path) {
            try files.createDirectory(at: url, withIntermediateDirectories: true, attributes: protection)
        }
        try files.setAttributes(protection, ofItemAtPath: path)
        if let inside = files.enumerator(atPath: path) {
            for case let relative as String in inside {
                try files.setAttributes(protection, ofItemAtPath: url.appendingPathComponent(relative).path)
            }
        }
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var target = url
        try target.setResourceValues(values)
    }

    /// Whether Go has kept an identity here: `node.json`, which Join's
    /// exchange writes before Join answers. Read from the file's existence
    /// alone, so it answers before the first unlock too.
    public var holdsIdentity: Bool {
        FileManager.default.fileExists(atPath: url.appendingPathComponent("node.json").path)
    }

    public func protection() -> FileProtectionType? {
        (try? FileManager.default.attributesOfItem(atPath: path))?[.protectionKey] as? FileProtectionType
    }

    public func wipe() throws {
        guard FileManager.default.fileExists(atPath: path) else { return }
        try FileManager.default.removeItem(at: url)
    }
}
