import Foundation

/// An item of a Home row's long-press menu (`HomeModel.menu(for:now:)`).
public enum HomeMenuItem: Equatable, Sendable {
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
        case .managedOnMac: nil
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

/// A project's letter tile: square for a repository, round for a looper run.
public struct Chip: Hashable, Sendable {
    public let letter: String
    public let round: Bool
    /// Into `ChipColor`'s eighteen hues.
    public let colorIndex: Int

    public init(letter: String, round: Bool, colorIndex: Int) {
        self.letter = letter
        self.round = round
        self.colorIndex = colorIndex
    }
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

/// The colour of a project's chip: a port of `hue` in
/// `desk/ui/src/sidebar/chip.logic.ts`, so a project is one colour on the
/// phone and the Mac. Change one, change both.
public enum ChipColor {
    /// The hues, in the Mac's order: gray, red, orange, amber, yellow, lime,
    /// green, emerald, teal, cyan, sky, blue, indigo, violet, purple,
    /// fuchsia, pink, rose.
    public static let count = 18

    public static func index(for name: String) -> Int {
        let normal = name.precomposedStringWithCompatibilityMapping.trimmingCharacters(in: .whitespacesAndNewlines)
        let lowered = normal.lowercased(with: Locale(identifier: "en_US"))
        let seed = lowered.isEmpty ? "project" : lowered
        var index = 0
        for scalar in seed.unicodeScalars {
            index = (index * 31 + Int(scalar.value)) % count
        }
        return index
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
    public let chip: Chip?
    /// The repository's name, or "Looper run".
    public let place: String?
    public let word: StatusWord?
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
        let chipName = repository ?? (row.looper ? "looper" : nil)
        chip = chipName.map { name in
            Chip(letter: name.first.map { String($0).uppercased() } ?? "?", round: row.looper, colorIndex: ChipColor.index(for: name))
        }
        place = row.looper ? "Looper run" : repository
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
        age = Ago.compact(row.lastEventAt, now: now)
        branch = row.branch.flatMap { $0.isEmpty ? nil : $0 }
        let asleep = !row.live && !row.deskOnline
        mac = MacMark(name: showsMacName ? row.macLabel : nil, asleep: asleep)
        needsYou = Shelves.shelf(of: row, now: now) == .needsYou
        recedes = !needsYou && (snoozed != nil || row.settledAt != nil || asleep)
        attentionLine = needsYou ? row.waitingLine : nil

        var parts = [row.title]
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
