import Combine
import XCTest
@testable import DeskPhoneKit

final class FakeClaims: ClaimTransport, @unchecked Sendable {
    var reply: Result<ClaimReply, Error>
    var answers: [PollAnswer]
    /// Thrown by the next polls, before any answer is given.
    var pollFailures: [Error] = []
    private(set) var names: [String] = []
    private(set) var polls = 0

    init(reply: Result<ClaimReply, Error>, answers: [PollAnswer] = []) {
        self.reply = reply
        self.answers = answers
    }

    func claim(code: String, phoneName: String) async throws -> ClaimReply {
        names.append(phoneName)
        return try reply.get()
    }

    func poll(secret: String) async throws -> PollAnswer {
        polls += 1
        if !pollFailures.isEmpty { throw pollFailures.removeFirst() }
        return answers.isEmpty ? .waiting("pending") : answers.removeFirst()
    }
}

/// A clock that moves only when the flow sleeps.
final class SleepClock: @unchecked Sendable {
    private let lock = NSLock()
    private var instant: Date

    init(_ start: Date) {
        instant = start
    }

    var now: Date { lock.withLock { instant } }

    func sleep(_ duration: Duration) {
        let (seconds, attoseconds) = duration.components
        lock.withLock { instant = instant.addingTimeInterval(Double(seconds) + Double(attoseconds) / 1e18) }
    }
}

@MainActor
final class PairingFlowTests: XCTestCase {
    private let link = ClaimLink.parse("https://mcp.gentility.test/phone/claim#q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U")!
    private var directory: NodeDirectory!
    private var states: [PairingFlow.State] = []
    private var watching: AnyCancellable?

    override func setUp() async throws {
        directory = NodeDirectory(url: FileManager.default.temporaryDirectory.appendingPathComponent("node-\(UUID().uuidString)"))
        states = []
    }

    private func reply(expiresAt: String = "2099-01-01T00:00:00.000000Z") -> ClaimReply {
        ClaimReply(claimID: "c-1", machineLabel: "Morgan's MacBook Pro", expiresAt: expiresAt, pollSecret: "ps", match: "47")
    }

    private func granted() throws -> PollAnswer {
        .allowed(try Granted(JSONValue.parse(#"{"status":"allowed","access_token":"bearer-1","expires_in":2592000,"scope":"agent:connect agent:execute","enrollment":{"enrollment_id":"e-1","preauth_key":"k"}}"#)))
    }

    private func pending(expiresAt: String = "2099-01-01T00:00:00.000000Z") -> PendingClaim {
        PendingClaim(origin: link.origin, host: link.host, pollSecret: "ps", machineLabel: "Morgan's MacBook Pro", match: "47", expiresAt: expiresAt, phoneName: "Morgan's iPhone")
    }

    private func flow(
        _ store: MemoryStore, _ claims: FakeClaims,
        origins: @escaping (URL) -> Void = { _ in },
        join: @escaping (Credential) async -> Void = { _ in },
        now: @escaping () -> Date = Date.init,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in await Task.yield() },
        joinWait: Duration = .milliseconds(50)
    ) -> PairingFlow {
        let flow = PairingFlow(
            store: store, nodeDirectory: directory,
            transport: { origins($0); return claims },
            join: join,
            sleep: sleep,
            now: now,
            joinWait: joinWait
        )
        watching = flow.$state.sink { [weak self] in self?.states.append($0) }
        return flow
    }

    func testTheNumberScreenNamesTheMacTheDigitsAndTheHost() async {
        let claims = FakeClaims(reply: .success(reply()), answers: [.waiting("pending"), .waiting("asking"), .denied])
        await flow(MemoryStore(), claims).begin(link, phoneName: "Morgan's iPhone")
        XCTAssertTrue(states.contains(.number(machineLabel: "Morgan's MacBook Pro", match: "47", host: "mcp.gentility.test")))
        XCTAssertEqual(states.last, .failed(.denied))
        XCTAssertEqual(claims.names, ["Morgan's iPhone"])
    }

    func testAllowedSavesTheBearerBeforeTheNodeJoins() async throws {
        let store = MemoryStore()
        var savedWhenJoined: Credential?
        let joined = expectation(description: "Join ran")
        let claims = FakeClaims(reply: .success(reply()), answers: [try granted()])
        // A wait no test reaches: begin returns because Join finished.
        await flow(store, claims, join: { _ in savedWhenJoined = try? store.credential(); joined.fulfill() }, joinWait: .seconds(60))
            .begin(link, phoneName: "Morgan's iPhone")
        await fulfillment(of: [joined], timeout: 5)
        XCTAssertEqual(savedWhenJoined?.bearer, "bearer-1", "the keychain held the bearer before Join ran")
        XCTAssertEqual(savedWhenJoined?.enrollmentID, "e-1")
        XCTAssertEqual(savedWhenJoined?.origin.absoluteString, "https://mcp.gentility.test")
        XCTAssertNil(try store.pendingClaim(), "the poll secret is spent")
        XCTAssertTrue(states.contains(.joining(machineLabel: "Morgan's MacBook Pro")))
        XCTAssertEqual(states.last, .paired)
    }

    func testAJoinThatTakesLongerThanFiveSecondsDoesNotHoldHome() async throws {
        let claims = FakeClaims(reply: .success(reply()), answers: [try granted()])
        let pairing = flow(MemoryStore(), claims, join: { _ in try? await Task.sleep(for: .seconds(60)) })
        await pairing.begin(link, phoneName: "Morgan's iPhone")
        XCTAssertEqual(pairing.state, .paired)
    }

    func testABearerTheKeychainRefusesIsNotPairedAndSaysSo() async throws {
        let store = MemoryStore()
        store.failSaves = true
        var joined = false
        let claims = FakeClaims(reply: .success(reply()), answers: [try granted()])
        await flow(store, claims, join: { _ in joined = true }).begin(link, phoneName: "Morgan's iPhone")
        XCTAssertFalse(joined)
        XCTAssertEqual(states.last, .failed(.cannotJoin("this_phone")))
    }

    /// Review focus 1: killed between the allowed poll and the keychain
    /// write. Relaunch finds the poll secret; the server says `spent`.
    func testAKillBeforeTheBearerWasSavedSaysSpentOnRelaunch() async throws {
        let store = MemoryStore(pending: pending())
        await flow(store, FakeClaims(reply: .success(reply()), answers: [.spent])).resume()
        XCTAssertEqual(states.last, .failed(.codeSpent))
        XCTAssertNil(try store.pendingClaim())
    }

    func testAKillWhileTheMacWasAskingResumesTheNumberScreen() async throws {
        let store = MemoryStore(pending: pending())
        await flow(store, FakeClaims(reply: .success(reply()), answers: [.waiting("asking"), try granted()])).resume()
        XCTAssertTrue(states.contains(.number(machineLabel: "Morgan's MacBook Pro", match: "47", host: "mcp.gentility.test")))
        XCTAssertEqual(states.last, .paired)
        XCTAssertEqual(try store.credential()?.bearer, "bearer-1")
    }

    /// Killed after the bearer was kept and before the claim was cleared (or
    /// the clear failed): the claim is spent, and polling it would say so and
    /// send the person to Scan again, which wipes a good bearer.
    func testALaunchThatFindsTheBearerKeptDoesNotPollItsClaimAgain() async throws {
        let kept = Credential(origin: link.origin, bearer: "bearer-1", expiresAt: .distantFuture, enrollmentJSON: #"{"enrollment_id":"e-1"}"#, enrollmentID: "e-1", phoneName: "Morgan's iPhone")
        let store = MemoryStore(credential: kept, pending: pending())
        let claims = FakeClaims(reply: .success(reply()), answers: [.spent])
        await flow(store, claims).resume()
        XCTAssertEqual(claims.polls, 0)
        XCTAssertNil(try store.pendingClaim())
        XCTAssertEqual(try store.credential(), kept, "the bearer and its single-use enrolment are kept")
        XCTAssertEqual(states.last, .paired)
        XCTAssertFalse(store.log.contains("wipe"))
    }

    func testResetCallsOffARunningPoll() async throws {
        let store = MemoryStore()
        let claims = FakeClaims(reply: .success(reply()))
        let pairing = flow(store, claims)
        let running = Task { await pairing.begin(link, phoneName: "n") }
        while claims.polls < 3 { await Task.yield() }
        pairing.reset()
        await running.value
        let polls = claims.polls
        for _ in 0..<20 { await Task.yield() }
        XCTAssertEqual(claims.polls, polls, "no poll after the reset")
        XCTAssertEqual(pairing.state, .scanning)
    }

    func testASecondScanCallsOffTheFirstsPoll() async throws {
        let store = MemoryStore()
        let claims = FakeClaims(reply: .success(reply()))
        let pairing = flow(store, claims)
        let first = Task { await pairing.begin(link, phoneName: "first") }
        while claims.polls < 3 { await Task.yield() }
        claims.answers = [.denied]
        await pairing.begin(link, phoneName: "second")
        await first.value
        XCTAssertEqual(claims.names, ["first", "second"])
        XCTAssertEqual(pairing.state, .failed(.denied), "the first loop changed nothing after it was called off")
        XCTAssertEqual(store.log.suffix(3), ["wipe", "savePending", "clearPending"])
        XCTAssertNil(try store.pendingClaim())
    }

    func testALaunchWithNoClaimPollsNothing() async {
        let claims = FakeClaims(reply: .success(reply()))
        await flow(MemoryStore(), claims).resume()
        XCTAssertEqual(states.last, .scanning)
        XCTAssertEqual(claims.polls, 0)
    }

    /// A poll secret kept by an older build in a shape this one cannot read
    /// would refuse every launch: it is forgotten, and the phone pairs again.
    func testAPendingClaimThatNoLongerReadsIsForgottenOnLaunch() async throws {
        let store = MemoryStore(pending: pending())
        store.unreadable = true
        let claims = FakeClaims(reply: .success(reply()))
        await flow(store, claims).resume()
        XCTAssertEqual(states.last, .scanning)
        XCTAssertEqual(claims.polls, 0)
        XCTAssertEqual(store.log, ["wipe"])
        XCTAssertNil(try store.pendingClaim())
    }

    /// Review focus 2: a second QR from another deployment.
    func testClaimingFromAnotherOriginLeavesNothingOfTheLast() async throws {
        let earlier = Credential(origin: URL(string: "https://staging.gentility.test")!, bearer: "old", expiresAt: .distantFuture, enrollmentJSON: nil, enrollmentID: nil, phoneName: "Morgan's iPhone")
        let store = MemoryStore(credential: earlier)
        try directory.prepare()
        try Data("old node".utf8).write(to: directory.url.appendingPathComponent("tailscaled.state"))
        var origins: [URL] = []
        await flow(store, FakeClaims(reply: .success(reply()), answers: [.denied]), origins: { origins.append($0) }).begin(link, phoneName: "Morgan's iPhone")
        XCTAssertEqual(store.log.first, "wipe")
        XCTAssertNil(try store.credential())
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
        XCTAssertEqual(Set(origins.map(\.absoluteString)), ["https://mcp.gentility.test"])
    }

    func testEachEndingLandsOnItsScreen() async {
        let endings: [(PollAnswer, Failure)] = [
            (.denied, .denied), (.spent, .codeSpent), (.expired, .codeSpent), (.unknown, .codeSpent),
            (.failed("no_gateway"), .cannotJoin("no_gateway")), (.failed("blank_name"), .cannotJoin("blank_name")),
        ]
        for (answer, failure) in endings {
            await flow(MemoryStore(), FakeClaims(reply: .success(reply()), answers: [answer])).begin(link, phoneName: "n")
            XCTAssertEqual(states.last, .failed(failure), "\(answer)")
        }
    }

    func testARefusedClaimAndAnUnreachableServerLandOnTheirScreens() async {
        await flow(MemoryStore(), FakeClaims(reply: .failure(ClaimRefusal.expired))).begin(link, phoneName: "n")
        XCTAssertEqual(states.last, .failed(.codeSpent))
        await flow(MemoryStore(), FakeClaims(reply: .failure(ClaimRefusal.malformed))).begin(link, phoneName: "n")
        XCTAssertEqual(states.last, .failed(.cannotJoin("server_error")), "a 202 this phone cannot read")
        await flow(MemoryStore(), FakeClaims(reply: .failure(URLError(.notConnectedToInternet)))).begin(link, phoneName: "n")
        XCTAssertEqual(states.last, .failed(.unreachable(host: "mcp.gentility.test")))
    }

    func testAClaimPastItsExpiryStopsPolling() async {
        let claims = FakeClaims(reply: .success(reply(expiresAt: "2026-01-01T00:05:00.000000Z")))
        await flow(MemoryStore(), claims, now: { WireDate.parse("2026-01-01T00:06:00Z")! }).begin(link, phoneName: "n")
        XCTAssertEqual(states.last, .failed(.codeSpent))
        XCTAssertEqual(claims.polls, 0)
    }

    // F5: the poll is asked again only when it got no answer or a 5xx, and
    // never past the code's expiry.

    func testAPollThatGotNoAnswerIsAskedAgain() async throws {
        let store = MemoryStore()
        let claims = FakeClaims(reply: .success(reply()), answers: [try granted()])
        claims.pollFailures = [PollUnanswered(reason: "no answer"), PollUnanswered(reason: "HTTP 503")]
        await flow(store, claims).begin(link, phoneName: "n")
        XCTAssertEqual(claims.polls, 3)
        XCTAssertEqual(states.last, .paired)
    }

    func testAPollRefusalEndsThePairing() async throws {
        let store = MemoryStore()
        let claims = FakeClaims(reply: .success(reply()))
        claims.pollFailures = [ClaimRefusal.other("redirected")]
        await flow(store, claims).begin(link, phoneName: "n")
        XCTAssertEqual(claims.polls, 1, "a refusal is not asked again")
        XCTAssertEqual(states.last, .failed(.cannotJoin("server_error")))
        XCTAssertNil(try store.pendingClaim())
    }

    func testPollingStopsAtTheCodesExpiry() async {
        let clock = SleepClock(WireDate.parse("2026-01-01T00:00:00Z")!)
        let claims = FakeClaims(reply: .success(reply(expiresAt: "2026-01-01T00:00:10.000000Z")))
        claims.pollFailures = [PollUnanswered(reason: "no answer")]
        await flow(MemoryStore(), claims, now: { clock.now }, sleep: { clock.sleep($0) }).begin(link, phoneName: "n")
        XCTAssertEqual(claims.polls, 5, "at 0, 2, 4, 6 and 8 s; at 10 s the code is dead")
        XCTAssertEqual(states.last, .failed(.codeSpent))
    }

    func testAnExpiryThatDoesNotReadStopsPollingAfterFiveMinutes() async {
        let clock = SleepClock(WireDate.parse("2026-01-01T00:00:00Z")!)
        let claims = FakeClaims(reply: .success(reply(expiresAt: "five minutes from now")))
        await flow(MemoryStore(), claims, now: { clock.now }, sleep: { clock.sleep($0) }).begin(link, phoneName: "n")
        XCTAssertEqual(claims.polls, 150, "every 2 s for 300 s")
        XCTAssertEqual(states.last, .failed(.codeSpent))
    }
}
