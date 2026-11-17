import Foundation

/// `URLSessionWebSocketTask`, with the upgrade's answer waited for so that a
/// refused bearer is a status (403) and not a silent close. Its own ephemeral
/// session (no cookies, no cache), made per open and invalidated on close.
/// Single-use: once closed, an open is refused with `.closed`, so an open
/// still to start when its socket gave up makes no session to leak.
/// Like every call that carries a bearer, it follows no redirect
/// (`RefusingRedirects`): a 3xx to the upgrade is a refusal with that status.
/// Nothing it throws is a `URLError`, whose description carries the URL and
/// with it the bearer: it throws `PhoenixError` alone.
public final class URLSessionTransport: NSObject, RPCTransport, URLSessionWebSocketDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private var session: URLSession?
    private var task: URLSessionWebSocketTask?
    private var opening: CheckedContinuation<Void, Error>?
    private var shut = false

    public override init() {}

    /// The socket's task, taking frames up to `DoorLimits.largestFrame`
    /// rather than URLSession's 1 MiB default, so a large reply is read
    /// whole instead of dropping the socket.
    static func task(_ session: URLSession, _ url: URL) -> URLSessionWebSocketTask {
        task(session, URLRequest(url: url))
    }

    static func task(_ session: URLSession, _ request: URLRequest) -> URLSessionWebSocketTask {
        let task = session.webSocketTask(with: request)
        task.maximumMessageSize = DoorLimits.largestFrame
        return task
    }

    public func open(_ url: URL) async throws {
        try await open(URLRequest(url: url))
    }

    public func open(_ request: URLRequest) async throws {
        let made: URLSessionWebSocketTask? = lock.withLock {
            guard !shut, session == nil else { return nil }
            let session = URLSession(configuration: .ephemeral, delegate: self, delegateQueue: nil)
            let task = Self.task(session, request)
            self.session = session
            self.task = task
            return task
        }
        guard let task = made else { throw PhoenixError.closed }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            let closed = lock.withLock {
                if !shut { opening = continuation }
                return shut
            }
            if closed {
                continuation.resume(throwing: PhoenixError.closed)
            } else {
                task.resume()
            }
        }
    }

    public func send(_ text: String) async throws {
        guard let task = lock.withLock({ task }) else { throw PhoenixError.closed }
        do {
            try await task.send(.string(text))
        } catch {
            Log.socket.error("a send failed: \(Self.code(of: error), privacy: .public)")
            throw PhoenixError.closed
        }
    }

    public var closeCode: Int? {
        lock.withLock {
            guard let code = task?.closeCode, code != .invalid else { return nil }
            return code.rawValue
        }
    }

    public func ping() async throws {
        guard let task = lock.withLock({ task }) else { throw PhoenixError.closed }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            task.sendPing { error in
                if error != nil { continuation.resume(throwing: PhoenixError.closed) }
                else { continuation.resume() }
            }
        }
    }

    public func receive() async throws -> String {
        guard let task = lock.withLock({ task }) else { throw PhoenixError.closed }
        let message: URLSessionWebSocketTask.Message
        do {
            message = try await task.receive()
        } catch {
            Log.socket.info("the read ended: \(Self.code(of: error), privacy: .public)")
            throw PhoenixError.closed
        }
        switch message {
        case .string(let text): return text
        case .data(let data): return String(decoding: data, as: UTF8.self)
        @unknown default: throw PhoenixError.notAFrame
        }
    }

    public func close() {
        let (task, session) = lock.withLock {
            shut = true
            return (self.task, self.session)
        }
        task?.cancel(with: .normalClosure, reason: nil)
        session?.finishTasksAndInvalidate()
        takeOpening()?.resume(throwing: PhoenixError.closed)
    }

    public func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask, didOpenWithProtocol chosen: String?) {
        takeOpening()?.resume()
    }

    public func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask, didCloseWith closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?) {
        Log.socket.info("the server closed the socket (code \(closeCode.rawValue), \(reason?.count ?? 0) bytes of reason)")
    }

    public func urlSession(
        _ session: URLSession, task: URLSessionTask,
        willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest,
        completionHandler: @escaping (URLRequest?) -> Void
    ) {
        RefusingRedirects.delegate.urlSession(session, task: task, willPerformHTTPRedirection: response, newRequest: request, completionHandler: completionHandler)
    }

    public func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        let status = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        Log.socket.info("the socket's task ended (status \(status), \(error.map(Self.code(of:)) ?? "no error", privacy: .public))")
        // Only an upgrade still waiting hears this; any status but the 101
        // that opens a socket is the server's refusal.
        takeOpening()?.resume(throwing: status != 0 && status != 101 ? PhoenixError.refused(status: status) : PhoenixError.closed)
    }

    private func takeOpening() -> CheckedContinuation<Void, Error>? {
        lock.withLock {
            defer { opening = nil }
            return opening
        }
    }

    /// An error by its domain and code alone.
    private static func code(of error: Error) -> String {
        let error = error as NSError
        return "\(error.domain) \(error.code)"
    }
}
