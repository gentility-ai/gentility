import XCTest
@testable import DeskPhoneKit

@MainActor
final class ComposerTests: XCTestCase {
    private var bridge: FakeBridge!
    private var ids: Ids!
    private var opening: Opening { Opening(bridge: bridge) }
    private var queueID: String { (try? Fixture.door("prompt_queued"))?["response"]?["queue_id"]?.string ?? "" }
    private let mac = "Morgan's MacBook Pro"

    override func setUp() async throws {
        bridge = FakeBridge()
        ids = Ids()
    }

    private func opened(
        working: Bool, queued: [JSONValue] = [],
        sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in throw CancellationError() }
    ) async throws -> ConversationModel {
        let conversation = Conversations.model(bridge, ids: ids, sleep: sleep)
        async let open: Void = conversation.open()
        try await opening.answer(working: working, queued: queued)
        await open
        XCTAssertEqual(conversation.phase, .open)
        return conversation
    }

    func testIdleSendIsAPromptUnderItsOp() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("  make it three minutes \n")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request, .object([
            "type": .string("session_prompt"), "session_id": .string(Conversations.row.id),
            "text": .string("make it three minutes"), "op": .string("op-1"),
        ]))
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        XCTAssertTrue(conversation.working, "a turn begins")
        XCTAssertFalse(conversation.sending)
    }

    func testBlankWordsSendNothing() async throws {
        let conversation = try await opened(working: false)
        let before = bridge.sent.count
        let took = await conversation.send("  \n ")
        XCTAssertFalse(took)
        XCTAssertEqual(bridge.sent.count, before)
    }

    func testDuringATurnQueueShowsAPendingLine() async throws {
        let conversation = try await opened(working: true)
        async let took = conversation.send("and then the tests")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request, .object([
            "type": .string("session_prompt"), "session_id": .string(Conversations.row.id),
            "text": .string("and then the tests"), "queue": .bool(true), "op": .string("op-1"),
        ]))
        bridge.say(try Fixture.door("prompt_queued"), id: sent.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        XCTAssertEqual(conversation.pending.lines, [PendingLine(queueID: queueID, text: "and then the tests")])
    }

    func testASecondSendWhileOneIsOutSendsNothing() async throws {
        let conversation = try await opened(working: false)
        async let first = conversation.send("one")
        let sent = await bridge.nextRequest()
        XCTAssertTrue(conversation.sending)
        let second = await conversation.send("two")
        XCTAssertFalse(second, "the words stay in the composer")
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        let took = await first
        XCTAssertTrue(took)
        XCTAssertEqual(bridge.sent.filter { $0.contains("session_prompt") }.count, 1)
    }

    func testTakeBackWithdrawsTheLineAndHandsBackItsWords() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then")])
        XCTAssertEqual(conversation.pending.lines.map(\.queueID), ["q-1"])
        async let words = conversation.takeBack("q-1")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request, .object(["type": .string("session_withdraw"), "session_id": .string(Conversations.row.id), "queue_id": .string("q-1")]))
        bridge.say(try Fixture.door("withdraw"), id: sent.id)
        let back = await words
        XCTAssertEqual(back, "and then")
        XCTAssertEqual(conversation.pending.lines, [])
    }

    /// The row the Mac says as it withdraws can land before the reply.
    func testTakeBackHandsBackTheWordsWhenTheRowLandsFirst() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then")])
        async let words = conversation.takeBack("q-1")
        let sent = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true)))
        try await until { conversation.pending.lines.isEmpty }
        bridge.say(try Fixture.door("withdraw"), id: sent.id)
        let back = await words
        XCTAssertEqual(back, "and then")
    }

    func testTakingBackALineAlreadySentSaysSo() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then")])
        async let words = conversation.takeBack("q-1")
        let sent = await bridge.nextRequest()
        bridge.say(Asks.refusal("unknown_option", "nothing is queued as q-1"), id: sent.id)
        let back = await words
        XCTAssertNil(back)
        XCTAssertEqual(conversation.pending.lines, [])
        XCTAssertEqual(conversation.composerNote, ConversationModel.alreadySent)
    }

    func testTakingBackALineBeingSteeredSaysItIsTooLate() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then", steer: "claimed")])
        async let words = conversation.takeBack("q-1")
        let sent = await bridge.nextRequest()
        bridge.say(Asks.refusal("steering", "q-1 is being steered"), id: sent.id)
        let back = await words
        XCTAssertNil(back)
        XCTAssertEqual(conversation.pending.lines, [PendingLine(queueID: "q-1", text: "and then", note: ConversationModel.tooLateToTakeBack)])
    }

    func testSteerNowQueuesThenSteers() async throws {
        let conversation = try await opened(working: true)
        async let took = conversation.steer("stop and run the tests")
        let prompt = await bridge.nextRequest()
        XCTAssertEqual(prompt.request["queue"], .bool(true))
        bridge.say(try Fixture.door("prompt_queued"), id: prompt.id)
        let steer = await bridge.nextRequest()
        XCTAssertEqual(steer.request, .object(["type": .string("session_steer"), "session_id": .string(Conversations.row.id), "queue_id": .string(queueID)]))
        bridge.say(try Fixture.door("steer"), id: steer.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        XCTAssertEqual(conversation.pending.lines, [PendingLine(queueID: queueID, text: "stop and run the tests", note: PendingLines.steering)])
    }

    /// Review focus 4.
    func testSteerNowWithNothingRunningIsSentAtOnce() async throws {
        let conversation = try await opened(working: true)
        async let took = conversation.steer("now")
        let prompt = await bridge.nextRequest()
        XCTAssertEqual(prompt.request["queue"], .bool(true))
        bridge.say(try Fixture.door("prompt"), id: prompt.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertFalse(bridge.sent.contains { $0.contains("session_steer") }, "the turn had ended: nothing to steer")
        XCTAssertEqual(conversation.pending.lines, [])
    }

    func testSteerNowWhenIdleIsAPlainPrompt() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.steer("go")
        let prompt = await bridge.nextRequest()
        XCTAssertNil(prompt.request["queue"])
        bridge.say(try Fixture.door("prompt"), id: prompt.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        XCTAssertFalse(bridge.sent.contains { $0.contains("session_steer") })
    }

    /// Review focus 4.
    func testARefusedSteerStaysQueuedWithANote() async throws {
        let conversation = try await opened(working: true)
        async let took = conversation.steer("stop")
        let prompt = await bridge.nextRequest()
        bridge.say(try Fixture.door("prompt_queued"), id: prompt.id)
        let steer = await bridge.nextRequest()
        let refused = try Fixture.door("steer_refused")
        XCTAssertEqual(refused["response"]?["code"]?.string, "unknown_option")
        bridge.say(refused, id: steer.id)
        let cleared = await took
        XCTAssertTrue(cleared, "the words are queued, so the draft clears")
        XCTAssertEqual(conversation.pending.lines, [PendingLine(queueID: queueID, text: "stop", note: PendingLines.steerRefused)])
    }

    /// Review focus 4.
    func testTheRowsQueueIsWhatThePendingLinesShow() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "a"), Asks.queued("q-2", "b", steer: "refused")])
        XCTAssertEqual(conversation.pending.lines, [
            PendingLine(queueID: "q-1", text: "a"),
            PendingLine(queueID: "q-2", text: "b", note: PendingLines.steerRefused),
        ])
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: false)))
        try await until { conversation.pending.lines.isEmpty }
    }

    /// A line's note stays where the row says nothing of its steer, and a
    /// steer the row reports replaces it.
    func testTheRowsSteerNotesALineAndASilentRowKeepsItsNote() async throws {
        var lines = PendingLines()
        lines.queued("q-1", "a")
        lines.note("q-1", PendingLines.steering)
        lines.row([QueuedPrompt(queueID: "q-1", text: "a", at: "2026-01-01T00:00:00Z", steer: nil, steerReason: nil)])
        XCTAssertEqual(lines.lines, [PendingLine(queueID: "q-1", text: "a", note: PendingLines.steering)])
        lines.row([QueuedPrompt(queueID: "q-1", text: "a", at: "2026-01-01T00:00:00Z", steer: "unknown", steerReason: nil)])
        XCTAssertEqual(lines.lines, [PendingLine(queueID: "q-1", text: "a", note: PendingLines.steerLost)], "the core never sends it by itself")
        lines.queued("q-1", "a")
        XCTAssertEqual(lines.lines.count, 1, "a line the row already holds is not added twice")
    }

    /// The row's requested prompt is the feed's pending bubble from the row
    /// that carries it to the row that does not: its turn began.
    func testTheRowsRequestedPromptIsThePendingBubbleUntilItsTurnStarts() async throws {
        let conversation = try await opened(working: true)
        XCTAssertNil(conversation.waitingPrompt)
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true, requested: Asks.requested("p-1", "and then", compacting: true))))
        try await until { conversation.waitingPrompt != nil }
        XCTAssertEqual(conversation.waitingPrompt, RequestedPrompt(id: "p-1", text: "and then", compacting: true))
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true, requested: Asks.requested("p-1", "and then", compacting: false))))
        try await until { conversation.waitingPrompt?.compacting == false }
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true)))
        try await until { conversation.waitingPrompt == nil }
        XCTAssertNil(conversation.requested)
    }

    /// A row said before the reply to this phone's prompt carries its words
    /// while they are still in the composer: one place draws them, the
    /// composer until the Mac took them, then the bubble.
    func testWordsStillBeingSentAreNotDrawnTwice() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("and then")
        let sent = await bridge.nextRequest()
        XCTAssertTrue(conversation.sending)
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true, requested: Asks.requested("p-1", "and then", compacting: true))))
        try await until { conversation.requested != nil }
        XCTAssertNil(conversation.waitingPrompt, "the words are still in the composer")
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        let cleared = await took
        XCTAssertTrue(cleared)
        XCTAssertEqual(conversation.waitingPrompt, RequestedPrompt(id: "p-1", text: "and then", compacting: true))
    }

    /// Another window's prompt waiting draws while this phone sends other words.
    func testAnotherPromptWaitingDrawsWhileThisPhoneSends() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("mine")
        let sent = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true, requested: Asks.requested("p-1", "the Mac's", compacting: true))))
        try await until { conversation.waitingPrompt != nil }
        XCTAssertEqual(conversation.waitingPrompt?.text, "the Mac's")
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        _ = await took
    }

    func testATurnThatStartedMeanwhileKeepsTheWordsAndOffersQueue() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("hello")
        let sent = await bridge.nextRequest()
        bridge.say(Asks.refusal("turn_in_progress", "a turn is running"), id: sent.id)
        let cleared = await took
        XCTAssertFalse(cleared)
        XCTAssertTrue(conversation.working)
        XCTAssertEqual(conversation.composerNote, ConversationModel.turnStarted)
    }

    /// `FakeBridge.nextRequest` yields every frame sent, the failed one
    /// included, in order: the failed send's frame, then the retry's.
    func testASendThatDoesNotReachTheMacKeepsTheWordsAndItsOp() async throws {
        let conversation = try await opened(working: false)
        bridge.sendAnswer = #"{"error":"no route","code":"send_failed"}"#
        let first = await conversation.send("hello")
        XCTAssertFalse(first)
        XCTAssertEqual(conversation.composerNote, ConversationModel.unreachableToSend(mac))
        let failed = await bridge.nextRequest()
        bridge.sendAnswer = #"{"ok":true}"#
        async let again = conversation.send("hello")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(failed.request["op"]?.string, "op-1")
        XCTAssertEqual(sent.request["op"]?.string, "op-1", "the same words again, the same op")
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        let took = await again
        XCTAssertTrue(took)
        XCTAssertNil(conversation.composerNote)
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// A prompt whose door drops keeps its words for the person to send
    /// again, and nothing dials by itself. The redial's row says the session
    /// is idle, so the prompt never ran or has finished: the same words are a
    /// new prompt (the row saying it runs: the next test).
    func testAPromptLostToADropIsTheirsToSendAgainOnceTheDoorIsBack() async throws {
        let conversation = try await opened(working: false, sleep: { _ in await Task.yield() })
        async let took = conversation.send("hello")
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        let first = await took
        XCTAssertFalse(first)
        XCTAssertEqual(conversation.composerNote, ConversationModel.unreachableToSend(mac))
        try await until { conversation.phase == .lost(ConversationModel.dropped(self.mac)) }
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(bridge.calls.filter { $0.hasPrefix("open ") }.count, 1, "a prompt is not owed: no dial by itself")

        bridge.openAnswer = #"{"door_id":"d-2"}"#
        async let back: Void = conversation.retry()
        try await opening.answer(door: "d-2")
        await back
        XCTAssertNil(conversation.composerNote, "the door is back")
        async let again = conversation.send("hello")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request["op"]?.string, "op-2")
        bridge.say(try Fixture.door("prompt"), id: sent.id, door: "d-2")
        let took2 = await again
        XCTAssertTrue(took2)
    }

    /// A conversation left with a prompt out dials nothing when its door
    /// goes.
    func testALeftConversationsPromptDialsNothing() async throws {
        let conversation = try await opened(working: true, sleep: { _ in await Task.yield() })
        async let took = conversation.steer("stop")
        _ = await bridge.nextRequest()
        conversation.leave()
        bridge.drop("closed")
        let cleared = await took
        XCTAssertFalse(cleared)
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(bridge.calls.filter { $0.hasPrefix("open ") }.count, 1, "no dial after leaving")
        XCTAssertFalse(bridge.sent.contains { $0.contains("session_steer") })
    }

    // MARK: fix round 1

    private static func queued(_ id: String, steer: String?) -> QueuedPrompt {
        QueuedPrompt(queueID: id, text: "a", at: "2026-01-01T00:00:00Z", steer: steer, steerReason: nil)
    }

    /// A lost idle prompt goes again exactly as it went, `queue` and all,
    /// though the turn it may have started now runs: the Mac answers it from
    /// its record. Only a `turn_in_progress` lets Queue mint a new op.
    func testALostIdlePromptGoesAgainAsItWentWhileATurnRuns() async throws {
        let conversation = try await opened(working: false)
        bridge.sendAnswer = #"{"error":"no route","code":"send_failed"}"#
        let first = await conversation.send("hello")
        XCTAssertFalse(first)
        let lost = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true)))
        try await until { conversation.working }

        bridge.sendAnswer = #"{"ok":true}"#
        async let again = conversation.send("hello")
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, lost.request, "op-1, and no queue")
        XCTAssertNil(resent.request["queue"])
        bridge.say(Asks.refusal("turn_in_progress", "a turn is running"), id: resent.id)
        let second = await again
        XCTAssertFalse(second)
        XCTAssertEqual(conversation.composerNote, ConversationModel.turnStarted)

        async let queue = conversation.send("hello")
        let queued = await bridge.nextRequest()
        XCTAssertEqual(queued.request["op"]?.string, "op-2")
        XCTAssertEqual(queued.request["queue"], .bool(true))
        bridge.say(try Fixture.door("prompt_queued"), id: queued.id)
        let third = await queue
        XCTAssertTrue(third)
    }

    /// The same, across a drop: a row said while the prompt is out does not
    /// let it go, and the redial's row says the turn runs.
    func testAPromptLostToADropGoesAgainAsItWentAfterTheRedial() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("hello")
        let lost = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: false)))
        try await Task.sleep(for: .milliseconds(20))
        bridge.drop("lost: EOF")
        let first = await took
        XCTAssertFalse(first)

        bridge.openAnswer = #"{"door_id":"d-2"}"#
        async let back: Void = conversation.retry()
        try await opening.answer(working: true, door: "d-2")
        await back
        async let again = conversation.send("hello")
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, lost.request)
        bridge.say(try Fixture.door("prompt"), id: resent.id, door: "d-2")
        let second = await again
        XCTAssertTrue(second)
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// Once the row says the session is idle with nothing queued, the lost
    /// prompt has had its effect: the same words typed again are a new
    /// prompt, not op-1's old record.
    func testTheSameWordsAfterTheTurnEndedAreANewPrompt() async throws {
        let conversation = try await opened(working: false)
        bridge.sendAnswer = #"{"error":"no route","code":"send_failed"}"#
        let first = await conversation.send("continue")
        XCTAssertFalse(first)
        _ = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true)))
        try await until { conversation.working }
        bridge.say(try Fixture.door("said_turn_ended"))
        let reread = await bridge.nextRequest()
        bridge.say(try Fixture.door("session_transcript"), id: reread.id)
        try await until { !conversation.working }
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: false)))
        try await until { conversation.composerNote == nil }

        bridge.sendAnswer = #"{"ok":true}"#
        async let again = conversation.send("continue")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request["op"]?.string, "op-2")
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        let second = await again
        XCTAssertTrue(second)
    }

    /// A turn's end is not idle: the core starts the next queued prompt at
    /// once, and a queued prompt is not part of `working`. A held queued
    /// prompt stays held through the turn's end, a row between turns that
    /// still queues it, and the row of the turn it began.
    func testAHeldQueuedPromptStaysHeldThroughTheTurnsEnd() async throws {
        let conversation = try await opened(working: true)
        bridge.sendAnswer = #"{"error":"no route","code":"send_failed"}"#
        let first = await conversation.send("and then")
        XCTAssertFalse(first)
        let lost = await bridge.nextRequest()
        XCTAssertEqual(lost.request["queue"], .bool(true))
        bridge.say(try Fixture.door("said_turn_ended"))
        let reread = await bridge.nextRequest()
        bridge.say(try Fixture.door("session_transcript"), id: reread.id)
        try await until { !conversation.working }
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: false, queued: [Asks.queued("q-9", "and then")])))
        try await until { conversation.pending.lines.count == 1 }
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: true)))
        try await until { conversation.working }
        XCTAssertEqual(conversation.composerNote, ConversationModel.unreachableToSend(mac), "still held, so still here")

        bridge.sendAnswer = #"{"ok":true}"#
        async let again = conversation.send("and then")
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, lost.request, "op-1, queue: true")
        bridge.say(try Fixture.door("prompt_queued"), id: resent.id)
        _ = await again
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// A row saying the session is idle lets a lost prompt go too.
    func testTheSameWordsAfterAnIdleRowAreANewPrompt() async throws {
        let conversation = try await opened(working: true)
        bridge.sendAnswer = #"{"error":"no route","code":"send_failed"}"#
        let first = await conversation.send("continue")
        XCTAssertFalse(first)
        _ = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, working: false)))
        try await until { !conversation.working }
        XCTAssertNil(conversation.composerNote, "let go: the words are no longer the lost prompt's")

        bridge.sendAnswer = #"{"ok":true}"#
        async let again = conversation.send("continue")
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request["op"]?.string, "op-2")
        XCTAssertNil(sent.request["queue"])
        bridge.say(try Fixture.door("prompt"), id: sent.id)
        _ = await again
    }

    /// The Mac may have taken words whose reply does not read: they go
    /// again under the same op.
    func testAnUnreadableReplyKeepsTheWordsAndTheirOp() async throws {
        let conversation = try await opened(working: false)
        async let took = conversation.send("hello")
        let sent = await bridge.nextRequest()
        bridge.say(.object(["response": .object(["type": .string("mystery")])]), id: sent.id)
        let first = await took
        XCTAssertFalse(first)
        XCTAssertEqual(conversation.composerNote, ConversationModel.unreadableReply(mac))
        async let again = conversation.send("hello")
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, sent.request)
        bridge.say(try Fixture.door("prompt"), id: resent.id)
        _ = await again
    }

    /// A double tap on Take back sends one withdraw, and says nothing of
    /// "Already sent".
    func testASecondTakeBackOfALineWhileOneIsOutSendsNothing() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then")])
        async let words = conversation.takeBack("q-1")
        let sent = await bridge.nextRequest()
        let twice = await conversation.takeBack("q-1")
        XCTAssertNil(twice)
        bridge.say(try Fixture.door("withdraw"), id: sent.id)
        let back = await words
        XCTAssertEqual(back, "and then")
        XCTAssertNil(conversation.composerNote)
        XCTAssertEqual(bridge.sent.filter { $0.contains("session_withdraw") }.count, 1)
    }

    /// A steer turned down because another row is being steered goes at the
    /// turn's end like any refused steer, in the composer's words.
    func testASteerRefusedWhileAnotherIsSteeredGoesAtTheTurnsEnd() async throws {
        let conversation = try await opened(working: true)
        async let took = conversation.steer("stop")
        let prompt = await bridge.nextRequest()
        bridge.say(try Fixture.door("prompt_queued"), id: prompt.id)
        let steer = await bridge.nextRequest()
        bridge.say(Asks.refusal("steering", "another prompt is being steered"), id: steer.id)
        _ = await took
        XCTAssertEqual(conversation.pending.lines.first?.note, PendingLines.steerRefused)
    }

    /// A note this phone set stands while the row's steer for the line
    /// stays as it was, and goes once the row's steer moves on.
    func testTheRowClearsANoteOnceTheLinesSteerMovesOn() {
        var lines = PendingLines()
        lines.row([Self.queued("q-1", steer: "claimed")])
        lines.note("q-1", ConversationModel.tooLateToTakeBack)
        lines.row([Self.queued("q-1", steer: "claimed")])
        XCTAssertEqual(lines.lines.first?.note, ConversationModel.tooLateToTakeBack, "the row has not moved on")
        lines.row([Self.queued("q-1", steer: nil)])
        XCTAssertNil(lines.lines.first?.note, "requeued: nothing is being steered")

        lines.note("q-1", PendingLines.steering)
        lines.row([Self.queued("q-1", steer: "asked")])
        lines.row([Self.queued("q-1", steer: nil)])
        XCTAssertNil(lines.lines.first?.note)
    }

    func testTakeBackWithNoDoorSaysTheMacIsUnreachable() async throws {
        let conversation = try await opened(working: true, queued: [Asks.queued("q-1", "and then")])
        bridge.drop("lost: EOF")
        try await until { conversation.phase == .lost(ConversationModel.dropped(self.mac)) }
        let before = bridge.sent.count
        let back = await conversation.takeBack("q-1")
        XCTAssertNil(back)
        XCTAssertEqual(bridge.sent.count, before)
        XCTAssertEqual(conversation.composerNote, ConversationModel.unreachableToSend(mac))
        XCTAssertEqual(conversation.pending.lines.map(\.queueID), ["q-1"], "the line stays until the Mac says otherwise")
    }
}
