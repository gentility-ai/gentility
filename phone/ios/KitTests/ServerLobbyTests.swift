import XCTest
@testable import DeskPhoneKit

final class ServerLobbyTests: XCTestCase {
    private let credential = Credential(origin: URL(string: "https://mcp.gentility.test")!, bearer: "bearer-1", expiresAt: .distantFuture, enrollmentJSON: nil, enrollmentID: "e-1", phoneName: "Morgan's iPhone")

    override func setUp() {
        StubURLProtocol.reset()
    }

    override func tearDown() {
        StubURLProtocol.reset()
    }

    func testOrganisationsAskTheOriginWithTheBearer() async throws {
        StubURLProtocol.answer = { _ in (200, #"{"organizations":[{"id":"o-1","slug":"northwind","name":"Northwind"}],"person":{"id":"u-morgan","email":"morgan@example.com","name":"Morgan"}}"#) }
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { FakeTransport() })
        let organizations = try await lobby.organizations()
        XCTAssertEqual(organizations.organizations.first?.id, "o-1")
        XCTAssertEqual(organizations.person.id, "u-morgan")
        let request = try XCTUnwrap(StubURLProtocol.seen.first?.request)
        XCTAssertEqual(request.url?.absoluteString, "https://mcp.gentility.test/oauth/organizations")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer bearer-1")
    }

    func testA401IsUnauthorized() async {
        StubURLProtocol.answer = { _ in (401, #"{"error":"invalid_token"}"#) }
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { FakeTransport() })
        do {
            _ = try await lobby.organizations()
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? LobbyError, .unauthorized)
        }
    }

    /// F4: the bearer rides this request's header, so a redirect would hand
    /// it to wherever the redirect points. None is followed, whatever
    /// session the lobby was handed.
    func testARedirectedOrganisationsIsRefusedAndTheBearerGoesNowhereElse() async {
        StubURLProtocol.answer = { request in request.url?.host == "elsewhere.test" ? (200, "{}") : (307, "") }
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { FakeTransport() })
        do {
            _ = try await lobby.organizations()
            XCTFail("a redirect was followed")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("redirected"))
        }
        XCTAssertEqual(StubURLProtocol.seen.map { $0.request.url?.host }, ["mcp.gentility.test"])
    }

    func testNoAnswerIsUnreachableAndAnUnreadableAnswerIsRefused() async {
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { FakeTransport() })
        StubURLProtocol.fails = URLError(.notConnectedToInternet)
        do {
            _ = try await lobby.organizations()
            XCTFail("answered")
        } catch {
            guard case .unreachable = error as? LobbyError else { return XCTFail("\(error)") }
        }
        StubURLProtocol.fails = nil
        StubURLProtocol.answer = { _ in (200, #"{"organizations":"none"}"#) }
        do {
            _ = try await lobby.organizations()
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("unreadable organizations"))
        }
    }

    func testInboxSendsOneFieldAndReadsWhatTheServerTook() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        let until = Date(timeIntervalSince1970: 1_800_000_000.25)
        async let snoozed = lobby.inbox(organizationID: "o-1", id: "s-1", field: .snoozedUntil, value: until)
        let join = await transport.nextSent()
        transport.reply(to: join, try Fixture.response("work_sessions/join"))
        let ask = await transport.nextSent()
        XCTAssertEqual(ask.topic, "work_sessions:o-1")
        XCTAssertEqual(ask.event, "inbox")
        XCTAssertEqual(ask.payload["session_id"], .string("s-1"))
        XCTAssertEqual(ask.payload["changes"], .object(["snoozed_until": .string("2027-01-15T08:00:00.250Z")]))
        XCTAssertNotNil(ask.payload["op_id"]?.string.flatMap(UUID.init(uuidString:)))
        XCTAssertNotNil(ask.payload["at"]?.string.flatMap(WireDate.parse))
        transport.reply(to: ask, try Fixture.response("work_sessions/inbox_stale"))
        let reply = try await snoozed
        XCTAssertEqual(reply, InboxReply(applied: [], stale: ["rank"]))

        async let cleared = lobby.inbox(organizationID: "o-1", id: "s-1", field: .archivedAt, value: nil)
        let again = await transport.nextSent()
        XCTAssertEqual(again.payload["changes"], .object(["archived_at": .null]))
        XCTAssertNotEqual(again.payload["op_id"], ask.payload["op_id"], "each command is its own operation")
        transport.reply(to: again, status: "error", try Fixture.response("work_sessions/inbox_attention_pending"))
        do {
            _ = try await cleared
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("attention_pending"))
        }
    }

    func testInboxIsNotAskedOfAServerWithoutIt() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        async let settled = lobby.inbox(organizationID: "o-1", id: "s-1", field: .settledAt, value: Date())
        let join = await transport.nextSent()
        transport.reply(to: join, .object(["features": .array([.string("lease")])]))
        do {
            _ = try await settled
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("inbox_unsupported"))
        }
    }

    func testAListJoinsTheOrganisationsLobbyOnceAndAsksOverIt() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        async let first = lobby.list(organizationID: "o-1", since: nil)
        let join = await transport.nextSent()
        XCTAssertEqual(join.topic, "work_sessions:o-1")
        transport.reply(to: join, try Fixture.response("work_sessions/join"))
        let list = await transport.nextSent()
        XCTAssertEqual(list.event, "list")
        XCTAssertEqual(list.payload, .object([:]))
        transport.reply(to: list, try Fixture.response("work_sessions/list"))
        let listed = try await first
        XCTAssertEqual(listed.sessions.count, 1)
        XCTAssertTrue(transport.opened.first?.absoluteString.hasPrefix("wss://mcp.gentility.test/socket/websocket?vsn=2.0.0&bearer=") == true)

        async let second = lobby.list(organizationID: "o-1", since: "2026-01-01T00:00:00.000000Z")
        let again = await transport.nextSent()
        XCTAssertEqual(again.event, "list", "no second join")
        XCTAssertEqual(again.payload["since"]?.string, "2026-01-01T00:00:00.000000Z")
        transport.reply(to: again, try Fixture.response("work_sessions/list_since"))
        _ = try await second

        async let machines = lobby.machines(organizationID: "o-1")
        let asked = await transport.nextSent()
        XCTAssertEqual(asked.event, "machines", "the same channel")
        XCTAssertEqual(asked.joinRef, join.joinRef)
        transport.reply(to: asked, try Fixture.response("work_sessions/machines"))
        let reply = try await machines
        XCTAssertEqual(reply.machines.map(\.deskMachineID), ["00000000-0000-7000-8000-000000000001", nil])
        XCTAssertEqual(transport.opened.count, 1, "one socket")
    }

    func testTwoAsksAtOnceShareOneSocketAndOneJoin() async throws {
        let transport = FakeTransport()
        let made = Made([transport])
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { made.next() })
        async let list = lobby.list(organizationID: "o-1", since: nil)
        async let machines = lobby.machines(organizationID: "o-1")
        let join = await transport.nextSent()
        XCTAssertEqual(join.event, "phx_join")
        transport.reply(to: join, .object([:]))
        var asked: [String: PhoenixFrame] = [:]
        for _ in 0..<2 {
            let frame = await transport.nextSent()
            asked[frame.event] = frame
        }
        XCTAssertEqual(Set(asked.keys), ["list", "machines"], "no second join")
        transport.reply(to: asked["list"]!, try Fixture.response("work_sessions/list"))
        transport.reply(to: asked["machines"]!, try Fixture.response("work_sessions/machines"))
        _ = try await (list, machines)
        XCTAssertEqual(made.count, 1)
    }

    /// F10: the server ended the topic's channel (a crash, a deploy) while
    /// the socket stayed open. The next ask joins it again.
    func testAChannelTheServerEndedIsJoinedAgainBeforeTheNextAsk() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        let join = try await listed(lobby, on: transport)
        transport.end(join, with: "phx_error", .object(["reason": .string("channel_crash")]))
        try await until { await !lobby.isJoined("work_sessions:o-1") }

        async let again = lobby.list(organizationID: "o-1", since: nil)
        let rejoin = await transport.nextSent()
        XCTAssertEqual(rejoin.event, "phx_join", "the gone channel is joined again, not pushed into")
        XCTAssertEqual(rejoin.topic, "work_sessions:o-1")
        XCTAssertNotEqual(rejoin.joinRef, join.joinRef)
        transport.reply(to: rejoin, .object([:]))
        let list = await transport.nextSent()
        XCTAssertEqual(list.event, "list")
        XCTAssertEqual(list.joinRef, rejoin.joinRef)
        transport.reply(to: list, try Fixture.response("work_sessions/list"))
        _ = try await again
        XCTAssertEqual(transport.opened.count, 1, "the socket was kept")
    }

    /// F10: a channel that ends while an ask waits on it fails the ask with
    /// `.closed`; the socket is still open, so the topic is joined again and
    /// the ask made again, once.
    func testAChannelThatEndsUnderAnAskIsJoinedAgainAndAskedOnce() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        let join = try await listed(lobby, on: transport)

        async let machines = lobby.machines(organizationID: "o-1")
        let first = await transport.nextSent()
        XCTAssertEqual(first.event, "machines")
        transport.end(first, with: "phx_close")
        let rejoin = await transport.nextSent()
        XCTAssertEqual(rejoin.event, "phx_join")
        XCTAssertNotEqual(rejoin.joinRef, join.joinRef)
        transport.reply(to: rejoin, .object([:]))
        let second = await transport.nextSent()
        XCTAssertEqual(second.event, "machines")
        XCTAssertEqual(second.joinRef, rejoin.joinRef)
        transport.reply(to: second, try Fixture.response("work_sessions/machines"))
        let reply = try await machines
        XCTAssertEqual(reply.machines.count, 2)
    }

    func testAChannelThatEndsTwiceUnderOneAskIsUnreachable() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        _ = try await listed(lobby, on: transport)

        async let machines = lobby.machines(organizationID: "o-1")
        transport.end(await transport.nextSent(), with: "phx_error")
        let rejoin = await transport.nextSent()
        transport.reply(to: rejoin, .object([:]))
        transport.end(await transport.nextSent(), with: "phx_error")
        do {
            _ = try await machines
            XCTFail("asked a third time")
        } catch {
            XCTAssertEqual(error as? LobbyError, .unreachable("closed"))
        }
    }

    func testAClosedSocketIsMadeAgainForTheNextAsk() async throws {
        let transports = [FakeTransport(), FakeTransport()]
        let made = Made(transports)
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { made.next() })
        _ = try await listed(lobby, on: transports[0])
        transports[0].close()
        try await until { await !lobby.isConnected }

        _ = try await listed(lobby, on: transports[1])
        XCTAssertEqual(made.count, 2)
    }

    func testARefusedJoinIsRefusedWithItsReason() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        async let registered = lobby.register(personID: "u-1", name: "n", enrollmentID: nil, pushToken: nil, environment: nil)
        let join = await transport.nextSent()
        transport.reply(to: join, status: "error", try Fixture.response("phone/join_unauthorized"))
        let answered = await registered
        XCTAssertFalse(answered, "a refused register says so, to be tried again")

        async let list = lobby.list(organizationID: "o-1", since: nil)
        let other = await transport.nextSent()
        transport.reply(to: other, status: "error", .object(["reason": .string("unauthorized")]))
        do {
            _ = try await list
            XCTFail("listed")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("unauthorized"))
        }
    }

    /// A 403 to the upgrade is a refusal, not `unauthorized`: a server bug or
    /// a missing route says 403 too, and only `/oauth/organizations`' 401
    /// ends the pairing.
    func testARefusedSocketIsRefusedAndItsTransportClosed() async {
        let transport = FakeTransport()
        transport.refusal = 403
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        do {
            _ = try await lobby.list(organizationID: "o-1", since: nil)
            XCTFail("listed")
        } catch {
            XCTAssertEqual(error as? LobbyError, .refused("socket 403"))
        }
        XCTAssertEqual(transport.closes, 1)
    }

    func testRegisterJoinsThePersonsPhoneTopicAndNamesThisPhone() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        async let registered = lobby.register(personID: "u-1", name: "Morgan's iPhone", enrollmentID: "e-1", pushToken: nil, environment: nil)
        let join = await transport.nextSent()
        XCTAssertEqual(join.topic, "phone:u-1")
        transport.reply(to: join, try Fixture.response("phone/join"))
        let push = await transport.nextSent()
        XCTAssertEqual(push.event, "register")
        XCTAssertEqual(push.payload["platform"]?.string, "ios")
        XCTAssertEqual(push.payload["name"]?.string, "Morgan's iPhone")
        XCTAssertEqual(push.payload["enrollment_id"]?.string, "e-1")
        transport.reply(to: push, try Fixture.response("phone/register"))
        let answered = await registered
        XCTAssertTrue(answered)
    }

    func testRegisterCarriesThePushTokenAndEnvironmentAsTheRecordingHasThem() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        let recording = try Fixture.server("phone/register")
        guard case .object(let recorded)? = recording["payload"] else { return XCTFail("the recording's payload is not an object") }
        let token = try XCTUnwrap(recorded["push_token"]?.string)
        let environment = try XCTUnwrap(recorded["environment"]?.string, "the recorded push registration includes environment")
        let name = try XCTUnwrap(recorded["name"]?.string)
        async let registered = lobby.register(personID: "u-1", name: name, enrollmentID: nil, pushToken: token, environment: environment)
        let join = await transport.nextSent()
        XCTAssertEqual(join.topic, "phone:u-1")
        transport.reply(to: join, try Fixture.response("phone/join"))
        let push = await transport.nextSent()
        XCTAssertEqual(push.event, recording["event"]?.string)
        XCTAssertEqual(push.payload, .object(recorded), "the payload the server's recording sent, key for key")
        transport.reply(to: push, try Fixture.response("phone/register"))
        let answered = await registered
        XCTAssertTrue(answered)
    }

    /// The environment names the token's APNs host: with no token, it is
    /// not sent either.
    func testRegisterWithoutATokenSendsNeither() async throws {
        let transport = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), forcePhoenix: true, makeTransport: { transport })
        async let registered = lobby.register(personID: "u-1", name: "n", enrollmentID: nil, pushToken: nil, environment: "sandbox")
        let join = await transport.nextSent()
        transport.reply(to: join, try Fixture.response("phone/join"))
        let push = await transport.nextSent()
        XCTAssertNil(push.payload["push_token"])
        XCTAssertNil(push.payload["environment"])
        transport.reply(to: push, try Fixture.response("phone/register"))
        _ = await registered
    }

    /// Lists `o-1` once over `transport`, joining it, and returns the join.
    private func listed(_ lobby: ServerLobby, on transport: FakeTransport) async throws -> PhoenixFrame {
        async let listed = lobby.list(organizationID: "o-1", since: nil)
        let join = await transport.nextSent()
        XCTAssertEqual(join.event, "phx_join")
        transport.reply(to: join, .object([:]))
        let list = await transport.nextSent()
        XCTAssertEqual(list.event, "list")
        transport.reply(to: list, try Fixture.response("work_sessions/list"))
        _ = try await listed
        return join
    }

    private func until(_ condition: () async -> Bool) async throws {
        for _ in 0..<400 {
            if await condition() { return }
            try await Task.sleep(for: .milliseconds(5))
        }
        XCTFail("never")
    }
}

/// Hands out the test's transports in turn, and counts them.
final class Made: @unchecked Sendable {
    private let lock = NSLock()
    private let transports: [FakeTransport]
    private var made = 0

    init(_ transports: [FakeTransport]) {
        self.transports = transports
    }

    var count: Int { lock.withLock { made } }

    func next() -> WebSocketTransport {
        lock.withLock {
            defer { made += 1 }
            return transports[min(made, transports.count - 1)]
        }
    }
}
