import XCTest
@testable import DeskPhoneKit

final class ClaimOriginTests: XCTestCase {
    func testReleasePairsOnlyWithProductionOrigin() {
        let code = "abcdefghijklmnop"
        for origin in ["https://mcp.gentility.ai", "https://mcp.gentility.ai:443", "HTTPS://MCP.GENTILITY.AI"] {
            XCTAssertNotNil(ClaimLink.parse("\(origin)/phone/claim#\(code)"), origin)
        }
        #if !DEBUG
        for origin in [
            "https://evil.example", "https://mcp.gentility.ai.example.test",
            "https://mcp.gentility.ai:8443", "http://mcp.gentility.ai",
            "http://localhost", "https://localhost", "http://mcp.localhost",
            "https://[::1]", "https://gentility.ai"
        ] {
            XCTAssertNil(ClaimLink.parse("\(origin)/phone/claim#\(code)"), origin)
        }
        #endif
    }

    #if DEBUG
    func testDebugStillPairsWithDevelopment() {
        XCTAssertNotNil(ClaimLink.parse("http://mcp.localhost/phone/claim#abcdefghijklmnop"))
        XCTAssertNotNil(ClaimLink.parse("https://staging.example/phone/claim#abcdefghijklmnop"))
    }
    #endif
}
