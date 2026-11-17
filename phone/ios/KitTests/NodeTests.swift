import XCTest
@testable import DeskPhoneKit

final class NodeTests: XCTestCase {
    private var bridge: FakeBridge!
    private var node: Node!

    override func setUp() {
        bridge = FakeBridge()
        node = Node(bridge: bridge)
    }

    // MARK: the node

    func testAJoinIsTheAddressAndNameGoAnswered() async throws {
        let joined = await node.join(dir: "/tmp/n", hostname: "morgan-s-iphone", enrollmentJSON: "{}")
        XCTAssertEqual(try joined.get().address, "fd06:6e7:0:2::9")
        XCTAssertEqual(try joined.get().name, "morgan-s-iphone")
        XCTAssertEqual(bridge.calls, ["join morgan-s-iphone"])
    }

    func testAnErrorAnswerIsARefusalInGosWordAndSentence() async {
        bridge.rejoinAnswer = #"{"error":"/tmp/n holds no node identity: pair this phone again","code":"not_enrolled"}"#
        let rejoined = await node.rejoin(dir: "/tmp/n", hostname: "h")
        XCTAssertEqual(rejoined, .failure(.refused(code: "not_enrolled", message: "/tmp/n holds no node identity: pair this phone again")))
    }

    func testAnErrorWithNoWordIsARefusalWithAnEmptyCode() async {
        bridge.joinAnswer = #"{"error":"something new"}"#
        let joined = await node.join(dir: "/tmp/n", hostname: "h", enrollmentJSON: "{}")
        XCTAssertEqual(joined, .failure(.refused(code: "", message: "something new")))
    }

    func testAnAnswerThatIsNotJSONIsUnreadable() async {
        bridge.joinAnswer = "panic: runtime error"
        let joined = await node.join(dir: "/tmp/n", hostname: "h", enrollmentJSON: "{}")
        XCTAssertEqual(joined, .failure(.unreadableAnswer("join")))
    }

    func testStatusIsTheStateGoReports() async throws {
        let up = try await node.status().get()
        XCTAssertEqual(up.state, "up")
        XCTAssertEqual(up.addresses, ["fd06:6e7:0:2::9"])

        bridge.statusAnswer = #"{"state":"down","ended":"the mothership refused this node","ended_code":"revoked"}"#
        let ended = try await node.status().get()
        XCTAssertEqual(ended.state, "down")
        XCTAssertEqual(ended.endedCode, "revoked")
        XCTAssertEqual(ended.ended, "the mothership refused this node")
    }

    /// Status's `status_unknown` is still a state: the node is up, ask again.
    func testAStatusGoCouldNotTakeIsStillUp() async throws {
        bridge.statusAnswer = #"{"state":"up","error":"context deadline exceeded","code":"status_unknown","addresses":[],"grants":[],"node_id":"n-1"}"#
        let unknown = try await node.status().get()
        XCTAssertEqual(unknown.state, "up")
        XCTAssertEqual(unknown.code, "status_unknown")
        XCTAssertEqual(unknown.addresses, [])
    }

    func testTheDefaultRouteIsHandedToGoByName() async throws {
        let answered = await node.defaultRoute(interface: "en0")
        XCTAssertEqual(try answered.get().ok, true)
        XCTAssertEqual(bridge.calls, ["default_route en0"])
    }

    /// F6: a Down calls off a held Join; it must not wait behind it.
    func testDownReturnsWhileAJoinIsHeld() async throws {
        let join = bridge.hold("join")
        let node = node!
        let joined = Task { await node.join(dir: "/tmp/n", hostname: "h", enrollmentJSON: "{}") }
        await join.entered()

        let downed = expectation(description: "down returned")
        Task {
            await node.down()
            downed.fulfill()
        }
        await fulfillment(of: [downed], timeout: 2)
        XCTAssertEqual(bridge.calls, ["join h", "down"])

        bridge.joinAnswer = #"{"error":"Down was called before the node came up","code":"cancelled"}"#
        join.release()
        let answer = await joined.value
        XCTAssertEqual(answer, .failure(.refused(code: "cancelled", message: "Down was called before the node came up")))
    }

    /// F6: Status never waits on a join, in Go or here.
    func testStatusAnswersWhileAJoinIsHeld() async throws {
        let join = bridge.hold("join")
        let node = node!
        let joined = Task { await node.join(dir: "/tmp/n", hostname: "h", enrollmentJSON: "{}") }
        await join.entered()

        bridge.statusAnswer = #"{"state":"joining"}"#
        let asked = expectation(description: "status answered")
        let status = Task { () -> Result<NodeAnswer, NodeError> in
            let answer = await node.status()
            asked.fulfill()
            return answer
        }
        await fulfillment(of: [asked], timeout: 2)
        let answered = await status.value
        XCTAssertEqual(try answered.get().state, "joining")

        join.release()
        _ = await joined.value
    }

    // MARK: a door

    func testARequestIsAnsweredByTheReplyWithItsId() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        XCTAssertEqual(bridge.calls, ["open [fd06:6e7:0:2::4]:6446"])
        async let answered = door.request(.hello(wants: ["operate"]))
        let (id, request) = await bridge.nextRequest()
        XCTAssertEqual(request["type"]?.string, "hello")
        XCTAssertEqual(bridge.sent, [#"{"id":\#(id),"request":{"type":"hello","wants":["operate"]}}"#])
        bridge.say(try Fixture.door("hello"), id: id)
        guard case .hello(let hello) = try await answered else { return XCTFail("not a hello") }
        XCTAssertEqual(hello.accepted, ["operate"])
    }

    func testAReplyIsWeighedInBytesAndTime() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        async let weighed = door.weigh(.hello(wants: ["operate"]))
        let asked = await bridge.nextRequest()
        let text = try Fixture.doorText("hello")
        bridge.say(try Fixture.door("hello"), id: asked.id)
        let reply = try await weighed
        guard case .hello = reply.response else { return XCTFail("not a hello") }
        // The fake re-encodes with sorted keys, so the byte count is the fake's frame's, not the file's.
        XCTAssertGreaterThan(reply.cost.bytes, text.utf8.count / 2)
        XCTAssertGreaterThan(reply.cost.waited, .zero)
        XCTAssertGreaterThanOrEqual(reply.cost.decoded, .zero)
        XCTAssertLessThan(reply.cost.decoded, reply.cost.waited)
    }

    func testTwoRequestsInFlightAreAnsweredOutOfOrder() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        async let first = door.request(.hello(wants: ["view"]))
        let one = await bridge.nextRequest()
        async let second = door.request(.sessionTranscript(sessionID: "s", fromTurn: nil, lastTurns: 20))
        let two = await bridge.nextRequest()
        XCTAssertNotEqual(one.id, two.id)
        bridge.say(try Fixture.door("session_transcript"), id: two.id)
        bridge.say(try Fixture.door("hello"), id: one.id)
        guard case .transcript = try await second, case .hello = try await first else { return XCTFail("crossed") }
    }

    func testEventsArriveOnTheStream() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        var events = door.events.makeAsyncIterator()
        bridge.say(try Fixture.door("said_text"))
        guard case .said(_, _, .text) = await events.next() else { return XCTFail("no text") }
    }

    /// One frame this app cannot read is logged and skipped; the door and
    /// the requests on it carry on.
    func testAFrameThatDoesNotDecodeIsSkipped() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        var events = door.events.makeAsyncIterator()
        async let answered = door.request(.hello(wants: ["view"]))
        let (id, _) = await bridge.nextRequest()
        bridge.sayRaw("not json")
        bridge.sayRaw(#"{"neither":"a reply nor an event"}"#)
        bridge.sayRaw(#"{"event":{"type":"said","session_id":"s"}}"#)
        bridge.say(try Fixture.door("said_text"))
        bridge.say(try Fixture.door("hello"), id: id)
        guard case .said(_, _, .text) = await events.next() else { return XCTFail("no text") }
        guard case .hello = try await answered else { return XCTFail("not a hello") }
        XCTAssertTrue(door.isOpen)
    }

    /// Review focus 4, at the bottom: OnClose fails every waiting request.
    func testADoorThatClosesFailsEveryWaitingRequest() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        async let transcript = door.request(.sessionTranscript(sessionID: "s", fromTurn: nil, lastTurns: 20))
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        do {
            _ = try await transcript
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? DoorError, .closed("lost: EOF"))
        }
        var events = door.events.makeAsyncIterator()
        let after = await events.next()
        XCTAssertNil(after, "the stream ends with the door")
        XCTAssertFalse(door.isOpen)
    }

    func testARequestAfterTheCloseFailsAtOnceAndLateFramesAreDropped() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        bridge.drop("door_silent")
        bridge.say(try Fixture.door("said_text"))
        do {
            _ = try await door.request(.hello(wants: ["view"]))
            XCTFail("sent")
        } catch {
            XCTAssertEqual(error as? DoorError, .closed("door_silent"))
        }
        XCTAssertTrue(bridge.sent.isEmpty)
        var events = door.events.makeAsyncIterator()
        let after = await events.next()
        XCTAssertNil(after, "nothing is yielded after the close")
    }

    func testClosingTheDoorAsksGoAndEndsTheConnection() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        door.close()
        var events = door.events.makeAsyncIterator()
        let after = await events.next()
        XCTAssertNil(after)
        XCTAssertEqual(bridge.calls, ["open [fd06:6e7:0:2::4]:6446", "close d-1"])
        XCTAssertFalse(door.isOpen)
    }

    func testAnUnansweredRequestTimesOut() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        do {
            _ = try await door.request(.hello(wants: ["view"]), within: .milliseconds(100))
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? DoorError, .timeout)
        }
        XCTAssertTrue(door.isOpen, "a timeout does not close the door")
    }

    /// F13: a transcript is one frame of up to 32 MB over DERP.
    func testATranscriptIsGivenLongerThanAHello() {
        XCTAssertEqual(DoorConnection.patience(for: .sessionTranscript(sessionID: "s", fromTurn: nil, lastTurns: 20)), .seconds(30))
        XCTAssertEqual(DoorConnection.patience(for: .hello(wants: ["view"])), .seconds(10))
    }

    func testASendGoRefusesFailsThatRequest() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        bridge.sendAnswer = #"{"error":"failed to write msg: broken pipe","code":"send_failed"}"#
        do {
            _ = try await door.request(.hello(wants: ["view"]))
            XCTFail("answered")
        } catch {
            XCTAssertEqual(error as? DoorError, .closed("send_failed: failed to write msg: broken pipe"))
        }
    }

    func testACancelledRequestStopsWaiting() async throws {
        let door = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        let asked = Task { try await door.request(.hello(wants: ["view"])) }
        let (id, _) = await bridge.nextRequest()
        asked.cancel()
        do {
            _ = try await asked.value
            XCTFail("answered")
        } catch {
            XCTAssertTrue(error is CancellationError, "\(error)")
        }
        bridge.say(try Fixture.door("hello"), id: id)
        XCTAssertTrue(door.isOpen, "a late reply to a cancelled request is dropped")
    }

    func testADialGoRefusesIsItsWordAndSentence() async {
        bridge.openAnswer = #"{"error":"dial tcp [fd06:6e7:0:2::4]:6446: i/o timeout","code":"timed_out"}"#
        do {
            _ = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
            XCTFail("opened")
        } catch {
            XCTAssertEqual(error as? NodeError, .refused(code: "timed_out", message: "dial tcp [fd06:6e7:0:2::4]:6446: i/o timeout"))
        }
    }

    func testAnOpenThatNamesNoDoorIsUnreadable() async {
        bridge.openAnswer = #"{"ok":true}"#
        do {
            _ = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
            XCTFail("opened")
        } catch {
            XCTAssertEqual(error as? NodeError, .unreadableAnswer("open_door"))
        }
    }

    /// F6: one door's Send never waits behind another door's dial.
    func testASendOnOneDoorIsNotBlockedByAnotherDoorsDial() async throws {
        let doorA = try await node.openDoor(address: "fd06:6e7:0:2::4", port: 6446)
        bridge.answerOpen("fd06:6e7:0:2::5", with: #"{"door_id":"d-2"}"#)
        let dial = bridge.hold("open fd06:6e7:0:2::5")
        let node = node!
        let opening = Task { try await node.openDoor(address: "fd06:6e7:0:2::5", port: 6446) }
        await dial.entered()

        async let answered = doorA.request(.hello(wants: ["view"]))
        let sent = expectation(description: "door A's hello went out")
        let bridge = bridge!
        let heard = Task { () -> Int in
            let (id, _) = await bridge.nextRequest()
            sent.fulfill()
            return id
        }
        await fulfillment(of: [sent], timeout: 2)
        let id = await heard.value
        bridge.say(try Fixture.door("hello"), id: id)
        guard case .hello = try await answered else { return XCTFail("not a hello") }

        dial.release()
        let doorB = try await opening.value
        XCTAssertTrue(doorB.isOpen)
    }
}
