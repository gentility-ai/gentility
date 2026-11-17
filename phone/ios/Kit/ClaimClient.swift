import Foundation

/// The phone's two unauthenticated calls while it holds nothing but a code.
/// `claim` throws `ClaimRefusal` for a refusal and anything else for no
/// answer. `poll` throws `PollUnanswered` when the same poll may be asked
/// again (no answer, or a 5xx) and `ClaimRefusal` for any other status that
/// is not an answer, which ends the poll.
public protocol ClaimTransport: Sendable {
    func claim(code: String, phoneName: String) async throws -> ClaimReply
    func poll(secret: String) async throws -> PollAnswer
}

/// A poll that got no answer at all, or a 5xx: ask again. The reason is a
/// status or a transport error's code, never anything of the body.
public struct PollUnanswered: Error, Equatable, Sendable {
    public let reason: String

    public init(reason: String) {
        self.reason = reason
    }
}

public struct ClaimClient: ClaimTransport {
    public let origin: URL
    private let session: URLSession

    /// `session` is any session; whichever it is, no redirect is followed,
    /// since one would resend the code or the poll secret wherever it points.
    public init(origin: URL, session: URLSession = RefusingRedirects.shared) {
        self.origin = origin
        self.session = session
    }

    public func claim(code: String, phoneName: String) async throws -> ClaimReply {
        let (status, body) = try await post("oauth/phone/claim", ["code": .string(code), "phone_name": .string(phoneName)])
        guard status == 202 else { throw Self.refusal(status, body) }
        // The server spent the code to answer: a reply this phone cannot read
        // is the server's fault, not the network's, and scanning again needs
        // a new code.
        guard let reply = try? body.decode(ClaimReply.self) else {
            Log.pairing.error("the claim's 202 does not read as a claim reply")
            throw ClaimRefusal.malformed
        }
        return reply
    }

    public func poll(secret: String) async throws -> PollAnswer {
        let status: Int
        let body: JSONValue
        do {
            (status, body) = try await post("oauth/phone/poll", ["poll_secret": .string(secret)])
        } catch let error as URLError {
            throw PollUnanswered(reason: "URLError \(error.code.rawValue)")
        }
        switch status {
        case 200: return try PollAnswer(body)
        // The poll secret is not one the server handed out.
        case 404: return .unknown
        case 500...599: throw PollUnanswered(reason: "HTTP \(status)")
        default: throw Self.refusal(status, body)
        }
    }

    private static func refusal(_ status: Int, _ body: JSONValue) -> ClaimRefusal {
        (300...399).contains(status) ? .other("redirected") : ClaimRefusal(word: body["error"]?.string)
    }

    private func post(_ path: String, _ fields: [String: JSONValue]) async throws -> (Int, JSONValue) {
        var request = URLRequest(url: origin.appendingPathComponent(path))
        request.httpMethod = "POST"
        request.timeoutInterval = 15
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.httpBody = try JSONValue.object(fields).data()
        let started = ContinuousClock.now
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request, delegate: RefusingRedirects.delegate)
        } catch {
            Log.pairing.error("POST /\(path, privacy: .public) got no answer after \(String(describing: ContinuousClock.now - started), privacy: .public): \(String(describing: error), privacy: .public)")
            throw error
        }
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        Log.pairing.info("POST /\(path, privacy: .public) answered \(status) in \(String(describing: ContinuousClock.now - started), privacy: .public)")
        return (status, (try? JSONValue.parse(data)) ?? .null)
    }
}

/// Follows no redirect: the task is answered with the 3xx itself. Every call
/// that carries a code, a poll secret or a bearer goes through it, as the
/// session's delegate (`shared`, `session(_:)`) or the task's (`delegate`).
public final class RefusingRedirects: NSObject, URLSessionTaskDelegate, Sendable {
    public static let delegate = RefusingRedirects()

    /// Ephemeral: no cookies, no cache.
    public static let shared = session(.ephemeral)

    /// A session with this delegate. A session holds its delegate until it
    /// is invalidated, so make one per configuration, not one per call.
    public static func session(_ configuration: URLSessionConfiguration) -> URLSession {
        URLSession(configuration: configuration, delegate: delegate, delegateQueue: nil)
    }

    public func urlSession(
        _ session: URLSession, task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        Log.app.error("refused a \(response.statusCode) redirect from \(response.url?.host ?? "?", privacy: .public) to \(request.url?.host ?? "?", privacy: .public)")
        completionHandler(nil)
    }
}
