import XCTest
@testable import DeskPhoneKit

final class ServerRPCLobbyTests: XCTestCase {
    private let credential = Credential(origin: URL(string: "https://mcp.gentility.test")!, bearer: "private-token", expiresAt: .distantFuture, enrollmentJSON: nil, enrollmentID: nil, phoneName: "Phone")
    private let words = ["rpc_sessions", "rpc_hosts", "rpc_inbox", "rpc_phone", "inbox"]

    private func hello(_ transport: FakeTransport, words: [String]) async {
        let request = await transport.nextRPC()
        XCTAssertEqual(request["method"], .string("rpc.hello"))
        XCTAssertNil(request["params"]?["machine_id"])
        XCTAssertTrue(request["params"]?["build"]?.string?.hasPrefix("ios-") == true)
        transport.replyRPC(to: request, .object([
            "principal": .object(["kind": .string("person"), "user_id": .string("u-1"), "machine_id": .null]),
            "features": .array(words.map(JSONValue.string)), "orgs": .array([]),
        ]))
    }

    func testFourCallsUseRPCAndConcurrentReadsShareHello() async throws {
        let transport = FakeTransport()
        let legacy = FakeTransport()
        let lobby = ServerLobby(credential: credential, makeRPCTransport: { transport }, makeTransport: { legacy })
        async let listed = lobby.list(organizationID: "o-1", since: nil)
        async let machines = lobby.machines(organizationID: "o-1")
        await hello(transport, words: words)
        for _ in 0..<2 {
            let frame = await transport.nextRPC()
            XCTAssertEqual(frame["params"]?["org"], .string("o-1"))
            let fixture = frame["method"] == .string("sessions.list") ? "work_sessions/list" : "work_sessions/machines"
            transport.replyRPC(to: frame, try Fixture.response(fixture))
        }
        _ = try await (listed, machines)
        async let inbox = lobby.inbox(organizationID: "o-1", id: "s-1", field: .settledAt, value: nil)
        let command = await transport.nextRPC()
        XCTAssertEqual(command["method"], .string("inbox.command"))
        transport.replyRPC(to: command, try Fixture.response("work_sessions/inbox_stale"))
        _ = try await inbox
        async let registered = lobby.register(personID: "u-1", name: "Phone", enrollmentID: nil, pushToken: nil, environment: nil)
        let registration = await transport.nextRPC()
        XCTAssertEqual(registration["method"], .string("phone.register"))
        XCTAssertNil(registration["params"]?["org"])
        XCTAssertNil(registration["params"]?["user_id"])
        transport.replyRPC(to: registration, try Fixture.response("phone/register"))
        let ok = await registered
        XCTAssertTrue(ok)
        XCTAssertEqual(transport.opened.count, 1)
        XCTAssertTrue(legacy.opened.isEmpty)
    }

    func testEachMissingDomainUsesPhoenixIndependently() async throws {
        for missing in ["rpc_sessions", "rpc_hosts", "rpc_inbox", "rpc_phone"] {
            let rpc = FakeTransport()
            let phoenix = FakeTransport()
            let lobby = ServerLobby(credential: credential, makeRPCTransport: { rpc }, makeTransport: { phoenix })
            let call = Task {
                switch missing {
                case "rpc_sessions": _ = try await lobby.list(organizationID: "o-1", since: nil)
                case "rpc_hosts": _ = try await lobby.machines(organizationID: "o-1")
                case "rpc_inbox": _ = try await lobby.inbox(organizationID: "o-1", id: "s-1", field: .settledAt, value: nil)
                default:
                    let ok = await lobby.register(personID: "u-1", name: "Phone", enrollmentID: nil, pushToken: nil, environment: nil)
                    XCTAssertTrue(ok)
                }
            }
            await hello(rpc, words: words.filter { $0 != missing })
            let join = await phoenix.nextSent()
            XCTAssertNotNil(join.payload["desk"]?["build"])
            phoenix.reply(to: join, .object(["features": .array([.string("inbox")])]))
            let sent = await phoenix.nextSent()
            let fixtures = ["list": "work_sessions/list", "machines": "work_sessions/machines", "inbox": "work_sessions/inbox_stale", "register": "phone/register"]
            phoenix.reply(to: sent, try Fixture.response(fixtures[sent.event]!))
            try await call.value
            // A different domain on this same link still uses RPC.
            async let other = lobby.register(personID: "u-2", name: "Phone", enrollmentID: nil, pushToken: nil, environment: nil)
            if missing == "rpc_phone" {
                let secondJoin = await phoenix.nextSent()
                phoenix.reply(to: secondJoin, .object([:]))
                phoenix.reply(to: await phoenix.nextSent(), try Fixture.response("phone/register"))
            } else {
                rpc.replyRPC(to: await rpc.nextRPC(), try Fixture.response("phone/register"))
            }
            let ok = await other
            XCTAssertTrue(ok)
        }
    }

    func testAbsentEndpointFallsBackButAuthenticationDoesNot() async throws {
        let old = FakeTransport()
        old.refusal = 404
        let phoenix = FakeTransport()
        let lobby = ServerLobby(credential: credential, makeRPCTransport: { old }, makeTransport: { phoenix })
        async let listed = lobby.list(organizationID: "o-1", since: nil)
        phoenix.reply(to: await phoenix.nextSent(), .object([:]))
        phoenix.reply(to: await phoenix.nextSent(), try Fixture.response("work_sessions/list"))
        _ = try await listed
        let refused = FakeTransport()
        refused.refusal = 403
        let unused = FakeTransport()
        let denied = ServerLobby(credential: credential, http: StubURLProtocol.session(), makeRPCTransport: { refused }, makeTransport: { unused })
        for _ in 0..<2 {
            do { _ = try await denied.list(organizationID: "o-1", since: nil); XCTFail("accepted") }
            catch { XCTAssertEqual(error as? LobbyError, .refused("socket 403")) }
        }
        XCTAssertTrue(unused.opened.isEmpty)
        XCTAssertEqual(refused.opened.count, 2)
        StubURLProtocol.reset()
        defer { StubURLProtocol.reset() }
        StubURLProtocol.answer = { _ in (200, #"{"organizations":[],"person":{"id":"u-1","email":"person@example.test","name":"Person"}}"#) }
        let valid = try await denied.organizations()
        XCTAssertEqual(valid.person.id, "u-1", "an intermediary's 403 must not latch a valid credential as revoked")
    }

    func testLostRPCInboxIsNotReplayedOnPhoenix() async throws {
        let rpc = FakeTransport()
        let phoenix = FakeTransport()
        let lobby = ServerLobby(credential: credential, makeRPCTransport: { rpc }, makeTransport: { phoenix })
        async let command = lobby.inbox(organizationID: "o-1", id: "s-1", field: .settledAt, value: nil)
        await hello(rpc, words: words)
        let frame = await rpc.nextRPC()
        XCTAssertEqual(frame["method"], .string("inbox.command"))
        rpc.close()
        do { _ = try await command; XCTFail("answered") }
        catch { guard case LobbyError.unreachable = error else { return XCTFail("\(error)") } }
        XCTAssertTrue(phoenix.opened.isEmpty)
    }

    func testMissingWordPinSurvivesReconnectAndRestorationButIsScopedToOrganization() async throws {
        let first = FakeTransport()
        let second = FakeTransport()
        let made = Made([first, second])
        let phoenix = FakeTransport()
        let lobby = ServerLobby(credential: credential, makeRPCTransport: { made.next() as! RPCTransport }, makeTransport: { phoenix })
        async let initial = lobby.list(organizationID: "o-1", since: nil)
        await hello(first, words: words.filter { $0 != "rpc_sessions" })
        phoenix.reply(to: await phoenix.nextSent(), .object([:]))
        phoenix.reply(to: await phoenix.nextSent(), try Fixture.response("work_sessions/list"))
        _ = try await initial
        first.close()
        let deadline = ContinuousClock.now + .seconds(2)
        while await lobby.isRPCConnected, ContinuousClock.now < deadline { await Task.yield() }
        let stillOpen = await lobby.isRPCConnected
        XCTAssertFalse(stillOpen)
        async let machines = lobby.machines(organizationID: "o-1")
        await hello(second, words: words)
        second.replyRPC(to: await second.nextRPC(), try Fixture.response("work_sessions/machines"))
        _ = try await machines
        async let pinned = lobby.list(organizationID: "o-1", since: nil)
        let legacy = await phoenix.nextSent()
        XCTAssertEqual(legacy.event, "list")
        phoenix.reply(to: legacy, try Fixture.response("work_sessions/list"))
        _ = try await pinned
        async let other = lobby.list(organizationID: "o-2", since: nil)
        let modern = await second.nextRPC()
        XCTAssertEqual(modern["method"], .string("sessions.list"))
        XCTAssertEqual(modern["params"]?["org"], .string("o-2"))
        second.replyRPC(to: modern, try Fixture.response("work_sessions/list"))
        _ = try await other
        XCTAssertEqual(made.count, 2)
    }

    func testRevokedCredentialStopsPinnedPhoenixAndOrganizations() async throws {
        let rpc = FakeTransport()
        let phoenix = FakeTransport()
        let lobby = ServerLobby(credential: credential, makeRPCTransport: { rpc }, makeTransport: { phoenix })
        async let initial = lobby.list(organizationID: "o-1", since: nil)
        await hello(rpc, words: words.filter { $0 != "rpc_sessions" })
        phoenix.reply(to: await phoenix.nextSent(), .object([:]))
        phoenix.reply(to: await phoenix.nextSent(), try Fixture.response("work_sessions/list"))
        _ = try await initial
        async let pending = lobby.machines(organizationID: "o-1")
        _ = await rpc.nextRPC()
        rpc.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("rpc.revoked"),
                             "params": .object(["scope": .string("connection")])]))
        do { _ = try await pending; XCTFail("accepted") }
        catch { XCTAssertEqual(error as? LobbyError, .unauthorized) }
        do { _ = try await lobby.list(organizationID: "o-1", since: nil); XCTFail("used pin") }
        catch { XCTAssertEqual(error as? LobbyError, .unauthorized) }
        do { _ = try await lobby.organizations(); XCTFail("used HTTP") }
        catch { XCTAssertEqual(error as? LobbyError, .unauthorized) }
        XCTAssertEqual(rpc.opened.count, 1)
    }

    func testPhoneBuildCarriesRevisionAndTime() {
        XCTAssertTrue(PhoneBuild.stamp.hasPrefix("ios-"))
        XCTAssertNotEqual(PhoneBuild.stamp, "ios-unstamped")
        XCTAssertTrue(PhoneBuild.stamp.hasSuffix("Z"))
    }

    func testAuthenticatedOrganizationReadRestoresRevokedAccessOnTheSameLink() async throws {
        StubURLProtocol.reset()
        defer { StubURLProtocol.reset() }
        let rpc = FakeTransport()
        let lobby = ServerLobby(credential: credential, http: StubURLProtocol.session(), makeRPCTransport: { rpc })
        async let first = lobby.list(organizationID: "o-1", since: nil)
        await hello(rpc, words: words)
        _ = await rpc.nextRPC()
        rpc.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("rpc.revoked"),
                             "params": .object(["scope": .string("org"), "org": .string("o-1")])]))
        do { _ = try await first; XCTFail("accepted") }
        catch { XCTAssertEqual(error as? LobbyError, .refused("forbidden")) }
        StubURLProtocol.answer = { _ in (200, #"{"organizations":[],"person":{"id":"u-1","email":"person@example.test","name":"Person"}}"#) }
        _ = try await lobby.organizations()
        do { _ = try await lobby.list(organizationID: "o-1", since: nil); XCTFail("restored absent membership") }
        catch { XCTAssertEqual(error as? LobbyError, .refused("forbidden")) }
        StubURLProtocol.answer = { _ in (200, #"{"organizations":[{"id":"o-1","slug":"a","name":"A"}],"person":{"id":"u-1","email":"person@example.test","name":"Person"}}"#) }
        _ = try await lobby.organizations()
        async let restored = lobby.list(organizationID: "o-1", since: nil)
        let sent = await rpc.nextRPC()
        XCTAssertEqual(sent["method"], .string("sessions.list"))
        rpc.replyRPC(to: sent, try Fixture.response("work_sessions/list"))
        _ = try await restored
        XCTAssertEqual(rpc.opened.count, 1)
    }

    func testLostPhoenixInboxIsNotReplayedAfterChannelClose() async throws {
        let phoenix = FakeTransport()
        let lobby = ServerLobby(credential: credential, forcePhoenix: true, makeTransport: { phoenix })
        async let command = lobby.inbox(organizationID: "o-1", id: "s-1", field: .settledAt, value: nil)
        let join = await phoenix.nextSent()
        phoenix.reply(to: join, .object(["features": .array([.string("inbox")])]))
        let sent = await phoenix.nextSent()
        XCTAssertEqual(sent.event, "inbox")
        phoenix.end(join, with: "phx_close")
        do { _ = try await command; XCTFail("answered") }
        catch { XCTAssertEqual(error as? LobbyError, .unreachable("closed")) }
        // A following read must be the next application request, not a
        // replayed write; it can safely rejoin the closed channel.
        async let list = lobby.list(organizationID: "o-1", since: nil)
        phoenix.reply(to: await phoenix.nextSent(), .object([:]))
        let next = await phoenix.nextSent()
        XCTAssertEqual(next.event, "list")
        phoenix.reply(to: next, try Fixture.response("work_sessions/list"))
        _ = try await list
    }
}
