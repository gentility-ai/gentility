import Foundation
import XCTest
@testable import DeskPhoneKit

/// Pieces of the door's wire in api.rs's shapes, for values no recording
/// holds. Rows start from the recorded one.
enum Asks {
    static let options: [JSONValue] = [
        .object(["id": .string("allow"), "kind": .string("allow_once"), "name": .string("Allow once")]),
        .object(["id": .string("always"), "kind": .string("allow_always"), "name": .null]),
        .object(["id": .string("deny"), "kind": .string("reject_once"), "name": .string("Deny")]),
    ]

    static func approvalJSON(_ id: String = "r-1", callID: String = "call-1", tool: String = "Bash", server: String? = nil) -> JSONValue {
        .object([
            "request_id": .string(id), "call_id": .string(callID), "tool": .string(tool), "server": server.map(JSONValue.string) ?? .null,
            "kind": .string("execute"), "input": .object(["command": .string("cargo test -p desk_core")]),
            "changes": .array([]), "options": .array(options),
        ])
    }

    static func approval(_ id: String = "r-1") -> PermissionRequest { try! approvalJSON(id).decode() }

    static func field(_ name: String, _ kind: JSONValue, required: Bool = false, title: String? = nil, secret: Bool = false) -> JSONValue {
        .object([
            "name": .string(name), "title": title.map(JSONValue.string) ?? .null, "description": .null,
            "required": .bool(required), "secret": .bool(secret), "kind": kind,
        ])
    }

    static func text(min: Int? = nil, max: Int? = nil, default value: String? = nil) -> JSONValue {
        .object([
            "type": .string("text"), "format": .null, "pattern": .null,
            "min_len": min.map { .number(Double($0)) } ?? .null, "max_len": max.map { .number(Double($0)) } ?? .null,
            "default": value.map(JSONValue.string) ?? .null,
        ])
    }

    static func number(integer: Bool = false, min: Double? = nil, max: Double? = nil, default value: Double? = nil) -> JSONValue {
        .object([
            "type": .string("number"), "integer": .bool(integer),
            "min": min.map(JSONValue.number) ?? .null, "max": max.map(JSONValue.number) ?? .null,
            "default": value.map(JSONValue.number) ?? .null,
        ])
    }

    static func boolean(default value: Bool? = nil) -> JSONValue {
        .object(["type": .string("boolean"), "default": value.map(JSONValue.bool) ?? .null])
    }

    static func choice(_ values: [String], default value: String? = nil) -> JSONValue {
        .object([
            "type": .string("choice"),
            "options": .array(values.map { .object(["value": .string($0), "title": .string($0.capitalized), "description": .null]) }),
            "default": value.map(JSONValue.string) ?? .null,
        ])
    }

    static func multiChoice(_ values: [String]) -> JSONValue {
        .object([
            "type": .string("multi_choice"), "min": .null, "max": .null, "default": .array([]),
            "options": .array(values.map { .object(["value": .string($0), "title": .string($0), "description": .null]) }),
        ])
    }

    static func fields(_ fields: [JSONValue]) -> [FormField] { try! JSONValue.array(fields).decode() }

    static func formJSON(_ id: String = "e-1", fields: [JSONValue] = [field("branch", text(), required: true)]) -> JSONValue {
        .object([
            "request_id": .string(id), "call_id": .null, "message": .string("Which branch should I use?"),
            "mode": .object(["type": .string("form"), "title": .null, "fields": .array(fields)]),
        ])
    }

    static func form(_ id: String = "e-1", fields: [JSONValue] = [field("branch", text(), required: true)]) -> Elicitation {
        try! formJSON(id, fields: fields).decode()
    }

    static func approvalAttention(_ id: String = "r-1") -> JSONValue {
        .object(["type": .string("approval"), "request": approvalJSON(id)])
    }

    static func formAttention(_ id: String = "e-1") -> JSONValue {
        .object(["type": .string("elicitation"), "request": formJSON(id)])
    }

    static func questionAttention(_ id: String = "q-1", expires: String = "2099-01-01T00:00:00.000000Z") -> JSONValue {
        .object([
            "type": .string("question"), "question_id": .string(id), "expires_at": .string(expires),
            "request": formJSON(id, fields: [field("why", text(), required: true)]),
        ])
    }

    static func attention(_ json: JSONValue?) -> Attention? { json.map { try! $0.decode() } }

    static func said(_ said: JSONValue, session: String, turn: Int = 1) -> JSONValue {
        .object(["event": .object([
            "type": .string("said"), "session_id": .string(session), "turn": .number(Double(turn)), "said": said,
        ])])
    }

    static func answered(_ id: String, option: String, by: String = "person") -> JSONValue {
        .object(["kind": .string("answered"), "request_id": .string(id), "option_id": .string(option), "by": .string(by)])
    }

    static func refusal(_ code: String, _ message: String) -> JSONValue {
        .object(["response": .object(["type": .string("error"), "code": .string(code), "message": .string(message)])])
    }

    static func queued(_ id: String, _ text: String, steer: String? = nil) -> JSONValue {
        .object([
            "queue_id": .string(id), "text": .string(text), "at": .string("2026-01-01T00:00:00Z"),
            "steer": steer.map(JSONValue.string) ?? .null, "steer_reason": .null,
        ])
    }

    /// A row's `requested_prompt`, as `RequestedPrompt` is written.
    static func requested(_ id: String, _ text: String, compacting: Bool, attachments: [JSONValue] = []) -> JSONValue {
        var fields: [String: JSONValue] = [
            "id": .string(id), "text": .string(text), "at": .string("2026-01-01T00:00:00Z"), "compacting": .bool(compacting),
        ]
        if !attachments.isEmpty { fields["attachments"] = .array(attachments) }
        return .object(fields)
    }

    /// The recorded row (`door/session.json`), as `session`.
    static func row(
        _ session: String, organization: String = "o-1", attention: JSONValue? = nil, working: Bool = false, queued: [JSONValue] = [],
        requested: JSONValue? = nil, lastOutcome: String? = nil, turnStartedAt: String? = nil
    ) throws -> JSONValue {
        guard case .object(var fields)? = try Fixture.door("session")["event"]?["session"] else {
            throw FixtureError.missing("session")
        }
        fields["id"] = .string(session)
        fields["organization_id"] = .string(organization)
        fields["attention"] = attention ?? .null
        fields["working"] = .bool(working)
        fields["queued"] = queued.isEmpty ? nil : .array(queued)
        fields["requested_prompt"] = requested
        if let lastOutcome { fields["last_outcome"] = .string(lastOutcome) }
        if let turnStartedAt { fields["turn_started_at"] = .string(turnStartedAt) }
        return .object(fields)
    }

    static func rowEvent(_ row: JSONValue) -> JSONValue {
        .object(["event": .object(["type": .string("session"), "session": row])])
    }

    /// The recorded row (`door/session.json`), as a `session_get` reply.
    static func sessionReply(_ row: JSONValue) -> JSONValue {
        .object(["response": .object(["type": .string("session"), "session": row])])
    }

    /// The recorded hello without `session_get`: a Desk before protocol
    /// 1.47, whose row is read from the list.
    static func helloWithoutOneRow() throws -> JSONValue {
        guard case .object(var reply)? = try Fixture.door("hello")["response"],
              let features = reply["features"]?.array else {
            throw FixtureError.missing("hello")
        }
        reply["features"] = .array(features.filter { $0.string != "session_get" })
        return .object(["response": .object(reply)])
    }

    static func sessions(_ rows: [JSONValue]) -> JSONValue {
        .object(["response": .object(["type": .string("sessions"), "sessions": .array(rows)])])
    }

    /// The recorded `environments_list` reply, its one environment `e-1`,
    /// the mothership, holding `organization`.
    static func environments(holding organization: String) throws -> JSONValue {
        guard case .object(var environment)? = try Fixture.door("environments_list")["response"]?["environments"]?.array?.first else {
            throw FixtureError.missing("environments_list")
        }
        environment["id"] = .string("e-1")
        environment["mothership"] = .bool(true)
        environment["organizations"] = .array([.object(["id": .string(organization), "slug": .string("northwind"), "name": .string("Northwind")])])
        return .object(["response": .object(["type": .string("environments"), "environments": .array([.object(environment)])])])
    }
}

/// Ops as a test can predict them: op-1, op-2…
final class Ids {
    private(set) var minted: [String] = []

    func next() -> String {
        minted.append("op-\(minted.count + 1)")
        return minted[minted.count - 1]
    }
}

/// A conversation's model as the tests build it.
@MainActor
enum Conversations {
    static let row = HomeRow(
        id: "00000000-0000-7000-8000-000000000001", organizationID: "o-1", title: "Phone pairing spec",
        state: HomeRow.waitingOnYou, ago: "now", machineID: "recorded", macLabel: "Morgan's MacBook Pro",
        recapLine: nil, lastEventAt: nil, attentionLine: "Wants to execute"
    )

    static func model(
        _ bridge: FakeBridge, row: HomeRow = row, ids: Ids = Ids(), live: @escaping () -> Bool = { true },
        sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in throw CancellationError() }
    ) -> ConversationModel {
        ConversationModel(
            row: row,
            reach: { _ in .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: row.macLabel, live: live())) },
            wake: {},
            doors: Node(bridge: bridge),
            sleep: sleep,
            ids: { ids.next() },
            now: { WireDate.parse("2026-06-01T00:00:00Z")! }
        )
    }
}

/// The Mac's side of a conversation's opening, played on `bridge`.
@MainActor
struct Opening {
    let bridge: FakeBridge
    var sessionID = Conversations.row.id
    var organizationID = "o-1"

    /// hello at operate, the transcript, then the row read.
    func answer(
        attention: JSONValue? = nil, working: Bool = false, queued: [JSONValue] = [],
        environments: Bool = true, oneRow: Bool = true, door: String = "d-1"
    ) async throws {
        try await greet(oneRow: oneRow, door: door)
        try await answerRowRead(attention: attention, working: working, queued: queued, environments: environments, oneRow: oneRow, door: door)
    }

    /// hello at operate and the transcript, as recorded; without
    /// `session_get` when not `oneRow`.
    func greet(oneRow: Bool = true, door: String = "d-1") async throws {
        let hello = await bridge.nextRequest()
        XCTAssertEqual(hello.request, .object(["type": .string("hello"), "wants": .array([.string("operate"), .string("unknown_words")])]))
        bridge.say(try oneRow ? Fixture.door("hello") : Asks.helloWithoutOneRow(), id: hello.id, door: door)
        let transcript = await bridge.nextRequest()
        XCTAssertEqual(transcript.request["type"]?.string, "session_transcript")
        XCTAssertEqual(transcript.request["session_id"]?.string, sessionID)
        // The recorded hello offers transcript_brief, so the read asks for it.
        XCTAssertEqual(transcript.request["shape"]?.string, "brief")
        bridge.say(try Fixture.door("session_transcript_brief"), id: transcript.id, door: door)
    }

    /// A reopen's read: `after_seq` at the cursor, answered with these rows.
    func answerChanges(afterSeq: Int, items: [JSONValue] = [], through: Int, reset: Bool = false, door: String = "d-1") async throws {
        let read = await bridge.nextRequest()
        XCTAssertEqual(read.request["type"]?.string, "session_transcript")
        XCTAssertEqual(read.request["after_seq"]?.int, afterSeq)
        XCTAssertNil(read.request["last_turns"])
        XCTAssertNil(read.request["from_turn"])
        var response: [String: JSONValue] = [
            "type": .string("transcript"), "items": .array(items), "from_turn": .null, "earlier": .number(0),
            "through_seq": .number(Double(through)), "reset": .bool(reset),
        ]
        if reset { response["from_turn"] = .number(1) }
        bridge.say(.object(["response": .object(response)]), id: read.id, door: door)
    }

    /// `session_get` where the hello offered it (the recorded hello does);
    /// else `environments_list` when `environments` (the first open of a
    /// conversation), then `sessions_list` — answered with this row.
    func answerRowRead(
        attention: JSONValue? = nil, working: Bool = false, queued: [JSONValue] = [],
        environments: Bool = false, oneRow: Bool = true, door: String = "d-1"
    ) async throws {
        let built = try Asks.row(sessionID, organization: organizationID, attention: attention, working: working, queued: queued)
        if oneRow {
            let asked = await bridge.nextRequest()
            XCTAssertEqual(asked.request, .object(["type": .string("session_get"), "session_id": .string(sessionID)]))
            bridge.say(Asks.sessionReply(built), id: asked.id, door: door)
            return
        }
        if environments {
            let asked = await bridge.nextRequest()
            XCTAssertEqual(asked.request, .object(["type": .string("environments_list")]))
            bridge.say(try Asks.environments(holding: organizationID), id: asked.id, door: door)
        }
        let list = await bridge.nextRequest()
        XCTAssertEqual(list.request, .object(["type": .string("sessions_list"), "environment_id": .string("e-1")]))
        bridge.say(Asks.sessions([built]), id: list.id, door: door)
    }
}
