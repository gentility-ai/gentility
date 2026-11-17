import DeskPhoneKit
import Foundation
import Phonenode

/// The one file that speaks gomobile. Everything above it is DeskPhoneKit's,
/// and tested without Go. Every call blocks; `Node` makes them off the main
/// thread.
final class PhonenodeBridge: NodeBridge, @unchecked Sendable {
    private let logger = GoLog()

    func join(dir: String, hostname: String, enrollmentJSON: String) -> String {
        PhonenodeJoin(dir, hostname, enrollmentJSON, logger)
    }

    func rejoin(dir: String, hostname: String) -> String {
        PhonenodeRejoin(dir, hostname, logger)
    }

    func status() -> String { PhonenodeStatus() }

    func down() -> String { PhonenodeDown() }

    func openDoor(address: String, port: Int, sink: DoorSink) -> String {
        PhonenodeOpenDoor(address, port, Listener(sink))
    }

    func send(doorID: String, frame: String) -> String { PhonenodeSend(doorID, frame) }

    func close(doorID: String) -> String { PhonenodeClose(doorID) }

    func peerPath(address: String) -> String { PhonenodePeerPath(address) }

    func defaultRoute(interface: String) -> String { PhonenodeDefaultRoute(interface, logger) }
}

/// Go's log lines, into the app's log. They arrive in order on one goroutine
/// of phonenode's own; each hops to a serial queue of ours, so Go's line is
/// handed back at once and the order holds. Contract C: Go never writes a
/// bearer, a preauth key or a code into one.
private final class GoLog: NSObject, PhonenodeLoggerProtocol {
    private let lines = DispatchQueue(label: "ai.gentility.desk.phone.go-log", qos: .utility)

    func log(_ line: String?) {
        guard let line else { return }
        lines.async { Log.go.info("\(line, privacy: .public)") }
    }
}

/// `FrameListener`: Go holds it for the door's life (gomobile keeps the
/// reference) and calls it on the door's own goroutine.
private final class Listener: NSObject, PhonenodeFrameListenerProtocol {
    private let sink: DoorSink

    init(_ sink: DoorSink) {
        self.sink = sink
    }

    func onFrame(_ doorID: String?, frame: String?) {
        sink.frame(doorID: doorID ?? "", text: frame ?? "")
    }

    func onClose(_ doorID: String?, reason: String?) {
        sink.closed(doorID: doorID ?? "", reason: reason ?? "")
    }
}
