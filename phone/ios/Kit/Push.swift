import Foundation

/// Which APNs host this install's tokens belong to: the one its provisioning
/// profile was made for (`Entitlements.aps-environment`). A development
/// profile is `sandbox`, a distribution one `production`. Nil when there is
/// no profile (the simulator; App Store and TestFlight installs, whose
/// profile is stripped), when it does not read, or when it names neither:
/// `register` then leaves the word out, and the server pushes through
/// production.
public enum PushEnvironment {
    /// The profile in the app's bundle, read.
    public static func current(in bundle: URL = Bundle.main.bundleURL) -> String? {
        let url = bundle.appendingPathComponent("embedded.mobileprovision")
        return read(profile: try? Data(contentsOf: url))
    }

    /// A profile is Apple's signed envelope (CMS, DER) around an XML plist.
    /// The plist is read from its `<?xml` to its `</plist>`, as
    /// `security cms -D` shows it; the signature is not checked.
    public static func read(profile: Data?) -> String? {
        guard let profile else {
            Log.push.info("no provisioning profile in the bundle; no environment is sent")
            return nil
        }
        guard let start = profile.range(of: Data("<?xml".utf8)),
              let end = profile.range(of: Data("</plist>".utf8), in: start.lowerBound..<profile.endIndex),
              let plist = try? PropertyListSerialization.propertyList(from: profile[start.lowerBound..<end.upperBound], format: nil) as? [String: Any],
              let entitlements = plist["Entitlements"] as? [String: Any] else {
            Log.push.error("the provisioning profile (\(profile.count) bytes) does not read; no environment is sent")
            return nil
        }
        switch entitlements["aps-environment"] as? String {
        case "development":
            Log.push.info("the provisioning profile is a development one: sandbox")
            return "sandbox"
        case "production":
            Log.push.info("the provisioning profile is a distribution one: production")
            return "production"
        case let other:
            Log.push.error("the provisioning profile's aps-environment is \(other ?? "absent", privacy: .public); no environment is sent")
            return nil
        }
    }
}

public enum DeviceToken {
    /// APNs' token as `register` carries it: lower-case hex, two digits a byte.
    public static func hex(_ token: Data) -> String {
        token.map { String(format: "%02x", $0) }.joined()
    }
}

/// A notification's payload:
/// the conversation a tap opens.
public struct PushPayload: Equatable, Sendable {
    public let organizationID: String
    public let sessionID: String
    public let deskMachineID: String?
    /// `approval_requested`, `question_asked`, `turn_completed`, `turn_aborted`.
    public let fact: String

    public init(organizationID: String, sessionID: String, deskMachineID: String?, fact: String) {
        self.organizationID = organizationID
        self.sessionID = sessionID
        self.deskMachineID = deskMachineID
        self.fact = fact
    }

    /// Nil unless it names an organisation and a session to open.
    public init?(userInfo: [AnyHashable: Any]) {
        guard let organization = userInfo["org_id"] as? String, !organization.isEmpty,
              let session = userInfo["session_id"] as? String, !session.isEmpty else {
            return nil
        }
        self.init(
            organizationID: organization, sessionID: session,
            deskMachineID: userInfo["desk_machine_id"] as? String, fact: userInfo["fact"] as? String ?? ""
        )
    }
}

public enum PushAuthorization: Equatable, Sendable {
    case notDetermined, denied, allowed
}

/// The phone's notification settings and APNs, as the app reaches them
/// (`UNUserNotificationCenter`, `UIApplication`): the kit imports neither.
@MainActor
public protocol Notifications: AnyObject {
    func authorization() async -> PushAuthorization
    /// Asks the person; true when allowed. iOS shows the prompt only while
    /// the answer is not determined.
    func requestAuthorization() async -> Bool
    /// The token comes back through the app delegate
    /// (`AppModel.gotPushToken` or `pushTokenFailed`).
    func registerForRemote()
    /// Takes this session's delivered notifications off the screen.
    func removeDelivered(sessionID: String) async
}
