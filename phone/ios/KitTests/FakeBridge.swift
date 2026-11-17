import Foundation
@testable import DeskPhoneKit

/// `phonenode` as a test plays it: answers are scripted, doors are held so
/// the test can speak for the Mac, and a call can be held open (`hold`) the
/// way a real Join waits on the head's netmap or a dial waits on DERP.
final class FakeBridge: NodeBridge, @unchecked Sendable {
    private let lock = NSLock()
    private var _joinAnswer = #"{"ok":true,"address":"fd06:6e7:0:2::9","name":"morgan-s-iphone"}"#
    private var _rejoinAnswer = #"{"ok":true,"address":"fd06:6e7:0:2::9","name":"morgan-s-iphone"}"#
    private var _statusAnswer = #"{"state":"up","address":"fd06:6e7:0:2::9","addresses":["fd06:6e7:0:2::9"]}"#
    private var _openAnswers: [String: String] = [:]
    private var _sendAnswer = #"{"ok":true}"#
    private var _calls: [String] = []
    private var _sent: [String] = []
    private var sinks: [String: DoorSink] = [:]
    private var holds: [String: Hold] = [:]
    private let sentFeed: AsyncStream<String>.Continuation
    private var sentFrames: AsyncStream<String>.Iterator

    init() {
        let (stream, feed) = AsyncStream.makeStream(of: String.self)
        sentFeed = feed
        sentFrames = stream.makeAsyncIterator()
    }

    var joinAnswer: String {
        get { lock.withLock { _joinAnswer } }
        set { lock.withLock { _joinAnswer = newValue } }
    }

    var rejoinAnswer: String {
        get { lock.withLock { _rejoinAnswer } }
        set { lock.withLock { _rejoinAnswer = newValue } }
    }

    var statusAnswer: String {
        get { lock.withLock { _statusAnswer } }
        set { lock.withLock { _statusAnswer = newValue } }
    }

    /// What `OpenDoor` answers for every address not in `openAnswers`.
    var openAnswer: String {
        get { lock.withLock { _openAnswers["*"] ?? #"{"door_id":"d-1"}"# } }
        set { lock.withLock { _openAnswers["*"] = newValue } }
    }

    var sendAnswer: String {
        get { lock.withLock { _sendAnswer } }
        set { lock.withLock { _sendAnswer = newValue } }
    }

    private var _peerPathAnswer = #"{"path":"direct","via":"[fd8c::1]:41641"}"#
    var peerPathAnswer: String {
        get { lock.withLock { _peerPathAnswer } }
        set { lock.withLock { _peerPathAnswer = newValue } }
    }

    /// What `OpenDoor` answers for this address.
    func answerOpen(_ address: String, with answer: String) {
        lock.withLock { _openAnswers[address] = answer }
    }

    var calls: [String] { lock.withLock { _calls } }
    var sent: [String] { lock.withLock { _sent } }

    // MARK: the bridge

    func join(dir: String, hostname: String, enrollmentJSON: String) -> String {
        record("join \(hostname)")
        held("join")
        return joinAnswer
    }

    func rejoin(dir: String, hostname: String) -> String {
        record("rejoin \(hostname)")
        held("rejoin")
        return rejoinAnswer
    }

    func status() -> String {
        record("status")
        held("status")
        return statusAnswer
    }

    func down() -> String {
        record("down")
        held("down")
        return #"{"ok":true}"#
    }

    func openDoor(address: String, port: Int, sink: DoorSink) -> String {
        record("open [\(address)]:\(port)")
        held("open \(address)")
        let answer = lock.withLock { _openAnswers[address] ?? _openAnswers["*"] ?? #"{"door_id":"d-1"}"# }
        if let id = (try? JSONValue.parse(answer))?["door_id"]?.string {
            lock.withLock { sinks[id] = sink }
        }
        return answer
    }

    func send(doorID: String, frame: String) -> String {
        lock.withLock { _sent.append(frame) }
        sentFeed.yield(frame)
        return sendAnswer
    }

    func close(doorID: String) -> String {
        record("close \(doorID)")
        sink(doorID)?.closed(doorID: doorID, reason: "closed")
        return #"{"ok":true}"#
    }

    func peerPath(address: String) -> String {
        record("peer_path \(address)")
        return peerPathAnswer
    }

    func defaultRoute(interface: String) -> String {
        record("default_route \(interface)")
        return #"{"ok":true}"#
    }

    // MARK: the test's side

    /// Holds the next call named `name` (`join`, `status`, `open <address>`…)
    /// until the hold is released.
    func hold(_ name: String) -> Hold {
        let hold = Hold()
        lock.withLock { holds[name] = hold }
        return hold
    }

    /// The next request the app sent, read back.
    func nextRequest() async -> (id: Int, request: JSONValue) {
        let frame = try! JSONValue.parse(await sentFrames.next()!)
        return (frame["id"]!.int!, frame["request"]!)
    }

    /// The Mac says this frame on `door`, with the reply's id replaced by `id`.
    func say(_ frame: JSONValue, id: Int? = nil, door: String = "d-1") {
        var frame = frame
        if let id, case .object(var fields) = frame {
            fields["id"] = .number(Double(id))
            frame = .object(fields)
        }
        sink(door)?.frame(doorID: door, text: try! frame.text())
    }

    /// The Mac says text this app may not read.
    func sayRaw(_ text: String, door: String = "d-1") {
        sink(door)?.frame(doorID: door, text: text)
    }

    func drop(_ reason: String, door: String = "d-1") { sink(door)?.closed(doorID: door, reason: reason) }

    private func sink(_ door: String) -> DoorSink? { lock.withLock { sinks[door] } }

    private func record(_ call: String) { lock.withLock { _calls.append(call) } }

    private func held(_ name: String) {
        guard let hold = lock.withLock({ holds.removeValue(forKey: name) }) else { return }
        hold.enter()
    }
}

/// A bridge call kept open: `entered` says the call has begun, and the call
/// returns once `release()` is called.
final class Hold: @unchecked Sendable {
    private let gate = DispatchSemaphore(value: 0)
    private let entry: AsyncStream<Void>.Continuation
    private var entries: AsyncStream<Void>.Iterator

    init() {
        let (stream, entry) = AsyncStream.makeStream(of: Void.self)
        self.entry = entry
        entries = stream.makeAsyncIterator()
    }

    /// Returns once the held call has begun.
    func entered() async { _ = await entries.next() }

    func release() { gate.signal() }

    fileprivate func enter() {
        entry.yield()
        entry.finish()
        gate.wait()
    }
}
