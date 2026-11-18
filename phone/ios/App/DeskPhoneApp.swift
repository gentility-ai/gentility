import DeskPhoneKit
import SwiftUI
import UIKit

@main
struct DeskPhoneApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var app: AppModel
    @Environment(\.scenePhase) private var phase
    private let routes = RouteMonitor()

    /// Builds the parts and nothing more: no keychain read and no node call
    /// happens here. `launch()` waits for protected data first.
    init() {
        HomeActionsTip.configure()
        let directory: NodeDirectory
        do {
            directory = try NodeDirectory.standard()
        } catch {
            Log.app.error("no Application Support directory (\(String(describing: error), privacy: .public)); the node keeps its state in tmp")
            directory = NodeDirectory(url: FileManager.default.temporaryDirectory.appendingPathComponent("phonenode", isDirectory: true))
        }
        #if DEBUG
        if let credential = Injected.credential {
            _app = StateObject(wrappedValue: AppModel(
                store: MemoryStore(credential: credential),
                nodeDirectory: directory,
                node: Node(bridge: OfflineBridge()),
                permission: AVCameraPermission(),
                pushEnvironment: Self.pushEnvironment
            ))
            return
        }
        #endif
        _app = StateObject(wrappedValue: AppModel(
            store: KeychainStore(),
            nodeDirectory: directory,
            node: Node(bridge: PhonenodeBridge()),
            permission: AVCameraPermission(),
            notifications: LiveNotifications(),
            pushEnvironment: Self.pushEnvironment,
            protectedData: { await ProtectedData.available() }
        ))
    }

    /// The APNs host this install's tokens belong to (contract deviation 4).
    /// A simulator build carries no profile and gets sandbox tokens.
    private static var pushEnvironment: String? {
        #if targetEnvironment(simulator)
        return "sandbox"
        #else
        return PushEnvironment.current()
        #endif
    }

    /// The app, with the delegate's token and taps handed to it, and the
    /// default route watched for the node.
    private var root: some View {
        RootView(app: app).onAppear { delegate.attach(app); routes.start(app) }
    }

    var body: some Scene {
        WindowGroup {
            Group {
                #if DEBUG
                if let screen = ProcessInfo.processInfo.environment["DESK_PHONE_SHOWCASE"] {
                    Showcase(screen: screen)
                } else {
                    root
                }
                #else
                root
                #endif
            }
            .tint(Theme.accent)
        }
        .onChange(of: phase) { _, now in
            Log.app.info("scene \(String(describing: now), privacy: .public)")
            #if DEBUG
            if ProcessInfo.processInfo.environment["DESK_PHONE_SHOWCASE"] != nil { return }
            #endif
            switch now {
            case .background: app.background()
            case .active: Task { await app.foreground() }
            default: break
            }
        }
    }
}

/// The keychain item and the node's files are readable after the first
/// unlock; a launch before it (a background wake) waits for it.
@MainActor
enum ProtectedData {
    static func available() async {
        guard !UIApplication.shared.isProtectedDataAvailable else { return }
        Log.app.info("protected data is not available yet (before the first unlock); waiting")
        while !UIApplication.shared.isProtectedDataAvailable {
            do { try await Task.sleep(for: .seconds(1)) } catch { return }
        }
        Log.app.info("protected data is available")
    }
}
