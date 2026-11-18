import Security
import XCTest
@testable import DeskPhoneKit

/// The node as AppModel drives it: every call recorded, Join and Rejoin
/// answered from queues, Rejoin held until the test lets it answer, and any
/// one call held open by `hold`.
final class FakeNode: NodeControl, DoorOpening, @unchecked Sendable {
    static let up = Result<NodeAnswer, NodeError>.success(answer(#"{"ok":true,"address":"fd06:6e7:0:2::9"}"#))

    static func answer(_ json: String) -> NodeAnswer {
        try! Wire.decoder.decode(NodeAnswer.self, from: Data(json.utf8))
    }

    private let lock = NSLock()
    private var _calls: [String] = []
    private var held: CheckedContinuation<Void, Never>?
    private var released = false
    private var holds: [String: NodeHold] = [:]
    var calls: [String] { lock.withLock { _calls } }
    var holdRejoin = false
    var joinFails = false
    /// Join's answers, one per call; up (or `joinFails`) once they run out.
    var joinAnswers: [Result<NodeAnswer, NodeError>] = []
    /// What Go's exchange does before Join answers (writes node.json).
    var duringJoin: () -> Void = {}
    /// Rejoin's answers, one per call; up once they run out.
    var rejoinAnswers: [Result<NodeAnswer, NodeError>] = []
    var statusAnswer = FakeNode.up

    func join(dir: String, hostname: String, enrollmentJSON: String) async -> Result<NodeAnswer, NodeError> {
        let answer: Result<NodeAnswer, NodeError> = lock.withLock {
            _calls.append("join")
            if !joinAnswers.isEmpty { return joinAnswers.removeFirst() }
            return joinFails ? .failure(.refused(code: "not_enrolled", message: "the preauth key has expired")) : Self.up
        }
        duringJoin()
        return answer
    }

    func rejoin(dir: String, hostname: String) async -> Result<NodeAnswer, NodeError> {
        let answer: Result<NodeAnswer, NodeError> = lock.withLock {
            _calls.append("rejoin")
            return rejoinAnswers.isEmpty ? Self.up : rejoinAnswers.removeFirst()
        }
        if holdRejoin {
            await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
                let go = lock.withLock {
                    if !released { held = continuation }
                    return released
                }
                if go { continuation.resume() }
            }
        }
        return answer
    }

    func status() async -> Result<NodeAnswer, NodeError> {
        record("status")
        await heldOpen("status")
        return lock.withLock { statusAnswer }
    }

    func down() async {
        record("down")
        await heldOpen("down")
    }

    func openDoor(address: String, port: Int) async throws -> DoorConnection {
        record("open")
        throw NodeError.refused(code: "", message: "not asked")
    }

    func peerPath(address: String) async -> PeerPath? {
        record("peer_path")
        return nil
    }

    func defaultRoute(interface: String) async -> Result<NodeAnswer, NodeError> {
        record("default_route \(interface)")
        return .success(Self.answer(#"{"ok":true}"#))
    }

    /// Lets a held Rejoin answer, now or as soon as it is asked.
    func release() {
        let continuation: CheckedContinuation<Void, Never>? = lock.withLock {
            released = true
            defer { held = nil }
            return held
        }
        continuation?.resume()
    }

    /// Holds the next call named `name` (`status`, `down`) open until the
    /// hold is released; the calls after it answer at once.
    func hold(_ name: String) -> NodeHold {
        let hold = NodeHold()
        lock.withLock { holds[name] = hold }
        return hold
    }

    private func heldOpen(_ name: String) async {
        guard let hold = lock.withLock({ holds.removeValue(forKey: name) }) else { return }
        await hold.pass()
    }

    private func record(_ call: String) { lock.withLock { _calls.append(call) } }
}

/// One FakeNode call kept open: `entered` once it has begun, and it answers
/// once `release()` is called.
final class NodeHold: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Void, Never>?
    private var released = false
    private var _entered = false
    var entered: Bool { lock.withLock { _entered } }

    func pass() async {
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            let go = lock.withLock {
                _entered = true
                if !released { self.continuation = continuation }
                return released
            }
            if go { continuation.resume() }
        }
    }

    func release() {
        let continuation: CheckedContinuation<Void, Never>? = lock.withLock {
            released = true
            defer { self.continuation = nil }
            return continuation
        }
        continuation?.resume()
    }
}

/// A keychain that refuses its first reads as one does before the first
/// unlock, then answers as `MemoryStore` does.
final class LockedStore: CredentialStore, @unchecked Sendable {
    let inner: MemoryStore
    var refusals: Int

    init(_ inner: MemoryStore, refusals: Int) {
        self.inner = inner
        self.refusals = refusals
    }

    func credential() throws -> Credential? {
        if refusals > 0 {
            refusals -= 1
            throw KeychainError(status: errSecInteractionNotAllowed)
        }
        return try inner.credential()
    }

    func save(_ credential: Credential) throws { try inner.save(credential) }
    func pendingClaim() throws -> PendingClaim? { try inner.pendingClaim() }
    func savePending(_ claim: PendingClaim) throws { try inner.savePending(claim) }
    func clearPending() throws { try inner.clearPending() }
    func wipe() throws { try inner.wipe() }
}

/// Protected data, held unavailable until the test opens it.
@MainActor
final class Gate {
    private(set) var waiting = false
    private var continuation: CheckedContinuation<Void, Never>?

    func wait() async {
        waiting = true
        await withCheckedContinuation { continuation = $0 }
    }

    func open() {
        continuation?.resume()
        continuation = nil
    }
}

@MainActor
final class AppModelTests: XCTestCase {
    private let marker = "ai.gentility.desk.phone.installed"
    private var store: MemoryStore!
    private var directory: NodeDirectory!
    private var node: FakeNode!
    private var lobby: FakeLobby!
    private var defaults: UserDefaults!
    private var delays: Delays!

    override func setUp() async throws {
        store = MemoryStore()
        directory = NodeDirectory(url: FileManager.default.temporaryDirectory.appendingPathComponent("node-\(UUID().uuidString)"))
        node = FakeNode()
        lobby = FakeLobby()
        defaults = UserDefaults(suiteName: "test.\(UUID().uuidString)")
        defaults.set(true, forKey: marker)
        delays = Delays()
    }

    private func app(claims: FakeClaims? = nil, store: CredentialStore? = nil, protectedData: @escaping () async -> Void = {}, notifications: Notifications? = nil, pushEnvironment: String? = nil) -> AppModel {
        let delays = delays!
        return AppModel(
            store: store ?? self.store, nodeDirectory: directory, node: node, permission: FakePermission(.granted),
            makeLobby: { [lobby] _ in lobby! },
            transport: { _ in claims ?? FakeClaims(reply: .failure(ClaimRefusal.unknown)) },
            notifications: notifications, pushEnvironment: pushEnvironment,
            defaults: defaults,
            now: { WireDate.parse("2026-06-01T00:00:00Z")! },
            protectedData: protectedData,
            sleep: { await delays.add($0); await Task.yield() }
        )
    }

    private func credential(enrollment: String? = nil, expires: String = "2026-06-30T00:00:00Z") -> Credential {
        Credential(origin: URL(string: "https://mcp.gentility.test")!, bearer: "bearer-1", expiresAt: WireDate.parse(expires)!, enrollmentJSON: enrollment, enrollmentID: "e-1", phoneName: "Morgan's iPhone")
    }

    private func pending() -> PendingClaim {
        PendingClaim(origin: URL(string: "https://mcp.gentility.test")!, host: "mcp.gentility.test", pollSecret: "ps", machineLabel: "Morgan's MacBook Pro", match: "47", expiresAt: "2099-01-01T00:00:00Z", phoneName: "Morgan's iPhone")
    }

    private func row(_ id: String = "s-1", title: String = "Phone pairing spec", state: String = "working") -> HomeRow {
        HomeRow(id: id, organizationID: "o-1", title: title, state: state, ago: "2 min", machineID: "m-1", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: nil)
    }

    func testNoBearerOpensOnTheScanner() async {
        let app = app()
        await app.launch()
        XCTAssertEqual(app.route, .pairing)
        XCTAssertEqual(app.pairing.state, .scanning)
        XCTAssertTrue(node.calls.isEmpty)
    }

    func testABearerOpensHomeAtOnceAndRejoinsBehindIt() async throws {
        try store.save(credential())
        node.holdRejoin = true
        let app = app()
        await app.launch()
        XCTAssertEqual(app.route, .home, "Home does not wait for 6net")
        XCTAssertNotNil(app.home)
        try await until { self.node.calls == ["rejoin"] }
        XCTAssertEqual(app.nodeState, .joining)
        node.release()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
    }

    /// The bearer reached the keychain, the join did not finish.
    func testABearerSavedButNeverJoinedJoinsOnLaunch() async throws {
        try store.save(credential(enrollment: #"{"enrollment_id":"e-1"}"#))
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["join"])
        XCTAssertNil(try store.credential()?.enrollmentJSON, "the enrolment is spent once Join takes it")
    }

    func testAnEnrolmentThatDiedUnjoinedSaysPairAgain() async throws {
        try store.save(credential(enrollment: #"{"enrollment_id":"e-1","expires_at":"2026-05-31T23:50:00Z"}"#))
        node.joinFails = true
        let app = app()
        await app.launch()
        try await until { app.route == .failure(.notJoined) }
        XCTAssertNil(try store.credential())
    }

    func testAJoinThatFailsWhileTheKeyLivesIsTriedAgainNotForgotten() async throws {
        try store.save(credential(enrollment: #"{"enrollment_id":"e-1","expires_at":"2026-06-01T00:09:00Z"}"#))
        node.joinFails = true
        let app = app()
        await app.launch()
        try await until { if case .failed = app.nodeState { return true } else { return false } }
        XCTAssertEqual(app.route, .home)
        XCTAssertNotNil(try store.credential()?.enrollmentJSON)
    }

    func testARefusedBearerForgetsEverythingAndSaysRemoved() async throws {
        try store.save(credential())
        try directory.prepare()
        lobby.unauthorized = true
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        await app.home?.refresh()
        try await until { app.route == .failure(.removed) }
        XCTAssertNil(try store.credential())
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
        XCTAssertTrue(node.calls.contains("down"))
        XCTAssertNil(app.home)
        XCTAssertEqual(app.nodeState, .down)
        app.again()
        XCTAssertEqual(app.route, .pairing)
    }

    func testAnExpiredBearerSaysThePairingRanOut() async throws {
        try store.save(credential(expires: "2026-05-01T00:00:00Z"))
        let app = app()
        await app.launch()
        XCTAssertEqual(app.route, .failure(.lapsed))
        XCTAssertFalse(node.calls.contains("rejoin"))
    }

    func testAPendingClaimResumesTheNumberScreen() async throws {
        try store.savePending(pending())
        let app = app(claims: FakeClaims(reply: .failure(ClaimRefusal.unknown), answers: [.denied]))
        await app.launch()
        XCTAssertEqual(app.route, .pairing)
        XCTAssertEqual(app.pairing.state, .failed(.denied))
    }

    /// Killed after the bearer was kept and before the claim was cleared:
    /// resume reports the phone paired, and the launch brings the node up
    /// itself, from the kept enrolment.
    func testAKeptBearerBesideItsClaimResumesPairedAndJoins() async throws {
        try store.save(credential(enrollment: #"{"enrollment_id":"e-1"}"#))
        try store.savePending(pending())
        let claims = FakeClaims(reply: .failure(ClaimRefusal.unknown))
        let app = app(claims: claims)
        await app.launch()
        XCTAssertEqual(app.route, .home)
        XCTAssertNil(try store.pendingClaim())
        XCTAssertEqual(claims.polls, 0, "a spent claim is not polled")
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["join"])
    }

    func testPairedGoesHomeAndRegistersThePhoneOnceThePersonIsKnown() async throws {
        let granted = try Granted(JSONValue.parse(#"{"status":"allowed","access_token":"bearer-1","expires_in":2592000,"scope":"s","enrollment":{"enrollment_id":"e-1"}}"#))
        let claims = FakeClaims(reply: .success(ClaimReply(claimID: "c", machineLabel: "Morgan's MacBook Pro", expiresAt: "2099-01-01T00:00:00Z", pollSecret: "ps", match: "47")), answers: [.allowed(granted)])
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        let app = app(claims: claims)
        await app.launch()
        await app.scanned(ClaimLink.parse("https://mcp.gentility.test/phone/claim#q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U")!, phoneName: "Morgan's iPhone")
        try await until { app.route == .home }
        XCTAssertEqual(node.calls, ["join"], "the pairing's own Join, and no second one")
        await app.home?.refresh()
        try await until { self.lobby.registered == [Build.morgan] }
    }

    func testNothingIsReadBeforeProtectedDataIsAvailable() async throws {
        try store.save(credential())
        defaults.removeObject(forKey: marker)
        let gate = Gate()
        let app = app(protectedData: { await gate.wait() })
        let launching = Task { await app.launch() }
        try await until { gate.waiting }
        XCTAssertEqual(app.route, .launching)
        XCTAssertEqual(store.log, ["save"], "no reinstall check and no read while the keychain is locked")
        gate.open()
        await launching.value
        XCTAssertEqual(store.log, ["save", "wipe"], "a fresh install forgets the earlier one's keychain")
        XCTAssertEqual(app.route, .pairing)
        XCTAssertTrue(defaults.bool(forKey: marker))
    }

    func testAnUnreadableBearerIsForgottenAndThePhoneScans() async throws {
        try store.save(credential())
        store.unreadable = true
        let app = app()
        await app.launch()
        XCTAssertEqual(app.route, .pairing)
        XCTAssertEqual(store.log.last, "wipe")
        XCTAssertTrue(node.calls.isEmpty)
    }

    func testAKeychainThatRefusesAReadIsAskedAgainNotForgotten() async throws {
        try store.save(credential())
        let locked = LockedStore(store, refusals: 2)
        let app = app(store: locked)
        await app.launch()
        XCTAssertEqual(app.route, .home)
        XCTAssertEqual(store.log, ["save"])
        let waited = await delays.all
        XCTAssertEqual(waited, [.seconds(1), .seconds(2)])
    }

    func testARejoinRefusedAsNotEnrolledForgetsAndSaysRemoved() async throws {
        try store.save(credential())
        node.rejoinAnswers = [.failure(.refused(code: "not_enrolled", message: "no node state"))]
        let app = app()
        await app.launch()
        try await until { app.route == .failure(.removed) }
        XCTAssertNil(try store.credential())
        XCTAssertTrue(node.calls.contains("down"))
    }

    func testARejoinRefusedAsRevokedForgetsAndSaysRemoved() async throws {
        try store.save(credential())
        node.rejoinAnswers = [.failure(.refused(code: "revoked", message: "the grant is gone"))]
        let app = app()
        await app.launch()
        try await until { app.route == .failure(.removed) }
    }

    /// Before the first unlock the node's state file does not read: Rejoin
    /// is asked again, further apart each time, and nothing is forgotten.
    func testARejoinThatCannotReadItsStateYetIsTriedAgain() async throws {
        try store.save(credential())
        node.rejoinAnswers = [
            .failure(.refused(code: "unreadable", message: "node.json is locked")),
            .failure(.refused(code: "joining", message: "a join is under way")),
        ]
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin", "rejoin", "rejoin"])
        let waited = await delays.all
        XCTAssertEqual(waited, [.seconds(1), .seconds(2)])
        XCTAssertEqual(app.route, .home)
        XCTAssertNotNil(try store.credential())
    }

    func testBackFromTheBackgroundANodeThatWentDownRejoins() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        node.statusAnswer = .success(FakeNode.answer(#"{"state":"down","ended":"the link dropped","ended_code":"join_failed"}"#))
        app.background()
        await app.foreground()
        XCTAssertEqual(node.calls, ["rejoin", "status", "rejoin"])
        XCTAssertEqual(app.nodeState, .up(address: "fd06:6e7:0:2::9"))
    }

    func testBackFromTheBackgroundANodeEndedAsRevokedSaysRemoved() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        node.statusAnswer = .success(FakeNode.answer(#"{"state":"down","ended":"revoked by the head","ended_code":"revoked"}"#))
        app.background()
        await app.foreground()
        XCTAssertEqual(app.route, .failure(.removed))
    }

    func testANodeThatIsUpIsLeftAloneOnForeground() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        node.statusAnswer = .success(FakeNode.answer(#"{"state":"up","address":"fd06:6e7:0:2::9"}"#))
        app.background()
        await app.foreground()
        XCTAssertEqual(node.calls, ["rejoin", "status"])
    }

    /// A6: the route's interface goes to the node whatever state it is in;
    /// the library keeps the hint for the next socket it binds.
    func testADefaultRouteChangeIsHandedToTheNodeEvenBeforeItIsUp() async throws {
        let app = app()
        await app.defaultRoute("pdp_ip0")
        XCTAssertEqual(node.calls, ["default_route pdp_ip0"])
        try store.save(credential())
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        await app.defaultRoute("en0")
        XCTAssertEqual(node.calls.last, "default_route en0")
    }

    /// Becoming active without having been away (a pulled-down notification
    /// centre, the first activation) asks nothing.
    func testAnActivationWithoutABackgroundAsksNothing() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        await app.foreground()
        XCTAssertEqual(node.calls, ["rejoin"])
    }

    func testTheBackgroundStopsHomesMinuteAndTheForegroundStartsItAgain() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let home = try XCTUnwrap(app.home)
        home.appear()
        try await until { self.lobby.refreshes == 1 }
        app.background()
        await app.foreground()
        try await until { self.lobby.refreshes == 2 }
    }

    func testHomeTickerStopsWhileAnOrdinaryConversationIsShowing() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let home = try XCTUnwrap(app.home)
        home.appear()
        XCTAssertTrue(home.periodicallyRefreshing)
        let shown = try XCTUnwrap(app.conversation(for: row()))
        app.showing(shown)
        XCTAssertFalse(home.periodicallyRefreshing)
        app.hid(shown)
        XCTAssertTrue(home.periodicallyRefreshing)
        app.background()
        XCTAssertFalse(home.periodicallyRefreshing)
    }

    func testOnlyVisibleUnopenedConversationKeepsHomeTicker() async throws {
        try store.save(credential())
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
            try Build.machine(desk: "m-1", installs: [.object([
                "desk_machine_id": .string("m-1"), "desk_presence": .string("online"), "door_port": .null,
            ])]),
        ], now: Build.now))
        let app = app()
        await app.launch()
        let home = try XCTUnwrap(app.home)
        home.appear()
        try await until { self.lobby.refreshes == 1 }
        XCTAssertTrue(home.periodicallyRefreshing)
        let shown = try XCTUnwrap(app.conversation(for: row()))
        app.showing(shown)
        XCTAssertFalse(home.periodicallyRefreshing)
        await shown.open()
        XCTAssertEqual(shown.phase, .notOpen)
        XCTAssertTrue(home.periodicallyRefreshing)
        app.background()
        XCTAssertFalse(home.periodicallyRefreshing)
        let readsBeforeForeground = lobby.refreshes
        await app.foreground()
        XCTAssertEqual(lobby.refreshes, readsBeforeForeground + 1)
        XCTAssertEqual(shown.phase, .notOpen)
        XCTAssertTrue(home.periodicallyRefreshing)
        let other = try XCTUnwrap(app.conversation(for: row("other")))
        app.showing(other)
        XCTAssertFalse(home.periodicallyRefreshing)
        shown.leave()
        other.leave()
        app.background()
    }

    func testOnlyAVisibleConversationDialsAgainOnForeground() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let hidden = try XCTUnwrap(app.conversation(for: row("s-hidden")))
        let shown = try XCTUnwrap(app.conversation(for: row()))
        app.showing(shown)
        app.background()
        await app.foreground()
        XCTAssertEqual(shown.phase, .asleep(macLabel: "Morgan's MacBook Pro"), "dialled again, and found no door")
        XCTAssertEqual(hidden.phase, .connecting, "never asked")
        shown.leave()
        app.hid(shown)
    }

    func testAConversationThatFindsThePhoneRemovedForgetsIt() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let conversation = try XCTUnwrap(app.conversation(for: row()))
        conversation.onRemoved?()
        try await until { app.route == .failure(.removed) }
        XCTAssertNil(try store.credential())
    }

    /// B5: the same conversation comes back for the same row, with the row
    /// as Home lists it now.
    func testAReopenedConversationIsTheOneKeptWithTheRowAsItIsNow() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let first = try XCTUnwrap(app.conversation(for: row("s-1", title: "Before")))
        let again = try XCTUnwrap(app.conversation(for: row("s-1", title: "After")))
        XCTAssertTrue(first === again)
        XCTAssertEqual(again.row.title, "After")
    }

    /// A reopen onto a Mac with no door draws the row as Home lists it now:
    /// not the last visit's working cursor under "Status: idle". Asleep
    /// stays asleep until the open asks again.
    func testAReopenOntoAnAsleepMacIsWorkingOnlyAsTheRowIsNow() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let first = try XCTUnwrap(app.conversation(for: row("s-1", state: "working")))
        await first.open()
        XCTAssertEqual(first.phase, .asleep(macLabel: "Morgan's MacBook Pro"))
        XCTAssertTrue(first.working)
        first.leave()
        let again = try XCTUnwrap(app.conversation(for: row("s-1", state: "idle")))
        XCTAssertTrue(first === again)
        XCTAssertEqual(again.phase, .asleep(macLabel: "Morgan's MacBook Pro"), "reopened() leaves asleep alone")
        await again.open()
        XCTAssertEqual(again.phase, .asleep(macLabel: "Morgan's MacBook Pro"))
        XCTAssertFalse(again.working)
        again.leave()
    }

    /// Review focus 3.
    func testTheNinthConversationEvictsTheOldestWhichOpensFreshNextTime() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        let first = try XCTUnwrap(app.conversation(for: row("s-1")))
        let firstOfS2 = try XCTUnwrap(app.conversation(for: row("s-2")))
        for n in 3...8 { _ = app.conversation(for: row("s-\(n)")) }
        XCTAssertTrue(first === app.conversation(for: row("s-1")), "eight are kept; touching one keeps it newest")
        _ = app.conversation(for: row("s-9"))
        XCTAssertTrue(first === app.conversation(for: row("s-1")), "s-1 was touched after s-2, so s-2 went")
        let second = try XCTUnwrap(app.conversation(for: row("s-2")))
        XCTAssertFalse(second === firstOfS2, "s-2 was let go, so a fresh model")
        // Kept now, oldest first: s-4…s-9, s-1, s-2. Seven more let s-1 go.
        for n in 10...16 { _ = app.conversation(for: row("s-\(n)")) }
        let later = try XCTUnwrap(app.conversation(for: row("s-1")))
        XCTAssertFalse(first === later, "evicted, so a fresh model")
    }

    /// A new pairing's Home is another HomeModel; nothing of the old is kept.
    func testForgettingThePairingDropsTheKeptConversations() async throws {
        try store.save(credential())
        let granted = try Granted(JSONValue.parse(#"{"status":"allowed","access_token":"bearer-1","expires_in":2592000,"scope":"s","enrollment":{"enrollment_id":"e-1"}}"#))
        let claims = FakeClaims(reply: .success(ClaimReply(claimID: "c", machineLabel: "Morgan's MacBook Pro", expiresAt: "2099-01-01T00:00:00Z", pollSecret: "ps", match: "47")), answers: [.allowed(granted)])
        let app = app(claims: claims)
        await app.launch()
        let first = try XCTUnwrap(app.conversation(for: row("s-1")))
        await app.forget(.removed)
        XCTAssertNil(app.conversation(for: row("s-1")), "no Home after forget")
        app.again()
        await app.scanned(ClaimLink.parse("https://mcp.gentility.test/phone/claim#q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U")!, phoneName: "Morgan's iPhone")
        try await until { app.route == .home }
        let after = try XCTUnwrap(app.conversation(for: row("s-1")))
        XCTAssertFalse(first === after, "the new pairing's conversation, not the old one")
    }

    // MARK: an identity kept beside the enrolment

    /// What Go's exchange writes inside Join, before Join answers.
    private func keepIdentity() throws {
        try directory.prepare()
        XCTAssertTrue(FileManager.default.createFile(atPath: directory.url.appendingPathComponent("node.json").path, contents: Data("{}".utf8)))
    }

    private let deadKey = #"{"enrollment_id":"e-1","expires_at":"2026-05-31T23:50:00Z"}"#
    private let liveKey = #"{"enrollment_id":"e-1","expires_at":"2026-06-01T00:09:00Z"}"#

    /// Killed after Join's exchange wrote node.json and before the spent
    /// enrolment was forgotten, and opened again past the key's ten minutes:
    /// the identity brings the node back, and the enrolment is forgotten now.
    func testAKillAfterJoinsExchangeRejoinsOnTheIdentityAndForgetsTheEnrolment() async throws {
        try store.save(credential(enrollment: deadKey))
        try keepIdentity()
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin"], "no Join with a spent key")
        XCTAssertEqual(app.route, .home)
        XCTAssertNil(try store.credential()?.enrollmentJSON, "the spent enrolment is forgotten once Rejoin brings the node back")
        XCTAssertNotNil(try store.credential())
    }

    func testAnIdentityThatDoesNotBringTheNodeBackJoinsWithTheEnrolment() async throws {
        try store.save(credential(enrollment: liveKey))
        try keepIdentity()
        node.rejoinAnswers = [.failure(.refused(code: "not_enrolled", message: "node.json names no head"))]
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin", "join"])
        XCTAssertNil(try store.credential()?.enrollmentJSON)
    }

    /// Join's exchange kept an identity, then the join failed (a server
    /// deploy mid-join): Rejoin is tried on it at once.
    func testAJoinThatFailsAfterItsExchangeRejoinsOnTheIdentity() async throws {
        try store.save(credential(enrollment: deadKey))
        node.joinAnswers = [.failure(.refused(code: "join_failed", message: "the config channel ended"))]
        node.duringJoin = { [directory] in
            try? directory!.prepare()
            FileManager.default.createFile(atPath: directory!.url.appendingPathComponent("node.json").path, contents: Data("{}".utf8))
        }
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["join", "rejoin"])
        XCTAssertEqual(app.route, .home, "not forgotten although the key died")
        XCTAssertNil(try store.credential()?.enrollmentJSON)
    }

    func testAMalformedEnrolmentIsNotRejoined() async throws {
        try store.save(credential(enrollment: liveKey))
        node.joinAnswers = [.failure(.refused(code: "malformed", message: "the join material names no preauth_key"))]
        node.duringJoin = { [directory] in
            try? directory!.prepare()
            FileManager.default.createFile(atPath: directory!.url.appendingPathComponent("node.json").path, contents: Data("{}".utf8))
        }
        let app = app()
        await app.launch()
        try await until { if case .failed = app.nodeState { return true } else { return false } }
        XCTAssertEqual(node.calls, ["join"])
    }

    /// Both the identity and the key fail, and the key is past its ten
    /// minutes: nothing is left to join with.
    func testAnIdentityAndAKeyThatBothFailPastTheKeysDeathSayPairAgain() async throws {
        try store.save(credential(enrollment: deadKey))
        try keepIdentity()
        node.rejoinAnswers = [.failure(.refused(code: "not_enrolled", message: "node.json names no head"))]
        node.joinFails = true
        let app = app()
        await app.launch()
        try await until { app.route == .failure(.notJoined) }
        XCTAssertEqual(node.calls.prefix(2), ["rejoin", "join"])
        XCTAssertNil(try store.credential())
    }

    /// The identity is there and does not read yet (before the first
    /// unlock): it is asked again, and the dead key changes nothing.
    func testAnIdentityThatCannotBeReadYetIsTriedAgainWhateverTheKey() async throws {
        try store.save(credential(enrollment: deadKey))
        try keepIdentity()
        node.rejoinAnswers = [.failure(.refused(code: "unreadable", message: "node.json is locked"))]
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin", "rejoin"])
        XCTAssertEqual(app.route, .home)
        XCTAssertNil(try store.credential()?.enrollmentJSON)
    }

    // MARK: a node that does not come up

    /// A server deploy mid-join, or a head slow to answer: tried again,
    /// further apart each time.
    func testARejoinThatFailsOrTimesOutIsTriedAgain() async throws {
        try store.save(credential())
        node.rejoinAnswers = [
            .failure(.refused(code: "join_failed", message: "the head hung up")),
            .failure(.refused(code: "timed_out", message: "no netmap in time")),
        ]
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin", "rejoin", "rejoin"])
        let waited = await delays.all
        XCTAssertEqual(waited, [.seconds(1), .seconds(2)])
    }

    func testAJoinThatFailsWhileTheKeyLivesIsTriedAgainLater() async throws {
        try store.save(credential(enrollment: liveKey))
        node.joinAnswers = [.failure(.refused(code: "join_failed", message: "the head hung up"))]
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["join", "join"])
        let waited = await delays.all
        XCTAssertEqual(waited, [.seconds(1)])
    }

    /// The node ended in the foreground: a conversation's dial says
    /// node_down, and the node is asked how it stands and brought up.
    func testAConversationThatFindsTheNodeDownBringsItUp() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        node.statusAnswer = .success(FakeNode.answer(#"{"state":"down","ended":"the head hung up","ended_code":"join_failed"}"#))
        let conversation = try XCTUnwrap(app.conversation(for: row()))
        conversation.onNodeDown?()
        try await until { self.node.calls == ["rejoin", "status", "rejoin"] }
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
    }

    func testOneNodeCheckAtATime() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        node.statusAnswer = .success(FakeNode.answer(#"{"state":"down"}"#))
        let status = node.hold("status")
        let first = Task { await app.nodeWentDown() }
        try await until { status.entered }
        await app.nodeWentDown()
        XCTAssertEqual(node.calls, ["rejoin", "status"], "the second asks nothing while the first is out")
        status.release()
        await first.value
        XCTAssertEqual(node.calls, ["rejoin", "status", "rejoin"])
    }

    func testANodeAlreadySetToComeUpLaterIsNotAskedAgain() async throws {
        try store.save(credential())
        node.rejoinAnswers = [.failure(.refused(code: "join_failed", message: "the head hung up"))]
        let ticks = Ticks()
        let app = AppModel(
            store: store, nodeDirectory: directory, node: node, permission: FakePermission(.granted),
            makeLobby: { [lobby] _ in lobby! }, transport: { _ in FakeClaims(reply: .failure(ClaimRefusal.unknown)) },
            defaults: defaults, now: { WireDate.parse("2026-06-01T00:00:00Z")! },
            sleep: { _ in try await ticks.wait() }
        )
        await app.launch()
        try await ticks.untilWaiting(1)
        await app.nodeWentDown()
        XCTAssertEqual(node.calls, ["rejoin"], "the Rejoin set for later stands")
        await ticks.tick()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        XCTAssertEqual(node.calls, ["rejoin", "rejoin"])
    }

    // MARK: forgetting

    func testASecondForgetWhileTheFirstIsOutIsANoOp() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let down = node.hold("down")
        let first = Task { await app.forget(.removed) }
        try await until { down.entered }
        XCTAssertEqual(app.route, .failure(.removed), "said at once, never blank while the node goes down")
        await app.forget(.lapsed)
        XCTAssertEqual(app.route, .failure(.removed))
        down.release()
        await first.value
        XCTAssertEqual(node.calls.filter { $0 == "down" }.count, 1)
        XCTAssertEqual(app.route, .failure(.removed))
    }

    /// "Pair again" pressed while the node is still going down: the scanner
    /// opens once the old node and its directory are gone.
    func testPairAgainWhileForgettingWaitsForTheForgetting() async throws {
        try store.save(credential())
        try directory.prepare()
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let down = node.hold("down")
        let first = Task { await app.forget(.removed) }
        try await until { down.entered }
        app.again()
        XCTAssertEqual(app.route, .failure(.removed))
        down.release()
        await first.value
        XCTAssertEqual(app.route, .pairing)
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    func testPairAgainFromSettingsForgetsWithNoFailureAndOpensTheScanner() async throws {
        try store.save(credential())
        try directory.prepare()
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let down = node.hold("down")
        let asked = Task { await app.pairAgain() }
        try await until { down.entered }
        XCTAssertEqual(app.route, .launching, "nothing failed, so no failure is said while the node goes down")
        await app.launch()
        XCTAssertEqual(app.route, .launching, "a launch while forgetting reads nothing")
        down.release()
        await asked.value
        XCTAssertEqual(app.route, .pairing)
        XCTAssertNil(app.settings)
        XCTAssertNil(try store.credential())
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    func testSettingsSayThePhoneItsServerAndHowTheNodeStands() async throws {
        try store.save(credential())
        let app = app()
        XCTAssertNil(app.settings)
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let settings = try XCTUnwrap(app.settings)
        XCTAssertEqual(settings.name, "Morgan's iPhone")
        XCTAssertEqual(settings.server, "mcp.gentility.test")
        XCTAssertEqual(settings.connection, .connected)
        XCTAssertEqual(settings.left(now: WireDate.parse("2026-06-01T00:00:00Z")!), "29 days left")
    }

    // MARK: push

    private func payload(_ session: String = "s-1", organization: String = "o-1") -> PushPayload {
        PushPayload(organizationID: organization, sessionID: session, deskMachineID: "m-1", fact: "approval_requested")
    }

    private func listOne(_ more: LobbyRow...) throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Fix the recap timing", mac: "m-1", host: "Morgan's MacBook Pro", attention: "approval", attentionLine: "Wants to execute"),
        ] + more, now: Build.now)]
    }

    func testHomeAsksForNotificationsOnceAndRegistersTheTokenWithItsEnvironment() async throws {
        try store.save(credential())
        let notifications = FakeNotifications()
        let app = app(notifications: notifications, pushEnvironment: "sandbox")
        await app.launch()
        await app.home?.refresh()
        await app.homeAppeared()
        XCTAssertEqual(notifications.asked, 1)
        XCTAssertEqual(notifications.registeredForRemote, 1)
        XCTAssertFalse(app.notificationsOff)
        app.gotPushToken(Data([0x0a, 0xbc]))
        try await until { self.lobby.registrations.last?.pushToken == "0abc" }
        XCTAssertEqual(lobby.registrations.map(\.environment), [nil, "sandbox"], "the environment goes with a token only")
        await app.homeAppeared()
        XCTAssertEqual(notifications.asked, 1, "iOS asks once")
        XCTAssertEqual(notifications.registeredForRemote, 1, "a token once a launch")
        app.gotPushToken(Data([0x0a, 0xbc]))
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(lobby.registrations.map(\.pushToken), [nil, "0abc"], "the same token is not registered twice")
        app.gotPushToken(Data([0x0d]))
        try await until { self.lobby.registrations.map(\.pushToken) == [nil, "0abc", "0d"] }
    }

    func testDeniedNotificationsSayHowToTurnThemOnAndRegisterNoToken() async throws {
        try store.save(credential())
        let notifications = FakeNotifications()
        notifications.grants = false
        let app = app(notifications: notifications, pushEnvironment: "sandbox")
        await app.launch()
        await app.home?.refresh()
        await app.homeAppeared()
        XCTAssertTrue(app.notificationsOff)
        XCTAssertEqual(notifications.registeredForRemote, 0)
        try await until { self.lobby.registrations.count == 1 }
        XCTAssertEqual(lobby.registrations, [.init(personID: Build.morgan, pushToken: nil, environment: nil)], "registered, with no token and so no environment")
    }

    func testNotificationsTurnedOnInSettingsRegisterOnTheWayBack() async throws {
        try store.save(credential())
        let notifications = FakeNotifications()
        notifications.status = .denied
        let app = app(notifications: notifications)
        await app.launch()
        await app.homeAppeared()
        XCTAssertTrue(app.notificationsOff)
        XCTAssertEqual(notifications.asked, 0, "iOS no longer lets the app ask")
        notifications.status = .allowed
        app.background()
        await app.foreground()
        XCTAssertFalse(app.notificationsOff)
        XCTAssertEqual(notifications.registeredForRemote, 1)
    }

    /// A permission prompt left open (the app went to the background under
    /// it) is answered whenever the person gets to it; the way back does not
    /// wait on it to dial the conversation on screen again.
    func testAPromptLeftOpenDoesNotHoldTheWayBack() async throws {
        try store.save(credential())
        let notifications = FakeNotifications()
        let gate = Gate()
        notifications.prompting = { await gate.wait() }
        let app = app(notifications: notifications)
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let shown = try XCTUnwrap(app.conversation(for: row()))
        app.showing(shown)
        app.background()
        let back = Task { await app.foreground() }
        try await until { gate.waiting }
        try await until { shown.phase == .asleep(macLabel: "Morgan's MacBook Pro") }
        gate.open()
        await back.value
        XCTAssertFalse(app.notificationsOff)
        XCTAssertEqual(notifications.registeredForRemote, 1)
        shown.leave()
        app.hid(shown)
    }

    /// Final review 1: conversation A is on screen, the phone locks, and a
    /// tap on B's notification opens B while the way back re-lists Home.
    /// A was left: it never dials again.
    func testATapThatOpensAnotherConversationOnTheWayBackLeavesTheFirstAlone() async throws {
        try store.save(credential())
        let app = app()
        await app.launch()
        try await until { app.nodeState == .up(address: "fd06:6e7:0:2::9") }
        let first = try XCTUnwrap(app.conversation(for: row()))
        app.showing(first)
        app.background()
        let gate = Gate()
        lobby.refreshing = { await gate.wait() }
        let back = Task { await app.foreground() }
        try await until { gate.waiting }
        // The tap's navigation: A's screen goes, B's comes.
        first.leave()
        app.hid(first)
        let second = try XCTUnwrap(app.conversation(for: HomeRow(
            id: "s-2", organizationID: "o-1", title: "Flaky store test", state: "idle", ago: "now",
            machineID: "m-1", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: nil
        )))
        app.showing(second)
        lobby.refreshing = {}
        gate.open()
        await back.value
        XCTAssertEqual(first.phase, .connecting, "the conversation left behind was never dialled")
        app.hid(second)
    }

    func testATokenBeforeThePersonIsKnownRegistersOnceBothAre() async throws {
        try store.save(credential())
        let app = app(pushEnvironment: "production")
        await app.launch()
        app.gotPushToken(Data([0x01]))
        XCTAssertEqual(lobby.registrations, [])
        await app.home?.refresh()
        try await until { self.lobby.registrations.count == 1 }
        XCTAssertEqual(lobby.registrations, [.init(personID: Build.morgan, pushToken: "01", environment: "production")])
    }

    func testARefusedRegisterIsTriedAgainWhenHomeNextAppears() async throws {
        try store.save(credential())
        lobby.registerRefusals = 1
        let app = app(pushEnvironment: "sandbox")
        await app.launch()
        app.gotPushToken(Data([0x01]))
        await app.home?.refresh()
        try await until { self.lobby.registrations.count == 1 }
        try await Task.sleep(for: .milliseconds(20))
        await app.homeAppeared()
        try await until { self.lobby.registrations.count == 2 }
        await app.homeAppeared()
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(lobby.registrations.count, 2, "once it answered, not again")
        XCTAssertEqual(lobby.registrations.last, .init(personID: Build.morgan, pushToken: "01", environment: "sandbox"))
    }

    func testAFailedTokenRequestIsAskedAgainWhenHomeNextAppears() async throws {
        try store.save(credential())
        let notifications = FakeNotifications()
        notifications.status = .allowed
        let app = app(notifications: notifications)
        await app.launch()
        await app.homeAppeared()
        await app.homeAppeared()
        XCTAssertEqual(notifications.registeredForRemote, 1, "a token once a launch")
        app.pushTokenFailed(NSError(domain: NSCocoaErrorDomain, code: 3010))
        await app.homeAppeared()
        XCTAssertEqual(notifications.registeredForRemote, 2, "asked again after APNs gave none")
    }

    /// A register out holds the next: the newest token goes once it is back,
    /// and the tokens in between never go.
    func testRegistersGoOneAtATimeAndTheNewestTokenGoesNext() async throws {
        try store.save(credential())
        lobby.holdRegisters = 1
        let app = app(pushEnvironment: "sandbox")
        await app.launch()
        await app.home?.refresh()
        try await until { self.lobby.holdingRegister }
        app.gotPushToken(Data([0x01]))
        app.gotPushToken(Data([0x02]))
        XCTAssertEqual(lobby.registrations.count, 1, "the token waits on the register out")
        lobby.releaseRegister()
        try await until { self.lobby.registrations.count == 2 }
        XCTAssertEqual(lobby.registrations, [
            .init(personID: Build.morgan, pushToken: nil, environment: nil),
            .init(personID: Build.morgan, pushToken: "02", environment: "sandbox"),
        ])
        XCTAssertEqual(lobby.mostRegistersOut, 1)
    }

    /// The pairing is forgotten while its register is out, and the same
    /// person pairs again: the old reply does not count for the new pairing.
    func testARegisterOutWhenThePairingIsForgottenDoesNotCountForTheNext() async throws {
        try store.save(credential())
        lobby.holdRegisters = 1
        let granted = try Granted(JSONValue.parse(#"{"status":"allowed","access_token":"bearer-1","expires_in":2592000,"scope":"s","enrollment":{"enrollment_id":"e-1"}}"#))
        let claims = FakeClaims(reply: .success(ClaimReply(claimID: "c", machineLabel: "Morgan's MacBook Pro", expiresAt: "2099-01-01T00:00:00Z", pollSecret: "ps", match: "47")), answers: [.allowed(granted)])
        let app = app(claims: claims)
        await app.launch()
        await app.home?.refresh()
        try await until { self.lobby.holdingRegister }
        await app.forget(.removed)
        app.again()
        await app.scanned(ClaimLink.parse("https://mcp.gentility.test/phone/claim#q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U")!, phoneName: "Morgan's iPhone")
        try await until { app.route == .home }
        await app.home?.refresh()
        XCTAssertEqual(lobby.registrations.count, 1, "the new pairing's register waits on the one out")
        lobby.releaseRegister()
        try await until { self.lobby.registrations.count == 2 }
        XCTAssertEqual(lobby.registrations.map(\.personID), [Build.morgan, Build.morgan])
        XCTAssertEqual(lobby.mostRegistersOut, 1)
    }

    func testATapOpensThatConversation() async throws {
        try store.save(credential())
        try listOne()
        let app = app()
        await app.launch()
        await app.home?.refresh()
        app.opened(payload())
        XCTAssertEqual(app.path.map(\.id), ["s-1"])
        XCTAssertEqual(app.path.first?.attentionLine, "Wants to execute")
    }

    /// Review focus 5.
    func testATapBeforeHomeHasListedOpensTheConversationOnceItHas() async throws {
        try store.save(credential())
        try listOne()
        let app = app()
        app.opened(payload())
        await app.launch()
        XCTAssertEqual(app.path, [], "Home has not listed yet")
        await app.home?.refresh()
        XCTAssertEqual(app.path.map(\.id), ["s-1"])
    }

    /// Review focus 5.
    func testATapForASessionHomeDoesNotListReadsOnceMoreThenStaysHome() async throws {
        try store.save(credential())
        try listOne()
        let app = app()
        await app.launch()
        await app.home?.refresh()
        let before = lobby.refreshes
        app.opened(payload("s-9"))
        try await until { self.lobby.refreshes == before + 1 }
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(lobby.refreshes, before + 1, "read once more, not again and again")
        XCTAssertEqual(app.path, [])
        XCTAssertEqual(app.route, .home)
    }

    func testATapForASessionListedOnTheReadAgainOpensIt() async throws {
        try store.save(credential())
        try listOne()
        let app = app()
        await app.launch()
        await app.home?.refresh()
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-2", title: "Started a minute ago", mac: "m-1", host: "Morgan's MacBook Pro", attention: "question", attentionLine: "Asks a question"),
        ], now: Build.now)]
        app.opened(payload("s-2"))
        try await until { app.path.map(\.id) == ["s-2"] }
    }

    /// A teammate's session in the same organisation, or the person's own
    /// session id named in another organisation: Home lists neither, so the
    /// tap opens nothing, and the conversation on screen gives way to Home.
    func testATapForASessionThePersonCannotSeeGoesHome() async throws {
        try store.save(credential())
        try listOne(try Build.row("s-3", title: "A teammate's", mac: "m-1", host: "Morgan's MacBook Pro", owner: "u-teammate", attention: "approval", attentionLine: "Wants to execute"))
        let app = app()
        await app.launch()
        await app.home?.refresh()
        app.opened(payload())
        XCTAssertEqual(app.path.map(\.id), ["s-1"])
        let before = lobby.refreshes
        app.opened(payload("s-3"))
        try await until { app.path == [] }
        XCTAssertEqual(lobby.refreshes, before + 1)
        app.opened(payload("s-1", organization: "o-2"))
        try await until { self.lobby.refreshes == before + 2 }
        try await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(app.path, [])
        XCTAssertEqual(app.route, .home)
    }

    func testATapOnTheConversationOnScreenLeavesItBe() async throws {
        try store.save(credential())
        try listOne()
        let app = app()
        await app.launch()
        await app.home?.refresh()
        let row = try XCTUnwrap(app.home?.row(organizationID: "o-1", sessionID: "s-1"))
        let conversation = try XCTUnwrap(app.conversation(for: row))
        app.showing(conversation)
        let before = lobby.refreshes
        app.opened(payload())
        XCTAssertEqual(app.path, [], "already on screen; nothing pushed on top")
        XCTAssertEqual(lobby.refreshes, before)
        app.hid(conversation)
    }

    func testTheBannerShowsUnlessThatConversationIsOnScreenAndOpeningOneClearsItsNotifications() async throws {
        try store.save(credential())
        try listOne()
        let notifications = FakeNotifications()
        let app = app(notifications: notifications)
        await app.launch()
        await app.home?.refresh()
        XCTAssertTrue(app.presents(payload()))
        let row = try XCTUnwrap(app.home?.row(organizationID: "o-1", sessionID: "s-1"))
        let conversation = try XCTUnwrap(app.conversation(for: row))
        app.showing(conversation)
        XCTAssertFalse(app.presents(payload()))
        XCTAssertTrue(app.presents(payload("s-2")))
        XCTAssertTrue(app.presents(payload("s-1", organization: "o-2")))
        try await until { notifications.removed == ["s-1"] }
        app.background()
        await app.foreground()
        try await until { notifications.removed == ["s-1", "s-1"] }
        conversation.leave()
        app.hid(conversation)
        XCTAssertTrue(app.presents(payload()))
    }

    func testALaunchCancelledWhileWaitingForProtectedDataReadsNothing() async throws {
        try store.save(credential())
        let gate = Gate()
        var waits = 0
        let app = app(protectedData: {
            waits += 1
            if waits == 1 { await gate.wait() }
        })
        let launching = Task { await app.launch() }
        try await until { gate.waiting }
        launching.cancel()
        gate.open()
        await launching.value
        XCTAssertEqual(app.route, .launching)
        XCTAssertEqual(store.log, ["save"], "nothing read on a cancelled wait")
        XCTAssertTrue(node.calls.isEmpty)
        await app.launch()
        XCTAssertEqual(app.route, .home, "the next launch reads")
    }
}
