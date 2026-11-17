import os

/// Where every line goes. Console.app (or `log stream`, filtered on this
/// subsystem) shows them; a Debug launch with `OS_ACTIVITY_DT_MODE=YES`
/// mirrors them to the console `devicectl --console` carries. No line ever
/// carries a bearer, a code, a poll secret, a preauth key or a socket URL:
/// call sites never log a secret value, and interpolate any dynamic value
/// that could carry one with `privacy: .private`.
public enum Log {
    private static let subsystem = "ai.gentility.desk.phone"
    public static let app = Logger(subsystem: subsystem, category: "app")
    public static let pairing = Logger(subsystem: subsystem, category: "pairing")
    public static let socket = Logger(subsystem: subsystem, category: "socket")
    public static let node = Logger(subsystem: subsystem, category: "node")
    public static let door = Logger(subsystem: subsystem, category: "door")
    public static let home = Logger(subsystem: subsystem, category: "home")
    public static let conversation = Logger(subsystem: subsystem, category: "conversation")
    public static let go = Logger(subsystem: subsystem, category: "go")
    /// Notifications: permission, the token's length and environment (never
    /// its bytes), every tap and where it went.
    public static let push = Logger(subsystem: subsystem, category: "push")
}
