import XCTest
@testable import DeskPhoneKit

@MainActor
final class ConversationModelTests: XCTestCase {
    private var bridge: FakeBridge!
    private var live = true
    /// When set, what the row's Mac reaches as; else a door that is `live`.
    private var reach: Reach?
    /// Home's refreshes asked by the conversation, and what each does.
    private var wakes = 0
    private var onWake: () -> Void = {}
    private let row = HomeRow(
        id: "00000000-0000-7000-8000-000000000001", organizationID: "o-1", title: "Phone pairing spec",
        state: "idle", ago: "yesterday", machineID: "recorded", macLabel: "Morgan's MacBook Pro",
        recapLine: nil, lastEventAt: nil
    )

    override func setUp() async throws {
        bridge = FakeBridge()
        live = true
        reach = nil
        wakes = 0
        onWake = {}
    }

    private func model(sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in throw CancellationError() }) -> ConversationModel {
        ConversationModel(
            row: row,
            reach: { [unowned self] _ in self.reach ?? .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: "Morgan's MacBook Pro", live: self.live)) },
            wake: { @MainActor [unowned self] in
                self.wakes += 1
                self.onWake()
            },
            doors: Node(bridge: bridge),
            sleep: sleep
        )
    }

    private func hello(major: Int = 1, features: [String] = ["transcript_paging"], accepted: [String] = ["operate"]) -> JSONValue {
        .object(["response": .object([
            "type": .string("hello"), "version": .string("0.0.0"), "build": .string("0.0.0"),
            "capabilities": .array([]), "accepted": .array(accepted.map(JSONValue.string)),
            "features": .array(features.map(JSONValue.string)),
            "protocol": .object(["major": .number(Double(major)), "minor": .number(37)]),
        ])])
    }

    /// Answers a conversation's first opening as the recordings have it:
    /// the hello, the transcript and the row.
    private func answerOpening(_ conversation: ConversationModel, door: String = "d-1") async throws {
        try await Opening(bridge: bridge, sessionID: row.id).answer(door: door)
    }

    private var opens: Int { bridge.calls.filter { $0.hasPrefix("open ") }.count }

    func testOnlineUnopenedInstallWaitsForListChangeWithoutDialOrWake() async throws {
        reach = .notOpen
        let ticks = Ticks()
        let conversation = model(sleep: { _ in try await ticks.wait() })
        await conversation.open()
        XCTAssertEqual(conversation.phase, .notOpen)
        XCTAssertEqual(conversation.asleepLines.first, "The Desk running this session on Morgan's MacBook Pro is online but not yet open to your phone.")
        await conversation.listed()
        XCTAssertTrue(bridge.calls.isEmpty)
        XCTAssertEqual(wakes, 0)
        let waited = await ticks.waited
        XCTAssertEqual(waited, 0)
        reach = nil
        async let changed: Void = conversation.listed()
        try await answerOpening(conversation)
        await changed
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertEqual(opens, 1)
        XCTAssertEqual(wakes, 0)
        conversation.leave()
    }

    /// B6: a live Mac with no door in the row's organisation says so, dials
    /// nothing and waits for no wake.
    func testAMacWithNoDoorHereSaysSoAndDoesNotWakeLoop() async throws {
        reach = .noDoor(organization: "paper-kites")
        let ticks = Ticks()
        let conversation = model(sleep: { _ in try await ticks.wait() })
        await conversation.open()
        XCTAssertEqual(conversation.phase, .noDoor(organization: "paper-kites"))
        XCTAssertEqual(conversation.asleepLines.first, "Morgan's MacBook Pro can't be reached for paper-kites. Ask an administrator to check access.")
        XCTAssertEqual(conversation.asleepLines[1], "Showing what the server last heard.")
        XCTAssertNil(conversation.report, "nothing is measured")
        XCTAssertTrue(bridge.calls.isEmpty, "nothing is dialled")
        try await Task.sleep(for: .milliseconds(50))
        let waited = await ticks.waited
        XCTAssertEqual(waited, 0, "no wake loop")
        conversation.leave()
    }

    /// B6: Retry with no door here asks Home again before it dials, so a
    /// grant that just landed is seen.
    func testRetryWithNoDoorAsksHomeAgainBeforeItDials() async throws {
        reach = .noDoor(organization: "paper-kites")
        let conversation = model()
        await conversation.open()
        XCTAssertEqual(conversation.phase, .noDoor(organization: "paper-kites"))
        XCTAssertEqual(wakes, 0, "an open asks nothing of Home")
        var phaseAtWake: ConversationModel.Phase?
        onWake = {
            phaseAtWake = conversation.phase
            self.reach = nil
        }
        async let retried: Void = conversation.retry()
        try await until { self.opens == 1 }
        XCTAssertEqual(wakes, 1)
        XCTAssertEqual(phaseAtWake, .connecting, "Retry says it is connecting while Home is asked")
        try await answerOpening(conversation)
        await retried
        XCTAssertEqual(conversation.phase, .open)
        conversation.leave()
    }

    /// B6: nothing tries again with no door here, so an owed answer says
    /// Retry sends it.
    func testAnAnswerOwedWithNoDoorWaitsForRetryAndSaysSo() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).answer(attention: Asks.approvalAttention())
        await opened
        async let answered: Void = conversation.answer(.option("allow"))
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        XCTAssertEqual(conversation.card?.owed, true)
        reach = .noDoor(organization: "paper-kites")
        await conversation.foreground()
        XCTAssertEqual(conversation.phase, .noDoor(organization: "paper-kites"))
        XCTAssertEqual(conversation.card?.owed, true)
        XCTAssertEqual(conversation.card?.note, "Your answer hasn't gone yet. Retry sends it.")
        conversation.leave()
    }

    func testAnAnswerOwedToUnopenedInstallDoesNotPromiseRetryControl() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).answer(attention: Asks.approvalAttention())
        await opened
        async let answered: Void = conversation.answer(.option("allow"))
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        reach = .notOpen
        await conversation.foreground()
        XCTAssertEqual(conversation.card?.owed, true)
        XCTAssertEqual(conversation.card?.note, "Your answer hasn't gone yet. It will send when Desk on Morgan's MacBook Pro is open to your phone.")
        conversation.leave()
    }

    /// An asleep Mac that wakes up in another organisation only is doorless
    /// here: the wake loop says so and ends.
    func testAnAsleepMacThatWakesWithNoDoorHereEndsTheWakeLoop() async throws {
        live = false
        let ticks = Ticks()
        let conversation = model(sleep: { _ in try await ticks.wait() })
        await conversation.open()
        XCTAssertEqual(conversation.phase, .asleep(macLabel: "Morgan's MacBook Pro"))
        try await ticks.untilWaiting(1)
        reach = .noDoor(organization: "paper-kites")
        await ticks.tick()
        try await until { conversation.phase == .noDoor(organization: "paper-kites") }
        try await Task.sleep(for: .milliseconds(50))
        let waited = await ticks.waited
        XCTAssertEqual(waited, 1, "the wake loop ended")
        XCTAssertTrue(bridge.calls.isEmpty, "nothing is dialled")
        conversation.leave()
    }

    /// Once the grant lands, Home's refresh answers a door and the way back
    /// from the background dials it.
    func testAGrantedDoorOpensOnTheWayBackFromTheBackground() async throws {
        reach = .noDoor(organization: "paper-kites")
        let conversation = model()
        await conversation.open()
        XCTAssertEqual(conversation.phase, .noDoor(organization: "paper-kites"))
        reach = nil
        async let again: Void = conversation.foreground()
        try await answerOpening(conversation)
        await again
        XCTAssertEqual(conversation.phase, .open)
        conversation.leave()
    }

    /// B5: a kept conversation shown again says it is connecting, not its
    /// last visit's open, until its open dials. One still on screen is
    /// left alone.
    func testAReopenedConversationLeftOpenSaysConnecting() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        conversation.reopened()
        XCTAssertEqual(conversation.phase, .open, "on screen: nothing changes")
        conversation.leave()
        conversation.reopened()
        XCTAssertEqual(conversation.phase, .connecting)
    }

    /// B5: nor its last visit's drop.
    func testAReopenedConversationLeftLostSaysConnecting() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        bridge.drop("EOF")
        let lost = ConversationModel.Phase.lost("The link to Morgan's MacBook Pro dropped.")
        try await until { conversation.phase == lost }
        conversation.reopened()
        XCTAssertEqual(conversation.phase, lost, "on screen: nothing changes")
        conversation.leave()
        conversation.reopened()
        XCTAssertEqual(conversation.phase, .connecting)
    }

    func testOpeningSaysHelloAsksTheLastTwentyTurnsAndDrawsThem() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertFalse(conversation.fold.turns.isEmpty)
    }

    /// B1: the path is asked of the node once the door is open, for the
    /// row's Mac's address, and again when the door closes.
    func testThePathIsAskedWhenTheDoorOpensAndWhenItCloses() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        XCTAssertEqual(bridge.calls.filter { $0 == "peer_path fd06:6e7:0:2::4" }.count, 1)
        conversation.leave()
        try await until { self.bridge.calls.filter { $0 == "peer_path fd06:6e7:0:2::4" }.count == 2 }
    }

    /// B1: a failed dial drops its open's measure, and Retry measures its own.
    func testAFailedDialDropsItsMeasureAndRetryStartsAnother() async throws {
        bridge.openAnswer = #"{"error":"websocket: bad handshake (403)","code":"refused"}"#
        let conversation = model()
        await conversation.open()
        XCTAssertNil(conversation.report)
        bridge.openAnswer = #"{"door_id":"d-1"}"#
        async let retried: Void = conversation.retry()
        try await until { conversation.report != nil }
        try await answerOpening(conversation)
        await retried
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertNil(conversation.report, "said, then cleared")
    }

    /// Final review 1: a conversation the person left dials nothing on the
    /// way back from the background, and its door stays closed; only the
    /// screen's own open (it appearing again) dials it.
    func testALeftConversationDialsNothingOnTheWayBackUntilItIsOpenedAgain() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        conversation.leave()
        try await until { self.bridge.calls.contains("close d-1") }
        let back = Task { await conversation.foreground() }
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(opens, 1, "a left conversation dials nothing")
        conversation.leave()
        await back.value

        async let again: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).answer(door: "d-1")
        await again
        XCTAssertEqual(opens, 2, "the screen's own open dials it")
        XCTAssertEqual(conversation.phase, .open)
        conversation.leave()
    }

    /// The app's background closes the door and keeps the conversation
    /// for the foreground to dial again.
    func testABackgroundedConversationDialsAgainOnTheWayBack() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        conversation.background()
        try await until { self.bridge.calls.contains("close d-1") }
        async let back: Void = conversation.foreground()
        try await Opening(bridge: bridge, sessionID: row.id).answer(door: "d-1")
        await back
        XCTAssertEqual(opens, 2)
        conversation.leave()
    }

    func testANewTurnFetchesThePersonsWordsAndTheAgentsStreamIn() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let next = (conversation.fold.lastTurn ?? 0) + 1
        bridge.say(.object(["event": .object([
            "type": .string("said"), "session_id": .string(row.id), "turn": .number(Double(next)),
            "said": .object(["kind": .string("text"), "message_id": .string("a9"), "tail": .string("Here are the frames…"), "parent_call_id": .null]),
        ])]))
        // The recorded hello offers transcript_seq: the turn is read as the
        // rows changed since the cursor.
        let reread = await bridge.nextRequest()
        XCTAssertEqual(reread.request["after_seq"]?.int, recordedThrough)
        XCTAssertNil(reread.request["from_turn"])
        bridge.say(.object(["response": .object([
            "type": .string("transcript"), "from_turn": .null, "earlier": .number(0),
            "through_seq": .number(Double(recordedThrough + 1)), "reset": .bool(false),
            "items": .array([.object([
                "id": .string("u9"), "session_id": .string(row.id), "ordinal": .number(99), "kind": .string("message"),
                "role": .string("user"), "content": .string("draw this workflow out in storyboards"),
                "turn": .number(Double(next)), "parent_call_id": .null, "message_item_id": .null, "remote_id": .null,
                "created_at": .string("2026-01-01T00:00:00.000000Z"),
            ])]),
        ])]), id: reread.id)
        try await until { conversation.fold.turns.last?.lines.count == 2 }
        XCTAssertEqual(conversation.fold.turns.last?.lines, [
            .you(id: "u9", text: "draw this workflow out in storyboards"),
            .agent(id: "a9", text: "Here are the frames…"),
        ])
        XCTAssertTrue(conversation.working)
    }

    func testTheEndOfATurnStopsWorkingAndReReadsIt() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let turn = try XCTUnwrap(conversation.fold.lastTurn)
        bridge.say(try Fixture.door("said_text"))
        try await until { conversation.working }
        bridge.say(try Fixture.door("said_turn_ended"))
        let reread = await bridge.nextRequest()
        XCTAssertEqual(reread.request["after_seq"]?.int, recordedThrough, "turn \(turn) is read as what changed")
        XCTAssertFalse(conversation.working)
        bridge.say(try Fixture.door("session_transcript_brief"), id: reread.id)
    }

    /// a turn that failed says so with its error, and a row read
    /// after a reopen says how the last turn ended where nothing heard did.
    func testAFailedTurnKeepsItsErrorAndTheRowSaysHowTheLastTurnEnded() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).greet()
        let asked = await bridge.nextRequest()
        bridge.say(Asks.sessionReply(try Asks.row(row.id, lastOutcome: "stopped from outside by Morgan")), id: asked.id)
        await opened
        let last = try XCTUnwrap(conversation.fold.turns.last)
        XCTAssertEqual(last.end, TurnEnd(outcome: "stopped from outside by Morgan"))
        XCTAssertEqual(last.end?.words, "Stopped from outside by Morgan")

        bridge.say(Asks.said(.object(["kind": .string("turn_ended"), "outcome": .string("the agent exited: code 1")]), session: row.id, turn: last.number))
        let reread = await bridge.nextRequest()
        XCTAssertEqual(conversation.fold.turns.last?.end?.more, "the agent exited: code 1")
        bridge.say(try Fixture.door("session_transcript_brief"), id: reread.id)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(conversation.fold.turns.last?.end?.tone, .failed, "a re-read that does not say the end keeps it")
    }

    /// the strip's clock counts from when the Mac says the turn
    /// began, and stops with the turn.
    func testTheWorkingClockCountsFromTheRowsStart() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).greet()
        let asked = await bridge.nextRequest()
        bridge.say(Asks.sessionReply(try Asks.row(row.id, working: true, turnStartedAt: "2026-01-01T00:00:00.000000Z")), id: asked.id)
        await opened
        XCTAssertEqual(conversation.workingSince, WireDate.parse("2026-01-01T00:00:00.000000Z"))
        bridge.say(try Fixture.door("said_turn_ended"))
        let reread = await bridge.nextRequest()
        XCTAssertFalse(conversation.working)
        XCTAssertNil(conversation.workingSince)
        bridge.say(try Fixture.door("session_transcript_brief"), id: reread.id)
    }

    func testAnotherSessionsMomentsAreNotFolded() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let before = conversation.fold
        bridge.say(.object(["event": .object([
            "type": .string("said"), "session_id": .string("someone-else"), "turn": .number(99),
            "said": .object(["kind": .string("text"), "message_id": .string("x"), "tail": .string("no"), "parent_call_id": .null]),
        ])]))
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(conversation.fold, before)
        XCTAssertEqual(bridge.sent.count, 3, "nothing re-read after hello, transcript, session_get")
    }

    /// Review focus 4.
    func testADoorThatDropsMidTranscriptSaysSoAndRetryOpensAgain() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(try Fixture.door("hello"), id: hello.id)
        _ = await bridge.nextRequest()
        bridge.drop("EOF")
        await opened
        XCTAssertEqual(conversation.phase, .lost("The link to Morgan's MacBook Pro dropped."))

        async let again: Void = conversation.retry()
        try await answerOpening(conversation)
        await again
        XCTAssertEqual(conversation.phase, .open)
    }

    func testADoorThatGoesSilentAfterOpeningKeepsTheTurns() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let turns = conversation.fold.turns
        bridge.drop("door_silent")
        try await until { conversation.phase != .open }
        XCTAssertEqual(conversation.phase, .lost("The link to Morgan's MacBook Pro dropped."))
        XCTAssertEqual(conversation.fold.turns, turns)
    }

    func testAnAsleepMacSaysF4AndOpensWhenItWakes() async throws {
        live = false
        let ticks = Ticks()
        let conversation = model(sleep: { _ in try await ticks.wait() })
        await conversation.open()
        XCTAssertEqual(conversation.phase, .asleep(macLabel: "Morgan's MacBook Pro"))
        XCTAssertEqual(conversation.asleepLines, [
            "Morgan's MacBook Pro is asleep. This session will open when that Mac wakes.",
            "Showing what the server last heard.",
            "Status: idle · last turn yesterday",
        ])
        XCTAssertTrue(bridge.calls.isEmpty, "an asleep Mac is not dialled")
        live = true
        try await ticks.untilWaiting(1)
        await ticks.tick()
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request["type"]?.string, "hello")
        conversation.leave()
    }

    func testOnlineDeskWithStale6netSaysLinkUnavailableAndRefreshesUntilRecovery() async throws {
        reach = .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: row.macLabel, live: false, deskPresence: "online"))
        let ticks = Ticks()
        let delays = Delays()
        let conversation = model(sleep: { delay in
            await delays.add(delay)
            try await ticks.wait()
        })
        await conversation.open()
        XCTAssertEqual(conversation.phase, .linkUnavailable(macLabel: row.macLabel))
        XCTAssertEqual(conversation.asleepLines.first, "Desk recently heard from Morgan's MacBook Pro, but this phone can't reach it. Retrying.")
        XCTAssertTrue(conversation.phase.offline)
        XCTAssertTrue(bridge.calls.isEmpty)
        try await ticks.untilWaiting(1)

        reach = .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: row.macLabel, live: false, deskPresence: "offline"))
        await ticks.tick()
        try await ticks.untilWaiting(2)
        XCTAssertEqual(conversation.phase, .asleep(macLabel: row.macLabel))
        XCTAssertEqual(wakes, 1)

        reach = .linkUnavailable
        await ticks.tick()
        try await ticks.untilWaiting(3)
        XCTAssertEqual(conversation.phase, .linkUnavailable(macLabel: row.macLabel))
        XCTAssertEqual(wakes, 2)
        let intervals = await delays.all
        XCTAssertEqual(intervals, [.seconds(30), .seconds(30), .seconds(30)])

        reach = nil
        await ticks.tick()
        try await answerOpening(conversation)
        try await until { conversation.phase == .open }
        XCTAssertEqual(wakes, 3)
        conversation.leave()
    }

    func testFailedDialsUseDeskEvidenceAndLeavingCancelsTheRetry() async throws {
        for code in ["timed_out", "unreachable"] {
            for presence: String? in ["online", "offline", nil] {
                bridge = FakeBridge()
                bridge.openAnswer = #"{"error":"unavailable","code":"\#(code)"}"#
                reach = .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: row.macLabel, live: true, deskPresence: presence))
                let ticks = Ticks()
                let conversation = model(sleep: { _ in try await ticks.wait() })
                await conversation.open()
                XCTAssertEqual(conversation.phase, presence == "online" ? .linkUnavailable(macLabel: row.macLabel) : .asleep(macLabel: row.macLabel))
                try await ticks.untilWaiting(1)
                conversation.leave()
                await ticks.tick()
                XCTAssertEqual(wakes, 0)
                XCTAssertEqual(opens, 1)
            }
        }
    }

    /// F7: Go's code says what happened; its sentence is never read.
    func testADialIsJudgedOnItsCodeNotItsWords() async {
        let asleep = ConversationModel.Phase.asleep(macLabel: "Morgan's MacBook Pro")
        let refused = ConversationModel.Phase.refused("Couldn't open this session on Morgan's MacBook Pro.")
        let cases: [(code: String, phase: ConversationModel.Phase)] = [
            ("timed_out", asleep), ("unreachable", asleep),
            ("refused", refused), ("dropped", refused), ("no_grant", refused), ("malformed", refused), ("", refused),
        ]
        for (code, phase) in cases {
            bridge = FakeBridge()
            let codeField = code.isEmpty ? "" : #","code":"\#(code)""#
            // Every sentence says "timeout": only the code may decide.
            bridge.openAnswer = #"{"error":"dial tcp [fd06:6e7:0:2::4]:6446: i/o timeout"\#(codeField)}"#
            let conversation = model()
            await conversation.open()
            XCTAssertEqual(conversation.phase, phase, "code \(code)")
            conversation.leave()
        }
    }

    func testARefusedDialSaysSoUnderTheTitle() async {
        bridge.openAnswer = #"{"error":"websocket: bad handshake (403)","code":"refused"}"#
        let conversation = model()
        await conversation.open()
        XCTAssertEqual(conversation.phase, .refused("Couldn't open this session on Morgan's MacBook Pro."))
    }

    func testAnUnreadableDialIsRefused() async {
        bridge.openAnswer = "not json"
        let conversation = model()
        await conversation.open()
        XCTAssertEqual(conversation.phase, .refused("Couldn't open this session on Morgan's MacBook Pro."))
    }

    /// F7: the node is still joining, or not up: the door is tried again,
    /// later and later, and the screen keeps saying it is connecting.
    func testANodeNotUpYetStaysConnectingAndTriesAgain() async throws {
        for code in ["joining", "node_down"] {
            bridge = FakeBridge()
            bridge.openAnswer = #"{"error":"not yet","code":"\#(code)"}"#
            let ticks = Ticks()
            let delays = Delays()
            let conversation = model(sleep: { delay in
                await delays.add(delay)
                try await ticks.wait()
            })
            await conversation.open()
            XCTAssertEqual(conversation.phase, .connecting, code)
            try await ticks.untilWaiting(1)
            await ticks.tick()
            try await until { self.opens == 2 }
            XCTAssertEqual(conversation.phase, .connecting, code)
            try await ticks.untilWaiting(2)
            let waited = await delays.all
            XCTAssertEqual(waited.count, 2, code)
            XCTAssertLessThan(waited[0], waited[1], "\(code): each wait is longer")

            bridge.openAnswer = #"{"door_id":"d-1"}"#
            await ticks.tick()
            try await answerOpening(conversation)
            try await until { conversation.phase == .open }
            XCTAssertEqual(opens, 3, code)
            conversation.leave()
        }
    }

    /// A node that is down is the app's to bring up: each node_down dial
    /// says so, and a joining one does not.
    func testANodeDownDialAsksTheAppToBringTheNodeUp() async throws {
        for (code, asks) in [("node_down", 1), ("joining", 0)] {
            bridge = FakeBridge()
            bridge.openAnswer = #"{"error":"not yet","code":"\#(code)"}"#
            var asked = 0
            let conversation = model()
            conversation.onNodeDown = { asked += 1 }
            await conversation.open()
            XCTAssertEqual(asked, asks, code)
            XCTAssertEqual(conversation.phase, .connecting, code)
            conversation.leave()
        }
    }

    func testADeskThatDoesNotAcceptOperateIsRefusedAndItsDoorClosed() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(accepted: []), id: hello.id)
        await opened
        XCTAssertEqual(conversation.phase, .refused("Couldn't open this session on Morgan's MacBook Pro."))
        // The door's close goes to Go on the door's own queue: wait for it.
        try await until { self.bridge.calls.contains("close d-1") }
        XCTAssertEqual(bridge.sent.count, 1, "no transcript asked")
    }

    /// F7 → F5: a phone the account no longer holds pairs again.
    func testARemovedPhoneIsHandedBackToPairAgain() async {
        for code in ["not_enrolled", "revoked"] {
            bridge = FakeBridge()
            bridge.openAnswer = #"{"error":"gone","code":"\#(code)"}"#
            var removed = 0
            let conversation = model()
            conversation.onRemoved = { removed += 1 }
            await conversation.open()
            XCTAssertEqual(removed, 1, code)
            XCTAssertEqual(conversation.phase, .refused("This phone was removed from your account."), code)
            XCTAssertEqual(opens, 1, "\(code): not tried again")
        }
    }

    func testACoreOnAnotherProtocolIsRefused() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(major: 2), id: hello.id)
        await opened
        XCTAssertEqual(conversation.phase, .refused("Desk on Morgan's MacBook Pro speaks a protocol this app does not. Update both."))
    }

    func testWithoutPagingTheWholeTranscriptIsAsked() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(features: []), id: hello.id)
        let transcript = await bridge.nextRequest()
        XCTAssertNil(transcript.request["last_turns"])
        XCTAssertNil(transcript.request["from_turn"])
        bridge.say(try Fixture.door("session_transcript"), id: transcript.id)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead(environments: true, oneRow: false)
        await opened
        XCTAssertEqual(conversation.phase, .open)
    }

    /// B3: against a Desk that offers session_get, the open asks its one row
    /// and nothing of the list.
    func testTheOpenAsksItsOneRowWhereTheDeskOffersIt() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).greet()
        let asked = await bridge.nextRequest()
        XCTAssertEqual(asked.request, .object(["type": .string("session_get"), "session_id": .string(row.id)]))
        bridge.say(try Fixture.door("session_get"), id: asked.id)
        await opened
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertEqual(bridge.sent.count, 3, "hello, transcript, session_get")
        XCTAssertFalse(bridge.sent.contains { $0.contains("environments_list") || $0.contains("sessions_list") })
    }

    func testARowThisMacDoesNotHoldLeavesTheConversationOpenWithNoCard() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).greet()
        let asked = await bridge.nextRequest()
        bridge.say(try Fixture.door("session_get_unknown_session"), id: asked.id)
        await opened
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertNil(conversation.card)
    }

    func testWithoutSessionGetTheListIsReadAsBefore() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(features: ["transcript_paging"]), id: hello.id)
        let transcript = await bridge.nextRequest()
        bridge.say(try Fixture.door("session_transcript"), id: transcript.id)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead(environments: true, oneRow: false)
        await opened
        XCTAssertEqual(conversation.phase, .open)
    }

    /// F8: back from the background, the door is closed and dialled again,
    /// and the turns stay on screen meanwhile.
    func testForegroundDialsAgainAndKeepsTheTurns() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let turns = conversation.fold.turns
        bridge.openAnswer = #"{"door_id":"d-2"}"#
        async let back: Void = conversation.foreground()
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request["type"]?.string, "hello")
        try await until { self.bridge.calls.contains("close d-1") }
        XCTAssertEqual(conversation.phase, .connecting)
        XCTAssertEqual(conversation.fold.turns, turns, "the turns stay while it dials")
        bridge.say(try Fixture.door("hello"), id: hello.id, door: "d-2")
        let transcript = await bridge.nextRequest()
        XCTAssertEqual(transcript.request["after_seq"]?.int, recordedThrough)
        XCTAssertEqual(transcript.request["shape"]?.string, "brief")
        bridge.say(try Fixture.door("session_transcript_brief"), id: transcript.id, door: "d-2")
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead(door: "d-2")
        await back
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertEqual(conversation.fold.turns, turns)
        XCTAssertEqual(opens, 2)
    }

    /// An open overtaken by another (a foreground mid-dial) leaves the newer
    /// one's door and phase alone.
    func testAnOpenOvertakenByAnotherLeavesItAlone() async throws {
        let conversation = model()
        async let first: Void = conversation.open()
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request["type"]?.string, "hello")
        bridge.openAnswer = #"{"door_id":"d-2"}"#
        async let second: Void = conversation.foreground()
        await first
        try await answerOpening(conversation, door: "d-2")
        await second
        XCTAssertEqual(conversation.phase, .open)
        bridge.say(try Fixture.door("said_text"), door: "d-2")
        try await until { conversation.working }
    }

    /// The recorded brief transcript's cursor (`through_seq`).
    private var recordedThrough: Int {
        guard case .reply(_, .transcript(let brief))? = try? DoorFrame(Fixture.doorText("session_transcript_brief")) else { return -1 }
        return brief.throughSeq ?? -1
    }

    /// B5: a conversation opened again reads the rows changed since its
    /// cursor, not a page, and folds them in place.
    func testAReopenReadsAfterItsCursorAndFoldsTheChanges() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        let turns = conversation.fold.turns
        conversation.leave()
        try await until { self.bridge.calls.contains("close d-1") }

        async let again: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(try Fixture.door("hello"), id: hello.id)
        let last = try XCTUnwrap(turns.last?.number)
        try await Opening(bridge: bridge, sessionID: row.id).answerChanges(afterSeq: recordedThrough, items: [.object([
            "id": .string("a-new"), "session_id": .string(row.id), "ordinal": .number(99), "kind": .string("message"),
            "role": .string("assistant"), "content": .string("And one more thing."), "turn": .number(Double(last)),
            "parent_call_id": .null, "message_item_id": .null, "remote_id": .string("ledger"), "seq": .number(30),
            "created_at": .string("2026-01-01T00:00:00.000000Z"),
        ])], through: 30)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead()
        await again
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertEqual(conversation.fold.turns.count, turns.count, "no turn lost")
        XCTAssertEqual(conversation.fold.turns.last?.lines.last, .agent(id: "a-new", text: "And one more thing."))
        XCTAssertEqual(conversation.cursor?.throughSeq, 30)
        conversation.leave()
    }

    /// B2: a rebuild older than the cursor answers a page marked reset, and
    /// the fold is replaced by it.
    func testAResetReplacesTheFoldWithThePage() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        conversation.leave()
        try await until { self.bridge.calls.contains("close d-1") }

        async let again: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(try Fixture.door("hello"), id: hello.id)
        try await Opening(bridge: bridge, sessionID: row.id).answerChanges(afterSeq: recordedThrough, items: [.object([
            "id": .string("only"), "session_id": .string(row.id), "ordinal": .number(1), "kind": .string("message"),
            "role": .string("user"), "content": .string("rebuilt"), "turn": .number(7),
            "parent_call_id": .null, "message_item_id": .null, "remote_id": .string("ledger"), "seq": .number(50),
            "created_at": .string("2026-01-01T00:00:00.000000Z"),
        ])], through: 50, reset: true)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead()
        await again
        XCTAssertEqual(conversation.fold.turns.map(\.number), [7])
        XCTAssertEqual(conversation.cursor?.throughSeq, 50)
        conversation.leave()
    }

    /// Review focus 1.
    func testAnotherStoreIsRefusedBeforeReadingItsTranscript() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        conversation.leave()
        try await until { self.bridge.calls.contains("close d-1") }

        async let again: Void = conversation.open()
        let hello = await bridge.nextRequest()
        guard case .object(var fields) = try Fixture.door("hello"), case .object(var response)? = fields["response"],
              case .object(var host)? = response["host"] else { return XCTFail("no host in the recorded hello") }
        host["store_id"] = .string("another-mac")
        response["host"] = .object(host)
        fields["response"] = .object(response)
        let sentBeforeHello = bridge.sent.count
        bridge.say(.object(fields), id: hello.id)
        await again
        XCTAssertEqual(bridge.sent.count, sentBeforeHello, "no transcript or row read reaches a different store")
        XCTAssertEqual(conversation.phase, .refused("This connection reaches a different Desk on Morgan's MacBook Pro. Retry checks Home again and reopens it."))
        let opensBeforeRetry = opens
        onWake = {
            XCTAssertEqual(self.opens, opensBeforeRetry, "Home is asked before a new dial")
            self.reach = .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6447, macLabel: self.row.macLabel, live: true))
        }
        async let retried: Void = conversation.retry()
        try await answerOpening(conversation)
        await retried
        XCTAssertEqual(wakes, 1, "Retry must refresh Home before using its cached target")
        XCTAssertTrue(bridge.calls.contains("open [fd06:6e7:0:2::4]:6447"))
        conversation.leave()
    }

    /// Review focus 1.
    func testWithoutTranscriptSeqTheCursorIsDroppedAndAPageIsRead() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        XCTAssertNotNil(conversation.cursor)
        conversation.leave()
        try await until { self.bridge.calls.contains("close d-1") }

        async let again: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(features: ["transcript_paging", "transcript_brief", "session_get"]), id: hello.id)
        let read = await bridge.nextRequest()
        XCTAssertNil(read.request["after_seq"])
        XCTAssertEqual(read.request["last_turns"]?.int, 20)
        XCTAssertEqual(read.request["shape"]?.string, "brief")
        bridge.say(try Fixture.door("session_transcript_brief"), id: read.id)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead()
        await again
        XCTAssertNil(conversation.cursor)
        conversation.leave()
    }

    /// B5: the core's lagged event reads the changes since the cursor, and
    /// the row again.
    func testLaggedReadsAfterTheCursorAndTheRow() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        try await answerOpening(conversation)
        await opened
        bridge.say(.object(["event": .object(["type": .string("lagged"), "missed": .number(3)])]))
        try await Opening(bridge: bridge, sessionID: row.id).answerChanges(afterSeq: recordedThrough, through: recordedThrough + 1)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead()
        try await until { conversation.cursor?.throughSeq == self.recordedThrough + 1 }
        conversation.leave()
    }

    /// Against a Desk without the words, the whole transcript is read as
    /// before, in full shape.
    func testWithoutBriefTheFullShapeIsAsked() async throws {
        let conversation = model()
        async let opened: Void = conversation.open()
        let hello = await bridge.nextRequest()
        bridge.say(self.hello(features: ["transcript_paging"]), id: hello.id)
        let read = await bridge.nextRequest()
        XCTAssertNil(read.request["shape"])
        bridge.say(try Fixture.door("session_transcript"), id: read.id)
        try await Opening(bridge: bridge, sessionID: row.id).answerRowRead(environments: true, oneRow: false)
        await opened
        XCTAssertEqual(conversation.phase, .open)
    }
}

/// Polls a condition the main actor will make true.
@MainActor
func until(_ condition: () -> Bool, within: Duration = .seconds(2)) async throws {
    let deadline = ContinuousClock.now + within
    while !condition() {
        guard ContinuousClock.now < deadline else { throw CancellationError() }
        try await Task.sleep(for: .milliseconds(5))
    }
}

/// The waits a model asked for, in order.
actor Delays {
    private(set) var all: [Duration] = []
    func add(_ delay: Duration) { all.append(delay) }
}
