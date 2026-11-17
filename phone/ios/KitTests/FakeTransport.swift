import Foundation
@testable import DeskPhoneKit

/// A socket the test speaks for: it reads what the client sent, and says
/// what the server would.
final class FakeTransport: RPCTransport, @unchecked Sendable {
    private(set) var requests: [URLRequest] = []
    var closeCode: Int?
    func ping() async throws {}
    func open(_ request: URLRequest) async throws {
        requests.append(request)
        try await open(request.url!)
    }
    var refusal: Int?
    /// The upgrade is never answered: `open` waits until `close`.
    var hangs = false
    private(set) var opened: [URL] = []
    private(set) var closes = 0
    private let lock = NSLock()
    private var hanging: CheckedContinuation<Void, Error>?
    private let inbound: AsyncThrowingStream<String, Error>
    private let inboundFeed: AsyncThrowingStream<String, Error>.Continuation
    private var reading: AsyncThrowingStream<String, Error>.Iterator
    private let outbound: AsyncStream<String>
    private let outboundFeed: AsyncStream<String>.Continuation
    private var sent: AsyncStream<String>.Iterator

    init() {
        (inbound, inboundFeed) = AsyncThrowingStream.makeStream(of: String.self)
        reading = inbound.makeAsyncIterator()
        (outbound, outboundFeed) = AsyncStream.makeStream(of: String.self)
        sent = outbound.makeAsyncIterator()
    }

    func open(_ url: URL) async throws {
        opened.append(url)
        if let refusal { throw PhoenixError.refused(status: refusal) }
        guard hangs else { return }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            let shut = lock.withLock {
                if closes == 0 { hanging = continuation }
                return closes > 0
            }
            if shut { continuation.resume(throwing: PhoenixError.closed) }
        }
    }

    func send(_ text: String) async throws { outboundFeed.yield(text) }

    func receive() async throws -> String {
        guard let next = try await reading.next() else { throw PhoenixError.closed }
        return next
    }

    func close() {
        let hung = lock.withLock {
            closes += 1
            defer { hanging = nil }
            return hanging
        }
        hung?.resume(throwing: PhoenixError.closed)
        inboundFeed.finish()
    }

    /// The server says this.
    func deliver(_ frame: JSONValue) { inboundFeed.yield(try! frame.text()) }

    /// The server says this, as it arrives: not necessarily a frame.
    func deliver(text: String) { inboundFeed.yield(text) }

    /// The next frame the client sent.
    func nextSent() async -> PhoenixFrame {
        try! PhoenixFrame(await sent.next()!)
    }

    func nextRPC() async -> JSONValue {
        try! JSONValue.parse(await sent.next()!)
    }

    func replyRPC(to frame: JSONValue, _ result: JSONValue) {
        deliver(.object(["jsonrpc": .string("2.0"), "id": frame["id"]!, "result": result]))
    }

    /// The reply Phoenix would make to `frame`.
    func reply(to frame: PhoenixFrame, status: String = "ok", _ response: JSONValue) {
        deliver(.array([
            frame.joinRef.map(JSONValue.string) ?? .null, frame.ref.map(JSONValue.string) ?? .null,
            .string(frame.topic), .string("phx_reply"),
            .object(["status": .string(status), "response": response]),
        ]))
    }

    /// What Phoenix sends when the channel `join` made has gone:
    /// `[join_ref, join_ref, topic, "phx_error" | "phx_close", payload]`
    /// (`Phoenix.Socket`'s `encode_error`/`encode_close`).
    func end(_ join: PhoenixFrame, with event: String, _ payload: JSONValue = .object([:])) {
        let joinRef = join.joinRef.map(JSONValue.string) ?? .null
        deliver(.array([joinRef, joinRef, .string(join.topic), .string(event), payload]))
    }
}
