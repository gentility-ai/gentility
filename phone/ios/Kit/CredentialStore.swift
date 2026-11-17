import Foundation
import Security

/// What a paired phone holds: where it paired, the bearer, and the enrolment
/// until `phonenode.Join` has taken it. Prints as redacted, however printed.
public struct Credential: Codable, Equatable, Sendable, CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public var origin: URL
    public var bearer: String
    public var expiresAt: Date
    /// Present from the allowed poll until the node first joins.
    public var enrollmentJSON: String?
    public var enrollmentID: String?
    public var phoneName: String

    public init(origin: URL, bearer: String, expiresAt: Date, enrollmentJSON: String?, enrollmentID: String?, phoneName: String) {
        self.origin = origin
        self.bearer = bearer
        self.expiresAt = expiresAt
        self.enrollmentJSON = enrollmentJSON
        self.enrollmentID = enrollmentID
        self.phoneName = phoneName
    }

    public var description: String {
        "Credential(\(origin.absoluteString), bearer redacted, enrolment \(enrollmentJSON == nil ? "spent" : "held"))"
    }

    public var debugDescription: String { description }
    /// `dump` and the debugger walk a mirror, not `description`: show them none.
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

/// A claim redeemed and not yet decided: enough to poll again after a kill.
public struct PendingClaim: Codable, Equatable, Sendable, CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public let origin: URL
    public let host: String
    public let pollSecret: String
    public let machineLabel: String
    public let match: String
    public let expiresAt: String
    public let phoneName: String

    public init(origin: URL, host: String, pollSecret: String, machineLabel: String, match: String, expiresAt: String, phoneName: String) {
        self.origin = origin
        self.host = host
        self.pollSecret = pollSecret
        self.machineLabel = machineLabel
        self.match = match
        self.expiresAt = expiresAt
        self.phoneName = phoneName
    }

    public var description: String { "PendingClaim(\(host), \(machineLabel), poll secret redacted)" }
    public var debugDescription: String { description }
    public var customMirror: Mirror { Mirror(self, children: [:]) }
}

public protocol CredentialStore: AnyObject, Sendable {
    func credential() throws -> Credential?
    func save(_ credential: Credential) throws
    func pendingClaim() throws -> PendingClaim?
    func savePending(_ claim: PendingClaim) throws
    func clearPending() throws
    func wipe() throws
}

extension CredentialStore {
    /// Keychain items outlive an uninstall; the app's files do not. A first
    /// launch after a reinstall would hold a bearer with no node behind it, so
    /// the keychain is forgotten when this install's own marker is missing.
    /// A launch before the first unlock cannot reach the keychain at all
    /// (`errSecInteractionNotAllowed`): nothing is wiped and no marker set, so
    /// the next launch decides.
    public func forgetIfReinstalled(_ defaults: UserDefaults) {
        let marker = "ai.gentility.desk.phone.installed"
        guard !defaults.bool(forKey: marker) else { return }
        do {
            try wipe()
            Log.app.info("a fresh install: the keychain holds nothing of an earlier one")
        } catch let error as KeychainError where error.status == errSecInteractionNotAllowed {
            Log.app.info("the keychain is locked until the first unlock; a fresh install is checked on a later launch")
            return
        } catch {
            Log.app.error("could not forget an earlier install's keychain: \(String(describing: error), privacy: .public)")
        }
        defaults.set(true, forKey: marker)
    }

    /// Whether `error` says a stored item no longer decodes (an older build's
    /// shape, say). Such an item refuses every later read too, so the store
    /// is wiped, and the caller sends the phone to pair again.
    @discardableResult
    public func forgetIfUnreadable(_ error: Error) -> Bool {
        guard let error = error as? KeychainError, error.status == errSecDecode else { return false }
        do {
            try wipe()
            Log.app.error("a kept item no longer reads; forgot this phone's pairing so it pairs again")
        } catch {
            Log.app.error("a kept item no longer reads, and could not be forgotten: \(String(describing: error), privacy: .public)")
        }
        return true
    }
}

/// A keychain call refused, by its `OSStatus`, and nothing of what was stored.
public struct KeychainError: Error, Equatable, CustomStringConvertible {
    public let status: OSStatus

    public init(status: OSStatus) {
        self.status = status
    }

    public var description: String { "KeychainError(\(status))" }
}

/// The keychain's four calls, so a test can answer as a keychain sometimes
/// does: a duplicate after a not-found, a refusal before the first unlock.
struct Keychain {
    var copy: (CFDictionary, UnsafeMutablePointer<CFTypeRef?>?) -> OSStatus
    var update: (CFDictionary, CFDictionary) -> OSStatus
    var add: (CFDictionary, UnsafeMutablePointer<CFTypeRef?>?) -> OSStatus
    var delete: (CFDictionary) -> OSStatus

    static let system = Keychain(copy: SecItemCopyMatching, update: SecItemUpdate, add: SecItemAdd, delete: SecItemDelete)
}

/// Generic-password items under one service, readable after the first unlock
/// and never synced or restored onto another phone.
public final class KeychainStore: CredentialStore, @unchecked Sendable {
    public let service: String
    private let keychain: Keychain

    public convenience init(service: String = "ai.gentility.desk.phone") {
        self.init(service: service, keychain: .system)
    }

    init(service: String, keychain: Keychain) {
        self.service = service
        self.keychain = keychain
    }

    public func credential() throws -> Credential? { try read("credential") }
    public func save(_ credential: Credential) throws { try write("credential", credential) }
    public func pendingClaim() throws -> PendingClaim? { try read("pending_claim") }
    public func savePending(_ claim: PendingClaim) throws { try write("pending_claim", claim) }
    public func clearPending() throws { try delete("pending_claim") }

    /// Every item is tried; the first refusal is thrown after.
    public func wipe() throws {
        var refused: Error?
        for account in ["credential", "pending_claim"] {
            do {
                try delete(account)
            } catch {
                Log.app.error("could not delete the keychain's \(account, privacy: .public) item: \(String(describing: error), privacy: .public)")
                refused = refused ?? error
            }
        }
        if let refused { throw refused }
    }

    private func base(_ account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    private func read<T: Decodable>(_ account: String) throws -> T? {
        var query = base(account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var found: CFTypeRef?
        let status = keychain.copy(query as CFDictionary, &found)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = found as? Data else { throw KeychainError(status: status) }
        // A decoder's error can quote what it read; the item is a secret.
        guard let value = try? JSONDecoder().decode(T.self, from: data) else {
            Log.app.error("the keychain's \(account, privacy: .public) item does not decode")
            throw KeychainError(status: errSecDecode)
        }
        return value
    }

    private func write<T: Encodable>(_ account: String, _ value: T) throws {
        let data = try JSONEncoder().encode(value)
        let changes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updated = keychain.update(base(account) as CFDictionary, changes as CFDictionary)
        if updated == errSecSuccess { return }
        guard updated == errSecItemNotFound else { throw KeychainError(status: updated) }
        let added = keychain.add(base(account).merging(changes) { $1 } as CFDictionary, nil)
        if added == errSecSuccess { return }
        guard added == errSecDuplicateItem else { throw KeychainError(status: added) }
        // Another write landed between the update and the add: update once more.
        Log.app.info("the keychain's \(account, privacy: .public) item appeared mid-write; updating it")
        let again = keychain.update(base(account) as CFDictionary, changes as CFDictionary)
        guard again == errSecSuccess else { throw KeychainError(status: again) }
    }

    private func delete(_ account: String) throws {
        let status = keychain.delete(base(account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw KeychainError(status: status) }
    }
}

/// A store in memory, for tests and the screenshot showcase. Records what was
/// done to it, in order.
public final class MemoryStore: CredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var held: Credential?
    private var pending: PendingClaim?
    public private(set) var log: [String] = []
    public var failSaves = false
    /// Reads refuse as an item that no longer decodes does, until a wipe.
    public var unreadable = false

    public init(credential: Credential? = nil, pending: PendingClaim? = nil) {
        held = credential
        self.pending = pending
    }

    public func credential() throws -> Credential? {
        try lock.withLock {
            if unreadable { throw KeychainError(status: errSecDecode) }
            return held
        }
    }

    public func save(_ credential: Credential) throws {
        try lock.withLock {
            if failSaves { throw KeychainError(status: errSecIO) }
            held = credential
            log.append("save")
        }
    }

    public func pendingClaim() throws -> PendingClaim? {
        try lock.withLock {
            if unreadable { throw KeychainError(status: errSecDecode) }
            return pending
        }
    }
    public func savePending(_ claim: PendingClaim) throws { lock.withLock { pending = claim; log.append("savePending") } }
    public func clearPending() throws { lock.withLock { pending = nil; log.append("clearPending") } }
    public func wipe() throws { lock.withLock { held = nil; pending = nil; unreadable = false; log.append("wipe") } }
}
