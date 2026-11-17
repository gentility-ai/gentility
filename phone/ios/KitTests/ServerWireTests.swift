import XCTest
@testable import DeskPhoneKit

final class ServerWireTests: XCTestCase {
    func testALobbyRowDecodesFromTheRecordedList() throws {
        let recorded = try Fixture.response("work_sessions/list")
        let listed: LobbyList = try recorded.decode()
        XCTAssertEqual(listed.now, "2026-01-01T00:00:00.000000Z")
        let row = try XCTUnwrap(listed.sessions.first)
        XCTAssertEqual(row.title, "Fix the flaky test")
        XCTAssertEqual(row.attention, "none")
        XCTAssertEqual(row.machineID, "00000000-0000-7000-8000-000000000001")
        XCTAssertEqual(row.ownerID, "00000000-0000-7000-8000-000000000003")
        XCTAssertEqual(row.host, "test-desktop")
        XCTAssertNil(row.archivedAt)
        XCTAssertFalse(row.working)
    }

    /// An older server sends none of the shelf fields: the row still decodes.
    func testALobbyRowWithoutTheShelfFieldsDecodes() throws {
        let row: LobbyRow = try JSONValue.object([
            "id": .string("s-1"), "title": .string("T"), "status": .string("open"), "working": .bool(false), "turns": .number(1),
        ]).decode()
        XCTAssertNil(row.settledAt)
        XCTAssertNil(row.snoozedUntil)
        XCTAssertNil(row.branch)
        XCTAssertNil(row.repository)
        XCTAssertNil(row.looperID)
    }

    func testAListSinceCarriesTheArchivedRow() throws {
        let listed: LobbyList = try Fixture.response("work_sessions/list_since").decode()
        XCTAssertNotNil(listed.sessions.first?.archivedAt)
        XCTAssertEqual(try Fixture.server("work_sessions/list_since")["payload"]?["since"]?.string, listed.now)
    }

    /// This exchange must come from a server recording, not a handwritten
    /// fixture: machine identity and routing fields must agree on the wire.
    func testAMachineCarriesItsDeskMachineAndItsDoor() throws {
        let reply: MachinesReply = try Fixture.response("work_sessions/machines").decode()
        let linked = try XCTUnwrap(reply.machines.first { $0.deskMachineID != nil })
        XCTAssertFalse(linked.address.isEmpty)
        XCTAssertEqual(linked.owner.kind, "user")
        XCTAssertNotNil(WireDate.parse(reply.now))
        let install = try XCTUnwrap(linked.deskInstalls?.first)
        XCTAssertEqual(install.deskMachineID, linked.deskMachineID)
        XCTAssertEqual(install.deskPresence, linked.deskPresence)
        XCTAssertEqual(install.doorPort, 6446)
        for service in linked.services {
            XCTAssertFalse(service.kind.isEmpty)
            XCTAssertGreaterThan(service.port, 0)
        }
    }

    func testOldServerWithoutInstallListStillDecodes() throws {
        let machine = try Build.machine(desk: "m-willow", deskPresence: "online")
        XCTAssertNil(machine.deskInstalls)
        XCTAssertEqual(machine.deskMachineID, "m-willow")
        XCTAssertEqual(machine.deskPresence, "online")
    }

    /// The recording holds one machine linked to a Desk (its door declared)
    /// and one 6net machine no Desk runs on: `desk_machine_id` null.
    func testALinkedAndAnUnlinkedMachineBothDecode() throws {
        let reply: MachinesReply = try Fixture.response("work_sessions/machines").decode()
        XCTAssertEqual(reply.machines.count, 2)

        let linked = try XCTUnwrap(reply.machines.first { $0.name == "test-desktop" })
        XCTAssertEqual(linked.deskMachineID, "00000000-0000-7000-8000-000000000001")
        XCTAssertEqual(linked.nodeID, "00000000-0000-7000-8000-000000000002")
        XCTAssertEqual(linked.services.map(\.kind), ["desk"])
        XCTAssertEqual(linked.services.first?.port, 6446)
        XCTAssertEqual(linked.services.first?.state, "approved")
        XCTAssertEqual(linked.presence.path, "offline")
        XCTAssertNil(linked.presence.lastSeenAt)

        let unlinked = try XCTUnwrap(reply.machines.first { $0.name == "white-heron" })
        XCTAssertNil(unlinked.deskMachineID)
        XCTAssertEqual(unlinked.services, [])
        XCTAssertEqual(unlinked.status, "active")
    }

    func testOrganisationsDecodeAsTheControllerWritesThem() throws {
        // The organizations response includes the authenticated person's id.
        // This shape has no recording.
        let body = #"{"organizations":[{"id":"o-1","slug":"northwind","name":"Northwind"}],"person":{"id":"u-morgan","email":"morgan@example.com","name":"Morgan"}}"#
        let organizations: Organizations = try JSONValue.parse(body).decode()
        XCTAssertEqual(organizations.organizations.map(\.slug), ["northwind"])
        XCTAssertEqual(organizations.person.id, "u-morgan")
        XCTAssertEqual(organizations.person.email, "morgan@example.com")
    }

    func testOrganisationsWithoutThePersonsIDAreRefused() {
        let body = #"{"organizations":[],"person":{"email":"morgan@example.com","name":null}}"#
        XCTAssertThrowsError(try JSONValue.parse(body).decode(Organizations.self))
    }

    func testAClaimReplyCarriesTheMatchNumber() throws {
        // The claim response includes the number the person matches on the Mac.
        let body = #"{"claim_id":"c-1","machine_label":"Morgan's MacBook Pro","expires_at":"2026-01-01T00:05:00.000000Z","poll_secret":"ps","match":"47"}"#
        let reply: ClaimReply = try JSONValue.parse(body).decode()
        XCTAssertEqual(reply.machineLabel, "Morgan's MacBook Pro")
        XCTAssertEqual(reply.match, "47")
        XCTAssertEqual(reply.pollSecret, "ps")
    }

    func testEveryPollStatusIsReadAndAnUnknownOneKeepsPolling() throws {
        let read = { (body: String) in try PollAnswer(JSONValue.parse(body)) }
        XCTAssertEqual(try read(#"{"status":"pending"}"#), .waiting("pending"))
        XCTAssertEqual(try read(#"{"status":"asking"}"#), .waiting("asking"))
        XCTAssertEqual(try read(#"{"status":"allowed_pending"}"#), .waiting("allowed_pending"))
        XCTAssertEqual(try read(#"{"status":"denied"}"#), .denied)
        XCTAssertEqual(try read(#"{"status":"spent"}"#), .spent)
        XCTAssertEqual(try read(#"{"status":"expired"}"#), .expired)
        XCTAssertEqual(try read(#"{"status":"failed","failure":"no_gateway"}"#), .failed("no_gateway"))
        XCTAssertEqual(try read(#"{"status":"a_word_from_a_newer_server"}"#), .waiting("a_word_from_a_newer_server"))
    }

    func testAnAllowedPollKeepsTheEnrolmentAsTheServerSpelledIt() throws {
        let body = #"{"status":"allowed","access_token":"bearer-1","token_type":"Bearer","expires_in":2592000,"scope":"agent:connect agent:execute","enrollment":{"enrollment_id":"e-1","preauth_key":"k","gateway_port":443}}"#
        guard case .allowed(let granted) = try PollAnswer(JSONValue.parse(body)) else {
            return XCTFail("not allowed")
        }
        XCTAssertEqual(granted.accessToken, "bearer-1")
        XCTAssertEqual(granted.expiresIn, 2_592_000)
        XCTAssertEqual(granted.enrollmentID, "e-1")
        XCTAssertEqual(try JSONValue.parse(granted.enrollmentJSON)["preauth_key"]?.string, "k")
        XCTAssertFalse(String(describing: granted).contains("bearer-1"), "a Granted never prints its token")
    }

    func testMicrosecondInstantsParse() throws {
        XCTAssertNotNil(WireDate.parse("2026-01-01T00:00:00.000000Z"))
        XCTAssertNotNil(WireDate.parse("2026-01-01T00:00:00Z"))
        let later = try XCTUnwrap(WireDate.parse("2026-01-01T00:00:01.500000Z"))
        let earlier = try XCTUnwrap(WireDate.parse("2026-01-01T00:00:00Z"))
        XCTAssertEqual(later.timeIntervalSince(earlier), 1.5, accuracy: 0.001)
        XCTAssertNil(WireDate.parse("yesterday"))
    }

    func testEachFractionLengthParsesToItsInstant() throws {
        let whole = try XCTUnwrap(WireDate.parse("2026-01-01T00:00:00Z"))
        let millis = try XCTUnwrap(WireDate.parse("2026-01-01T00:00:00.250Z"))
        let micros = try XCTUnwrap(WireDate.parse("2026-01-01T00:00:00.250125Z"))
        XCTAssertEqual(millis.timeIntervalSince(whole), 0.25, accuracy: 0.0001)
        XCTAssertEqual(micros.timeIntervalSince(whole), 0.250125, accuracy: 0.00001)
        XCTAssertEqual(whole.timeIntervalSince1970, 1_767_225_600, accuracy: 0.0001)
    }

    /// The server re-records both lists with `attention_line`. A row waiting
    /// on nothing carries null; a waiting row carries the push's line, built
    /// from the request's kind alone. A teammate's waiting row also carries
    /// null (the line is the owner's), so waiting does not imply a line.
    func testTheListRowCarriesWhatItWaitsOn() throws {
        let idle = try XCTUnwrap(Fixture.response("work_sessions/list")["sessions"]?.array?.first)
        guard case .object(let fields) = idle else { return XCTFail("not a row") }
        XCTAssertTrue(fields.keys.contains("attention_line"), "the recorded session includes attention_line")
        let idleRow: LobbyRow = try idle.decode()
        XCTAssertEqual(idleRow.attention, "none")
        XCTAssertNil(idleRow.attentionLine)

        let listed: LobbyList = try Fixture.response("work_sessions/list_attention").decode()
        let waiting = try XCTUnwrap(listed.sessions.first)
        XCTAssertEqual(waiting.attention, "approval")
        XCTAssertEqual(waiting.attentionLine, "Wants to edit")

        let built = try Build.row("s-1", title: "T", mac: "m", host: "h", attention: "approval", attentionLine: "Wants to execute")
        XCTAssertEqual(built.attentionLine, "Wants to execute")
    }
}
