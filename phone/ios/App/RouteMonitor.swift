import DeskPhoneKit
import Network

/// A6: watches the phone's default route and tells the node which interface
/// it is on, at start and on every change, as Tailscale's own app does. The
/// satisfied path's first available interface is the one the system routes
/// by. Updates arrive in order on the monitor's queue and go to the node one
/// at a time, each answered before the next is said, so Go's last hint is
/// the phone's last route. The monitor lives as long as the app and is
/// never cancelled.
@MainActor
final class RouteMonitor {
    private let monitor = NWPathMonitor()
    private var started = false

    func start(_ app: AppModel) {
        guard !started else { return }
        started = true
        let (routes, said) = AsyncStream.makeStream(of: String?.self)
        monitor.pathUpdateHandler = { path in
            said.yield(path.status == .satisfied ? path.availableInterfaces.first?.name : nil)
        }
        Task { @MainActor [weak app] in
            var last: String?
            for await name in routes {
                guard let app else { return }
                guard let name else {
                    Log.node.info("default route: no satisfied path, or none with an interface; the node keeps its last hint")
                    continue
                }
                // Go keeps the hint across an unsatisfied gap, so
                // en0 → none → en0 is said once.
                guard name != last else { continue }
                if await app.defaultRoute(name) { last = name }
            }
        }
        monitor.start(queue: DispatchQueue(label: "ai.gentility.desk.phone.route", qos: .utility))
    }
}
