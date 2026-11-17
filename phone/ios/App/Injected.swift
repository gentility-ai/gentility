#if DEBUG
import DeskPhoneKit
import Foundation

/// A pairing handed in by the environment (`DESK_PHONE_CREDENTIAL="<origin>
/// <bearer>"`, which `mix desk.phone_seed` prints), for the end-to-end tests:
/// no claim, no QR and no keychain. The phone joins no 6net node; Home's rows
/// and inbox go over HTTP and the socket, which is all the tests drive.
/// Debug builds only.
enum Injected {
    static var credential: Credential? {
        guard let line = ProcessInfo.processInfo.environment["DESK_PHONE_CREDENTIAL"] else { return nil }
        let parts = line.split(separator: " ")
        guard parts.count == 2, let origin = URL(string: String(parts[0])) else {
            Log.app.error("DESK_PHONE_CREDENTIAL is not \"<origin> <bearer>\"; pairing as usual")
            return nil
        }
        // A fresh install wipes the store before reading it
        // (`forgetIfReinstalled`): this install is marked as already seen.
        UserDefaults.standard.set(true, forKey: "ai.gentility.desk.phone.installed")
        return Credential(
            origin: origin, bearer: String(parts[1]), expiresAt: .distantFuture,
            enrollmentJSON: nil, enrollmentID: nil, phoneName: "E2E iPhone"
        )
    }
}

/// A node that never comes up: every call is refused with a word `AppModel`
/// neither retries nor forgets the pairing over.
final class OfflineBridge: NodeBridge, @unchecked Sendable {
    private let refused = #"{"error":"no 6net in the end-to-end tests","code":"offline"}"#

    func join(dir: String, hostname: String, enrollmentJSON: String) -> String { refused }
    func rejoin(dir: String, hostname: String) -> String { refused }
    func status() -> String { #"{"state":"down"}"# }
    func down() -> String { #"{"ok":true}"# }
    func openDoor(address: String, port: Int, sink: DoorSink) -> String { refused }
    func send(doorID: String, frame: String) -> String { refused }
    func close(doorID: String) -> String { refused }
    func peerPath(address: String) -> String { refused }
    func defaultRoute(interface: String) -> String { refused }
}
#endif
