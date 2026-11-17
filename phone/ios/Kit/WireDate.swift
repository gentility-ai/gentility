import Foundation

/// The server's instants: ISO 8601 UTC with microseconds
/// (`2026-01-01T00:00:00.000000Z`), with milliseconds, or with none.
public enum WireDate {
    private static let fractional: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter
    }()

    private static let whole: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter
    }()

    public static func parse(_ text: String) -> Date? {
        longFraction(text) ?? fractional.date(from: text) ?? whole.date(from: text)
    }

    /// An instant as the server reads one, with milliseconds.
    public static func say(_ date: Date) -> String {
        fractional.string(from: date)
    }

    /// A fraction longer than three digits. The formatter reads one, but
    /// keeps only its milliseconds, so it is cut to three digits here and
    /// the rest added back. Nil for any other text.
    private static func longFraction(_ text: String) -> Date? {
        guard let dot = text.firstIndex(of: "."),
              let zone = text[dot...].firstIndex(where: { $0 == "Z" || $0 == "+" || $0 == "-" }) else {
            return nil
        }
        let digits = text[text.index(after: dot)..<zone]
        guard digits.count > 3, digits.allSatisfy(\.isASCII), digits.allSatisfy(\.isNumber) else { return nil }
        let cut = String(text[..<dot]) + "." + digits.prefix(3) + text[zone...]
        guard let date = fractional.date(from: cut) else { return nil }
        return date.addingTimeInterval(Double("0.000" + digits.dropFirst(3)) ?? 0)
    }
}
