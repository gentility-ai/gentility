import XCTest
@testable import DeskPhoneKit

final class ClaimClientTests: XCTestCase {
    private let origin = URL(string: "https://mcp.gentility.test")!

    override func setUp() {
        StubURLProtocol.reset()
    }

    override func tearDown() {
        StubURLProtocol.reset()
    }

    private var client: ClaimClient { ClaimClient(origin: origin, session: StubURLProtocol.session()) }

    func testAClaimPostsTheCodeAndTheNameToTheLinksOrigin() async throws {
        StubURLProtocol.answer = { _ in
            (202, #"{"claim_id":"c-1","machine_label":"Morgan's MacBook Pro","expires_at":"2026-01-01T00:05:00.000000Z","poll_secret":"ps","match":"47"}"#)
        }
        let reply = try await client.claim(code: "the-code-from-the-fragment", phoneName: "Morgan's iPhone")
        XCTAssertEqual(reply.match, "47")
        let seen = try XCTUnwrap(StubURLProtocol.seen.first)
        XCTAssertEqual(seen.request.url?.absoluteString, "https://mcp.gentility.test/oauth/phone/claim")
        XCTAssertEqual(seen.request.httpMethod, "POST")
        let body = try JSONValue.parse(seen.body)
        XCTAssertEqual(body["code"]?.string, "the-code-from-the-fragment")
        XCTAssertEqual(body["phone_name"]?.string, "Morgan's iPhone")
    }

    /// The code is spent once the server answers 202, so a reply this phone
    /// cannot read is the server's fault, not the network's.
    func testAClaimReplyThatDoesNotReadIsMalformed() async {
        StubURLProtocol.answer = { _ in (202, #"{"claim_id":"c-1"}"#) }
        do {
            _ = try await client.claim(code: "c", phoneName: "n")
            XCTFail("an unreadable reply was read")
        } catch {
            XCTAssertEqual(error as? ClaimRefusal, .malformed)
        }
    }

    func testEveryClaimRefusalIsItsWord() async {
        let cases: [(Int, String, ClaimRefusal)] = [
            (404, "unknown", .unknown), (410, "expired", .expired), (410, "spent", .spent),
            (422, "blank_name", .blankName), (422, "malformed", .malformed), (500, "boom", .other("boom")),
        ]
        for (status, word, refusal) in cases {
            StubURLProtocol.answer = { _ in (status, #"{"error":"\#(word)","error_description":"a sentence"}"#) }
            do {
                _ = try await client.claim(code: "c", phoneName: "n")
                XCTFail("\(word) was not refused")
            } catch {
                XCTAssertEqual(error as? ClaimRefusal, refusal)
            }
        }
    }

    func testAPollAsksWithThePollSecretAlone() async throws {
        StubURLProtocol.answer = { _ in (200, #"{"status":"asking"}"#) }
        let answer = try await client.poll(secret: "ps")
        XCTAssertEqual(answer, .waiting("asking"))
        let body = try JSONValue.parse(XCTUnwrap(StubURLProtocol.seen.first).body)
        XCTAssertEqual(body, .object(["poll_secret": .string("ps")]))
        XCTAssertEqual(StubURLProtocol.seen.first?.request.url?.path, "/oauth/phone/poll")
    }

    func testAPollSecretTheServerNeverHandedOutIsUnknown() async throws {
        StubURLProtocol.answer = { _ in (404, #"{"error":"unknown"}"#) }
        let answer = try await client.poll(secret: "ps")
        XCTAssertEqual(answer, .unknown)
    }

    // F5: only no answer at all, or a 5xx, may be asked again.

    func testAPollThatGetsNoAnswerMayBeAskedAgain() async {
        StubURLProtocol.fails = URLError(.notConnectedToInternet)
        do {
            _ = try await client.poll(secret: "ps")
            XCTFail("no answer was an answer")
        } catch {
            XCTAssertTrue(error is PollUnanswered, "\(error)")
        }
    }

    func testAPollThatGetsA5xxMayBeAskedAgain() async {
        StubURLProtocol.answer = { _ in (503, "<html>down</html>") }
        do {
            _ = try await client.poll(secret: "ps")
            XCTFail("a 503 was an answer")
        } catch {
            XCTAssertEqual(error as? PollUnanswered, PollUnanswered(reason: "HTTP 503"))
        }
    }

    func testAnyOtherPollStatusIsItsAnswer() async {
        for (status, body, refusal) in [
            (422, #"{"error":"malformed"}"#, ClaimRefusal.malformed),
            (410, #"{"error":"spent"}"#, .spent),
            (401, "", .other("unreadable")),
        ] {
            StubURLProtocol.answer = { _ in (status, body) }
            do {
                _ = try await client.poll(secret: "ps")
                XCTFail("\(status) was not an answer")
            } catch {
                XCTAssertEqual(error as? ClaimRefusal, refusal, "\(status)")
            }
        }
    }

    // F4: a redirect would resend the code or the poll secret wherever it
    // points. None is followed, whatever session the client was handed.

    func testAClaimThatIsRedirectedIsRefusedAndNotResent() async {
        StubURLProtocol.answer = { _ in (307, "") }
        do {
            _ = try await client.claim(code: "the-code-from-the-fragment", phoneName: "n")
            XCTFail("a redirect was followed to an answer")
        } catch {
            XCTAssertEqual(error as? ClaimRefusal, .other("redirected"))
        }
        XCTAssertEqual(StubURLProtocol.seen.map { $0.request.url?.host }, ["mcp.gentility.test"], "the code went nowhere else")
    }

    func testAPollThatIsRedirectedIsRefusedAndNotResent() async {
        for status in [301, 302, 303, 307, 308] {
            StubURLProtocol.seen = []
            StubURLProtocol.answer = { _ in (status, "") }
            do {
                _ = try await client.poll(secret: "ps")
                XCTFail("\(status) was followed to an answer")
            } catch {
                XCTAssertEqual(error as? ClaimRefusal, .other("redirected"), "\(status) is a refusal, not a retry")
            }
            XCTAssertEqual(StubURLProtocol.seen.map { $0.request.url?.host }, ["mcp.gentility.test"], "\(status)")
        }
    }

    /// The session the bearer's own calls use (`/oauth/organizations`)
    /// refuses a redirect by itself, with no client in between.
    func testTheRefusingSessionFollowsNoRedirect() async throws {
        StubURLProtocol.answer = { _ in (307, "") }
        let session = RefusingRedirects.session(StubURLProtocol.configuration())
        defer { session.invalidateAndCancel() }
        let (_, response) = try await session.data(for: URLRequest(url: origin.appendingPathComponent("oauth/organizations")))
        XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 307)
        XCTAssertEqual(StubURLProtocol.seen.count, 1)
    }

    /// The stub's redirect is a real one: a plain session follows it. Without
    /// this the two tests above would pass for a stub that never redirects.
    func testAPlainSessionWouldHaveFollowedTheRedirect() async throws {
        StubURLProtocol.answer = { request in request.url?.host == "elsewhere.test" ? (200, "{}") : (307, "") }
        let (_, response) = try await StubURLProtocol.session().data(for: URLRequest(url: origin.appendingPathComponent("oauth/phone/poll")))
        XCTAssertEqual((response as? HTTPURLResponse)?.statusCode, 200)
        XCTAssertEqual(StubURLProtocol.seen.map { $0.request.url?.host }, ["mcp.gentility.test", "elsewhere.test"])
    }
}
