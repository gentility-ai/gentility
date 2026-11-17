import XCTest
@testable import DeskPhoneKit

final class PhoenixSocketTests: XCTestCase {
    private var transport: FakeTransport!
    private var socket: PhoenixSocket!

    override func setUp() async throws {
        transport = FakeTransport()
        socket = PhoenixSocket(
            transport: transport, url: URL(string: "wss://mcp.gentility.test/socket/websocket?vsn=2.0.0")!,
            heartbeatEvery: .seconds(3600), replyWithin: .seconds(5)
        )
        try await socket.connect()
    }

    override func tearDown() async throws {
        await socket.disconnect()
    }

    func testTheURLIsTheOriginsSocketWithTheBearerEscaped() {
        let url = PhoenixSocket.url(origin: URL(string: "https://mcp.gentility.test")!, bearer: "a+b/c=d e")
        XCTAssertEqual(url.absoluteString, "wss://mcp.gentility.test/socket/websocket?vsn=2.0.0&bearer=a%2Bb%2Fc%3Dd%20e")
        let local = PhoenixSocket.url(origin: URL(string: "http://mcp.wt.localhost:4102")!, bearer: "t")
        XCTAssertEqual(local.absoluteString, "ws://mcp.wt.localhost:4102/socket/websocket?vsn=2.0.0&bearer=t")
    }

    func testABearerOutsideASCIIIsEscapedToo() {
        let url = PhoenixSocket.url(origin: URL(string: "https://mcp.gentility.test")!, bearer: "é1")
        XCTAssertEqual(url.query, "vsn=2.0.0&bearer=%C3%A91")
    }

    func testTheSocketNeverPrintsItsURL() {
        let bearing = PhoenixSocket(transport: FakeTransport(), url: PhoenixSocket.url(origin: URL(string: "https://mcp.gentility.test")!, bearer: "s3cret"))
        var dumped = ""
        dump(bearing, to: &dumped)
        for printed in [String(describing: bearing), String(reflecting: bearing), dumped] {
            XCTAssertFalse(printed.contains("s3cret"), printed)
            XCTAssertFalse(printed.contains("bearer="), printed)
        }
    }

    func testAJoinIsAnsweredWithTheRecordedReply() async throws {
        async let joined = socket.join("work_sessions:o-1")
        let frame = await transport.nextSent()
        XCTAssertEqual(frame.event, "phx_join")
        XCTAssertEqual(frame.topic, "work_sessions:o-1")
        XCTAssertEqual(frame.joinRef, frame.ref)
        XCTAssertEqual(frame.payload, .object([:]), "no desk block: the phone is not a Desk")
        let recorded = try Fixture.response("work_sessions/join")
        transport.reply(to: frame, recorded)
        let reply = try await joined
        XCTAssertEqual(reply, recorded)
        let isJoined = await socket.isJoined("work_sessions:o-1")
        XCTAssertTrue(isJoined)
    }

    func testAPushCarriesItsJoinsRefAndAFreshRef() async throws {
        let join = try await joined("work_sessions:o-1")

        async let listed = socket.push("work_sessions:o-1", "list", .object(["since": .string("2026-01-01T00:00:00.000000Z")]))
        let push = await transport.nextSent()
        XCTAssertEqual(push.joinRef, join.joinRef)
        XCTAssertNotEqual(push.ref, join.ref)
        XCTAssertEqual(push.event, "list")
        XCTAssertEqual(push.payload["since"]?.string, "2026-01-01T00:00:00.000000Z")
        transport.reply(to: push, try Fixture.response("work_sessions/list"))
        let list: LobbyList = try await listed.decode()
        XCTAssertEqual(list.sessions.count, 1)
    }

    func testAnErrorReplyToAPushIsTheServersResponse() async throws {
        _ = try await joined("work_sessions:o-1")
        async let listed = socket.push("work_sessions:o-1", "list", .object(["since": .string("yesterday")]))
        let push = await transport.nextSent()
        let recorded = try Fixture.response("work_sessions/list_since_malformed")
        transport.reply(to: push, status: "error", recorded)
        do {
            _ = try await listed
            XCTFail("listed")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .replied(recorded))
        }
        let isJoined = await socket.isJoined("work_sessions:o-1")
        XCTAssertTrue(isJoined, "a refused push leaves the channel joined")
    }

    func testARefusedJoinSaysWhy() async throws {
        async let joined = socket.join("phone:u-1")
        let frame = await transport.nextSent()
        transport.reply(to: frame, status: "error", try Fixture.response("phone/join_unauthorized"))
        do {
            _ = try await joined
            XCTFail("joined")
        } catch PhoenixError.joinRefused(let response) {
            XCTAssertEqual(response["reason"]?.string, "unauthorized")
        }
        let isJoined = await socket.isJoined("phone:u-1")
        XCTAssertFalse(isJoined)
    }

    func testAPushToATopicNeverJoinedIsRefusedWithoutSending() async {
        do {
            _ = try await socket.push("work_sessions:o-9", "list")
            XCTFail("pushed")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }

    func testAPushWhileItsJoinIsUnansweredIsRefused() async throws {
        async let joining = socket.join("work_sessions:o-1")
        let join = await transport.nextSent()
        do {
            _ = try await socket.push("work_sessions:o-1", "list")
            XCTFail("pushed")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
        transport.reply(to: join, .object([:]))
        _ = try await joining
    }

    func testAPushTheServerMakesArrivesOnPushes() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        let join = try await joined("work_sessions:o-1")
        transport.deliver(.array([join.joinRef.map(JSONValue.string)!, .null, .string("work_sessions:o-1"), .string("changed"), .object([:])]))
        let push = await pushes.next()
        XCTAssertEqual(push, PhoenixPush(topic: "work_sessions:o-1", event: "changed", payload: .object([:])))
        XCTAssertEqual(push?.closesChannel, false)
    }

    func testWhatIsNotAFrameIsSkippedAndTheSocketReadsOn() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        transport.deliver(text: "not json")
        transport.deliver(.object(["topic": .string("work_sessions:o-1")]))
        transport.deliver(.array([.null, .null, .string("work_sessions:o-1"), .string("changed"), .object([:])]))
        let push = await pushes.next()
        XCTAssertEqual(push?.event, "changed")
        let open = await socket.isOpen
        XCTAssertTrue(open)
    }

    func testAReplyNobodyWaitsForIsNotAPush() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        transport.deliver(.array([.string("7"), .string("7"), .string("work_sessions:o-1"), .string("phx_reply"), .object(["status": .string("ok"), "response": .object([:])])]))
        transport.deliver(.array([.null, .null, .string("work_sessions:o-1"), .string("changed"), .object([:])]))
        let push = await pushes.next()
        XCTAssertEqual(push?.event, "changed")
    }

    func testAChannelTheServerEndsIsSaidAndCanBeJoinedAgain() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        for event in ["phx_error", "phx_close"] {
            let topic = "work_sessions:\(event)"
            let join = try await joined(topic)

            transport.end(join, with: event, event == "phx_error" ? .object(["reason": .string("channel_crash")]) : .object([:]))
            let said = await pushes.next()
            XCTAssertEqual(said?.topic, topic)
            XCTAssertEqual(said?.event, event)
            XCTAssertEqual(said?.closesChannel, true)
            let isJoined = await socket.isJoined(topic)
            XCTAssertFalse(isJoined, event)
            let open = await socket.isOpen
            XCTAssertTrue(open, "the socket outlives its channel")

            do {
                _ = try await socket.push(topic, "list")
                XCTFail("pushed into a channel that has gone")
            } catch {
                XCTAssertEqual(error as? PhoenixError, .closed)
            }

            let again = try await joined(topic)
            XCTAssertEqual(again.event, "phx_join", "nothing was sent between the end and the join")
            XCTAssertNotEqual(again.joinRef, join.joinRef)
            async let listed = socket.push(topic, "list")
            let push = await transport.nextSent()
            XCTAssertEqual(push.joinRef, again.joinRef)
            transport.reply(to: push, .object(["ok": .bool(true)]))
            _ = try await listed
        }
    }

    func testAnEndForAJoinAlreadyReplacedIsDropped() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        let first = try await joined("work_sessions:o-1")
        let second = try await joined("work_sessions:o-1")
        XCTAssertNotEqual(first.joinRef, second.joinRef)

        // Phoenix closes the superseded channel after the new join is answered.
        transport.end(first, with: "phx_close")
        transport.deliver(.array([second.joinRef.map(JSONValue.string)!, .null, .string("work_sessions:o-1"), .string("changed"), .object([:])]))
        let next = await pushes.next()
        XCTAssertEqual(next?.event, "changed", "the stale close is not said")
        let isJoined = await socket.isJoined("work_sessions:o-1")
        XCTAssertTrue(isJoined)
    }

    /// Phoenix pushes a channel's own messages with its join_ref; one naming
    /// a join already replaced, or a topic not joined, is from a channel that
    /// has gone, and is dropped as its close would be. A broadcast names none
    /// and is taken.
    func testAPushFromAJoinAlreadyReplacedIsDropped() async throws {
        var pushes = socket.pushes.makeAsyncIterator()
        let first = try await joined("work_sessions:o-1")
        let second = try await joined("work_sessions:o-1")
        transport.deliver(.array([first.joinRef.map(JSONValue.string)!, .null, .string("work_sessions:o-1"), .string("stale"), .object([:])]))
        transport.deliver(.array([.string("99"), .null, .string("work_sessions:o-9"), .string("never_joined"), .object([:])]))
        transport.deliver(.array([second.joinRef.map(JSONValue.string)!, .null, .string("work_sessions:o-1"), .string("current"), .object([:])]))
        transport.deliver(.array([.null, .null, .string("work_sessions:o-1"), .string("broadcast"), .object([:])]))
        let said = [await pushes.next(), await pushes.next()].map { $0?.event }
        XCTAssertEqual(said, ["current", "broadcast"])
    }

    /// The server answers nothing waiting on a channel it ended: what waits
    /// on it is refused with `.closed` at once, not left to time out, while
    /// what waits on another channel is not.
    func testAnEndRefusesWhatWaitsOnThatChannelAlone() async throws {
        let ended = try await joined("work_sessions:o-1")
        let kept = try await joined("work_sessions:o-2")
        async let lost = socket.push("work_sessions:o-1", "list")
        let lostPush = await transport.nextSent()
        async let answered = socket.push("work_sessions:o-2", "list")
        let keptPush = await transport.nextSent()
        XCTAssertEqual(keptPush.joinRef, kept.joinRef)
        transport.end(ended, with: "phx_error")
        do {
            _ = try await lost
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
        XCTAssertEqual(lostPush.joinRef, ended.joinRef)
        transport.reply(to: keptPush, .object(["ok": .bool(true)]))
        let reply = try await answered
        XCTAssertEqual(reply["ok"]?.bool, true)
    }

    func testAClosedSocketFailsWhatIsWaitingAndEndsItsPushes() async throws {
        async let joined = socket.join("work_sessions:o-1")
        _ = await transport.nextSent()
        transport.close()
        do {
            _ = try await joined
            XCTFail("joined")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
        var pushes = socket.pushes.makeAsyncIterator()
        let after = await pushes.next()
        XCTAssertNil(after)
        let open = await socket.isOpen
        XCTAssertFalse(open)
        do {
            _ = try await socket.join("work_sessions:o-1")
            XCTFail("joined a closed socket")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }

    func testDisconnectIsClosedAtOnce() async throws {
        _ = try await joined("work_sessions:o-1")
        await socket.disconnect()
        let open = await socket.isOpen
        XCTAssertFalse(open)
        let isJoined = await socket.isJoined("work_sessions:o-1")
        XCTAssertFalse(isJoined)
        var pushes = socket.pushes.makeAsyncIterator()
        let after = await pushes.next()
        XCTAssertNil(after)
    }

    func testAnUnansweredPushTimesOut() async throws {
        let quick = PhoenixSocket(transport: FakeTransport(), url: URL(string: "wss://x/socket/websocket")!, heartbeatEvery: .seconds(3600), replyWithin: .milliseconds(100))
        try await quick.connect()
        do {
            _ = try await quick.join("work_sessions:o-1")
            XCTFail("joined")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .timeout)
        }
        let isJoined = await quick.isJoined("work_sessions:o-1")
        XCTAssertFalse(isJoined)
        let open = await quick.isOpen
        XCTAssertTrue(open, "one slow answer does not close the socket")
        await quick.disconnect()
    }

    func testTheHeartbeatRidesThePhoenixTopicWithNoJoinRef() async throws {
        let beating = FakeTransport()
        let socket = PhoenixSocket(transport: beating, url: URL(string: "wss://x/socket/websocket")!, heartbeatEvery: .milliseconds(50))
        try await socket.connect()
        let beat = await beating.nextSent()
        XCTAssertEqual(beat.topic, "phoenix")
        XCTAssertEqual(beat.event, "heartbeat")
        XCTAssertNil(beat.joinRef)
        XCTAssertNotNil(beat.ref)
        beating.reply(to: beat, .object([:]))
        let next = await beating.nextSent()
        XCTAssertEqual(next.event, "heartbeat")
        XCTAssertNotEqual(next.ref, beat.ref)
        await socket.disconnect()
    }

    func testAnUnansweredHeartbeatClosesTheSocket() async throws {
        let silent = FakeTransport()
        let socket = PhoenixSocket(transport: silent, url: URL(string: "wss://x/socket/websocket")!, heartbeatEvery: .milliseconds(20), heartbeatGrace: .milliseconds(50))
        try await socket.connect()
        var pushes = socket.pushes.makeAsyncIterator()
        let after = await pushes.next()
        XCTAssertNil(after, "the pushes end when the heartbeat gives up")
        let open = await socket.isOpen
        XCTAssertFalse(open)
    }

    func testARefusedUpgradeIsItsStatusAndClosesTheTransport() async {
        let refused = FakeTransport()
        refused.refusal = 403
        do {
            try await PhoenixSocket(transport: refused, url: URL(string: "wss://x/socket/websocket")!).connect()
            XCTFail("connected")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .refused(status: 403))
        }
        XCTAssertEqual(refused.closes, 1, "the transport's session is let go, not left open per retry")
    }

    func testAnUpgradeNeverAnsweredTimesOutAndClosesTheTransport() async {
        let silent = FakeTransport()
        silent.hangs = true
        let socket = PhoenixSocket(transport: silent, url: URL(string: "wss://x/socket/websocket")!, heartbeatEvery: .seconds(3600), replyWithin: .milliseconds(100))
        let started = ContinuousClock.now
        do {
            try await socket.connect()
            XCTFail("connected")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .timeout)
        }
        XCTAssertLessThan(ContinuousClock.now - started, .seconds(5))
        XCTAssertGreaterThanOrEqual(silent.closes, 1)
        let open = await socket.isOpen
        XCTAssertFalse(open)
        do {
            try await socket.connect()
            XCTFail("connected a socket that gave up")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }

    /// Joins `topic` with an empty reply and returns the join frame.
    private func joined(_ topic: String) async throws -> PhoenixFrame {
        async let joining = socket.join(topic)
        let join = await transport.nextSent()
        XCTAssertEqual(join.event, "phx_join")
        transport.reply(to: join, .object([:]))
        _ = try await joining
        return join
    }
}

final class URLSessionTransportTests: XCTestCase {
    func testTheUpgradeFollowsNoRedirect() {
        let transport = URLSessionTransport()
        let from = URL(string: "wss://mcp.gentility.test/socket/websocket?vsn=2.0.0&bearer=s3cret")!
        let to = URL(string: "wss://elsewhere.test/socket/websocket?vsn=2.0.0&bearer=s3cret")!
        let response = HTTPURLResponse(url: from, statusCode: 307, httpVersion: "HTTP/1.1", headerFields: ["Location": to.absoluteString])!
        let session = URLSession(configuration: .ephemeral)
        let task = session.webSocketTask(with: from)
        var followed: URLRequest? = URLRequest(url: to)
        transport.urlSession(session, task: task, willPerformHTTPRedirection: response, newRequest: URLRequest(url: to)) { followed = $0 }
        XCTAssertNil(followed)
        session.invalidateAndCancel()
    }

    func testAClosedTransportRefusesToOpen() async {
        let transport = URLSessionTransport()
        transport.close()
        do {
            try await transport.open(URL(string: "wss://mcp.gentility.test/socket/websocket")!)
            XCTFail("opened")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }

    func testAnUnopenedTransportRefusesToSendOrReceive() async {
        let transport = URLSessionTransport()
        do {
            try await transport.send("[]")
            XCTFail("sent")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
        do {
            _ = try await transport.receive()
            XCTFail("received")
        } catch {
            XCTAssertEqual(error as? PhoenixError, .closed)
        }
    }
}
