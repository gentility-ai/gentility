import Foundation

/// The `phonenode` binding: strings in, JSON strings out. The
/// app's is `PhonenodeBridge`; the tests' is a fake. Every call may block:
/// Join and Rejoin for seconds (the head's netmap), OpenDoor up to 30 s, Send
/// up to 10 s, Status up to 5 s.
public protocol NodeBridge: AnyObject, Sendable {
    func join(dir: String, hostname: String, enrollmentJSON: String) -> String
    func rejoin(dir: String, hostname: String) -> String
    func status() -> String
    func down() -> String
    func openDoor(address: String, port: Int, sink: DoorSink) -> String
    func send(doorID: String, frame: String) -> String
    func close(doorID: String) -> String
    func peerPath(address: String) -> String
    func defaultRoute(interface: String) -> String
}

/// `FrameListener`, on the Swift side. Called on Go's threads: each frame in
/// the order it arrived, then exactly one `closed`.
public protocol DoorSink: AnyObject, Sendable {
    func frame(doorID: String, text: String)
    func closed(doorID: String, reason: String)
}

/// Any answer `phonenode` gives, read for the fields this app uses.
public struct NodeAnswer: Decodable, Equatable, Sendable {
    public let ok: Bool?
    public let address: String?
    public let addresses: [String]?
    public let name: String?
    /// `down`, `joining` or `up`, from `Status()`.
    public let state: String?
    public let error: String?
    /// Go's word beside `error` (`not_enrolled`, `revoked`, `joining`,
    /// `timed_out`, …). Status's `status_unknown` comes with state `up`.
    public let code: String?
    /// Why a node that ended on its own ended, from `Status()` when down.
    public let ended: String?
    /// The word for `ended` (`revoked`, `join_failed`, …).
    public let endedCode: String?
    public let doorID: String?
    /// `PeerPath()`'s answer: `direct`, `derp`, `peer_relay`, `idle` or `unknown`.
    public let path: String?
    /// The UDP address a direct path uses, `ip:port`; this phone's own network.
    public let via: String?
    /// A DERP region, when `path` is `derp`.
    public let relay: String?

    enum CodingKeys: String, CodingKey {
        case ok, address, addresses, name, state, error, code, ended, path, via, relay
        case endedCode = "ended_code"
        case doorID = "door_id"
    }
}

public enum NodeError: Error, Equatable, Sendable {
    /// Go said `{"error": message, "code": code}`. The app switches on `code`
    /// (empty when Go named none) and may show `message`.
    case refused(code: String, message: String)
    /// Go answered something that is not its JSON; the call's name.
    case unreadableAnswer(String)

    /// Go's word for the failure; empty when it named none or did not answer
    /// in JSON.
    public var code: String {
        switch self {
        case .refused(let code, _): code
        case .unreadableAnswer: ""
        }
    }
}

/// B1: the library's view of the path to one peer (`phonenode.PeerPath`).
public struct PeerPath: Decodable, Equatable, Sendable, CustomStringConvertible {
    public let path: String
    public let via: String?
    public let relay: String?

    public init(path: String, via: String?, relay: String?) {
        self.path = path
        self.via = via
        self.relay = relay
    }

    /// The spec's words: `direct [fd8c::…]:41641`, `derp syd`, `peer_relay`.
    public var description: String {
        switch path {
        case "direct": return via.map { "direct \($0)" } ?? "direct"
        case "derp": return relay.map { "derp \($0)" } ?? "derp"
        default: return path
        }
    }
}

public protocol NodeControl: Sendable {
    func join(dir: String, hostname: String, enrollmentJSON: String) async -> Result<NodeAnswer, NodeError>
    func rejoin(dir: String, hostname: String) async -> Result<NodeAnswer, NodeError>
    /// The node as it stands; ask on every foreground. A node that ended on
    /// its own reads state `down` with `endedCode`.
    func status() async -> Result<NodeAnswer, NodeError>
    func down() async
    /// A6: the interface the phone's default route is on now. The library
    /// pins its sockets to it; without the hint it guesses from the routing
    /// table. Safe in every node state.
    func defaultRoute(interface: String) async -> Result<NodeAnswer, NodeError>
}

public protocol DoorOpening: Sendable {
    func openDoor(address: String, port: Int) async throws -> DoorConnection
    /// B1: the library's path to the Mac at `address` now; nil when the node
    /// cannot say (down, joining, or its status did not read).
    func peerPath(address: String) async -> PeerPath?
}

/// The phone's own 6net node: one per process, as `phonenode` has it.
///
/// Every bridge call blocks its thread, so each runs on a thread of this
/// node's own, never on Swift's cooperative pool or the main thread. They
/// run side by side: a Down calls off a Join still under way, and Status
/// never waits on one. A door's Send and Close keep to the door's own queue.
public final class Node: NodeControl, DoorOpening, @unchecked Sendable {
    private let bridge: NodeBridge
    private let calls = DispatchQueue(label: "ai.gentility.desk.phone.node", qos: .userInitiated, attributes: .concurrent)

    public init(bridge: NodeBridge) {
        self.bridge = bridge
    }

    public func join(dir: String, hostname: String, enrollmentJSON: String) async -> Result<NodeAnswer, NodeError> {
        read("join", await call { [bridge] in bridge.join(dir: dir, hostname: hostname, enrollmentJSON: enrollmentJSON) })
    }

    public func rejoin(dir: String, hostname: String) async -> Result<NodeAnswer, NodeError> {
        read("rejoin", await call { [bridge] in bridge.rejoin(dir: dir, hostname: hostname) })
    }

    public func status() async -> Result<NodeAnswer, NodeError> {
        read("status", await call { [bridge] in bridge.status() }, stateIsAnAnswer: true)
    }

    public func down() async {
        _ = read("down", await call { [bridge] in bridge.down() })
    }

    public func defaultRoute(interface: String) async -> Result<NodeAnswer, NodeError> {
        read("default_route", await call { [bridge] in bridge.defaultRoute(interface: interface) })
    }

    public func openDoor(address: String, port: Int) async throws -> DoorConnection {
        let door = DoorConnection(bridge: bridge, label: "[\(address)]:\(port)")
        Log.door.info("opening a door to [\(address, privacy: .public)]:\(port)")
        let opened = read("open_door", await call { [bridge] in bridge.openDoor(address: address, port: port, sink: door) })
        switch opened {
        case .success(let answer):
            guard let id = answer.doorID else {
                Log.door.error("open_door to [\(address, privacy: .public)]:\(port) named no door")
                throw NodeError.unreadableAnswer("open_door")
            }
            door.attach(id)
            return door
        case .failure(let error):
            throw error
        }
    }

    public func peerPath(address: String) async -> PeerPath? {
        switch read("peer_path", await call { [bridge] in bridge.peerPath(address: address) }) {
        case .success(let answer):
            guard let path = answer.path else { return nil }
            return PeerPath(path: path, via: answer.via, relay: answer.relay)
        case .failure:
            return nil
        }
    }

    /// The bridge call on a thread of its own, and how long it took.
    private func call(_ work: @escaping @Sendable () -> String) async -> (text: String, took: Duration) {
        let started = ContinuousClock.now
        let text = await withCheckedContinuation { (done: CheckedContinuation<String, Never>) in
            calls.async { done.resume(returning: work()) }
        }
        return (text, ContinuousClock.now - started)
    }

    /// Go's answer, read. `stateIsAnAnswer`: Status's `status_unknown` names
    /// an error beside state `up`, and is still the node's state.
    private func read(_ label: String, _ answered: (text: String, took: Duration), stateIsAnAnswer: Bool = false) -> Result<NodeAnswer, NodeError> {
        let took = String(describing: answered.took)
        guard let answer = try? Wire.decoder.decode(NodeAnswer.self, from: Data(answered.text.utf8)) else {
            Log.node.error("\(label, privacy: .public) answered something unreadable (\(answered.text.utf8.count) bytes) after \(took, privacy: .public)")
            return .failure(.unreadableAnswer(label))
        }
        if let error = answer.error, !(stateIsAnAnswer && answer.state != nil) {
            let code = answer.code ?? ""
            Log.node.error("\(label, privacy: .public) refused after \(took, privacy: .public): \(code, privacy: .public): \(error, privacy: .public)")
            return .failure(.refused(code: code, message: error))
        }
        if let error = answer.error {
            Log.node.notice("\(label, privacy: .public) answered \(answer.state ?? "", privacy: .public) after \(took, privacy: .public), \(answer.code ?? "", privacy: .public): \(error, privacy: .public)")
        } else if let ended = answer.ended {
            Log.node.notice("\(label, privacy: .public) answered \(answer.state ?? "", privacy: .public) after \(took, privacy: .public); the node ended, \(answer.endedCode ?? "", privacy: .public): \(ended, privacy: .public)")
        } else {
            let said = answer.address ?? answer.state ?? answer.doorID ?? answer.path ?? "ok"
            Log.node.info("\(label, privacy: .public) answered after \(took, privacy: .public): \(said, privacy: .public)")
        }
        return .success(answer)
    }
}
