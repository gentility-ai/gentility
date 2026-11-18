import Foundation

/// What the Settings page says about this phone, as plain values.
public struct PhoneSettings: Equatable, Sendable {
    public enum Connection: Equatable, Sendable {
        case connected
        case connecting
        case off
        case failed

        public var words: String {
            switch self {
            case .connected: return "Connected"
            case .connecting: return "Connecting…"
            case .off: return "Not connected"
            case .failed: return "Can't connect"
            }
        }
    }

    /// The name the phone was paired under, as Desk lists it.
    public let name: String
    /// The server the pairing is with.
    public let server: String
    /// When the pairing's 30 days run out.
    public let expires: Date
    public let connection: Connection
    public let notificationsOff: Bool

    public init(name: String, server: String, expires: Date, connection: Connection, notificationsOff: Bool) {
        self.name = name
        self.server = server
        self.expires = expires
        self.connection = connection
        self.notificationsOff = notificationsOff
    }

    /// How long the pairing has left, in whole days, counted up: a pairing
    /// with an hour left still has "1 day left".
    public func left(now: Date) -> String {
        let seconds = expires.timeIntervalSince(now)
        guard seconds > 0 else { return "Run out" }
        let days = Int((seconds / 86_400).rounded(.up))
        return days == 1 ? "1 day left" : "\(days) days left"
    }

    /// The pairing is in its last week.
    public func endsSoon(now: Date) -> Bool {
        expires.timeIntervalSince(now) <= 7 * 86_400
    }
}
