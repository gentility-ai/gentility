import XCTest
@testable import DeskPhoneKit

final class DoorWireTests: XCTestCase {
    /// Pre-existing: this asserted minor 43 against a recording that moves
    /// with the core. The contract is the major; the minor only climbs.
    func testTheRecordedHelloSaysTheProtocolTheFeaturesAndWhatWasAccepted() throws {
        guard case .reply(let id, .hello(let hello)) = try DoorFrame(Fixture.doorText("hello")) else {
            return XCTFail("not a hello reply")
        }
        XCTAssertEqual(id, 1)
        XCTAssertEqual(hello.protocolVersion?.major, 1)
        XCTAssertGreaterThanOrEqual(hello.protocolVersion?.minor ?? 0, 47)
        XCTAssertTrue(hello.features.contains("transcript_paging"))
        XCTAssertEqual(hello.accepted, ["operate"])
    }

    func testTheRecordedHelloOffersTheFastOpenWordsAndNamesItsStore() throws {
        guard case .reply(_, .hello(let hello)) = try DoorFrame(Fixture.doorText("hello")) else {
            return XCTFail("not a hello reply")
        }
        for word in ["transcript_brief", "transcript_seq", "session_get"] {
            XCTAssertTrue(hello.features.contains(word), word)
        }
        XCTAssertEqual(hello.host?.storeID, "recorded")
    }

    func testTheRecordedBriefTranscriptHasNoThoughtsAndSlimToolCalls() throws {
        guard case .reply(_, .transcript(let brief)) = try DoorFrame(Fixture.doorText("session_transcript_brief")) else {
            return XCTFail("not a transcript reply")
        }
        XCTAssertFalse(brief.items.contains { $0.kind == "thought" })
        XCTAssertFalse(brief.items.contains { $0.parentCallID != nil })
        let call = try XCTUnwrap(brief.items.first { $0.kind == "tool_call" }?.toolCall)
        XCTAssertEqual(call.name, "Edit")
        XCTAssertEqual(call.status, "executed")
        XCTAssertNil(call.kind, "brief keeps no kind")
        XCTAssertNil(call.startedAt, "brief keeps no instants")
        XCTAssertNotNil(brief.throughSeq)
        XCTAssertFalse(brief.reset)
    }

    func testTheRecordedAfterSeqReplyIsACursorAndNoRows() throws {
        guard case .reply(_, .transcript(let changed)) = try DoorFrame(Fixture.doorText("session_transcript_after_seq")) else {
            return XCTFail("not a transcript reply")
        }
        XCTAssertEqual(changed.items, [])
        // The recording owns the number (its seqs are ranked per session); the contract is that a cursor came back.
        XCTAssertGreaterThan(try XCTUnwrap(changed.throughSeq), 0)
        XCTAssertFalse(changed.reset)
        XCTAssertNil(changed.fromTurn)
    }

    func testAResetReadsAsTrue() throws {
        let frame = try DoorFrame(#"{"id":4,"response":{"type":"transcript","items":[],"from_turn":3,"earlier":2,"through_seq":40,"reset":true}}"#)
        guard case .reply(4, .transcript(let page)) = frame else { return XCTFail("not a transcript") }
        XCTAssertTrue(page.reset)
        XCTAssertEqual(page.fromTurn, 3)
    }

    func testTheRecordedOneRowReplyAndItsRefusal() throws {
        guard case .reply(_, .session(let row)) = try DoorFrame(Fixture.doorText("session_get")) else {
            return XCTFail("not a session reply")
        }
        XCTAssertEqual(row.title, "Recorded for the phone")
        XCTAssertEqual(row.id, "00000000-0000-7000-8000-000000000001")
        guard case .reply(_, .error(let refusal)) = try DoorFrame(Fixture.doorText("session_get_unknown_session")) else {
            return XCTFail("not a refusal")
        }
        XCTAssertEqual(refusal.code, "unknown_session")
    }

    func testALagIsAnEventOfItsOwn() throws {
        XCTAssertEqual(try DoorFrame(#"{"event":{"type":"lagged","missed":12}}"#), .event(.lagged(missed: 12)))
        XCTAssertEqual(try DoorFrame(#"{"event":{"type":"lagged"}}"#), .event(.lagged(missed: 0)))
    }

    func testTheFastOpenRequestsAreTheFramesTheCoreReads() throws {
        XCTAssertEqual(
            try DoorRequest.sessionTranscript(sessionID: "s1", fromTurn: nil, lastTurns: 20, shape: .brief).frame(id: 2),
            #"{"id":2,"request":{"last_turns":20,"session_id":"s1","shape":"brief","type":"session_transcript"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionTranscript(sessionID: "s1", fromTurn: nil, lastTurns: nil, shape: .brief, afterSeq: 22).frame(id: 3),
            #"{"id":3,"request":{"after_seq":22,"session_id":"s1","shape":"brief","type":"session_transcript"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionGet(sessionID: "s1").frame(id: 6),
            #"{"id":6,"request":{"session_id":"s1","type":"session_get"}}"#
        )
        XCTAssertEqual(DoorConnection.patience(for: .sessionGet(sessionID: "s1")), .seconds(10))
    }

    func testAPeerPathSaysItselfInTheSpecsWords() throws {
        let direct = try Wire.decoder.decode(PeerPath.self, from: Data(#"{"path":"direct","via":"[fd8c:1::5]:41641"}"#.utf8))
        XCTAssertEqual(direct.description, "direct [fd8c:1::5]:41641")
        let derp = try Wire.decoder.decode(PeerPath.self, from: Data(#"{"path":"derp","relay":"syd"}"#.utf8))
        XCTAssertEqual(derp.description, "derp syd")
        let relay = try Wire.decoder.decode(PeerPath.self, from: Data(#"{"path":"peer_relay"}"#.utf8))
        XCTAssertEqual(relay.description, "peer_relay")
        XCTAssertEqual(PeerPath(path: "unknown", via: nil, relay: nil).description, "unknown")
    }

    func testTheRecordedTranscriptHoldsThePersonsWordsAndAToolCall() throws {
        guard case .reply(2, .transcript(let transcript)) = try DoorFrame(Fixture.doorText("session_transcript")) else {
            return XCTFail("not a transcript reply")
        }
        XCTAssertTrue(transcript.items.contains { $0.kind == "message" && $0.role == "user" })
        XCTAssertTrue(transcript.items.contains { $0.kind == "tool_call" })
        XCTAssertEqual(Set(transcript.items.map(\.sessionID)).count, 1)
    }

    /// A tool line drawn live is named by the provider's call id; the same
    /// call read back off the transcript must be findable by that id too.
    func testATranscriptToolCallCarriesTheProvidersCallIDTheLiveMomentDoes() throws {
        guard case .reply(_, .transcript(let transcript)) = try DoorFrame(Fixture.doorText("session_transcript")),
              case .event(.said(_, _, .toolCall(let liveCallID, _, _, _, _, _))) = try DoorFrame(Fixture.doorText("said_tool_call")) else {
            return XCTFail("not a transcript and a tool call")
        }
        let item = try XCTUnwrap(transcript.items.first { $0.kind == "tool_call" })
        let call = try XCTUnwrap(item.toolCall)
        XCTAssertEqual(call.providerToolCallID, liveCallID)
        XCTAssertNotEqual(item.id, liveCallID)
        XCTAssertEqual(call.name, "Edit")
        XCTAssertEqual(call.title, "Edit README.md")
        XCTAssertEqual(call.status, "executed")
        XCTAssertNil(call.parentCallID)
        XCTAssertNil(transcript.items.first { $0.kind == "message" }?.toolCall)
    }

    /// The row's instant has microseconds; the tool call's own has none.
    func testBothSpellingsOfAnInstantRead() throws {
        guard case .reply(_, .transcript(let transcript)) = try DoorFrame(Fixture.doorText("session_transcript")) else {
            return XCTFail("not a transcript reply")
        }
        let item = try XCTUnwrap(transcript.items.first { $0.kind == "tool_call" })
        let call = try XCTUnwrap(item.toolCall)
        let created = try XCTUnwrap(item.created)
        XCTAssertEqual(call.startedAt, created)
        XCTAssertEqual(call.endedAt, created)
    }

    func testARefusalIsItsCodeAndSentence() throws {
        guard case .reply(3, .error(let refusal)) = try DoorFrame(Fixture.doorText("session_transcript_unknown_session")) else {
            return XCTFail("not a refusal")
        }
        XCTAssertEqual(refusal.code, "unknown_session")
        XCTAssertFalse(refusal.message.isEmpty)
    }

    func testTheRecordedMomentsDecode() throws {
        guard case .event(.said(_, _, .text(let messageID, let tail, _, _))) = try DoorFrame(Fixture.doorText("said_text")) else {
            return XCTFail("not text")
        }
        XCTAssertFalse(messageID.isEmpty)
        XCTAssertFalse(tail.isEmpty)

        guard case .event(.said(_, _, .toolCall(let callID, let name, _, let status, _, _))) = try DoorFrame(Fixture.doorText("said_tool_call")) else {
            return XCTFail("not a tool call")
        }
        XCTAssertFalse(callID.isEmpty)
        XCTAssertFalse(name.isEmpty)
        XCTAssertFalse(status.isEmpty)

        guard case .event(.said(_, _, .turnEnded(let outcome, _))) = try DoorFrame(Fixture.doorText("said_turn_ended")) else {
            return XCTFail("not a turn's end")
        }
        XCTAssertFalse(outcome.isEmpty)
    }

    func testTheRecordedRowDecodes() throws {
        guard case .event(.session(let row)) = try DoorFrame(Fixture.doorText("session")) else {
            return XCTFail("not a row")
        }
        XCTAssertEqual(row.title, "Recorded for the phone")
    }

    func testWordsThisAppDoesNotKnowAreKeptNotRefused() throws {
        XCTAssertEqual(try DoorFrame(#"{"event":{"type":"projects_changed"}}"#), .event(.other(type: "projects_changed")))
        XCTAssertEqual(
            try DoorFrame(#"{"event":{"type":"said","session_id":"s","turn":2,"said":{"kind":"plan","entries":[],"revision":1,"parent_call_id":null}}}"#),
            .event(.said(sessionID: "s", turn: 2, said: .other(kind: "plan")))
        )
        XCTAssertEqual(try DoorFrame(#"{"id":9,"response":{"type":"settings"}}"#), .reply(id: 9, .other(type: "settings")))
        XCTAssertThrowsError(try DoorFrame("[1,2,3]"))
    }

    func testRequestsAreTheFramesTheCoreReads() throws {
        XCTAssertEqual(
            try DoorRequest.hello(wants: ["operate"]).frame(id: 1),
            #"{"id":1,"request":{"type":"hello","wants":["operate"]}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionTranscript(sessionID: "s1", fromTurn: nil, lastTurns: 20).frame(id: 2),
            #"{"id":2,"request":{"last_turns":20,"session_id":"s1","type":"session_transcript"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionTranscript(sessionID: "s1", fromTurn: 4, lastTurns: nil).frame(id: 3),
            #"{"id":3,"request":{"from_turn":4,"session_id":"s1","type":"session_transcript"}}"#
        )
    }

    func testTheVerbsAreTheFramesTheCoreReads() throws {
        let s = "s1"
        XCTAssertEqual(try DoorRequest.environmentsList.frame(id: 4), #"{"id":4,"request":{"type":"environments_list"}}"#)
        XCTAssertEqual(
            try DoorRequest.sessionsList(environmentID: "e-1").frame(id: 5),
            #"{"id":5,"request":{"environment_id":"e-1","type":"sessions_list"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionAnswer(sessionID: s, requestID: "call_fake_1#0", optionID: "allow", op: "op-1").frame(id: 6),
            #"{"id":6,"request":{"op":"op-1","option_id":"allow","request_id":"call_fake_1#0","session_id":"s1","type":"session_answer"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionElicitationAnswer(
                sessionID: s, requestID: "e-1", action: .accept,
                content: .object(["branch": .string("main"), "count": .number(3), "force": .bool(false)]), op: "op-2"
            ).frame(id: 7),
            #"{"id":7,"request":{"action":"accept","content":{"branch":"main","count":3,"force":false},"op":"op-2","request_id":"e-1","session_id":"s1","type":"session_elicitation_answer"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionElicitationAnswer(sessionID: s, requestID: "e-1", action: .decline, content: nil, op: "op-3").frame(id: 8),
            #"{"id":8,"request":{"action":"decline","op":"op-3","request_id":"e-1","session_id":"s1","type":"session_elicitation_answer"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionQuestionAnswer(sessionID: s, questionID: "q-1", action: .accept, content: .object(["why": .string("tidy")]), op: "op-4").frame(id: 9),
            #"{"id":9,"request":{"action":"accept","content":{"why":"tidy"},"op":"op-4","question_id":"q-1","session_id":"s1","type":"session_question_answer"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionPrompt(sessionID: s, text: "make it three minutes", queue: false, op: "op-5").frame(id: 10),
            #"{"id":10,"request":{"op":"op-5","session_id":"s1","text":"make it three minutes","type":"session_prompt"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionPrompt(sessionID: s, text: "and then", queue: true, op: "op-6").frame(id: 11),
            #"{"id":11,"request":{"op":"op-6","queue":true,"session_id":"s1","text":"and then","type":"session_prompt"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionSteer(sessionID: s, queueID: "queue-1").frame(id: 12),
            #"{"id":12,"request":{"queue_id":"queue-1","session_id":"s1","type":"session_steer"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.sessionWithdraw(sessionID: s, queueID: "queue-1").frame(id: 13),
            #"{"id":13,"request":{"queue_id":"queue-1","session_id":"s1","type":"session_withdraw"}}"#
        )
    }

    func testEachVerbWaitsAsLongAsItsReplyCanTake() {
        XCTAssertEqual(DoorConnection.patience(for: .sessionsList(environmentID: "e")), .seconds(30))
        XCTAssertEqual(DoorConnection.patience(for: .sessionPrompt(sessionID: "s", text: "t", queue: false, op: "o")), .seconds(15))
        XCTAssertEqual(DoorConnection.patience(for: .sessionAnswer(sessionID: "s", requestID: "r", optionID: "o", op: "o")), .seconds(10))
        XCTAssertEqual(DoorConnection.patience(for: .environmentsList), .seconds(10))
    }

    // MARK: the Unblock slice's frames

    func testTheRecordedApprovalMomentCarriesTheRequestAndItsOptions() throws {
        guard case .event(.said(_, _, .asked(let request))) = try DoorFrame(Fixture.doorText("said_asked")) else {
            return XCTFail("not asked")
        }
        XCTAssertEqual(request.requestID, "call_fake_1#0")
        XCTAssertEqual(request.callID, "call_fake_1")
        XCTAssertEqual(request.tool, "Edit README.md")
        XCTAssertEqual(request.kind, "edit")
        XCTAssertFalse(request.inputDropped)
        XCTAssertNotNil(request.input)
        XCTAssertEqual(request.options.map(\.id), ["allow-once", "allow-always", "reject"])
        XCTAssertEqual(request.options.map(\.kind), ["allow_once", "allow_always", "reject_once"])
        XCTAssertEqual(request.options.map(\.label), ["Yes", "Yes, and don't ask again", "No"])
        XCTAssertEqual(request.options.map(\.rejects), [false, false, true])
        XCTAssertEqual(request.summary, "Wants to edit", "the kind alone: the tool is the call's title")
    }

    /// Server ruling FR1: `tool` is the ACP call's title, for Bash the whole
    /// command, so the card's heading is built from the kind alone, as the
    /// push body and Home's line are.
    func testAnApprovalsHeadingNamesItsKindAndNeverItsTool() throws {
        func request(tool: String, kind: String) throws -> PermissionRequest {
            var json = Asks.approvalJSON()
            if case .object(var fields) = json {
                fields["tool"] = .string(tool)
                fields["kind"] = .string(kind)
                json = .object(fields)
            }
            return try json.decode()
        }
        let command = "cd desk && cargo test -p desk_core \\\n  --lib recorded 2>&1 | tee /tmp/core.log"
        XCTAssertEqual(try request(tool: command, kind: "execute").summary, "Wants to execute")
        for kind in ["read", "edit", "delete", "move", "search", "fetch"] {
            XCTAssertEqual(try request(tool: "Bash", kind: kind).summary, "Wants to \(kind)")
        }
        XCTAssertEqual(try request(tool: command, kind: "other").summary, "Wants approval")
        XCTAssertEqual(try request(tool: "Think", kind: "think").summary, "Wants approval")
        XCTAssertEqual(try request(tool: "Bash", kind: "").summary, "Wants approval")
    }

    func testTheRecordedAnswerNamesTheQuestionTheOptionAndWhoGaveIt() throws {
        guard case .event(.said(_, _, .asked(let asked))) = try DoorFrame(Fixture.doorText("said_asked")),
              case .event(.said(_, _, .answered(let requestID, let optionID, let by))) = try DoorFrame(Fixture.doorText("said_answered")) else {
            return XCTFail("not asked and answered")
        }
        XCTAssertEqual(requestID, asked.requestID)
        XCTAssertEqual(optionID, "allow-once")
        XCTAssertTrue(asked.options.contains { $0.id == optionID })
        XCTAssertEqual(by, "person")
    }

    func testTheRecordedUnansweredNamesItsQuestion() throws {
        guard case .event(.said(_, _, .unanswered(let requestID))) = try DoorFrame(Fixture.doorText("said_unanswered")) else {
            return XCTFail("not unanswered")
        }
        XCTAssertEqual(requestID, "call_fake_1#1")
    }

    func testTheRecordedFormQuestionHasItsFields() throws {
        guard case .event(.said(_, _, .elicited(let elicitation))) = try DoorFrame(Fixture.doorText("said_elicited")),
              case .form(let title, let fields) = elicitation.mode else {
            return XCTFail("not a form")
        }
        XCTAssertEqual(elicitation.requestID, "elicitation#0")
        XCTAssertNil(elicitation.callID)
        XCTAssertFalse(elicitation.message.isEmpty)
        XCTAssertEqual(title, "Report settings")
        XCTAssertEqual(fields.map(\.name), ["count", "email", "note", "notify"])
        XCTAssertEqual(fields.map(\.label), ["Copies", "Email", "Note", "Notify me"])
        XCTAssertEqual(fields.map(\.required), [true, true, false, false])
        XCTAssertEqual(fields[2].description, "Anything else")
        // The core writes every number as an f64: `1.0`, `5.0`, even with `integer`.
        XCTAssertEqual(fields.map(\.kind), [
            .number(integer: true, min: 1, max: 5, defaultValue: 1),
            .text(format: "email", minLength: nil, maxLength: 120, pattern: nil, defaultValue: nil),
            .text(format: nil, minLength: nil, maxLength: nil, pattern: nil, defaultValue: nil),
            .boolean(defaultValue: false),
        ])
    }

    func testTheRecordedLinkQuestionHasItsURL() throws {
        guard case .event(.said(_, _, .elicited(let elicitation))) = try DoorFrame(Fixture.doorText("said_elicited_url")),
              case .url(let url, let signIn) = elicitation.mode else {
            return XCTFail("not a link")
        }
        XCTAssertEqual(elicitation.requestID, "elicitation-mcp-fake-1#0")
        XCTAssertEqual(url, "https://github.com/settings/keys/new?title=fake")
        XCTAssertNotNil(URL(string: url))
        XCTAssertNil(signIn)
    }

    func testTheRecordedDecisionSaysItsOutcome() throws {
        guard case .event(.said(_, _, .elicitationDecided(let requestID, let outcome))) = try DoorFrame(Fixture.doorText("said_elicitation_decided")) else {
            return XCTFail("not decided")
        }
        XCTAssertEqual(requestID, "elicitation#0")
        XCTAssertEqual(outcome, "accepted")
    }

    func testTheRecordedRowsSayWhatEachWaitsOn() throws {
        guard case .event(.session(let approval)) = try DoorFrame(Fixture.doorText("session_attention_approval")),
              case .approval(let request)? = approval.attention else {
            return XCTFail("no approval on the row")
        }
        XCTAssertEqual(request.requestID, "call_fake_1#0")
        XCTAssertEqual(approval.waiting, 1)
        XCTAssertEqual(approval.organizationID, "00000000-0000-7000-8000-000000000008")
        XCTAssertEqual(approval.queued, [])

        guard case .event(.session(let elicitation)) = try DoorFrame(Fixture.doorText("session_attention_elicitation")),
              case .elicitation(let asked)? = elicitation.attention else {
            return XCTFail("no elicitation on the row")
        }
        XCTAssertEqual(asked.requestID, "elicitation#0")
        XCTAssertEqual(elicitation.waiting, 1)

        guard case .event(.session(let question)) = try DoorFrame(Fixture.doorText("session_attention_question")),
              case .question(let questionID, let expiresAt, let form)? = question.attention else {
            return XCTFail("no question on the row")
        }
        XCTAssertEqual(questionID, "00000000-0000-7000-8000-000000000016")
        XCTAssertEqual(expiresAt, "2099-01-01T00:00:00.000000Z")
        XCTAssertEqual(form.requestID, questionID)
        guard case .form(_, let fields) = form.mode else { return XCTFail("a question is always a form") }
        XCTAssertEqual(fields.map(\.kind), [.choice(options: [
            ChoiceOption(value: "ship", title: "ship", description: nil),
            ChoiceOption(value: "wait", title: "wait", description: nil),
        ], defaultValue: nil)])

        guard case .event(.session(let plain)) = try DoorFrame(Fixture.doorText("session")) else {
            return XCTFail("not a row")
        }
        XCTAssertNil(plain.attention)
        XCTAssertEqual(plain.waiting, 0)
        XCTAssertEqual(plain.queued, [])
    }

    func testTheRecordedRepliesToActing() throws {
        for name in ["answer", "elicitation_answer", "prompt", "steer", "withdraw"] {
            guard case .reply(_, .ok) = try DoorFrame(Fixture.doorText(name)) else {
                return XCTFail("\(name) is not ok")
            }
        }
        guard case .reply(_, .queued(let queueID)) = try DoorFrame(Fixture.doorText("prompt_queued")) else {
            return XCTFail("not queued")
        }
        XCTAssertEqual(queueID, "00000000-0000-7000-8000-000000000012")
        guard case .reply(_, .error(let answer)) = try DoorFrame(Fixture.doorText("answer_refused")),
              case .reply(_, .error(let steer)) = try DoorFrame(Fixture.doorText("steer_refused")) else {
            return XCTFail("not refusals")
        }
        XCTAssertEqual(answer.code, "harness")
        XCTAssertEqual(steer.code, "unknown_option")
    }

    /// The recorded Mac has two environments: a signed-in one holding the
    /// rows' organisation, and the built-in mothership, which holds none.
    func testTheRecordedReadsOfEnvironmentsAndRows() throws {
        guard case .reply(_, .environments(let environments)) = try DoorFrame(Fixture.doorText("environments_list")) else {
            return XCTFail("not environments")
        }
        XCTAssertEqual(environments.map(\.id), ["00000000-0000-7000-8000-000000000006", "00000000-0000-7000-8000-000000000010"])
        XCTAssertEqual(environments.map(\.mothership), [false, true])
        XCTAssertEqual(environments[0].organizations.map(\.id), ["00000000-0000-7000-8000-000000000008"])
        XCTAssertEqual(environments[1].organizations, [])

        guard case .reply(_, .sessions(let rows)) = try DoorFrame(Fixture.doorText("sessions_list")) else {
            return XCTFail("not sessions")
        }
        XCTAssertEqual(rows.map(\.title), ["Acted on from the phone", "Recorded for the phone"])
        XCTAssertEqual(rows.map(\.status), ["open", "closed"])
        guard case .approval(let request)? = rows[0].attention else { return XCTFail("the open row waits on no approval") }
        XCTAssertEqual(request.requestID, "call_fake_1#0")
        XCTAssertNil(rows[1].attention)
    }

    /// One transcript item this app cannot read does not cost the page around it.
    func testAnUnreadableTranscriptItemIsSkippedAndThePageKept() throws {
        let good = #"{"id":"m1","session_id":"s","ordinal":1,"kind":"message","role":"user","content":"go","created_at":"2026-01-01T00:00:00Z"}"#
        let bad = #"{"id":"m2","session_id":"s","kind":"message"}"#
        let frame = try DoorFrame(#"{"id":4,"response":{"type":"transcript","items":[\#(bad),\#(good)],"earlier":0}}"#)
        guard case .reply(4, .transcript(let page)) = frame else { return XCTFail("not a transcript") }
        XCTAssertEqual(page.items.map(\.id), ["m1"])
    }

    /// One row this app cannot read does not take the others with it.
    func testAnUnreadableRowIsSkippedAndTheRestOfTheListKept() throws {
        guard case .object(var recorded)? = try Fixture.door("sessions_list")["response"]?["sessions"]?.array?.first else {
            return XCTFail("no recorded row")
        }
        var unreadable = recorded
        unreadable["title"] = nil
        recorded["id"] = .string("s-kept")
        let reply = JSONValue.object(["type": .string("sessions"), "sessions": .array([.object(unreadable), .object(recorded)])])
        guard case .sessions(let rows) = try reply.decode(DoorResponse.self) else { return XCTFail("not sessions") }
        XCTAssertEqual(rows.map(\.id), ["s-kept"])
    }

    func testAReplyIsLoggedByItsKindNeverItsContent() throws {
        let row = try XCTUnwrap(Fixture.door("sessions_list")["response"]?["sessions"]?.array?.first)
        let queued = Asks.queued("q-1", "the person's own words")
        guard case .object(var fields) = row else { return XCTFail("not a row") }
        fields["queued"] = .array([queued])
        let reply = try JSONValue.object(["type": .string("sessions"), "sessions": .array([.object(fields)])]).decode(DoorResponse.self)
        XCTAssertEqual(reply.kind, "sessions (1)")
        XCTAssertEqual(try Fixture.door("answer_refused")["response"]!.decode(DoorResponse.self).kind, "error harness")
        XCTAssertEqual(DoorResponse.ok.kind, "ok")
    }

    func testEveryFieldKindDecodesAndAnUnknownOneIsText() throws {
        let fields: [FormField] = try JSONValue.parse(#"""
        [{"name":"a","title":"A","description":null,"required":true,"secret":false,"kind":{"type":"text","format":"email","min_len":1,"max_len":9,"pattern":null,"default":"x"}},
         {"name":"b","title":null,"description":null,"required":false,"secret":false,"kind":{"type":"number","integer":true,"min":0,"max":10,"default":3}},
         {"name":"c","title":null,"description":null,"required":false,"secret":false,"kind":{"type":"boolean","default":true}},
         {"name":"d","title":null,"description":null,"required":false,"secret":false,"kind":{"type":"choice","options":[{"value":"m","title":"Main","description":null}],"default":"m"}},
         {"name":"e","title":null,"description":null,"required":false,"secret":false,"kind":{"type":"multi_choice","options":[],"min":null,"max":null,"default":[]}},
         {"name":"f","title":null,"description":null,"required":false,"secret":true,"kind":{"type":"unknown","raw_type":"object"}},
         {"name":"g","title":null,"description":null,"required":false,"secret":false,"kind":{"type":"colour"}}]
        """#).decode()
        XCTAssertEqual(fields.map(\.kind), [
            .text(format: "email", minLength: 1, maxLength: 9, pattern: nil, defaultValue: "x"),
            .number(integer: true, min: 0, max: 10, defaultValue: 3),
            .boolean(defaultValue: true),
            .choice(options: [ChoiceOption(value: "m", title: "Main", description: nil)], defaultValue: "m"),
            .multiChoice(options: []),
            .unknown(rawType: "object"),
            .unknown(rawType: "colour"),
        ])
        XCTAssertEqual(fields[0].label, "A")
        XCTAssertEqual(fields[1].label, "b")
        XCTAssertTrue(fields[5].secret)
    }

    func testAnEmptyTitleIsLabelledByTheFieldsName() throws {
        let field: FormField = try JSONValue.parse(
            #"{"name":"branch","title":"","description":null,"required":true,"secret":false,"kind":{"type":"text"}}"#
        ).decode()
        XCTAssertEqual(field.label, "branch")
    }

    func testAQuestionOfAModeThisAppDoesNotKnowIsKeptNotRefused() throws {
        let question: Elicitation = try JSONValue.parse(
            #"{"request_id":"e-1","call_id":null,"message":"Pick a file","mode":{"type":"file_picker","accept":["*.md"]}}"#
        ).decode()
        XCTAssertEqual(question.mode, .other(type: "file_picker"))
    }

    func testAnEnvironmentWithoutOrganizationsHoldsNone() throws {
        let environments: [DoorEnvironment] = try JSONValue.parse(
            #"[{"id":"e-1","organizations":null,"mothership":true},{"id":"e-2"}]"#
        ).decode()
        XCTAssertEqual(environments.map(\.id), ["e-1", "e-2"])
        XCTAssertEqual(environments.map(\.organizations), [[], []])
        XCTAssertEqual(environments.map(\.mothership), [true, false])
    }

    func testALoopersAskedAndAnUnreadableAttentionPinNothingAndKeepTheRow() throws {
        guard case .object(var row) = try Fixture.door("session")["event"]?["session"] else { return XCTFail("no row") }
        row["attention"] = .object(["type": .string("asked"), "questions": .number(1), "access": .number(0)])
        let asked: DoorSession = try JSONValue.object(row).decode()
        XCTAssertEqual(asked.attention, .other(type: "asked"))
        row["attention"] = .object(["type": .string("approval")])
        let unreadable: DoorSession = try JSONValue.object(row).decode()
        XCTAssertEqual(unreadable.attention, .other(type: "unreadable"))
        XCTAssertEqual(unreadable.title, "Recorded for the phone")
    }

    func testAQueuedRowSaysItsSteer() throws {
        guard case .object(var row) = try Fixture.door("session")["event"]?["session"] else { return XCTFail("no row") }
        row["queued"] = .array([.object([
            "queue_id": .string("q-1"), "text": .string("and then"), "at": .string("2026-01-01T00:00:00Z"),
            "steer": .string("refused"), "steer_reason": .string("not_steerable"),
        ])])
        let read: DoorSession = try JSONValue.object(row).decode()
        XCTAssertEqual(read.queued, [QueuedPrompt(queueID: "q-1", text: "and then", at: "2026-01-01T00:00:00Z", steer: "refused", steerReason: "not_steerable")])
    }

    func testARowSaysThePromptWaitingBehindACompactionAndItsPhotos() throws {
        let photo = JSONValue.object(["path": .string("/p/a.png"), "name": .string("a.png"), "kind": .string("image"), "size": .number(10)])
        let compacting: DoorSession = try Asks.row("s", requested: Asks.requested("p-1", "and then the tests", compacting: true, attachments: [photo, photo])).decode()
        XCTAssertEqual(compacting.requestedPrompt, RequestedPrompt(id: "p-1", text: "and then the tests", attachments: 2, compacting: true))
        XCTAssertEqual(compacting.requestedPrompt?.caption, RequestedPrompt.waitingForCompaction)
        let waiting: DoorSession = try Asks.row("s", requested: Asks.requested("p-2", "go", compacting: false)).decode()
        XCTAssertEqual(waiting.requestedPrompt, RequestedPrompt(id: "p-2", text: "go", compacting: false))
        XCTAssertEqual(waiting.requestedPrompt?.caption, RequestedPrompt.waitingToStart)
    }

    func testARowWithoutARequestedPromptHasNone() throws {
        let read: DoorSession = try Asks.row("s").decode()
        XCTAssertNil(read.requestedPrompt)
        XCTAssertNil(try Fixture.door("session")["event"]?["session"]?.decode(DoorSession.self).requestedPrompt, "the recorded row waits on nothing")
        let null: DoorSession = try Asks.row("s", requested: .null).decode()
        XCTAssertNil(null.requestedPrompt)
    }

    func testAnUnreadableRequestedPromptIsNoneAndKeepsTheRow() throws {
        let unreadable: DoorSession = try Asks.row("s", requested: .object(["text": .string("no id")])).decode()
        XCTAssertNil(unreadable.requestedPrompt)
        XCTAssertEqual(unreadable.title, "Recorded for the phone")
        let mistyped: DoorSession = try Asks.row("s", requested: .string("p-1")).decode()
        XCTAssertNil(mistyped.requestedPrompt)
        // A photo this app cannot read is still a photo the prompt carries.
        let odd: DoorSession = try Asks.row("s", requested: Asks.requested("p-1", "", compacting: false, attachments: [.string("?")])).decode()
        XCTAssertEqual(odd.requestedPrompt?.attachments, 1)
    }

    /// B4: every recorded door frame decodes from its bytes, once, into the
    /// typed frame — and the typed result is what the text path gives.
    func testEveryRecordedDoorFrameDecodesFromItsBytes() throws {
        let door = Fixture.root.appendingPathComponent("door", isDirectory: true)
        let names = try FileManager.default.contentsOfDirectory(atPath: door.path).filter { $0.hasSuffix(".json") }
        XCTAssertGreaterThan(names.count, 25, "the recordings are in the bundle")
        for name in names {
            let data = try Data(contentsOf: door.appendingPathComponent(name))
            let fromBytes = try DoorFrame(data: data)
            let fromText = try DoorFrame(String(decoding: data, as: UTF8.self))
            XCTAssertEqual(fromBytes, fromText, name)
            if case .reply(_, .other(let type)) = fromBytes { XCTFail("\(name) decodes as other \(type)") }
            if case .event(.other(let type)) = fromBytes, type != "projects_changed" { XCTFail("\(name) decodes as other \(type)") }
        }
    }

    /// Review focus 5.
    func testWhatIsNotAFrameIsRefusedBeforeAndAfterDecodeOnce() {
        XCTAssertThrowsError(try DoorFrame(data: Data("[1,2,3]".utf8)))
        XCTAssertThrowsError(try DoorFrame(data: Data(#"{"id":1.5,"response":{"type":"ok"}}"#.utf8)))
        XCTAssertThrowsError(try DoorFrame(data: Data(#"{"hello":"there"}"#.utf8)))
        XCTAssertThrowsError(try DoorFrame(data: Data(#"{"response":{"type":"ok"}}"#.utf8)), "a reply with no id is not a frame")
        XCTAssertThrowsError(try DoorFrame(data: Data("not json".utf8)))
    }

    /// B4: a tool call's content is parsed when the item is, and kept.
    func testAToolCallsContentIsParsedOnceWithTheItem() throws {
        guard case .reply(_, .transcript(let transcript)) = try DoorFrame(Fixture.doorText("session_transcript")) else {
            return XCTFail("not a transcript")
        }
        let item = try XCTUnwrap(transcript.items.first { $0.kind == "tool_call" })
        let first = item.toolCall
        XCTAssertNotNil(first)
        XCTAssertEqual(item.toolCall, first)
        let message = try XCTUnwrap(transcript.items.first { $0.kind == "message" })
        XCTAssertNil(message.toolCall)
        let odd = try Wire.decoder.decode(TranscriptItem.self, from: Data(#"{"id":"c9","session_id":"s","ordinal":1,"kind":"tool_call","content":"[1,2]","created_at":"2026-01-01T00:00:00Z"}"#.utf8))
        XCTAssertNil(odd.toolCall, "content that is not an object reads as no call")
    }

    func testASessionListWithOneUnreadableRowKeepsTheOthers() throws {
        let frame = try DoorFrame(data: Data(#"{"id":5,"response":{"type":"sessions","sessions":[{"id":"a","title":"A","status":"open","working":false,"turns":1},{"nope":true},{"id":"b","title":"B","status":"open","working":true,"turns":2}]}}"#.utf8))
        guard case .reply(5, .sessions(let rows)) = frame else { return XCTFail("not a list") }
        XCTAssertEqual(rows.map(\.id), ["a", "b"])
    }
}
