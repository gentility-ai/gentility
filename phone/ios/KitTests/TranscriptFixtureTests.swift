import XCTest
@testable import DeskPhoneKit

/// The phone's `TranscriptFold` held to a golden transcript case
/// (`contract/transcript/`, written by the Elixir suite and folded
/// again by the server, the Desk core and the window). Only
/// `tool_call_declined` so far: a call refused at its question and then not
/// run, which a reload must draw as declined on every reader. The rest of the
/// cases wait for the follow-up the manifest's `readers` note names.
///
/// Read from the checkout through `#filePath` rather than from the bundle the
/// build copies `contract/` into, because that copy carries only
/// the server exchanges, the door's frames and `released.json`.
final class TranscriptFixtureTests: XCTestCase {
    private static let cases = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent() // KitTests
        .deletingLastPathComponent() // ios
        .deletingLastPathComponent() // phone
        .appendingPathComponent("contract/transcript", isDirectory: true)

    /// A case's `items.json`, as the rows a core hands the phone: a tool
    /// call's fields become its JSON content, as the core's store holds it.
    private func items(_ name: String) throws -> [TranscriptItem] {
        let file = Self.cases.appendingPathComponent("\(name)/items.json")
        guard let recorded = try JSONValue.parse(Data(contentsOf: file)).array else {
            throw FixtureError.missing("\(name)/items.json")
        }
        return try recorded.map { item in
            let content: JSONValue
            if case .object(var tool)? = item["tool"] {
                tool["kind"] = tool.removeValue(forKey: "tool_kind")
                content = .string(try JSONValue.object(tool).text())
            } else {
                content = item["content"] ?? .null
            }
            return try JSONValue.object([
                "id": item["id"] ?? .null,
                "session_id": .string("s"),
                "ordinal": item["position"] ?? .null,
                "kind": item["kind"] ?? .null,
                "role": item["role"] ?? .null,
                "content": content,
                "turn": item["turn"] ?? .null,
                "parent_call_id": item["parent_call_id"] ?? .null,
                "created_at": .string("2026-01-01T00:00:00.000000Z"),
            ]).decode()
        }
    }

    func testACallDeclinedAtItsQuestionCarriesTheDeclinedMark() throws {
        let turns = TranscriptFold.turns(from: try items("tool_call_declined"))
        XCTAssertEqual(turns.map(\.number), [1])
        XCTAssertEqual(turns.first?.lines, [
            .you(id: "0199f000-0000-7000-8000-0000000000b1", text: "fix the flaky test"),
            .agent(id: "0199f000-0000-7000-8000-0000000000b2", text: "Pinning the clock."),
            .tool(id: "call-d1", text: "⊖ Edit test/flaky_test.exs"),
        ])
    }
}
