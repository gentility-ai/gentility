import XCTest
@testable import DeskPhoneKit

final class NodeDirectoryTests: XCTestCase {
    private var directory: NodeDirectory!

    override func setUp() {
        directory = NodeDirectory(url: FileManager.default.temporaryDirectory.appendingPathComponent("node-\(UUID().uuidString)", isDirectory: true))
    }

    override func tearDown() {
        try? directory.wipe()
    }

    func testPrepareMakesTheDirectoryAndKeepsItOutOfBackups() throws {
        try directory.prepare()
        var isDirectory: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: directory.path, isDirectory: &isDirectory))
        XCTAssertTrue(isDirectory.boolValue)
        XCTAssertEqual(try directory.url.resourceValues(forKeys: [.isExcludedFromBackupKey]).isExcludedFromBackup, true)
    }

    func testTheDirectoryAndWhatIsInItAreProtectedUntilFirstUnlock() throws {
        try FileManager.default.createDirectory(at: directory.url, withIntermediateDirectories: true)
        let written = directory.url.appendingPathComponent("tailscaled.state")
        try Data("state".utf8).write(to: written)
        try directory.prepare()
        guard let protection = directory.protection() else {
            throw XCTSkip("this simulator reports no protection class; verify file protection on a physical phone")
        }
        XCTAssertEqual(protection, .completeUntilFirstUserAuthentication)
        let child = try FileManager.default.attributesOfItem(atPath: written.path)[.protectionKey] as? FileProtectionType
        XCTAssertEqual(child, .completeUntilFirstUserAuthentication)
    }

    func testWipeRemovesItAndPrepareIsIdempotent() throws {
        try directory.prepare()
        try directory.prepare()
        try directory.wipe()
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
        XCTAssertNoThrow(try directory.wipe())
    }

    /// Go keeps the identity Rejoin comes back on in node.json.
    func testItHoldsAnIdentityOnceGoHasWrittenNodeJSON() throws {
        XCTAssertFalse(directory.holdsIdentity)
        try directory.prepare()
        XCTAssertFalse(directory.holdsIdentity)
        try Data("{}".utf8).write(to: directory.url.appendingPathComponent("node.json"))
        XCTAssertTrue(directory.holdsIdentity)
        try directory.wipe()
        XCTAssertFalse(directory.holdsIdentity)
    }

    func testTheStandardDirectoryIsUnderApplicationSupport() throws {
        let standard = try NodeDirectory.standard()
        XCTAssertEqual(standard.url.lastPathComponent, "phonenode")
        XCTAssertEqual(standard.url.deletingLastPathComponent().lastPathComponent, "Application Support")
    }
}
