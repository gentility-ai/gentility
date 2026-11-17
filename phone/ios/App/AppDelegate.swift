import DeskPhoneKit
import UIKit
import UserNotifications

/// The device token, and what a notification does in the foreground and when
/// tapped. What each means is AppModel's; this hands them over, holding any
/// that come before the model is attached (a cold launch from a tap).
final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    private weak var app: AppModel?
    private var token: Data?
    private var taps: [PushPayload] = []

    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        // Before launch ends, so a tap that launched the app is delivered.
        UNUserNotificationCenter.current().delegate = self
        return true
    }

    @MainActor
    func attach(_ app: AppModel) {
        guard self.app == nil else { return }
        self.app = app
        if let token {
            app.gotPushToken(token)
            self.token = nil
        }
        for tap in taps { app.opened(tap) }
        taps = []
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        if let app { app.gotPushToken(deviceToken) } else { token = deviceToken }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        app?.pushTokenFailed(error)
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        let payload = PushPayload(userInfo: notification.request.content.userInfo)
        Task { @MainActor in
            let shows = payload.map { self.app?.presents($0) ?? true } ?? true
            completionHandler(shows ? [.banner, .list, .sound] : [])
        }
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        let action = response.actionIdentifier
        let payload = action == UNNotificationDefaultActionIdentifier
            ? PushPayload(userInfo: response.notification.request.content.userInfo) : nil
        Task { @MainActor in
            if let payload {
                if let app = self.app { app.opened(payload) } else { self.taps.append(payload) }
            } else if action == UNNotificationDefaultActionIdentifier {
                Log.push.notice("a notification was tapped that names no session to open")
            } else if action == UNNotificationDismissActionIdentifier {
                Log.push.info("a notification was dismissed")
            } else {
                Log.push.notice("a notification's action \(action, privacy: .public) opens nothing")
            }
            completionHandler()
        }
    }
}
