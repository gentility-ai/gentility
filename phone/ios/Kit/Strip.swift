import Foundation

/// The one strip above the composer: what the session is doing now, and how
/// many prompts wait behind it. It sits beside the jump to the latest line,
/// and leaves room at its end for a Stop.
public struct StatusStrip: Equatable, Sendable {
    public enum Doing: Equatable, Sendable {
        /// Dialling the Mac; `again` with turns already on screen.
        case connecting(again: Bool)
        /// A turn is running, since when the Mac says it began.
        case working(since: Date?)
        /// The turn running is compacting the agent's context.
        case compacting
    }

    public let doing: Doing?
    /// The prompts queued behind the turn.
    public let queued: Int

    /// Nil where the strip has nothing to say. Asleep, doorless, refused or
    /// dropped, the bar or banner over the transcript says it, and a turn
    /// the server last heard working is that banner's to say.
    public init?(phase: ConversationModel.Phase, held: Bool, working: Bool, compacting: Bool = false, since: Date?, queued: Int) {
        switch phase {
        case .connecting: doing = .connecting(again: held)
        case .open: doing = !working ? nil : compacting ? .compacting : .working(since: since)
        case .asleep, .linkUnavailable, .noDoor, .notOpen, .refused, .lost: return nil
        }
        self.queued = phase == .open ? queued : 0
        if doing == nil, self.queued == 0 { return nil }
    }

    /// Whether the strip draws a spinner. A turn at work has its live sign in
    /// the transcript, the caret or the running call, so the strip spins for
    /// it only once that end has scrolled out of sight. Connecting and
    /// compacting have no other sign.
    public func spins(atEnd: Bool) -> Bool {
        switch doing {
        case .connecting, .compacting: true
        case .working: !atEnd
        case nil: false
        }
    }

    /// "Working · 1m 05s · 2 queued".
    public func words(now: Date) -> String {
        var parts: [String] = []
        switch doing {
        case .connecting(let again): parts.append(again ? "Reconnecting…" : "Connecting…")
        case .working(let since):
            parts.append("Working")
            if let since { parts.append(Self.elapsed(now.timeIntervalSince(since))) }
        case .compacting: parts.append("Compacting…")
        case nil: break
        }
        if queued > 0 { parts.append("\(queued) queued") }
        return parts.joined(separator: " · ")
    }

    /// The window's clock (`elapsed` in desk/ui/src/lib/format.ts): `45s`,
    /// `3m 05s`, `1h 02m`. A Mac's clock ahead of the phone's says `0s`.
    public static func elapsed(_ interval: TimeInterval) -> String {
        let total = max(0, Int(interval))
        let hours = total / 3600, minutes = total / 60 % 60, seconds = total % 60
        if hours > 0 { return "\(hours)h " + String(format: "%02dm", minutes) }
        if minutes > 0 { return "\(minutes)m " + String(format: "%02ds", seconds) }
        return "\(seconds)s"
    }
}
