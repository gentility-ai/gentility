import XCTest
@testable import DeskPhoneKit

final class ToolRunsTests: XCTestCase {
    func testToolCallsBackToBackAreOneRow() {
        let rows = TranscriptRow.rows([
            .you(id: "u", text: "go"),
            .tool(id: "t1", text: "✓ Read docs/pairing.md"),
            .tool(id: "t2", text: "✗ Bash cargo test"),
            .tool(id: "t3", text: "… Write tmp/frame.html"),
            .agent(id: "a", text: "Done"),
            .tool(id: "t4", text: "✓ Edit a.swift"),
        ])
        XCTAssertEqual(rows.map(\.id), ["u", "run-t1", "a", "run-t4"])
        guard case .tools(let run) = rows[1] else { return XCTFail("the second row is the run") }
        XCTAssertEqual(run.steps.map(\.state), [.done, .failed, .running])
        XCTAssertEqual(run.steps.map(\.title), ["Read docs/pairing.md", "Bash cargo test", "Write tmp/frame.html"])
        XCTAssertEqual(run.failed, 1)
    }

    func testARunSaysTheCallGoingNowThenItsCount() {
        let going = ToolRun(steps: [
            ToolStep(id: "1", mark: "✓", title: "Read a"), ToolStep(id: "2", mark: "…", title: "Bash make"),
        ])
        XCTAssertEqual(going.headline, "Bash make")
        let done = ToolRun(steps: [
            ToolStep(id: "1", mark: "✓", title: "Read a"), ToolStep(id: "2", mark: "✓", title: "Bash make"),
        ])
        XCTAssertEqual(done.headline, "2 tool calls")
        let failed = ToolRun(steps: [
            ToolStep(id: "1", mark: "✗", title: "Read a"), ToolStep(id: "2", mark: "✓", title: "Bash make"),
        ])
        XCTAssertEqual(failed.headline, "2 tool calls, 1 failed")
        XCTAssertEqual(ToolRun(steps: [ToolStep(id: "1", mark: "⊘", title: "Write b")]).headline, "Write b")
    }

    func testARunKeepsItsIdAsCallsJoinIt() {
        let before = TranscriptRow.rows([.tool(id: "t1", text: "… Read a")])
        let after = TranscriptRow.rows([.tool(id: "t1", text: "✓ Read a"), .tool(id: "t2", text: "… Read b")])
        XCTAssertEqual(before.map(\.id), after.map(\.id))
    }

    func testATableReadsItsCells() {
        let table = MarkdownTable(source: "| Screen | Before | After |\n|---|:---:|---|\n| iPhone SE | clipped | fits |\n| a \\| b | only |")
        XCTAssertEqual(table?.header, ["Screen", "Before", "After"])
        XCTAssertEqual(table?.rows, [["iPhone SE", "clipped", "fits"], ["a | b", "only", ""]])
    }

    func testSourceWithoutASeparatorIsNotATable() {
        XCTAssertNil(MarkdownTable(source: "| a | b |\n| c | d |"))
        XCTAssertNil(MarkdownTable(source: "| a | b |"))
    }
}
