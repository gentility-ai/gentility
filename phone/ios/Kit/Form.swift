import Foundation

/// A form's answers as the person fills them in, and the content they make.
/// Held to the rules the Mac holds them to that can be checked here, in its
/// words (`harness::elicitation`); the Mac's `invalid_answer` covers the rest
/// (pattern, format).
public struct FormDraft: Equatable, Sendable {
    public let fields: [FormField]
    /// What was typed into text, number and free-text fields.
    public var texts: [String: String] = [:]
    public var flags: [String: Bool] = [:]
    /// Each choice field's picked option value.
    public var picks: [String: String] = [:]

    static let required = "Required."
    /// The largest whole number an f64 carries exactly, as the core bounds it.
    static let maxExactInteger = 9_007_199_254_740_991.0

    public init(fields: [FormField]) {
        self.fields = fields
        for field in fields {
            switch field.kind {
            case .text(_, _, _, _, let value?): texts[field.name] = value
            case .number(_, _, _, let value?): texts[field.name] = Self.shown(value)
            case .boolean(let value): flags[field.name] = value ?? false
            case .choice(_, let value?): picks[field.name] = value
            default: break
            }
        }
    }

    /// False while a field is one this app does not draw (pick-many).
    public var answerableHere: Bool {
        !fields.contains { if case .multiChoice = $0.kind { true } else { false } }
    }

    /// Each field that does not fit yet, by name, in the core's words.
    public func problems() -> [String: String] {
        var problems: [String: String] = [:]
        for field in fields {
            if let problem = problem(field) { problems[field.name] = problem }
        }
        return problems
    }

    /// The answer's `content`: each field with a value, by name, a number as
    /// a JSON number. Nil while any field does not fit.
    public func content() -> JSONValue? {
        guard problems().isEmpty else { return nil }
        var content: [String: JSONValue] = [:]
        for field in fields {
            if let value = value(field) { content[field.name] = value }
        }
        return .object(content)
    }

    private func typed(_ field: FormField) -> String {
        texts[field.name] ?? ""
    }

    private func value(_ field: FormField) -> JSONValue? {
        switch field.kind {
        case .text, .unknown:
            let text = typed(field)
            return text.isEmpty ? nil : .string(text)
        case .number:
            return Double(typed(field).trimmingCharacters(in: .whitespaces)).map(JSONValue.number)
        case .boolean(let value):
            return .bool(flags[field.name] ?? value ?? false)
        case .choice:
            return picks[field.name].map(JSONValue.string)
        case .multiChoice:
            return nil
        }
    }

    /// An empty string is nothing: required, or left out.
    private func problem(_ field: FormField) -> String? {
        switch field.kind {
        case .text(_, let min, let max, _, _):
            let text = typed(field)
            if text.isEmpty { return field.required ? Self.required : nil }
            let length = text.unicodeScalars.count
            if let min, length < min { return "At least \(min) characters." }
            if let max, length > max { return "At most \(max) characters." }
            return nil
        case .unknown:
            return typed(field).isEmpty && field.required ? Self.required : nil
        case .number(let integer, let min, let max, _):
            let text = typed(field).trimmingCharacters(in: .whitespaces)
            if text.isEmpty { return field.required ? Self.required : nil }
            guard let number = Double(text), number.isFinite else { return "Not a number." }
            if integer, number.rounded() != number { return "A whole number." }
            if integer, abs(number) > Self.maxExactInteger { return "Too large to carry exactly." }
            if let min, number < min { return "At least \(Self.shown(min))." }
            if let max, number > max { return "At most \(Self.shown(max))." }
            return nil
        case .boolean, .multiChoice:
            return nil
        case .choice:
            return picks[field.name] == nil && field.required ? Self.required : nil
        }
    }

    /// A whole number without its `.0`, as the core says it.
    static func shown(_ number: Double) -> String {
        number.rounded() == number && abs(number) < 1e15 ? String(Int(number)) : String(number)
    }
}
