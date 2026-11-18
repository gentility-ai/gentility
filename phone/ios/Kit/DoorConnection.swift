import Foundation

public enum DoorError: Error, Equatable, Sendable {
    /// The door closed, in Go's words: a reason that leads with a word
    /// (`closed`, `node_down`, `door_closed: …`, `lost: …`, `door_silent`,
    /// `listener_stalled`, `not_text`), or a Send Go refused (`send_failed: …`).
    case closed(String)
    case timeout
    /// The core answered `Response::Error`.
    case refusal(DoorRefusal)
    case unexpected(String)
}

/// One door, dialled: requests matched to their replies by id, events as a
/// stream. Go owns the socket; this owns the bookkeeping.
///
/// Go calls `frame` and `closed` on threads of its own. A lock and a
/// dictionary of continuations let a reply on any thread resume the request
/// that asked, and `closed` fail every request still waiting. Send and Close
/// keep to this door's own serial queue, so frames leave in order and no
/// other door's dial holds them up.
public final class DoorConnection: DoorSink, @unchecked Sendable {
    public let events: AsyncStream<DoorEvent>
    private let pushed: AsyncStream<DoorEvent>.Continuation
    private let bridge: NodeBridge
    private let label: String
    private let queue: DispatchQueue
    private let lock = NSLock()
    private var doorID: String?
    private var counter = 0
    private var waiting: [Int: Waiting] = [:]
    private var ended: String?

    /// What a reply cost: its bytes on the wire, how long it was waited for from
    /// the ask, and how long it took to decode (B1).
    public struct Cost: Equatable, Sendable {
        public let bytes: Int
        public let waited: Duration
        public let decoded: Duration
    }

    /// A reply and its cost.
    public struct Reply: Sendable {
        public let response: DoorResponse
        public let cost: Cost
    }

    private struct Waiting {
        let continuation: CheckedContinuation<Reply, Error>
        let kind: String
        let asked: ContinuousClock.Instant
        var timer: Task<Void, Never>?
    }

    init(bridge: NodeBridge, label: String) {
        self.bridge = bridge
        self.label = label
        queue = DispatchQueue(label: "ai.gentility.desk.phone.door", qos: .userInitiated)
        (events, pushed) = AsyncStream.makeStream(of: DoorEvent.self)
    }

    func attach(_ id: String) {
        let endedFirst: String? = lock.withLock {
            doorID = id
            return ended
        }
        if let endedFirst {
            Log.door.notice("door \(id, privacy: .public) to \(self.label, privacy: .public) opened, but had already closed: \(endedFirst, privacy: .public)")
        } else {
            Log.door.info("door \(id, privacy: .public) to \(self.label, privacy: .public) open")
        }
    }

    public var isOpen: Bool { lock.withLock { doorID != nil && ended == nil } }

    /// How long a request waits for its reply when the caller does not say:
    /// a transcript is one frame of up to 32 MB over DERP, and a session list
    /// every row an environment holds; a prompt waits on its write.
    public static func patience(for request: DoorRequest) -> Duration {
        switch request {
        case .sessionTranscript, .sessionsList: return .seconds(30)
        case .sessionPrompt: return .seconds(15)
        // A photo is one frame of up to 27 MiB of base64 each way over DERP.
        case .attachmentPut: return .seconds(60)
        case .attachmentRead, .fileImageRead: return .seconds(30)
        // A start answers before the worktree is made; readiness looks for
        // each agent's launcher.
        case .sessionStart, .readiness, .harnessConfig: return .seconds(20)
        case .hello, .environmentsList, .sessionAnswer, .sessionElicitationAnswer, .sessionQuestionAnswer,
             .sessionSteer, .sessionWithdraw, .sessionGet, .projectsList, .worktreeLayout, .sessionAbort:
            return .seconds(10)
        }
    }

    public func request(_ request: DoorRequest, within: Duration? = nil) async throws -> DoorResponse {
        try await weigh(request, within: within).response
    }

    /// The reply, with its cost.
    public func weigh(_ request: DoorRequest, within: Duration? = nil) async throws -> Reply {
        try Task.checkCancellation()
        let patience = within ?? Self.patience(for: request)
        let (id, doorID): (Int, String) = try lock.withLock {
            if let ended { throw DoorError.closed(ended) }
            guard let doorID = self.doorID else { throw DoorError.closed("not open") }
            counter += 1
            return (counter, doorID)
        }
        let text = try request.frame(id: id)
        let kind = request.kind
        Log.door.info("door \(doorID, privacy: .public) asks \(kind, privacy: .public) as \(id) (\(text.utf8.count) bytes, waits \(String(describing: patience), privacy: .public))")
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Reply, Error>) in
                let closedFirst: String? = lock.withLock {
                    if let ended { return ended }
                    waiting[id] = Waiting(continuation: continuation, kind: kind, asked: .now)
                    return nil
                }
                if let closedFirst {
                    continuation.resume(throwing: DoorError.closed(closedFirst))
                    return
                }
                if Task.isCancelled {
                    fail(id, CancellationError())
                    return
                }
                let timer = Task { [weak self] in
                    try? await Task.sleep(for: patience)
                    guard !Task.isCancelled else { return }
                    self?.fail(id, DoorError.timeout)
                }
                let stillWaiting: Bool = lock.withLock {
                    guard waiting[id] != nil else { return false }
                    waiting[id]?.timer = timer
                    return true
                }
                if !stillWaiting { timer.cancel() }
                queue.async { [bridge] in
                    let started = ContinuousClock.now
                    let answer = bridge.send(doorID: doorID, frame: text)
                    let took = String(describing: ContinuousClock.now - started)
                    guard let read = try? Wire.decoder.decode(NodeAnswer.self, from: Data(answer.utf8)) else {
                        Log.door.error("door \(doorID, privacy: .public): send of \(id) answered something unreadable after \(took, privacy: .public)")
                        return
                    }
                    if let refused = read.error {
                        let reason = read.code.map { "\($0): \(refused)" } ?? refused
                        Log.door.error("door \(doorID, privacy: .public): send of \(id) refused after \(took, privacy: .public): \(reason, privacy: .public)")
                        self.fail(id, DoorError.closed(reason))
                    } else {
                        Log.door.debug("door \(doorID, privacy: .public): sent \(id) in \(took, privacy: .public)")
                    }
                }
            }
        } onCancel: {
            fail(id, CancellationError())
        }
    }

    /// Asks Go to close the door; its `OnClose` ends this connection.
    public func close() {
        guard let doorID = lock.withLock({ ended == nil ? self.doorID : nil }) else { return }
        Log.door.info("door \(doorID, privacy: .public): closing")
        queue.async { [bridge] in
            let answer = bridge.close(doorID: doorID)
            if let refused = (try? Wire.decoder.decode(NodeAnswer.self, from: Data(answer.utf8)))?.error {
                Log.door.notice("door \(doorID, privacy: .public): close refused: \(refused, privacy: .public)")
            }
        }
    }

    // MARK: DoorSink, on Go's threads

    public func frame(doorID: String, text: String) {
        let data = Data(text.utf8)
        let bytes = data.count
        guard lock.withLock({ ended == nil }) else {
            Log.door.notice("door \(doorID, privacy: .public): a frame after the close (\(bytes) bytes), dropped")
            return
        }
        let frame: DoorFrame
        let began = ContinuousClock.now
        do {
            frame = try DoorFrame(data: data)
        } catch {
            Log.door.error("door \(doorID, privacy: .public): a frame this app cannot read (\(bytes) bytes), skipped: \(Self.unread(error), privacy: .public)")
            return
        }
        let decoded = ContinuousClock.now - began
        switch frame {
        case .reply(let id, let response):
            guard let answered = lock.withLock({ waiting.removeValue(forKey: id) }) else {
                Log.door.notice("door \(doorID, privacy: .public): a reply to \(id) (\(bytes) bytes), which nobody is waiting for")
                return
            }
            answered.timer?.cancel()
            let waited = ContinuousClock.now - answered.asked
            Log.door.info("door \(doorID, privacy: .public): \(answered.kind, privacy: .public) \(id) answered in \(String(describing: waited), privacy: .public), \(bytes) bytes, decoded in \(String(describing: decoded), privacy: .public)")
            answered.continuation.resume(returning: Reply(response: response, cost: Cost(bytes: bytes, waited: waited, decoded: decoded)))
        case .event(let event):
            Log.door.debug("door \(doorID, privacy: .public): an event (\(bytes) bytes, decoded in \(String(describing: decoded), privacy: .public))")
            pushed.yield(event)
        }
    }

    public func closed(doorID: String, reason: String) {
        let left: [Waiting]? = lock.withLock {
            guard ended == nil else { return nil }
            ended = reason
            defer { waiting.removeAll() }
            return Array(waiting.values)
        }
        guard let left else {
            Log.door.notice("door \(doorID, privacy: .public): a second close, ignored: \(reason, privacy: .public)")
            return
        }
        Log.door.info("door \(doorID, privacy: .public) to \(self.label, privacy: .public) closed: \(reason, privacy: .public); \(left.count) request(s) failed")
        for waiting in left {
            waiting.timer?.cancel()
            waiting.continuation.resume(throwing: DoorError.closed(reason))
        }
        pushed.finish()
    }

    /// Why a frame did not decode, without a byte of the frame: a JSON
    /// parser's message quotes the characters it choked on.
    static func unread(_ error: Error) -> String {
        guard let error = error as? DecodingError else { return String(describing: error) }
        let context: DecodingError.Context
        let what: String
        switch error {
        case .dataCorrupted(let c): (what, context) = ("not JSON or corrupt", c)
        case .keyNotFound(let key, let c): (what, context) = ("no \(key.stringValue)", c)
        case .typeMismatch(let type, let c): (what, context) = ("not a \(type)", c)
        case .valueNotFound(let type, let c): (what, context) = ("no \(type) value", c)
        @unknown default: return "a decoding error"
        }
        let path = context.codingPath.map { $0.intValue.map(String.init) ?? $0.stringValue }.joined(separator: ".")
        return path.isEmpty ? what : "\(what) at \(path)"
    }

    private func fail(_ id: Int, _ error: Error) {
        guard let failed = lock.withLock({ waiting.removeValue(forKey: id) }) else { return }
        failed.timer?.cancel()
        Log.door.notice("\(failed.kind, privacy: .public) \(id) failed: \(String(describing: error), privacy: .public)")
        failed.continuation.resume(throwing: error)
    }
}
