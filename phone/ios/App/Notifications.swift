import DeskPhoneKit
import UIKit
import UserNotifications

/// `Notifications`, on the phone's own centre.
@MainActor
final class LiveNotifications: Notifications {
    private let center = UNUserNotificationCenter.current()

    func authorization() async -> PushAuthorization {
        switch await center.notificationSettings().authorizationStatus {
        case .notDetermined: .notDetermined
        case .denied: .denied
        default: .allowed
        }
    }

    func requestAuthorization() async -> Bool {
        do {
            return try await center.requestAuthorization(options: [.alert, .sound])
        } catch {
            let error = error as NSError
            Log.push.error("the permission request failed: \(error.domain, privacy: .public) \(error.code)")
            return false
        }
    }

    func registerForRemote() {
        Log.push.info("asking APNs for a token")
        UIApplication.shared.registerForRemoteNotifications()
    }

    /// The server sets the session as the notification's `thread-id` and
    /// its `session_id` (the push payload); either one
    /// names it.
    func removeDelivered(sessionID: String) async {
        let ids = await center.deliveredNotifications()
            .filter { Self.names($0.request.content, sessionID) }
            .map(\.request.identifier)
        guard !ids.isEmpty else { return }
        center.removeDeliveredNotifications(withIdentifiers: ids)
        Log.push.info("took \(ids.count) notification(s) for \(sessionID, privacy: .public) off the screen")
    }

    private static func names(_ content: UNNotificationContent, _ sessionID: String) -> Bool {
        content.threadIdentifier == sessionID || (content.userInfo["session_id"] as? String) == sessionID
    }
}
