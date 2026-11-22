import XCTest
@testable import DeskPhoneKit

final class TranscriptFoldTests: XCTestCase {
    private var ordinal = 0

    private func item(_ id: String, _ kind: String, role: String? = nil, content: String?, turn: Int?, parent: String? = nil) -> TranscriptItem {
        ordinal += 1
        return try! JSONValue.object([
            "id": .string(id), "session_id": .string("s"), "ordinal": .number(Double(ordinal)), "kind": .string(kind),
            "role": role.map(JSONValue.string) ?? .null, "content": content.map(JSONValue.string) ?? .null,
            "turn": turn.map { .number(Double($0)) } ?? .null, "parent_call_id": parent.map(JSONValue.string) ?? .null,
            "message_item_id": .null, "remote_id": .null, "created_at": .string("2026-01-01T00:00:00.000000Z"),
        ]).decode()
    }

    private func transcript(_ items: [TranscriptItem], from: Int? = nil, through: Int? = nil, reset: Bool = false) -> Transcript {
        try! JSONValue.object([
            "items": .array(items.map { try! JSONValue.parse(Wire.encoder.encode(Encoded($0))) }),
            "from_turn": from.map { .number(Double($0)) } ?? .null, "earlier": .number(0),
            "through_seq": through.map { .number(Double($0)) } ?? .null, "reset": .bool(reset),
        ]).decode()
    }

    func testTheRecordedTranscriptFoldsToTurnsOfPlainLines() throws {
        guard case .reply(_, .transcript(let recorded)) = try DoorFrame(Fixture.doorText("session_transcript")) else {
            return XCTFail("no transcript")
        }
        var fold = TranscriptFold()
        fold.replace(with: recorded)
        let lines = fold.turns.flatMap(\.lines)
        XCTAssertTrue(lines.contains { if case .you = $0 { return true } else { return false } })
        XCTAssertTrue(lines.contains { if case .tool(_, let text) = $0 { return !text.isEmpty } else { return false } })
    }

    func testDeskStartRowsAndSaidStayOutOfThePhoneTranscript() throws {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("start", "start", content: "{}", turn: 1),
            item("prompt", "message", role: "user", content: "hello", turn: 1),
        ]))
        XCTAssertEqual(fold.turns.first?.lines, [.you(id: "prompt", text: "hello")])
        let said = try Wire.decoder.decode(Said.self, from: Data(#"{"kind":"start"}"#.utf8))
        guard case .other = said else { return XCTFail("start must stay unknown") }
        XCTAssertNil(fold.apply(said, turn: 1))
        XCTAssertEqual(fold.turns.first?.lines, [.you(id: "prompt", text: "hello")])
    }

    func testThoughtsAndSubAgentsAreNotDrawnAndAToolIsOneLine() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "draw this workflow out in storyboards", turn: 1),
            item("t1", "thought", content: "hmm", turn: 1),
            item("c1", "tool_call", content: #"{"name":"Write","title":"Write tmp/phone-pairing.html","status":"executed"}"#, turn: 1),
            item("c2", "tool_call", content: #"{"name":"Read","title":"Read a.rs","status":"executed"}"#, turn: 1, parent: "c1"),
            item("a1", "message", role: "assistant", content: "Here are the frames…", turn: 1),
        ]))
        XCTAssertEqual(fold.turns.first?.lines, [
            .you(id: "u1", text: "draw this workflow out in storyboards"),
            .tool(id: "c1", text: "✓ Write tmp/phone-pairing.html"),
            .agent(id: "a1", text: "Here are the frames…"),
        ])
    }

    func testACompactionIsOneLineThatTheLiveMomentRedraws() throws {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "go on", turn: 1),
            item("k1", "compaction", content: #"{"status":"in_progress","before":213000}"#, turn: 1),
        ]))
        XCTAssertEqual(fold.turns.first?.lines.last, .compaction(id: "k1", text: "Compacting context…"))
        let said = try Wire.decoder.decode(
            Said.self,
            from: Data(#"{"kind":"compaction","id":"k1","status":"completed","before":213000,"after":14600,"duration_ms":38000}"#.utf8)
        )
        XCTAssertEqual(said, .compaction(id: "k1", Compaction(status: "completed", before: 213_000, after: 14_600)))
        XCTAssertNil(fold.apply(said, turn: 1))
        XCTAssertEqual(fold.turns.first?.lines, [
            .you(id: "u1", text: "go on"),
            .compaction(id: "k1", text: "Context compacted 213K → 14.6K tokens"),
        ])
    }

    func testTheLastTurnIsCompactingWhileItsCompactionRuns() throws {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("k0", "compaction", content: #"{"status":"completed","before":90000,"after":9000}"#, turn: 1),
            item("u2", "message", role: "user", content: "/compact", turn: 2),
            item("k1", "compaction", content: #"{"status":"in_progress"}"#, turn: 2),
        ]))
        XCTAssertTrue(fold.compacting, "read running after a reopen")
        _ = fold.apply(.compaction(id: "k1", Compaction(status: "completed", before: 213_000, after: 14_600)), turn: 2)
        XCTAssertFalse(fold.compacting)
        _ = fold.apply(.compaction(id: "k2", Compaction(status: "in_progress")), turn: 3)
        XCTAssertTrue(fold.compacting, "heard live in a new turn")
        fold.update(with: transcript([item("k2", "compaction", content: #"{"status":"cancelled"}"#, turn: 3)]))
        XCTAssertFalse(fold.compacting)
    }

    func testTheCaretHidesWhileACompactionRunsAndComesBackAfter() {
        let compacting = [Turn(number: 1, lines: [.you(id: "u", text: "go on"), .compaction(id: "k", text: "Compacting context…")])]
        XCTAssertEqual(Caret(turns: compacting, working: true, compacting: true), .none, "the compaction line is the live sign")
        XCTAssertEqual(Caret(turns: compacting, working: true, compacting: false), .alone, "compacted, the turn still working")
        let writing = [Turn(number: 1, lines: [.compaction(id: "k", text: "Context compacted"), .agent(id: "a", text: "Carrying on")])]
        XCTAssertEqual(Caret(turns: writing, working: true, compacting: false), .inLine("a"))
        XCTAssertEqual(Caret(turns: writing, working: false, compacting: false), .none)
    }

    func testARunningToolCallIsItsOwnLiveSign() {
        let running = [Turn(number: 1, lines: [.you(id: "u", text: "go on"), .tool(id: "c", text: "… Bash mix test")])]
        XCTAssertEqual(Caret(turns: running, working: true, compacting: false), .none, "the running row spins")
        let ran = [Turn(number: 1, lines: [.you(id: "u", text: "go on"), .tool(id: "c", text: "✓ Bash mix test")])]
        XCTAssertEqual(Caret(turns: ran, working: true, compacting: false), .alone, "between calls the caret shows the turn is alive")
    }

    func testACompactionSaysWhatWasMeasured() {
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "completed", before: 213_000)), "Context compacted from 213K tokens")
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "completed", after: 14_600)), "Context compacted to 14.6K tokens")
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "completed")), "Context compacted")
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "cancelled", before: 213_000)), "Stopped compacting")
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "failed", before: 213_000)), "Compaction failed")
        XCTAssertEqual(TranscriptFold.words(Compaction(status: "unheard_of")), "Compacting context…")
    }

    func testTokensReadAsTheWindowsDo() {
        let cases: [(Int, String)] = [
            (842, "842"), (1_000, "1K"), (14_600, "14.6K"), (14_640, "14.6K"), (50_000, "50K"),
            (99_960, "100K"), (213_000, "213K"), (213_400, "213K"), (999_500, "1M"), (1_240_000, "1.2M"),
        ]
        for (count, words) in cases { XCTAssertEqual(TranscriptFold.tokens(count), words, "\(count)") }
    }

    func testTextStreamsByItsTailAndAWholeReplacesIt() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "go", turn: 1)]))
        XCTAssertNil(fold.apply(.text(messageID: "a1", tail: "Propo", whole: nil, parentCallID: nil), turn: 1))
        XCTAssertNil(fold.apply(.text(messageID: "a1", tail: "sed: number matching", whole: nil, parentCallID: nil), turn: 1))
        XCTAssertEqual(fold.turns.last?.lines.last, .agent(id: "a1", text: "Proposed: number matching"))
        _ = fold.apply(.text(messageID: "a1", tail: "", whole: "Proposed: number matching, then Allow", parentCallID: nil), turn: 1)
        XCTAssertEqual(fold.turns.last?.lines.last, .agent(id: "a1", text: "Proposed: number matching, then Allow"))
    }

    func testAToolCallOpensAndSettlesOnOneLine() {
        var fold = TranscriptFold()
        _ = fold.apply(.toolCall(callID: "c1", name: "Write", title: "Write tmp/a", status: "running", settled: false, parentCallID: nil), turn: 1)
        _ = fold.apply(.toolCall(callID: "c1", name: "Write", title: "Write tmp/a", status: "executed", settled: true, parentCallID: nil), turn: 1)
        XCTAssertEqual(fold.turns.last?.lines, [.tool(id: "c1", text: "✓ Write tmp/a")])
    }

    /// F9: the live moment names a call by its provider's id; the transcript
    /// item has an id of its own and carries the provider's in its content.
    func testALiveToolCallAndItsReReadAreOneLine() throws {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "rename it", turn: 1)], from: 1))
        _ = fold.apply(.toolCall(callID: "call_fake_1", name: "Edit", title: "Edit README.md", status: "pending", settled: false, parentCallID: nil), turn: 1)
        XCTAssertEqual(fold.turns.last?.lines.last, .tool(id: "call_fake_1", text: "… Edit README.md"))
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "rename it", turn: 1),
            item("i-4", "tool_call", content: #"{"provider_tool_call_id":"call_fake_1","name":"Edit","title":"Edit README.md","status":"executed"}"#, turn: 1),
        ], from: 1))
        XCTAssertEqual(fold.turns.last?.lines, [
            .you(id: "u1", text: "rename it"),
            .tool(id: "call_fake_1", text: "✓ Edit README.md"),
        ])
    }

    func testTheRecordedLiveToolCallMatchesTheRecordedTranscriptsLine() throws {
        guard case .event(.said(_, let turn, let said)) = try DoorFrame(Fixture.doorText("said_tool_call")),
              case .reply(_, .transcript(let recorded)) = try DoorFrame(Fixture.doorText("session_transcript")) else {
            return XCTFail("no recordings")
        }
        var fold = TranscriptFold()
        _ = fold.apply(said, turn: turn)
        fold.replace(with: recorded)
        let tools = fold.turns.flatMap(\.lines).filter { if case .tool = $0 { return true } else { return false } }
        XCTAssertEqual(tools, [.tool(id: "call_fake_1", text: "✓ Edit README.md")])
    }

    func testANewTurnAsksForTheTranscriptFromItBecauseThePromptIsNotSaidLive() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        XCTAssertEqual(fold.apply(.text(messageID: "a2", tail: "Two", whole: nil, parentCallID: nil), turn: 2), .from(2))
    }

    /// A moment from two turns on: the turn between was missed too, so the
    /// re-read starts just after the last turn on screen.
    func testAMomentThatSkipsATurnReReadsFromTheOneAfterTheLast() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        XCTAssertEqual(fold.apply(.text(messageID: "a3", tail: "Three", whole: nil, parentCallID: nil), turn: 3), .from(2))
        fold.replace(with: transcript([
            item("u2", "message", role: "user", content: "two", turn: 2),
            item("a2", "message", role: "assistant", content: "Two", turn: 2),
            item("u3", "message", role: "user", content: "three", turn: 3),
        ], from: 2))
        XCTAssertEqual(fold.turns.map(\.number), [1, 2, 3])
        XCTAssertEqual(fold.turns.last?.lines, [.you(id: "u3", text: "three"), .agent(id: "a3", text: "Three")])
    }

    func testTheFirstMomentOnAnEmptyFoldReReadsItsOwnTurn() {
        var fold = TranscriptFold()
        XCTAssertEqual(fold.apply(.text(messageID: "a4", tail: "Four", whole: nil, parentCallID: nil), turn: 4), .from(4))
    }

    /// A word this build has not heard of reads as neither going nor finished.
    func testAToolStatusThisBuildHasNotHeardOfIsNotStillGoing() {
        var live = TranscriptFold()
        _ = live.apply(.toolCall(callID: "c1", name: "Write", title: "Write tmp/a", status: "word_from_a_newer_core", settled: false, parentCallID: nil), turn: 1)
        XCTAssertEqual(live.turns.last?.lines, [.tool(id: "c1", text: "? Write tmp/a")])

        var read = TranscriptFold()
        read.replace(with: transcript([
            item("c2", "tool_call", content: #"{"name":"Read","title":"Read a.rs","status":"word_from_a_newer_core"}"#, turn: 1),
        ]))
        XCTAssertEqual(read.turns.last?.lines, [.tool(id: "c2", text: "? Read a.rs")])
    }

    /// A role this build has not heard of is neither the person's words nor the agent's.
    func testAMessageInARoleThisBuildHasNotHeardOfDrawsNoWords() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "go", turn: 1),
            item("x1", "message", role: "word_from_a_newer_core", content: "said by nobody we know", turn: 1),
            item("a1", "message", role: "assistant", content: "done", turn: 1),
        ]))
        XCTAssertEqual(fold.turns.first?.lines, [.you(id: "u1", text: "go"), .agent(id: "a1", text: "done")])
    }

    func testAToolLineLeadsWithItsMark() {
        var fold = TranscriptFold()
        for (call, status) in [("c1", "executed"), ("c2", "failed"), ("c3", "cancelled"), ("c4", "running")] {
            _ = fold.apply(.toolCall(callID: call, name: "Write", title: "Write tmp/a", status: status, settled: status != "running", parentCallID: nil), turn: 1)
        }
        XCTAssertEqual(fold.turns.last?.lines, [
            .tool(id: "c1", text: "✓ Write tmp/a"),
            .tool(id: "c2", text: "✗ Write tmp/a"),
            .tool(id: "c3", text: "⊘ Write tmp/a"),
            .tool(id: "c4", text: "… Write tmp/a"),
        ])
    }

    func testTheEndOfATurnAHeardAndAHandoffReReadAndALagReadsTheLastTurns() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        XCTAssertEqual(fold.apply(.turnEnded(outcome: "end_turn", detail: nil), turn: 1), .from(1))
        XCTAssertEqual(fold.apply(.heard(from: "person"), turn: 1), .from(1))
        XCTAssertEqual(fold.apply(.handoff, turn: 1), .from(1))
        XCTAssertEqual(fold.apply(.lagged(missed: 3), turn: 1), .last)
        XCTAssertNil(fold.apply(.other(kind: "context"), turn: 1))
    }

    /// a stop, a limit and a failure each end their turn with a line;
    /// a turn the agent finished ends with none.
    func testATurnThatDidNotFinishSaysHowItEnded() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "one", turn: 1),
            item("u2", "message", role: "user", content: "two", turn: 2),
            item("u3", "message", role: "user", content: "three", turn: 3),
            item("u4", "message", role: "user", content: "four", turn: 4),
        ]))
        _ = fold.apply(.turnEnded(outcome: "end_turn", detail: nil), turn: 1)
        _ = fold.apply(.turnEnded(outcome: "stopped by the person", detail: nil), turn: 2)
        _ = fold.apply(.turnEnded(outcome: "limited", detail: "You've hit your limit · resets 4pm"), turn: 3)
        _ = fold.apply(.turnEnded(outcome: "the agent exited: code 1", detail: nil), turn: 4)
        let ends = fold.turns.map(\.end)
        XCTAssertNil(ends[0])
        XCTAssertEqual(ends[1]?.words, "Stopped")
        XCTAssertNil(ends[1]?.more)
        XCTAssertEqual(ends[2]?.tone, .limited)
        XCTAssertEqual(ends[2]?.more, "You've hit your limit · resets 4pm")
        XCTAssertEqual(ends[3]?.words, "Failed")
        XCTAssertEqual(ends[3]?.more, "the agent exited: code 1", "the error is what it says")
    }

    /// A re-read whose rows say nothing of an end keeps the one heard live;
    /// a `turn_ended` row says one of its own, read whole or as a change.
    func testAnEndHeardLiveOutlastsAReReadAndATurnEndedRowSaysOne() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)], from: 1))
        _ = fold.apply(.turnEnded(outcome: "cancelled", detail: nil), turn: 1)
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "one", turn: 1),
            item("a1", "message", role: "assistant", content: "Partial", turn: 1),
        ], from: 1))
        XCTAssertEqual(fold.turns.first?.end, TurnEnd(outcome: "cancelled"))

        fold.replace(with: transcript([
            item("u2", "message", role: "user", content: "two", turn: 2),
            item("e2", "turn_ended", content: #"{"reason":"limited","detail":"resets 4pm"}"#, turn: 2),
        ], from: 2))
        XCTAssertEqual(fold.turns.last?.lines, [.you(id: "u2", text: "two")], "a turn_ended row is no line")
        XCTAssertEqual(fold.turns.last?.end, TurnEnd(outcome: "limited", detail: "resets 4pm"))

        fold.update(with: transcript([
            item("e3", "turn_ended", content: #"{"reason":"max_tokens"}"#, turn: 3),
        ], through: 9))
        XCTAssertEqual(fold.turns.map(\.number), [1, 2, 3])
        XCTAssertEqual(fold.turns.last?.end?.more, "This turn reached the model’s output limit. Send a message to continue.")
    }

    /// The core writes a row for every turn's end: one that ended as turns do
    /// says nothing, an earlier stop reads as one after a fresh read, and a
    /// late row, filed after every row, goes onto the turn it names.
    func testEveryTurnsEndRowEndsItsOwnTurn() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "one", turn: 1),
            item("e1", "turn_ended", content: #"{"cost":"0.25","input_tokens":1200}"#, turn: 1),
            item("u2", "message", role: "user", content: "two", turn: 2),
            item("e2", "turn_ended", content: #"{"reason":"cancelled"}"#, turn: 2),
            item("u3", "message", role: "user", content: "three", turn: 3),
            item("u4", "message", role: "user", content: "four", turn: 4),
            item("l3", "turn_ended_late", content: #"{"reason":"refusal"}"#, turn: 3),
        ]))
        XCTAssertEqual(fold.turns.map(\.number), [1, 2, 3, 4])
        XCTAssertEqual(fold.turns.map(\.end), [nil, TurnEnd(outcome: "cancelled"), TurnEnd(outcome: "refusal"), nil])
        XCTAssertEqual(fold.turns[2].lines, [.you(id: "u3", text: "three")], "a late row is no line")
    }

    /// A failed compaction ends its turn with the core's sentence, and its line
    /// already says it failed: the end is kept, and not drawn twice.
    func testAFailedCompactionsTurnDrawsItsLineAndNotItsEnd() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("k1", "compaction", content: #"{"status":"failed","error":"too long"}"#, turn: 1),
            item("e1", "turn_ended", content: #"{"reason":"compaction failed: too long"}"#, turn: 1),
            item("u2", "message", role: "user", content: "two", turn: 2),
            item("e2", "turn_ended", content: #"{"reason":"compaction failed: too long"}"#, turn: 2),
        ]))
        XCTAssertEqual(fold.turns[0].end, TurnEnd(outcome: "compaction failed: too long"))
        XCTAssertNil(fold.turns[0].drawnEnd)
        XCTAssertEqual(fold.turns[1].drawnEnd, TurnEnd(outcome: "compaction failed: too long"), "no line said it")
    }

    /// The row's `last_outcome` ends the last turn on screen, and only where
    /// nothing heard or read has said its end.
    func testTheRowsLastOutcomeEndsOnlyAnUnendedLastTurn() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        fold.lastEnded("end_turn")
        XCTAssertNil(fold.turns.last?.end)
        fold.lastEnded("cancelled")
        XCTAssertEqual(fold.turns.last?.end?.words, "Stopped")
        fold.lastEnded("refusal")
        XCTAssertEqual(fold.turns.last?.end?.words, "Stopped", "an end already said stands")
    }

    func testAReReadFromATurnKeepsTheEarlierTurnsAndWhatStreamedPastTheRead() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u1", "message", role: "user", content: "one", turn: 1),
            item("u2", "message", role: "user", content: "two", turn: 2),
        ], from: 1))
        _ = fold.apply(.text(messageID: "a2", tail: "streaming", whole: nil, parentCallID: nil), turn: 2)
        fold.replace(with: transcript([item("u2", "message", role: "user", content: "two", turn: 2)], from: 2))
        XCTAssertEqual(fold.turns.map(\.number), [1, 2])
        XCTAssertEqual(fold.turns.last?.lines, [.you(id: "u2", text: "two"), .agent(id: "a2", text: "streaming")])
    }

    /// Review focus 2. B5: changed rows redraw their line in place, a new
    /// row joins the end of its turn, a newer turn is appended in order, and
    /// a row for a turn paged out before the first on screen is dropped.
    func testChangedRowsRedrawInPlaceAppendNewTurnsAndDropPagedOutOnes() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([
            item("u3", "message", role: "user", content: "go", turn: 3),
            item("c3", "tool_call", content: #"{"provider_tool_call_id":"call-3","name":"Bash","title":"cargo test","status":"running"}"#, turn: 3),
            item("a3", "message", role: "assistant", content: "Running", turn: 3),
        ], from: 3))
        fold.update(with: transcript([
            item("c3", "tool_call", content: #"{"provider_tool_call_id":"call-3","name":"Bash","title":"cargo test","status":"executed"}"#, turn: 3),
            item("a3b", "message", role: "assistant", content: "Done.", turn: 3),
            item("u1", "message", role: "user", content: "an old prompt", turn: 1),
            item("u5", "message", role: "user", content: "next", turn: 5),
            item("u4", "message", role: "user", content: "between", turn: 4),
        ], through: 90))
        XCTAssertEqual(fold.turns.map(\.number), [3, 4, 5])
        XCTAssertEqual(fold.turns[0].lines, [
            .you(id: "u3", text: "go"),
            .tool(id: "call-3", text: "✓ cargo test"),
            .agent(id: "a3", text: "Running"),
            .agent(id: "a3b", text: "Done."),
        ])
        XCTAssertEqual(fold.turns[1].lines, [.you(id: "u4", text: "between")])
        XCTAssertEqual(fold.turns[2].lines, [.you(id: "u5", text: "next")])
    }

    /// A turn's prompt is never said live: read after the words that
    /// streamed, it still comes first.
    func testAnUpdatePutsATurnsPromptBeforeTheWordsThatStreamed() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "go", turn: 1)]))
        _ = fold.apply(.text(messageID: "a2", tail: "On it", whole: nil, parentCallID: nil), turn: 2)
        fold.update(with: transcript([item("u2", "message", role: "user", content: "next", turn: 2)], through: 5))
        XCTAssertEqual(fold.turns.last?.lines, [.you(id: "u2", text: "next"), .agent(id: "a2", text: "On it")])
    }

    func testAnUpdateOnAnEmptyFoldDrawsEverything() {
        var fold = TranscriptFold()
        fold.update(with: transcript([item("u1", "message", role: "user", content: "hi", turn: 1)], through: 3))
        XCTAssertEqual(fold.turns.map(\.number), [1])
    }

    func testAnUpdateKeepsToolTitlesForTheCards() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "go", turn: 1)]))
        fold.update(with: transcript([
            item("c1", "tool_call", content: #"{"provider_tool_call_id":"call-1","name":"Write","title":"Write a.rs","status":"running"}"#, turn: 1),
        ], through: 9))
        XCTAssertEqual(fold.toolTitle(callID: "call-1"), "Write a.rs")
    }

    func testAToolsTitleIsFoundByItsCallID() {
        var fold = TranscriptFold()
        _ = fold.apply(.toolCall(callID: "call-1", name: "Bash", title: "cargo test", status: "pending", settled: false, parentCallID: nil), turn: 1)
        XCTAssertEqual(fold.toolTitle(callID: "call-1"), "cargo test")
        XCTAssertNil(fold.toolTitle(callID: "call-2"))
        _ = fold.apply(.toolCall(callID: "call-3", name: "Bash", title: nil, status: "pending", settled: false, parentCallID: nil), turn: 1)
        XCTAssertNil(fold.toolTitle(callID: "call-3"), "a line drawn by the tool's name has no title")
    }

    /// A core without paging answers the whole transcript (no `from_turn`):
    /// it still keeps a line streamed past the read.
    func testAWholeReReadKeepsWhatStreamedPastIt() {
        var fold = TranscriptFold()
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        _ = fold.apply(.text(messageID: "a1", tail: "streaming", whole: nil, parentCallID: nil), turn: 1)
        fold.replace(with: transcript([item("u1", "message", role: "user", content: "one", turn: 1)]))
        XCTAssertEqual(fold.turns.last?.lines, [.you(id: "u1", text: "one"), .agent(id: "a1", text: "streaming")])
    }
}

/// `TranscriptItem` is read-only on the wire; tests write one back out.
private struct Encoded: Encodable {
    let item: TranscriptItem
    init(_ item: TranscriptItem) { self.item = item }
    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: TranscriptItem.CodingKeys.self)
        try container.encode(item.id, forKey: .id)
        try container.encode(item.sessionID, forKey: .sessionID)
        try container.encode(item.ordinal, forKey: .ordinal)
        try container.encode(item.kind, forKey: .kind)
        try container.encode(item.role, forKey: .role)
        try container.encode(item.content, forKey: .content)
        try container.encode(item.turn, forKey: .turn)
        try container.encode(item.parentCallID, forKey: .parentCallID)
        try container.encode(item.createdAt, forKey: .createdAt)
    }
}
