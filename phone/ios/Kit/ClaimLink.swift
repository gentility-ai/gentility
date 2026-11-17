import Foundation

/// A Desk QR, read: `https://<mcp host>/phone/claim#<code>`. The origin is
/// where this phone will pair, and every call it makes from then on goes
/// there; the code rides the fragment, which no server ever sees.
public struct ClaimLink: Equatable, Sendable, CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    /// `scheme://host[:port]` of the link, nothing else.
    public let origin: URL
    public let host: String
    public let code: String

    public static func parse(_ text: String) -> ClaimLink? {
        // The percent-encoded parts are compared, not the decoded ones: an
        // escape never disguises a host, a path or a code, since a real link
        // has none.
        guard let parts = URLComponents(string: text.trimmingCharacters(in: .whitespacesAndNewlines)),
              let scheme = parts.scheme?.lowercased(),
              let rawHost = parts.percentEncodedHost, !rawHost.isEmpty, parts.host == rawHost,
              parts.user == nil, parts.password == nil,
              parts.query == nil,
              parts.percentEncodedPath == "/phone/claim",
              let code = parts.percentEncodedFragment, valid(code) else {
            return nil
        }
        let host = rawHost.lowercased()
        // A trailing dot names the same host to DNS and another to TLS, cookies
        // and the server's own origin checks; no Desk link carries one.
        guard !host.hasSuffix(".") else { return nil }
        if let port = parts.port, !(1...65535).contains(port) { return nil }
        #if DEBUG
        let local = host == "localhost" || host.hasSuffix(".localhost")
        guard scheme == "https" || (scheme == "http" && local) else { return nil }
        #else
        // The production server's phone_claim_mint link names this origin.
        guard scheme == "https", host == "mcp.gentility.ai",
              parts.port == nil || parts.port == 443 else { return nil }
        #endif

        var origin = URLComponents()
        origin.scheme = scheme
        origin.host = host
        origin.port = parts.port
        guard let url = origin.url else { return nil }
        return ClaimLink(origin: url, host: host, code: code)
    }

    /// base64url, and the length of something minted rather than typed.
    private static func valid(_ code: String) -> Bool {
        (16...256).contains(code.count) && code.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }
    }

    public var description: String { "ClaimLink(\(origin.absoluteString), code redacted)" }
    public var debugDescription: String { description }
    /// What `dump` and a debugger show: the code is never one of them.
    public var customMirror: Mirror {
        Mirror(self, children: ["origin": origin, "host": host, "code": "redacted"], displayStyle: .struct)
    }
}
