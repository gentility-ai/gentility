import XCTest
@testable import DeskPhoneKit

final class ClaimLinkTests: XCTestCase {
    private let code = "q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U"

    func testAnHTTPSClaimLinkIsItsOriginAndItsCode() throws {
        let link = try XCTUnwrap(ClaimLink.parse("https://mcp.gentility.ai/phone/claim#\(code)"))
        XCTAssertEqual(link.origin.absoluteString, "https://mcp.gentility.ai")
        XCTAssertEqual(link.host, "mcp.gentility.ai")
        XCTAssertEqual(link.code, code)
    }

    func testAPortStaysOnTheOrigin() throws {
        #if DEBUG
        let link = try XCTUnwrap(ClaimLink.parse("https://staging.example:8443/phone/claim#\(code)"))
        XCTAssertEqual(link.origin.absoluteString, "https://staging.example:8443")
        #else
        XCTAssertNil(ClaimLink.parse("https://staging.example:8443/phone/claim#\(code)"))
        #endif
    }

    func testPlainHTTPIsOnlyForLocalhost() {
        #if DEBUG
        XCTAssertNotNil(ClaimLink.parse("http://localhost:4002/phone/claim#\(code)"))
        XCTAssertNotNil(ClaimLink.parse("http://mcp.let-s-revisit.localhost:4102/phone/claim#\(code)"))
        #else
        XCTAssertNil(ClaimLink.parse("http://localhost/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://mcp.localhost/phone/claim#\(code)"))
        #endif
        XCTAssertNil(ClaimLink.parse("http://mcp.gentility.ai/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://localhost.evil.example/phone/claim#\(code)"))
    }

    func testAnythingElseIsNotADeskCode() {
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim"), "no code")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim#"), "an empty code")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim/#\(code)"), "another path")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/device#\(code)"), "another path")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai@evil.example/phone/claim#\(code)"), "userinfo hides the host")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim?code=\(code)"), "the code is never in the query")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim#not a code!"), "not base64url")
        XCTAssertNil(ClaimLink.parse("WIFI:S:home;T:WPA;P:secret;;"))
        XCTAssertNil(ClaimLink.parse("gendesk://phone/claim#\(code)"))
    }

    func testAnEmptyHostIsRefused() {
        XCTAssertNil(ClaimLink.parse("https:///phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("https://:8443/phone/claim#\(code)"))
    }

    func testPercentEscapesNeverDisguiseTheHostThePathOrTheCode() {
        XCTAssertNil(ClaimLink.parse("http://evil.example%2F.localhost/phone/claim#\(code)"), "an escaped host")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone%2Fclaim#\(code)"), "an escaped path")
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai/phone/claim#%71\(code)"), "an escaped code")
    }

    func testTheOriginIsTheSchemeHostAndPortOnly() throws {
        #if DEBUG
        let link = try XCTUnwrap(ClaimLink.parse("  HTTP://MCP.Let-S-Revisit.LOCALHOST:4102/phone/claim#\(code)\n"))
        XCTAssertEqual(link.origin.absoluteString, "http://mcp.let-s-revisit.localhost:4102")
        XCTAssertEqual(link.host, "mcp.let-s-revisit.localhost")
        XCTAssertEqual(link.code, code)
        #else
        let link = try XCTUnwrap(ClaimLink.parse("  HTTPS://MCP.GENTILITY.AI:443/phone/claim#\(code)\n"))
        XCTAssertEqual(link.origin.absoluteString, "https://mcp.gentility.ai:443")
        XCTAssertEqual(link.code, code)
        #endif
    }

    func testAPortOutsideTheRangeIsRefused() throws {
        XCTAssertNil(ClaimLink.parse("https://staging.example:0/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("https://staging.example:65536/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("https://staging.example:70000/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://localhost:0/phone/claim#\(code)"))
        #if DEBUG
        let edges = try XCTUnwrap(ClaimLink.parse("https://staging.example:65535/phone/claim#\(code)"))
        XCTAssertEqual(edges.origin.absoluteString, "https://staging.example:65535")
        XCTAssertNotNil(ClaimLink.parse("https://staging.example:1/phone/claim#\(code)"))
        #endif
    }

    func testATrailingDotHostIsRefused() {
        XCTAssertNil(ClaimLink.parse("https://mcp.gentility.ai./phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://localhost./phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://mcp.let-s-revisit.localhost.:4102/phone/claim#\(code)"))
    }

    func testLocalhostMustEndTheHostForPlainHTTP() {
        XCTAssertNil(ClaimLink.parse("http://mcp.localhost.evil/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://mcp.localhost.evil:4102/phone/claim#\(code)"))
        XCTAssertNil(ClaimLink.parse("http://evillocalhost/phone/claim#\(code)"))
    }

    func testAnHTTPSIPv6LiteralKeepsItsBrackets() throws {
        #if DEBUG
        let link = try XCTUnwrap(ClaimLink.parse("https://[2001:DB8::1]:8443/phone/claim#\(code)"))
        XCTAssertEqual(link.origin.absoluteString, "https://[2001:db8::1]:8443")
        XCTAssertEqual(link.host, "[2001:db8::1]")
        #else
        XCTAssertNil(ClaimLink.parse("https://[2001:DB8::1]/phone/claim#\(code)"))
        #endif
        XCTAssertNil(ClaimLink.parse("http://[::1]/phone/claim#\(code)"), "plain HTTP is for a localhost name only")
    }

    func testALinkNeverPrintsItsCode() throws {
        let link = try XCTUnwrap(ClaimLink.parse("https://mcp.gentility.ai/phone/claim#\(code)"))
        XCTAssertFalse(String(describing: link).contains(code))
        XCTAssertFalse(String(reflecting: link).contains(code))
        var dumped = ""
        dump(link, to: &dumped)
        XCTAssertFalse(dumped.contains(code), dumped)
        XCTAssertTrue(dumped.contains("mcp.gentility.ai"), dumped)
        XCTAssertFalse(Mirror(reflecting: link).children.contains { "\($0.value)".contains(code) })
    }
}
