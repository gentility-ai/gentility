import Foundation

// Runs the release policy without a simulator, using the production parser.
let code = "abcdefghijklmnop"
var failures = 0
func check(_ origin: String, allowed: Bool) {
    let accepted = ClaimLink.parse("\(origin)/phone/claim#\(code)") != nil
    if accepted != allowed {
        print("FAIL: \(origin): accepted=\(accepted), expected=\(allowed)")
        failures += 1
    }
}
for origin in ["https://mcp.gentility.ai", "https://mcp.gentility.ai:443", "HTTPS://MCP.GENTILITY.AI"] {
    check(origin, allowed: true)
}
#if DEBUG
for origin in ["http://localhost", "http://mcp.localhost", "https://staging.example"] {
    check(origin, allowed: true)
}
#else
for origin in ["https://evil.example", "https://mcp.gentility.ai.example.test", "https://mcp.gentility.ai:8443", "http://localhost", "https://localhost", "http://mcp.localhost", "https://[::1]", "https://gentility.ai"] {
    check(origin, allowed: false)
}
#endif
check("http://mcp.gentility.ai", allowed: false)
check("http://localhost.evil.example", allowed: false)
if failures > 0 { exit(1) }
print("Claim origin checks passed")
