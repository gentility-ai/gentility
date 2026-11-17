import Foundation
@testable import DeskPhoneKit

/// The phone's notification settings as the test sets them: what iOS
/// answers, whether the person allows them when asked, and every call.
@MainActor
final class FakeNotifications: Notifications {
    var status: PushAuthorization = .notDetermined
    var grants = true
    /// Runs before the person answers the prompt: a test holds it open here.
    var prompting: () async -> Void = {}
    private(set) var asked = 0
    private(set) var registeredForRemote = 0
    private(set) var removed: [String] = []

    func authorization() async -> PushAuthorization { status }

    func requestAuthorization() async -> Bool {
        asked += 1
        await prompting()
        status = grants ? .allowed : .denied
        return grants
    }

    func registerForRemote() { registeredForRemote += 1 }

    func removeDelivered(sessionID: String) async { removed.append(sessionID) }
}
