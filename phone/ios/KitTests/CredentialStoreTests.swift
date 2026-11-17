import XCTest
import Security
@testable import DeskPhoneKit

final class CredentialStoreTests: XCTestCase {
    private var store: KeychainStore!

    override func setUp() {
        store = KeychainStore(service: "ai.gentility.desk.phone.test.\(UUID().uuidString)")
    }

    override func tearDown() {
        try? store.wipe()
    }

    private func credential(bearer: String = "bearer-1") -> Credential {
        Credential(
            origin: URL(string: "https://mcp.gentility.test")!,
            bearer: bearer,
            expiresAt: Date(timeIntervalSince1970: 1_800_000_000),
            enrollmentJSON: #"{"enrollment_id":"e-1"}"#,
            enrollmentID: "e-1",
            phoneName: "Morgan's iPhone"
        )
    }

    private func pending() -> PendingClaim {
        PendingClaim(
            origin: URL(string: "https://mcp.gentility.test")!, host: "mcp.gentility.test",
            pollSecret: "ps-secret", machineLabel: "Morgan's MacBook Pro", match: "47",
            expiresAt: "2026-01-01T00:05:00.000000Z", phoneName: "Morgan's iPhone"
        )
    }

    func testACredentialSurvivesARead() throws {
        XCTAssertNil(try store.credential())
        try store.save(credential())
        XCTAssertEqual(try store.credential(), credential())
        try store.save(credential(bearer: "bearer-2"))
        XCTAssertEqual(try store.credential()?.bearer, "bearer-2", "saving again replaces")
    }

    func testTheItemIsReadableAfterFirstUnlockAndOnThisPhoneOnly() throws {
        try store.save(credential())
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: store.service,
            kSecAttrAccount as String: "credential",
            kSecReturnAttributes as String: true,
        ]
        var found: CFTypeRef?
        XCTAssertEqual(SecItemCopyMatching(query as CFDictionary, &found), errSecSuccess)
        let attributes = try XCTUnwrap(found as? [String: Any])
        XCTAssertEqual(
            attributes[kSecAttrAccessible as String] as? String,
            kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly as String
        )
    }

    func testAPendingClaimIsKeptApartAndCleared() throws {
        try store.savePending(pending())
        XCTAssertEqual(try store.pendingClaim(), pending())
        try store.save(credential())
        try store.clearPending()
        XCTAssertNil(try store.pendingClaim())
        XCTAssertNotNil(try store.credential())
    }

    func testWipeForgetsEverything() throws {
        try store.save(credential())
        try store.savePending(pending())
        try store.wipe()
        XCTAssertNil(try store.credential())
        XCTAssertNil(try store.pendingClaim())
        XCTAssertNoThrow(try store.wipe(), "wiping nothing is fine")
    }

    func testAReinstallForgetsTheKeychain() throws {
        let defaults = try XCTUnwrap(UserDefaults(suiteName: "test.\(UUID().uuidString)"))
        try store.save(credential())
        store.forgetIfReinstalled(defaults)
        XCTAssertNil(try store.credential(), "no marker: a fresh install")
        try store.save(credential())
        store.forgetIfReinstalled(defaults)
        XCTAssertNotNil(try store.credential(), "the marker stays for this install")
    }

    // A keychain that answers as a real one sometimes does.

    private func account(_ query: CFDictionary) -> String? {
        (query as NSDictionary)[kSecAttrAccount as String] as? String
    }

    func testAnAddThatFindsTheItemAlreadyThereUpdatesItInstead() throws {
        var updates = 0
        var keychain = Keychain.system
        keychain.update = { query, changes in
            updates += 1
            return updates == 1 ? errSecItemNotFound : SecItemUpdate(query, changes)
        }
        keychain.add = { attributes, result in
            // Another writer got there between this one's update and add.
            XCTAssertEqual(SecItemAdd(attributes, result), errSecSuccess)
            return errSecDuplicateItem
        }
        let racing = KeychainStore(service: store.service, keychain: keychain)
        try racing.save(credential(bearer: "bearer-2"))
        XCTAssertEqual(updates, 2, "the update is tried once more, and only once")
        XCTAssertEqual(try store.credential()?.bearer, "bearer-2")
    }

    func testAnAddThatStillFindsNothingToUpdateIsRefused() {
        var keychain = Keychain.system
        keychain.update = { _, _ in errSecItemNotFound }
        keychain.add = { _, _ in errSecDuplicateItem }
        let racing = KeychainStore(service: store.service, keychain: keychain)
        XCTAssertThrowsError(try racing.save(credential())) { error in
            XCTAssertEqual(error as? KeychainError, KeychainError(status: errSecItemNotFound))
        }
    }

    func testWipeTriesEveryItemThenSaysTheFirstRefusal() throws {
        try store.save(credential())
        try store.savePending(pending())
        var keychain = Keychain.system
        keychain.delete = { [self] query in
            account(query) == "credential" ? errSecInteractionNotAllowed : SecItemDelete(query)
        }
        let locked = KeychainStore(service: store.service, keychain: keychain)
        XCTAssertThrowsError(try locked.wipe()) { error in
            XCTAssertEqual(error as? KeychainError, KeychainError(status: errSecInteractionNotAllowed))
        }
        XCTAssertNil(try store.pendingClaim(), "the item after the refused one was still deleted")
        XCTAssertNotNil(try store.credential())
    }

    /// A launch before the first unlock (a background wake) cannot touch the
    /// keychain: that is not a fresh install, and the next launch decides.
    func testAReinstallCheckBeforeTheFirstUnlockLeavesTheKeychainAndTheMarker() throws {
        let defaults = try XCTUnwrap(UserDefaults(suiteName: "test.\(UUID().uuidString)"))
        try store.save(credential())
        var keychain = Keychain.system
        keychain.delete = { _ in errSecInteractionNotAllowed }
        KeychainStore(service: store.service, keychain: keychain).forgetIfReinstalled(defaults)
        XCTAssertFalse(defaults.bool(forKey: "ai.gentility.desk.phone.installed"), "no marker yet")
        XCTAssertNotNil(try store.credential())
        store.forgetIfReinstalled(defaults)
        XCTAssertNil(try store.credential(), "unlocked: the earlier install is forgotten")
        XCTAssertTrue(defaults.bool(forKey: "ai.gentility.desk.phone.installed"))
    }

    func testAnItemThatNoLongerDecodesIsForgottenAndThePhonePairsAgain() throws {
        try store.savePending(pending())
        let attributes: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: store.service,
            kSecAttrAccount as String: "credential",
            kSecValueData as String: Data("an older build's shape".utf8),
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        XCTAssertEqual(SecItemAdd(attributes as CFDictionary, nil), errSecSuccess)
        var refusal: Error?
        XCTAssertThrowsError(try store.credential()) { refusal = $0 }
        let error = try XCTUnwrap(refusal)
        XCTAssertEqual(error as? KeychainError, KeychainError(status: errSecDecode))
        XCTAssertFalse(store.forgetIfUnreadable(KeychainError(status: errSecIO)), "only an unreadable item is forgotten")
        XCTAssertNotNil(try store.pendingClaim())
        XCTAssertTrue(store.forgetIfUnreadable(error))
        XCTAssertNil(try store.credential())
        XCTAssertNil(try store.pendingClaim(), "nothing of that pairing is kept")
    }

    func testNothingSecretPrints() {
        XCTAssertFalse(String(describing: credential()).contains("bearer-1"))
        XCTAssertFalse(String(describing: credential()).contains("e-1\""))
    }

    func testNothingSecretPrintsHoweverItIsPrinted() {
        func printed(_ value: Any) -> [String] {
            var dumped = ""
            dump(value, to: &dumped)
            return [String(describing: value), String(reflecting: value), dumped, "\(value)"]
        }
        for text in printed(credential()) {
            XCTAssertFalse(text.contains("bearer-1"), text)
            XCTAssertFalse(text.contains("enrollment_id"), text)
        }
        for text in printed(pending()) {
            XCTAssertFalse(text.contains("ps-secret"), text)
        }
        for text in printed([credential()]) + printed(Optional(pending()) as Any) {
            XCTAssertFalse(text.contains("bearer-1") || text.contains("ps-secret"), text)
        }
    }
}
