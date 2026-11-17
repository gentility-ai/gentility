import Foundation

/// One WebSocket, text frames only. `PhoenixSocket` speaks Phoenix over it;
/// `URLSessionTransport` is the real one and the tests' `FakeTransport` the
/// other.
public protocol WebSocketTransport: AnyObject, Sendable {
    /// Returns once the upgrade is answered; throws `.refused(status:)` for an
    /// HTTP refusal and `.closed` for no answer.
    func open(_ url: URL) async throws
    func send(_ text: String) async throws
    /// The next text frame; throws once the socket has closed.
    func receive() async throws -> String
    func close()
}

/// Every way the socket refuses. None carries the URL, whose query holds the
/// bearer.
public enum PhoenixError: Error, Equatable {
    /// The upgrade was answered with this HTTP status (403: `SocketRefusal`).
    case refused(status: Int)
    /// The socket has closed, or the topic is not joined on it.
    case closed
    case timeout
    case notAFrame
    /// The join's `phx_reply` was an error; its `response` (`reason`, …).
    case joinRefused(JSONValue)
    /// A push's `phx_reply` was an error; its `response`.
    case replied(JSONValue)
}

/// A frame the server sent without being asked. `phx_error` and `phx_close`
/// arrive here too, once each, for the join that is current: the topic is no
/// longer joined (`closesChannel`), and the socket stays open for a new join.
public struct PhoenixPush: Equatable, Sendable {
    public let topic: String
    public let event: String
    public let payload: JSONValue

    public init(topic: String, event: String, payload: JSONValue) {
        self.topic = topic
        self.event = event
        self.payload = payload
    }

    /// The server ended this topic's channel (a crash or a tidy close).
    /// Pushes to it are refused with `.closed` until it is joined again.
    public var closesChannel: Bool { event == "phx_error" || event == "phx_close" }
}

/// One Phoenix v2 frame: `[join_ref, ref, topic, event, payload]`.
public struct PhoenixFrame: Equatable, Sendable {
    public let joinRef: String?
    public let ref: String?
    public let topic: String
    public let event: String
    public let payload: JSONValue

    public init(_ text: String) throws {
        guard case .array(let parts) = try? JSONValue.parse(text), parts.count == 5,
              let topic = parts[2].string, let event = parts[3].string else {
            throw PhoenixError.notAFrame
        }
        joinRef = parts[0].string
        ref = parts[1].string
        self.topic = topic
        self.event = event
        payload = parts[4]
    }
}

/// The phone's socket to the mothership: the same `user_socket` a Desk
/// core uses, with the bearer alone. Single-use: once it closes, `pushes`
/// finishes, every call is refused with `.closed`, and its owner makes
/// another. Prints as redacted, however printed: its URL carries the bearer.
public actor PhoenixSocket: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public nonisolated let pushes: AsyncStream<PhoenixPush>
    private nonisolated let pushed: AsyncStream<PhoenixPush>.Continuation
    private nonisolated let transport: WebSocketTransport
    private nonisolated let url: URL
    private let heartbeatEvery: Duration
    private let heartbeatGrace: Duration
    private let replyWithin: Duration
    private let sleep: @Sendable (Duration) async throws -> Void
    private nonisolated let outbox: AsyncStream<String>
    private nonisolated let posting: AsyncStream<String>.Continuation
    private var counter = 0
    /// Each topic's latest join, and whether the server has said yes to it.
    private var channels: [String: (joinRef: String, joined: Bool)] = [:]
    private var waiting: [String: CheckedContinuation<JSONValue, Error>] = [:]
    /// The join each waiting ref rides (none for a heartbeat): a channel the
    /// server ends answers nothing more, so what waits on it is refused.
    private var riding: [String: String] = [:]
    private var opening: CheckedContinuation<Void, Error>?
    private var openingTimer: Task<Void, Never>?
    private var timers: [String: Task<Void, Never>] = [:]
    private var tasks: [Task<Void, Never>] = []
    public private(set) var isOpen = false
    private var ended = false

    public init(
        transport: WebSocketTransport,
        url: URL,
        heartbeatEvery: Duration = .seconds(30),
        heartbeatGrace: Duration = .seconds(10),
        replyWithin: Duration = .seconds(30),
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.transport = transport
        self.url = url
        self.heartbeatEvery = heartbeatEvery
        self.heartbeatGrace = heartbeatGrace
        self.replyWithin = replyWithin
        self.sleep = sleep
        (pushes, pushed) = AsyncStream.makeStream(of: PhoenixPush.self)
        (outbox, posting) = AsyncStream.makeStream(of: String.self)
    }

    public nonisolated var description: String { "PhoenixSocket(\(url.host ?? "?"), bearer redacted)" }
    public nonisolated var debugDescription: String { description }
    /// `dump` and the debugger walk a mirror, not `description`: show them none.
    public nonisolated var customMirror: Mirror { Mirror(self, children: [:]) }

    /// `ws(s)://<origin>/socket/websocket?vsn=2.0.0&bearer=…`, the fourth
    /// `UserSocket.connect/3`: no `organization_id`, so the socket is the
    /// person's and each join names its organisation. The bearer is escaped
    /// strictly, ASCII unreserved characters alone left bare: Phoenix reads a
    /// bare `+` as a space. Never log the result.
    public static func url(origin: URL, bearer: String) -> URL {
        var parts = URLComponents(url: origin, resolvingAgainstBaseURL: false) ?? URLComponents()
        parts.scheme = parts.scheme == "https" ? "wss" : "ws"
        parts.path = "/socket/websocket"
        parts.fragment = nil
        let strict = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~")
        parts.percentEncodedQueryItems = [
            URLQueryItem(name: "vsn", value: "2.0.0"),
            URLQueryItem(name: "bearer", value: bearer.addingPercentEncoding(withAllowedCharacters: strict)),
        ]
        return parts.url!
    }

    /// Opens the transport, waiting `replyWithin` for the upgrade's answer.
    /// An open that fails or goes unanswered closes the transport, so that
    /// its session is let go, and ends this socket.
    public func connect() async throws {
        guard !ended, !isOpen else { throw PhoenixError.closed }
        let started = ContinuousClock.now
        do {
            try await opened(within: replyWithin)
        } catch {
            ended = true
            pushed.finish()
            posting.finish()
            transport.close()
            Log.socket.error("the socket to \(self.url.host ?? "?", privacy: .public) did not open after \(String(describing: ContinuousClock.now - started), privacy: .public): \(Self.said(error), privacy: .public)")
            throw error as? PhoenixError ?? .closed
        }
        guard !ended else { throw PhoenixError.closed }
        isOpen = true
        Log.socket.info("socket open to \(self.url.host ?? "?", privacy: .public) in \(String(describing: ContinuousClock.now - started), privacy: .public)")
        tasks = [
            Task { await self.read() },
            Task { await self.write() },
            Task { await self.beat() },
        ]
    }

    /// The transport's open, or `.timeout` once `within` has passed,
    /// whichever comes first. An open still out after the timeout is ended by
    /// the transport's close (`connect`), and its late answer is ignored.
    private func opened(within: Duration) async throws {
        let transport = transport
        let url = url
        let sleep = sleep
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            opening = continuation
            openingTimer = Task {
                do { try await sleep(within) } catch { return }
                self.settleOpening(.failure(PhoenixError.timeout))
            }
            Task {
                do {
                    try await transport.open(url)
                    self.settleOpening(.success(()))
                } catch {
                    self.settleOpening(.failure(error))
                }
            }
        }
    }

    private func settleOpening(_ result: Result<Void, Error>) {
        openingTimer?.cancel()
        openingTimer = nil
        let waiting = opening
        opening = nil
        waiting?.resume(with: result)
    }

    /// Whether `topic` is joined now: false before its join is answered, after
    /// a refused or unanswered join, and after the server ended its channel.
    public func isJoined(_ topic: String) -> Bool {
        channels[topic]?.joined == true
    }

    /// Joins `topic` (again, if it is joined: the server closes the old
    /// channel) and returns the join's reply.
    public func join(_ topic: String, _ params: JSONValue = .object([:])) async throws -> JSONValue {
        guard isOpen else { throw PhoenixError.closed }
        let joinRef = nextRef()
        channels[topic] = (joinRef, false)
        do {
            let reply = try await send(joinRef: joinRef, ref: joinRef, topic: topic, event: "phx_join", payload: params, within: replyWithin)
            if channels[topic]?.joinRef == joinRef { channels[topic]?.joined = true }
            Log.socket.info("joined \(topic, privacy: .public)")
            return reply
        } catch {
            if channels[topic]?.joinRef == joinRef { channels[topic] = nil }
            guard case PhoenixError.replied(let response) = error else {
                Log.socket.error("\(topic, privacy: .public) join failed: \(Self.said(error), privacy: .public)")
                throw error
            }
            Log.socket.error("\(topic, privacy: .public) refused the join: \(response["reason"]?.string ?? "?", privacy: .public)")
            throw PhoenixError.joinRefused(response)
        }
    }

    /// Pushes `event` on a joined `topic` and returns the reply's `response`.
    /// A topic not joined now is refused with `.closed`, nothing sent.
    public func push(_ topic: String, _ event: String, _ payload: JSONValue = .object([:])) async throws -> JSONValue {
        guard isOpen, let channel = channels[topic], channel.joined else { throw PhoenixError.closed }
        do {
            return try await send(joinRef: channel.joinRef, ref: nextRef(), topic: topic, event: event, payload: payload, within: replyWithin)
        } catch {
            Log.socket.error("\(topic, privacy: .public) \(event, privacy: .public) failed: \(Self.said(error), privacy: .public)")
            throw error
        }
    }

    /// Closes the socket now: whatever waits is refused with `.closed`.
    public func disconnect() {
        closed("disconnected")
    }

    private func nextRef() -> String {
        counter += 1
        return String(counter)
    }

    private func send(joinRef: String?, ref: String, topic: String, event: String, payload: JSONValue, within: Duration) async throws -> JSONValue {
        let frame = JSONValue.array([joinRef.map(JSONValue.string) ?? .null, .string(ref), .string(topic), .string(event), payload])
        let text = try frame.text()
        let started = ContinuousClock.now
        let reply = try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<JSONValue, Error>) in
            wait(ref, joinRef: joinRef, continuation, text: text, within: within)
        }
        if topic != "phoenix" {
            Log.socket.info("\(topic, privacy: .public) \(event, privacy: .public) answered in \(String(describing: ContinuousClock.now - started), privacy: .public)")
        }
        return reply
    }

    private func wait(_ ref: String, joinRef: String?, _ continuation: CheckedContinuation<JSONValue, Error>, text: String, within: Duration) {
        guard isOpen else {
            continuation.resume(throwing: PhoenixError.closed)
            return
        }
        waiting[ref] = continuation
        riding[ref] = joinRef
        timers[ref] = Task { [sleep] in
            do { try await sleep(within) } catch { return }
            self.answer(ref, with: .failure(PhoenixError.timeout))
        }
        posting.yield(text)
    }

    private func answer(_ ref: String, with result: Result<JSONValue, Error>) {
        riding.removeValue(forKey: ref)
        timers.removeValue(forKey: ref)?.cancel()
        waiting.removeValue(forKey: ref)?.resume(with: result)
    }

    /// One writer, so frames go out in the order they were made. A send that
    /// fails ends the socket.
    private func write() async {
        for await text in outbox {
            do {
                try await transport.send(text)
            } catch {
                closed("a send failed: \(Self.said(error))")
                return
            }
        }
    }

    private func read() async {
        while true {
            let text: String
            do { text = try await transport.receive() } catch {
                closed("the read ended: \(Self.said(error))")
                return
            }
            guard let frame = try? PhoenixFrame(text) else {
                Log.socket.error("not a Phoenix v2 frame (\(text.utf8.count) bytes); skipped")
                continue
            }
            heard(frame)
        }
    }

    private func heard(_ frame: PhoenixFrame) {
        switch frame.event {
        case "phx_reply":
            guard let ref = frame.ref, waiting[ref] != nil else {
                // Its asker stopped waiting (a timeout), or never was.
                Log.socket.debug("\(frame.topic, privacy: .public) replied to a ref nobody waits on")
                return
            }
            let response = frame.payload["response"] ?? .object([:])
            answer(ref, with: frame.payload["status"]?.string == "ok" ? .success(response) : .failure(PhoenixError.replied(response)))
        case "phx_error", "phx_close":
            // About the channel its join_ref names. One naming an older join
            // is about a channel already replaced (Phoenix closes a superseded
            // join after answering the new one): dropped, since nothing held
            // here was lost. One naming none is taken at its word.
            guard let channel = channels[frame.topic], frame.joinRef == nil || frame.joinRef == channel.joinRef else {
                Log.socket.debug("\(frame.topic, privacy: .public) \(frame.event, privacy: .public) for a join already replaced; dropped")
                return
            }
            channels[frame.topic] = nil
            let lost = riding.filter { $0.value == channel.joinRef }.map(\.key)
            lost.forEach { answer($0, with: .failure(PhoenixError.closed)) }
            Log.socket.error("\(frame.topic, privacy: .public) said \(frame.event, privacy: .public): \(frame.payload["reason"]?.string ?? "no reason", privacy: .public); \(lost.count) waiting refused")
            pushed.yield(PhoenixPush(topic: frame.topic, event: frame.event, payload: frame.payload))
        default:
            guard frame.ref == nil else {
                Log.socket.debug("\(frame.topic, privacy: .public) \(frame.event, privacy: .public) carried a ref and is no push; dropped")
                return
            }
            // A channel's own push names its join; one naming a join already
            // replaced, or a topic not joined, is from a channel that has
            // gone, dropped as its close is. A broadcast names none.
            if let joinRef = frame.joinRef, channels[frame.topic]?.joinRef != joinRef {
                Log.socket.debug("\(frame.topic, privacy: .public) \(frame.event, privacy: .public) for a join already replaced; dropped")
                return
            }
            pushed.yield(PhoenixPush(topic: frame.topic, event: frame.event, payload: frame.payload))
        }
    }

    private func closed(_ reason: String) {
        guard !ended else { return }
        ended = true
        let wasOpen = isOpen
        isOpen = false
        tasks.forEach { $0.cancel() }
        timers.values.forEach { $0.cancel() }
        timers.removeAll()
        let left = waiting.values
        waiting.removeAll()
        riding.removeAll()
        channels.removeAll()
        left.forEach { $0.resume(throwing: PhoenixError.closed) }
        posting.finish()
        pushed.finish()
        transport.close()
        if wasOpen {
            Log.socket.info("socket closed (\(reason, privacy: .public)); \(left.count) waiting refused")
        }
    }

    private func beat() async {
        while !Task.isCancelled {
            do { try await sleep(heartbeatEvery) } catch { return }
            guard isOpen else { return }
            do {
                _ = try await send(joinRef: nil, ref: nextRef(), topic: "phoenix", event: "heartbeat", payload: .object([:]), within: heartbeatGrace)
            } catch PhoenixError.timeout {
                Log.socket.error("a heartbeat went unanswered for \(String(describing: self.heartbeatGrace), privacy: .public); closing")
                closed("heartbeat unanswered")
                return
            } catch PhoenixError.closed {
                return
            } catch {
                Log.socket.error("a heartbeat was answered with an error: \(Self.said(error), privacy: .public)")
            }
        }
    }

    /// An error as it may be logged: a `PhoenixError` as itself, anything
    /// else by its domain and code alone, since a `URLError`'s description
    /// carries the failing URL and with it the bearer.
    private static func said(_ error: Error) -> String {
        if let error = error as? PhoenixError { return String(describing: error) }
        let error = error as NSError
        return "\(error.domain) \(error.code)"
    }
}
