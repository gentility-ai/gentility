import Foundation

/// One line of the snooze menu: what it says, when it wakes, and that wake
/// as its right column.
public struct SnoozePreset: Equatable, Sendable {
    public let label: String
    public let until: Date
    public let when: String
}

/// The five times a snooze is offered in, as the Desk sidebar offers them:
/// a port of `snoozePresets` in `desk/ui/src/sidebar/park.logic.ts`, so the
/// two stay one rule. Change one, change both.
public enum SnoozePresets {
    private static let hour: TimeInterval = 3600
    /// How far out the furthest relative preset reaches.
    private static let reach: TimeInterval = 3 * hour
    private static let evening = 18
    private static let morning = 9

    public static func at(_ now: Date, calendar: Calendar = .current) -> [SnoozePreset] {
        let tonight = at(now, days: 0, hour: evening, calendar)
        // Six tonight rolls to tomorrow three hours early, the width of
        // "In 3 hours", so no two lines name one instant.
        let thisEvening = tonight > now.addingTimeInterval(reach) ? tonight : at(now, days: 1, hour: evening, calendar)
        return [
            preset("In an hour", now.addingTimeInterval(hour), now, calendar),
            preset("In 3 hours", now.addingTimeInterval(reach), now, calendar),
            preset("This evening", thisEvening, now, calendar),
            preset("Tomorrow morning", at(now, days: 1, hour: morning, calendar), now, calendar),
            preset("Next week", at(now, days: untilMonday(now, calendar), hour: morning, calendar), now, calendar),
        ]
    }

    private static func preset(_ label: String, _ until: Date, _ now: Date, _ calendar: Calendar) -> SnoozePreset {
        SnoozePreset(label: label, until: until, when: wakeClock(until, now, calendar))
    }

    /// The time alone today, the weekday before it within the week, and the
    /// date from seven days out, so a Sunday's two Mondays read apart.
    private static func wakeClock(_ when: Date, _ now: Date, _ calendar: Calendar) -> String {
        let days = calendar.dateComponents([.day], from: calendar.startOfDay(for: now), to: calendar.startOfDay(for: when)).day ?? 0
        let format = days == 0 ? "HH:mm" : days < 7 ? "EEE HH:mm" : "EEE d MMM HH:mm"
        let formatter = DateFormatter()
        formatter.calendar = calendar
        formatter.timeZone = calendar.timeZone
        formatter.locale = Locale(identifier: "en_GB")
        formatter.dateFormat = format
        return formatter.string(from: when)
    }

    /// A local wall-clock hour, `days` from today.
    private static func at(_ now: Date, days: Int, hour: Int, _ calendar: Calendar) -> Date {
        let day = calendar.date(byAdding: .day, value: days, to: calendar.startOfDay(for: now)) ?? now
        return calendar.date(bySettingHour: hour, minute: 0, second: 0, of: day) ?? day
    }

    /// Days to the Monday that starts next week: never nought (a Monday's is
    /// the one after) and never one (a Sunday's tomorrow is already on offer).
    private static func untilMonday(_ now: Date, _ calendar: Calendar) -> Int {
        let weekday = calendar.component(.weekday, from: now) - 1 // Sunday is 0, as in JS
        let days = (8 - weekday) % 7
        if days == 0 { return 7 }
        if days == 1 { return 8 }
        return days
    }
}
