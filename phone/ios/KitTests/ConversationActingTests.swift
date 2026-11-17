import XCTest
@testable import DeskPhoneKit

@MainActor
final class ConversationActingTests: XCTestCase {
    private var bridge: FakeBridge!
    private var ids: Ids!
    private var opening: Opening { Opening(bridge: bridge) }
    private let mac = "Morgan's MacBook Pro"

    override func setUp() async throws {
        bridge = FakeBridge()
        ids = Ids()
    }

    private func opened(
        attention: JSONValue? = nil, oneRow: Bool = true,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in throw CancellationError() }
    ) async throws -> ConversationModel {
        let conversation = Conversations.model(bridge, ids: ids, sleep: sleep)
        async let open: Void = conversation.open()
        try await opening.answer(attention: attention, oneRow: oneRow)
        await open
        XCTAssertEqual(conversation.phase, .open)
        return conversation
    }

    /// `Opening` checks the hello asks `operate` alone, and the row read.
    func testTheConversationOpensAtOperateAndReadsItsRow() async throws {
        let conversation = try await opened()
        XCTAssertNil(conversation.card)
        XCTAssertEqual(bridge.sent.count, 3, "hello, transcript, session_get")
    }

    /// B3: a `session_get` reply naming another session is not this row:
    /// nothing of it is applied.
    func testARowReadNamingAnotherSessionIsNotApplied() async throws {
        let conversation = Conversations.model(bridge, ids: ids)
        async let open: Void = conversation.open()
        try await opening.greet()
        let asked = await bridge.nextRequest()
        let other = try Asks.row("00000000-0000-7000-8000-000000000002", attention: Asks.approvalAttention(), working: true)
        bridge.say(Asks.sessionReply(other), id: asked.id)
        await open
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertNil(conversation.card)
        XCTAssertFalse(conversation.working)
    }

    /// Review focus 1.
    func testAnApprovalParkedBeforeTheDialIsPinnedFromTheRowRead() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention())
        guard case .approval(let request)? = conversation.card?.ask else { return XCTFail("nothing pinned") }
        XCTAssertEqual(request.options.map(\.id), ["allow", "always", "deny"])
    }

    /// Review focus 1, from the recording.
    func testTheRecordedApprovalRowPinsItsCard() async throws {
        let attention = try XCTUnwrap(Fixture.door("session_attention_approval")["event"]?["session"]?["attention"])
        let conversation = try await opened(attention: attention)
        guard case .approval(let request)? = conversation.card?.ask else { return XCTFail("nothing pinned") }
        XCTAssertEqual(request.requestID, "call_fake_1#0")
        XCTAssertEqual(conversation.detail(for: request), "Edit README.md", "the call's line, as the recorded transcript has it")
    }

    /// F5, against a Desk without `session_get`: the recorded reply holds two
    /// environments; the row's organisation is in the one that is not the
    /// mothership, and that is the one read.
    func testTheRowReadAsksTheEnvironmentThatHoldsTheRowsOrganisation() async throws {
        let recorded = try XCTUnwrap(Fixture.door("sessions_list")["response"]?["sessions"]?.array?.first)
        let row = HomeRow(
            id: try XCTUnwrap(recorded["id"]?.string), organizationID: try XCTUnwrap(recorded["organization_id"]?.string),
            title: "Acted on from the phone", state: HomeRow.waitingOnYou, ago: "now", machineID: "recorded", macLabel: mac,
            recapLine: nil, lastEventAt: nil
        )
        XCTAssertEqual(row.organizationID, "00000000-0000-7000-8000-000000000008")
        let conversation = Conversations.model(bridge, row: row, ids: ids)
        async let open: Void = conversation.open()
        try await Opening(bridge: bridge, sessionID: row.id).greet(oneRow: false)
        let environments = await bridge.nextRequest()
        XCTAssertEqual(environments.request, .object(["type": .string("environments_list")]))
        bridge.say(try Fixture.door("environments_list"), id: environments.id)
        let list = await bridge.nextRequest()
        XCTAssertEqual(list.request, .object([
            "type": .string("sessions_list"), "environment_id": .string("00000000-0000-7000-8000-000000000006"),
        ]))
        bridge.say(try Fixture.door("sessions_list"), id: list.id)
        await open
        XCTAssertEqual(conversation.card?.ask.id, "call_fake_1#0")
    }

    /// Review focus 1.
    func testARowReadThatFailsLeavesTheConversationOpen() async throws {
        let conversation = Conversations.model(bridge, ids: ids)
        async let open: Void = conversation.open()
        try await opening.greet()
        let asked = await bridge.nextRequest()
        bridge.say(Asks.refusal("internal", "the store is busy"), id: asked.id)
        await open
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertNil(conversation.card)
        bridge.say(Asks.said(.object(["kind": .string("asked"), "request": Asks.approvalJSON("r-7")]), session: Conversations.row.id))
        try await until { conversation.card?.ask.id == "r-7" }
    }

    /// Review focus 1, against a Desk without `session_get`.
    func testTheEnvironmentIsAskedOnceAConversation() async throws {
        let conversation = try await opened(oneRow: false)
        bridge.openAnswer = #"{"door_id":"d-2"}"#
        async let again: Void = conversation.retry()
        try await opening.answer(environments: false, oneRow: false, door: "d-2")
        await again
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertEqual(bridge.sent.filter { $0.contains("environments_list") }.count, 1)
    }

    /// A row the door says while the read is out is newer than the read's
    /// reply, which is dropped.
    func testARowReadAnsweredAfterANewerRowIsDropped() async throws {
        let conversation = Conversations.model(bridge, ids: ids)
        async let open: Void = conversation.open()
        try await opening.greet()
        let asked = await bridge.nextRequest()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, attention: Asks.approvalAttention())))
        try await until { conversation.card?.ask.id == "r-1" }
        bridge.say(Asks.sessionReply(try Asks.row(Conversations.row.id)), id: asked.id)
        await open
        XCTAssertEqual(conversation.card?.ask.id, "r-1", "the read was older than the row the door said")
    }

    func testARowSaidLiveSetsTheCard() async throws {
        let conversation = try await opened()
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id, attention: Asks.formAttention())))
        try await until { conversation.card?.ask.id == "e-1" }
        bridge.say(Asks.rowEvent(try Asks.row(Conversations.row.id)))
        try await until { conversation.card == nil }
        XCTAssertNil(conversation.cleared)
    }

    func testAllowOnceIsSentAndItsOkClearsTheCard() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention())
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request, .object([
            "type": .string("session_answer"), "session_id": .string(Conversations.row.id),
            "request_id": .string("r-1"), "option_id": .string("allow"), "op": .string("op-1"),
        ]))
        XCTAssertEqual(conversation.card?.sending, true)
        bridge.say(try Fixture.door("answer"), id: sent.id)
        await answered
        XCTAssertNil(conversation.card)
        XCTAssertNil(conversation.cleared)
    }

    func testAnotherDevicesAnswerSaidLiveClearsTheCardWithANote() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention())
        bridge.say(Asks.said(Asks.answered("r-1", option: "deny"), session: Conversations.row.id))
        try await until { conversation.card == nil }
        XCTAssertEqual(conversation.cleared, "Answered on another device")
    }

    /// Review focus 2.
    func testAnAnswerLostToADropIsResentWithItsOpAfterTheRedial() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention(), sleep: { _ in await Task.yield() })
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        XCTAssertEqual(conversation.card?.owed, true)
        XCTAssertEqual(conversation.card?.note, "Couldn't reach \(mac) — trying again")

        try await opening.answer(attention: Asks.approvalAttention())
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, sent.request, "the same answer, under the same op")
        bridge.say(try Fixture.door("answer"), id: resent.id)
        try await until { conversation.card == nil }
        XCTAssertNil(conversation.cleared, "the Mac's record says it was this phone's")
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// Review focus 2: a redial whose door drops too is not the last try
    /// while the answer is owed.
    func testAnOwedAnswerRedialsAgainWhenTheRedialDrops() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention(), sleep: { _ in await Task.yield() })
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request["type"]?.string, "hello")
        bridge.drop("lost: EOF")

        try await opening.answer(attention: Asks.approvalAttention())
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, sent.request)
        bridge.say(try Fixture.door("answer"), id: resent.id)
        try await until { conversation.card == nil }
        XCTAssertEqual(conversation.phase, .open)
        XCTAssertNil(conversation.cleared)
    }

    /// A redial the Mac refuses is not tried again by itself: the owed card
    /// stops saying "trying again" and says Retry sends it. Retry's own try
    /// says trying again, and sends the answer under its op.
    func testAnOwedAnswerWhoseRedialIsRefusedWaitsForRetryAndSaysSo() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention(), sleep: { _ in await Task.yield() })
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request["type"]?.string, "hello")
        bridge.say(Asks.refusal("internal", "the store is busy"), id: hello.id)
        try await until { conversation.phase == .refused("Couldn't open this session on \(self.mac).") }
        XCTAssertEqual(conversation.card?.owed, true)
        XCTAssertEqual(conversation.card?.note, "Your answer hasn't gone yet. Retry sends it.")
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(bridge.calls.filter { $0.hasPrefix("open ") }.count, 2, "nothing dials again by itself")

        async let retried: Void = conversation.retry()
        let again = await bridge.nextRequest()
        XCTAssertEqual(conversation.phase, .connecting)
        XCTAssertEqual(conversation.card?.note, "Couldn't reach \(mac) — trying again")
        bridge.say(try Fixture.door("hello"), id: again.id)
        let transcript = await bridge.nextRequest()
        bridge.say(try Fixture.door("session_transcript"), id: transcript.id)
        try await opening.answerRowRead(attention: Asks.approvalAttention())
        let resent = await bridge.nextRequest()
        XCTAssertEqual(resent.request, sent.request, "the same answer, under the same op")
        bridge.say(try Fixture.door("answer"), id: resent.id)
        await retried
        XCTAssertNil(conversation.card)
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// A conversation left (the app went to the background) with an answer
    /// out keeps it owed for the next open, and dials nothing by itself.
    func testALeftConversationKeepsItsAnswerOwedAndDoesNotRedial() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention(), sleep: { _ in await Task.yield() })
        async let answered: Void = conversation.answer(.option("allow"))
        _ = await bridge.nextRequest()
        conversation.leave()
        bridge.drop("closed")
        await answered
        XCTAssertEqual(conversation.card?.owed, true)
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(bridge.calls.filter { $0.hasPrefix("open ") }.count, 1, "no dial after leaving")
    }

    /// An owed answer on a door that opens and drops again waits longer each
    /// time; it is not dialled every second for ever.
    func testAnOwedAnswerOnADoorThatKeepsDroppingBacksOff() async throws {
        let delays = Delays()
        let conversation = try await opened(attention: Asks.approvalAttention(), sleep: { delay in
            await delays.add(delay)
            await Task.yield()
        })
        async let answered: Void = conversation.answer(.option("allow"))
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        await answered
        try await opening.answer(attention: Asks.approvalAttention())
        _ = await bridge.nextRequest()
        bridge.drop("lost: EOF")
        try await opening.answer(attention: Asks.approvalAttention())
        let resent = await bridge.nextRequest()
        bridge.say(try Fixture.door("answer"), id: resent.id)
        try await until { conversation.card == nil }
        let waited = await delays.all
        XCTAssertEqual(waited, [.seconds(1), .seconds(2)])
    }

    /// A call the transcript holds with no title draws the agent's input:
    /// the Desk, holding no title either, names the tool "a tool".
    func testATitlelessCallsDetailIsItsInput() async throws {
        let conversation = try await opened()
        bridge.say(Asks.said(.object([
            "kind": .string("tool_call"), "call_id": .string("call-1"), "name": .string("Bash"),
            "title": .null, "tool_kind": .string("execute"), "status": .string("pending"),
            "settled": .bool(false), "parent_call_id": .null,
        ]), session: Conversations.row.id, turn: conversation.fold.lastTurn ?? 1))
        try await until { conversation.fold.turns.last?.lines.last?.id == "call-1" }
        let request: PermissionRequest = try Asks.approvalJSON(tool: "a tool").decode()
        XCTAssertEqual(conversation.detail(for: request), #"{"command":"cargo test -p desk_core"}"#)
    }

    /// Review focus 2, ruling 2.
    func testARefusedAnswerWhoseQuestionIsGoneSaysAnsweredElsewhere() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention())
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        bridge.say(try Fixture.door("answer_refused"), id: sent.id)
        try await opening.answerRowRead(attention: nil)
        await answered
        XCTAssertNil(conversation.card)
        XCTAssertEqual(conversation.cleared, "Answered on another device")
    }

    /// Review focus 2, ruling 2.
    func testARefusedAnswerWhoseQuestionStandsShowsTheRefusal() async throws {
        let conversation = try await opened(attention: Asks.approvalAttention())
        async let answered: Void = conversation.answer(.option("allow"))
        let sent = await bridge.nextRequest()
        let refusal = try Fixture.door("answer_refused")
        bridge.say(refusal, id: sent.id)
        try await opening.answerRowRead(attention: Asks.approvalAttention())
        await answered
        XCTAssertEqual(conversation.card?.note, refusal["response"]?["message"]?.string)
        XCTAssertNil(conversation.card?.answer)
    }

    func testAQuestionIsAnsweredWithItsFormsContent() async throws {
        let conversation = try await opened(attention: Asks.questionAttention())
        async let answered: Void = conversation.answer(.accept(.object(["why": .string("tidy")])))
        let sent = await bridge.nextRequest()
        XCTAssertEqual(sent.request["type"]?.string, "session_question_answer")
        XCTAssertEqual(sent.request["question_id"]?.string, "q-1")
        XCTAssertEqual(sent.request["content"], .object(["why": .string("tidy")]))
        bridge.say(.object(["response": .object(["type": .string("ok")])]), id: sent.id)
        await answered
        XCTAssertNil(conversation.card)
    }

    /// Review fix 8: the request's `tool` is the call's whole ACP title, so
    /// before the transcript holds the call it is the detail, uncut, and
    /// Show all shows the whole command. Not the Desk's "a tool", and not an
    /// MCP call's bare tool name, whose arguments say more.
    func testTheApprovalsDetailIsTheCallsLineElseItsTitleElseItsInput() async throws {
        let conversation = try await opened()
        let command = (["cd desk"] + (1...40).map { "cargo test -p desk_core --lib recorded_\($0)" }).joined(separator: " && \\\n  ")
        XCTAssertGreaterThan(command.count, 1000)
        let titled: PermissionRequest = try Asks.approvalJSON(tool: command).decode()
        XCTAssertEqual(conversation.detail(for: titled), command, "the whole title, uncut")
        let untitled: PermissionRequest = try Asks.approvalJSON(tool: "a tool").decode()
        XCTAssertEqual(conversation.detail(for: untitled), #"{"command":"cargo test -p desk_core"}"#)
        let mcp: PermissionRequest = try Asks.approvalJSON(tool: "list_issues", server: "linear").decode()
        XCTAssertEqual(mcp.server, "linear")
        XCTAssertEqual(conversation.detail(for: mcp), #"{"command":"cargo test -p desk_core"}"#)
        let request = Asks.approval()
        bridge.say(Asks.said(.object([
            "kind": .string("tool_call"), "call_id": .string("call-1"), "name": .string("Bash"),
            "title": .string("cargo test -p desk_core"), "tool_kind": .string("execute"), "status": .string("pending"),
            "settled": .bool(false), "parent_call_id": .null,
        ]), session: Conversations.row.id, turn: conversation.fold.lastTurn ?? 1))
        try await until { conversation.detail(for: request) == "cargo test -p desk_core" }
    }

    func testTheAsleepScreenSaysWhatTheRowWaitsOn() async {
        let conversation = Conversations.model(bridge, ids: ids, live: { false })
        await conversation.open()
        XCTAssertEqual(conversation.asleepLines.last, "Waiting on you: Wants to execute")
        conversation.leave()
    }

    /// No ask, but the server's tag: the screen says the tag's reason.
    func testTheAsleepScreenSaysWhyATaggedRowWaitsOnYou() async {
        let tagged = HomeRow(
            id: Conversations.row.id, organizationID: "o-1", title: "Phone pairing spec", state: "idle", ago: "now",
            machineID: "recorded", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: nil,
            waitingOn: "person", waitingReason: "Should the web filter show only your own threads?"
        )
        let conversation = Conversations.model(bridge, row: tagged, ids: ids, live: { false })
        await conversation.open()
        XCTAssertEqual(conversation.asleepLines.last, "Waiting on you: Should the web filter show only your own threads?")
        conversation.leave()
    }
}
