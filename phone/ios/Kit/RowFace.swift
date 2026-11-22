import Foundation

/// Something of a thread's the menu copies to the pasteboard.
public enum HomeCopy: Equatable, Sendable {
    case threadID(String), branch(String), title(String)

    /// The menu line, worded as Desk words it on the Mac.
    public var label: String {
        switch self {
        case .threadID: "Copy thread id"
        case .branch: "Copy branch"
        case .title: "Copy title"
        }
    }

    public var symbol: String {
        switch self {
        case .threadID: "number"
        case .branch: "arrow.triangle.branch"
        case .title: "textformat"
        }
    }

    /// What lands on the pasteboard.
    public var text: String {
        switch self {
        case .threadID(let text), .branch(let text), .title(let text): text
        }
    }
}

/// An item of a Home row's long-press menu (`HomeModel.menu(for:now:)`).
public enum HomeMenuItem: Equatable, Sendable {
    /// What the thread offers to copy, first in every menu.
    case copy([HomeCopy])
    case settle, unsettle
    /// Snooze, offering these times.
    case snooze([SnoozePreset])
    case wake, archive
    /// A looper run: one disabled line saying where it is managed.
    case managedOnMac

    /// The action's name, for comparing with the swipes; nil for none.
    var kind: String? {
        switch self {
        case .settle: "settle"
        case .unsettle: "unsettle"
        case .snooze: "snooze"
        case .wake: "wake"
        case .archive: "archive"
        case .copy, .managedOnMac: nil
        }
    }
}

/// The colour a status word is drawn in; the view maps each to a system colour.
public enum StatusTone: Hashable, Sendable {
    case approval, input, working, snoozed, settled
}

/// A row's status: a word, an SF Symbol, and a tone.
public struct StatusWord: Hashable, Sendable {
    public let text: String
    public let symbol: String
    public let tone: StatusTone

    public init(text: String, symbol: String, tone: StatusTone) {
        self.text = text
        self.symbol = symbol
        self.tone = tone
    }
}

/// What a row that is not a plain thread is marked with.
public struct KindMark: Hashable, Sendable {
    public let symbol: String
    public let name: String
}

/// The Mac on line 3: named only when the person's rows span more than one.
public struct MacMark: Hashable, Sendable {
    public let name: String?
    public let asleep: Bool

    public init(name: String?, asleep: Bool) {
        self.name = name
        self.asleep = asleep
    }
}

extension Ago {
    /// The Mac's `rowAge`: `now`, `15m`, `2h`, `3d`; a future instant is
    /// `now`, and none is empty.
    public static func compact(_ then: Date?, now: Date) -> String {
        guard let then else { return "" }
        let seconds = Int(now.timeIntervalSince(then).rounded(.down))
        if seconds < 60 { return "now" }
        let minutes = seconds / 60
        if minutes < 60 { return "\(minutes)m" }
        let hours = minutes / 60
        if hours < 24 { return "\(hours)h" }
        return "\(hours / 24)d"
    }

    /// `compact`, as VoiceOver says it.
    static func spoken(_ then: Date?, now: Date) -> String? {
        guard let then else { return nil }
        let seconds = Int(now.timeIntervalSince(then).rounded(.down))
        func ago(_ n: Int, _ unit: String) -> String { "\(n) \(unit)\(n == 1 ? "" : "s") ago" }
        if seconds < 60 { return "Just now" }
        let minutes = seconds / 60
        if minutes < 60 { return ago(minutes, "minute") }
        let hours = minutes / 60
        if hours < 24 { return ago(hours, "hour") }
        return ago(hours / 24, "day")
    }
}

/// What a Home row draws, as plain values: the view only maps tones to colours.
public struct RowFace: Equatable, Sendable {
    /// The repository's name, or "Looper run" or "Responder run".
    public let place: String?
    /// Nil for a plain thread.
    public let kind: KindMark?
    public let starred: Bool
    public let word: StatusWord?
    /// The word asks something of the person: it is drawn in its colour at
    /// the row's end, where a calm row has its age.
    public let asks: Bool
    public let age: String
    public let branch: String?
    public let mac: MacMark
    /// Drawn with a secondary title: parked, or on a Mac that is asleep.
    public let recedes: Bool
    /// On the Needs you shelf: a heavier title and the attention line.
    public let needsYou: Bool
    public let attentionLine: String?
    public let accessibilityLabel: String

    public init(_ row: HomeRow, now: Date, showsMacName: Bool, calendar: Calendar = .current) {
        let repository = row.project.flatMap { $0.split(separator: "/").last.map(String.init) }
        place = row.looper ? (row.kind == .responder ? "Responder run" : "Looper run") : repository
        kind = row.kind.symbol.map { KindMark(symbol: $0, name: row.kind.singular) }
        starred = row.starred
        let snoozed = row.snoozedUntil.flatMap { $0 > now ? $0 : nil }
        word = switch row.attention {
        case .approval: StatusWord(text: "Approval", symbol: "checkmark.shield", tone: .approval)
        case .question: StatusWord(text: "Input", symbol: "questionmark.bubble", tone: .input)
        case .none:
            if let snoozed {
                StatusWord(text: Self.wakeClock(snoozed, now: now, calendar: calendar), symbol: "moon.zzz", tone: .snoozed)
            } else if row.settledAt != nil {
                StatusWord(text: "Settled", symbol: "checkmark.circle", tone: .settled)
            } else if row.state == "working" {
                StatusWord(text: "Working", symbol: "circle.dashed", tone: .working)
            } else if row.waitsOnYou {
                // A raised hand, not the question's bubble: no form waits
                // here, only the person's word. Indigo, as Input is.
                StatusWord(text: "Waiting on you", symbol: "hand.raised", tone: .input)
            } else {
                nil
            }
        }
        asks = word.map { $0.tone == .approval || $0.tone == .input } ?? false
        age = Ago.compact(row.lastEventAt, now: now)
        branch = row.branch.flatMap { $0.isEmpty ? nil : $0 }
        let asleep = !row.live && !row.deskOnline
        mac = MacMark(name: showsMacName ? row.macLabel : nil, asleep: asleep)
        needsYou = Shelves.shelf(of: row, now: now) == .needsYou
        recedes = !needsYou && (snoozed != nil || row.settledAt != nil || asleep)
        attentionLine = needsYou ? row.waitingLine : nil

        var parts = [row.title]
        if starred { parts.append("Starred") }
        if let kind, !row.looper { parts.append(kind.name) }
        if let word {
            parts.append(attentionLine.map { "\(word.text): \($0)" } ?? word.text)
        }
        switch (place, branch) {
        case let (place?, branch?): parts.append("\(place), branch \(branch)")
        case let (place?, nil): parts.append(place)
        case let (nil, branch?): parts.append("Branch \(branch)")
        case (nil, nil): break
        }
        parts.append(asleep ? "\(row.macLabel), asleep" : row.macLabel)
        if let spoken = Ago.spoken(row.lastEventAt, now: now) { parts.append(spoken) }
        accessibilityLabel = parts.map { $0 + "." }.joined(separator: " ")
    }

    /// True when `rows` name more than one Mac.
    public static func showsMacName(_ rows: [HomeRow]) -> Bool {
        Set(rows.map(\.macLabel)).count > 1
    }

    /// When a snooze wakes: the time alone today, the weekday before it
    /// within the week, the date beyond.
    static func wakeClock(_ when: Date, now: Date, calendar: Calendar) -> String {
        let days = calendar.dateComponents([.day], from: calendar.startOfDay(for: now), to: calendar.startOfDay(for: when)).day ?? 0
        let formatter = DateFormatter()
        formatter.calendar = calendar
        formatter.timeZone = calendar.timeZone
        formatter.locale = Locale(identifier: "en_GB")
        formatter.dateFormat = days == 0 ? "HH:mm" : days < 7 ? "EEE HH:mm" : "EEE d MMM"
        return formatter.string(from: when)
    }
}
