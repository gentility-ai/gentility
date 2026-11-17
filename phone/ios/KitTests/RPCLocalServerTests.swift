import XCTest
@testable import DeskPhoneKit

/// Launched only by desk_phone_simulator_test.exs, with sandbox credentials
/// passed in the child environment, never written to a recording or store.
final class RPCLocalServerTests: XCTestCase {
    func testActualPhoneClientAgainstIsolatedServer() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let origin = env["PHONE_RPC_URL"] else { throw XCTSkip("Run the macOS ExUnit simulator integration test.") }
        let token = try XCTUnwrap(env["PHONE_RPC_TOKEN"])
        let org = try XCTUnwrap(env["PHONE_RPC_ORG"])
        let person = try XCTUnwrap(env["PHONE_RPC_PERSON"])
        let session = try XCTUnwrap(env["PHONE_RPC_SESSION"])
        let credential = Credential(origin: try XCTUnwrap(URL(string: origin)), bearer: token, expiresAt: .distantFuture,
                                    enrollmentJSON: nil, enrollmentID: nil, phoneName: "Simulator proof")
        let lobby = ServerLobby(credential: credential, forcePhoenix: false,
                                makeTransport: { XCTFail("Unexpected Phoenix fallback"); return FakeTransport() })
        let listed = try await lobby.list(organizationID: org, since: nil)
        XCTAssertTrue(listed.sessions.contains { $0.id == session })
        _ = try await lobby.machines(organizationID: org)
        let changed = try await lobby.inbox(organizationID: org, id: session, field: .settledAt, value: Date())
        XCTAssertTrue(changed.applied.contains("settled_at"))
        let registered = await lobby.register(personID: person, name: "Simulator proof", enrollmentID: nil, pushToken: nil, environment: nil)
        XCTAssertTrue(registered)
        let repeated = await lobby.register(personID: person, name: "Simulator proof", enrollmentID: nil, pushToken: nil, environment: nil)
        XCTAssertTrue(repeated)
        print("PHONE_RPC_PROOF build=\(PhoneBuild.stamp) methods=4 registration_repeated=true")
    }
}
