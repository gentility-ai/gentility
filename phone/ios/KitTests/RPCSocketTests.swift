import XCTest
@testable import DeskPhoneKit

final class RPCSocketTests: XCTestCase {
    private func socket(_ transport: FakeTransport, within: Duration = .seconds(30), maximum: Int = 64) -> RPCSocket {
        RPCSocket(transport: transport, origin: URL(string: "https://mcp.gentility.test?old=value#fragment")!,
                  bearer: "private-token", replyWithin: within, maximumPending: maximum)
    }

    func testHeaderAuthenticationAndOutOfOrderReplies() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport)
        try await rpc.connect()
        XCTAssertEqual(transport.requests.first?.url?.absoluteString, "wss://mcp.gentility.test/desk/rpc")
        XCTAssertEqual(transport.requests.first?.value(forHTTPHeaderField: "Authorization"), "Bearer private-token")
        XCTAssertFalse(String(reflecting: rpc).contains("private-token"))
        async let one = rpc.call("sessions.list", .object(["org": .string("o-1")]))
        let first = await transport.nextRPC()
        async let two = rpc.call("hosts.machines", .object(["org": .string("o-1")]))
        let second = await transport.nextRPC()
        XCTAssertNotEqual(first["id"], second["id"])
        transport.replyRPC(to: second, .string("second"))
        transport.replyRPC(to: first, .string("first"))
        let replies = try await (one, two)
        XCTAssertEqual(replies.0, .string("first"))
        XCTAssertEqual(replies.1, .string("second"))
        await rpc.disconnect()
    }

    func testStructuredRefusalAndUnknownNotification() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport)
        try await rpc.connect()
        async let answer = rpc.call("inbox.command", .object([:]))
        let frame = await transport.nextRPC()
        transport.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("future.hint"), "params": .object([:])]))
        transport.deliver(.object(["jsonrpc": .string("2.0"), "id": frame["id"]!,
                                  "error": .object(["code": .number(-32001), "message": .string("Not allowed."),
                                                    "data": .object(["reason": .string("forbidden")])])]))
        do { _ = try await answer; XCTFail("accepted") }
        catch { XCTAssertEqual(error as? RPCError, .refused(code: -32001, reason: "forbidden", message: "Not allowed.")) }
        await rpc.disconnect()
    }

    func testRevocationEndsPendingCallsAndCannotReconnect() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport)
        try await rpc.connect()
        async let answer = rpc.call("phone.register", .object([:]))
        _ = await transport.nextRPC()
        transport.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("rpc.revoked"),
                                  "params": .object(["scope": .string("connection"), "reason": .string("revoked")])]))
        do { _ = try await answer; XCTFail("accepted") }
        catch { XCTAssertEqual(error as? RPCError, .revoked) }
        do { try await rpc.connect(); XCTFail("reconnected") }
        catch { XCTAssertEqual(error as? RPCError, .revoked) }
        XCTAssertEqual(transport.opened.count, 1)
    }

    func testOrganizationRevocationLeavesOtherOrganizationsUsable() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport)
        try await rpc.connect()
        async let answer = rpc.call("sessions.list", .object(["org": .string("other")]))
        let frame = await transport.nextRPC()
        transport.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("rpc.revoked"),
                                  "params": .object(["scope": .string("org"), "org": .string("gone")])]))
        transport.replyRPC(to: frame, .object([:]))
        _ = try await answer
        do { _ = try await rpc.call("sessions.list", .object(["org": .string("gone")])); XCTFail("sent") }
        catch { XCTAssertEqual(error as? RPCError, .refused(code: -32001, reason: "forbidden", message: "")) }
        await rpc.disconnect()
    }

    func testLostWriteReplyTimesOutWithoutReplay() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport, within: .milliseconds(20))
        try await rpc.connect()
        async let answer = rpc.call("inbox.command", .object([:]))
        let frame = await transport.nextRPC()
        XCTAssertEqual(frame["method"], .string("inbox.command"))
        do { _ = try await answer; XCTFail("answered") }
        catch { XCTAssertEqual(error as? RPCError, .timeout) }
        // A late result is ignored, and a new call has a new id.
        transport.replyRPC(to: frame, .null)
        async let next = rpc.call("sessions.list", .object([:]))
        let nextFrame = await transport.nextRPC()
        XCTAssertEqual(nextFrame["method"], .string("sessions.list"))
        transport.replyRPC(to: nextFrame, .null)
        _ = try await next
        await rpc.disconnect()
    }

    func testPendingLimitAndOversizedRequestRefuseBeforeSend() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport, maximum: 1)
        try await rpc.connect()
        do { _ = try await rpc.call("phone.register", .string(String(repeating: "x", count: RPCSocket.largestRequest))); XCTFail("sent") }
        catch { XCTAssertEqual(error as? RPCError, .tooLarge) }
        async let answer = rpc.call("sessions.list", .object([:]))
        let frame = await transport.nextRPC()
        do { _ = try await rpc.call("hosts.machines", .object([:])); XCTFail("sent") }
        catch { XCTAssertEqual(error as? RPCError, .busy) }
        transport.replyRPC(to: frame, .null)
        _ = try await answer
        await rpc.disconnect()
    }

    func testUpgradeRefusalIsPreservedAndUnansweredUpgradeIsBounded() async throws {
        let transport = FakeTransport()
        transport.refusal = 403
        do { try await socket(transport).connect(); XCTFail("opened") }
        catch { XCTAssertEqual(error as? PhoenixError, .refused(status: 403)) }
        let hanging = FakeTransport()
        hanging.hangs = true
        do { try await socket(hanging, within: .milliseconds(20)).connect(); XCTFail("opened") }
        catch { XCTAssertEqual(error as? RPCError, .timeout) }
        XCTAssertEqual(hanging.closes, 1)
    }

    func testCancellationReleasesPendingSlotAndLateAnswerCannotSettleNextCall() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport, maximum: 1)
        try await rpc.connect()
        let abandoned = Task { try await rpc.call("inbox.command", .object([:])) }
        let old = await transport.nextRPC()
        abandoned.cancel()
        do { _ = try await abandoned.value; XCTFail("answered") }
        catch { XCTAssertTrue(error is CancellationError) }
        async let next = rpc.call("sessions.list", .object([:]))
        let fresh = await transport.nextRPC()
        transport.replyRPC(to: old, .string("old"))
        transport.replyRPC(to: fresh, .string("new"))
        let reply = try await next
        XCTAssertEqual(reply, .string("new"))
        await rpc.disconnect()
    }

    func testCloseCodesPreserveRevocationAndOversizedFrameFailure() async throws {
        for code in [4401, 4409, 1009] {
            let transport = FakeTransport()
            let rpc = socket(transport)
            try await rpc.connect()
            async let answer = rpc.call("sessions.list", .object([:]))
            _ = await transport.nextRPC()
            transport.closeCode = code
            transport.close()
            do { _ = try await answer; XCTFail("answered") }
            catch { XCTAssertEqual(error as? RPCError, code == 1009 ? .closed(1009) : .revoked) }
        }
    }

    func testRevokedOrganizationRejectsItsInFlightReply() async throws {
        let transport = FakeTransport()
        let rpc = socket(transport)
        try await rpc.connect()
        async let answer = rpc.call("sessions.list", .object(["org": .string("gone")]))
        let frame = await transport.nextRPC()
        transport.deliver(.object(["jsonrpc": .string("2.0"), "method": .string("rpc.revoked"),
                                  "params": .object(["scope": .string("org"), "org": .string("gone")])]))
        transport.replyRPC(to: frame, .object([:]))
        do { _ = try await answer; XCTFail("accepted stale data") }
        catch { XCTAssertEqual(error as? RPCError, .refused(code: -32001, reason: "forbidden", message: "")) }
        await rpc.disconnect()
    }
}
