import Foundation

/// One item of a list.
public struct MarkdownListItem: Equatable, Sendable {
    /// "•", or "1." for a numbered item.
    public let marker: String
    /// 0 at the margin; 1 for every deeper item (one nesting level is shown).
    public let level: Int
    /// Inline Markdown; a task item leads with "☐ " or "☑ ".
    public let text: String

    public init(marker: String, level: Int, text: String) {
        self.marker = marker
        self.level = level
        self.text = text
    }
}

/// The blocks of an agent's Markdown the phone draws: a deliberate subset.
public enum MarkdownBlock: Equatable, Sendable {
    /// Inline Markdown, its line breaks kept.
    case paragraph(String)
    /// Level 1 to 3; deeper headings draw as 3.
    case heading(level: Int, text: String)
    case list([MarkdownListItem])
    case quote(String)
    /// `closed` is false for a fence still being written: it runs to the end.
    case code(language: String?, text: String, closed: Bool)
    case rule
    /// A paragraph that is only an image. `alt` is its description; `title`
    /// its caption.
    case image(alt: String, src: String, title: String?)
    /// A table, drawn as its source in a code block: no grid on a phone.
    case table(String)
}

public enum Markdown {
    /// The blocks of `text`, line by line. HTML is shown as text; footnotes
    /// and math are not read.
    public static func blocks(_ text: String) -> [MarkdownBlock] {
        let lines = text.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n")
        var blocks: [MarkdownBlock] = []
        var paragraph: [String] = []
        var quote: [String] = []
        var items: [MarkdownListItem] = []
        var at = 0

        func flush() {
            if !paragraph.isEmpty { blocks.append(.paragraph(paragraph.joined(separator: "\n"))) }
            if !quote.isEmpty { blocks.append(.quote(quote.joined(separator: "\n"))) }
            if !items.isEmpty { blocks.append(.list(items)) }
            paragraph = []
            quote = []
            items = []
        }

        while at < lines.count {
            let line = lines[at]
            let trimmed = line.trimmingCharacters(in: .whitespaces)

            if let fence = fence(trimmed) {
                flush()
                var body: [String] = []
                var closed = false
                at += 1
                while at < lines.count {
                    let inner = lines[at].trimmingCharacters(in: .whitespaces)
                    if inner.hasPrefix(fence.marker), inner.allSatisfy({ $0 == fence.marker.first }) {
                        closed = true
                        break
                    }
                    body.append(lines[at])
                    at += 1
                }
                blocks.append(.code(language: fence.language, text: body.joined(separator: "\n"), closed: closed))
                at += 1
                continue
            }

            if trimmed.isEmpty {
                flush()
                at += 1
                continue
            }

            if let heading = heading(trimmed) {
                flush()
                blocks.append(heading)
                at += 1
                continue
            }

            if isRule(trimmed) {
                flush()
                blocks.append(.rule)
                at += 1
                continue
            }

            if trimmed.hasPrefix("|"), at + 1 < lines.count, isTableSeparator(lines[at + 1]) {
                flush()
                var rows: [String] = []
                while at < lines.count, lines[at].trimmingCharacters(in: .whitespaces).hasPrefix("|") {
                    rows.append(lines[at].trimmingCharacters(in: .whitespaces))
                    at += 1
                }
                blocks.append(.table(rows.joined(separator: "\n")))
                continue
            }

            if trimmed.hasPrefix(">") {
                if !paragraph.isEmpty || !items.isEmpty { flush() }
                var inner = trimmed.dropFirst()
                if inner.hasPrefix(" ") { inner = inner.dropFirst() }
                quote.append(String(inner))
                at += 1
                continue
            }

            if let item = listItem(line) {
                if !paragraph.isEmpty || !quote.isEmpty { flush() }
                items.append(item)
                at += 1
                continue
            }

            if let image = image(trimmed) {
                flush()
                blocks.append(image)
                at += 1
                continue
            }

            if !items.isEmpty, line.hasPrefix("  ") || line.hasPrefix("\t") {
                // A list item's continuation.
                let last = items.removeLast()
                items.append(MarkdownListItem(marker: last.marker, level: last.level, text: last.text + " " + trimmed))
                at += 1
                continue
            }
            if !quote.isEmpty || !items.isEmpty { flush() }
            paragraph.append(trimmed)
            at += 1
        }
        flush()
        return blocks
    }

    /// Inline Markdown: bold, italic, code, strikethrough and links; what
    /// does not parse is drawn as written.
    public static func inline(_ text: String) -> AttributedString {
        let options = AttributedString.MarkdownParsingOptions(
            allowsExtendedAttributes: false,
            interpretedSyntax: .inlineOnlyPreservingWhitespace,
            failurePolicy: .returnPartiallyParsedIfPossible
        )
        return (try? AttributedString(markdown: text, options: options)) ?? AttributedString(text)
    }

    // MARK: lines

    private static func fence(_ line: String) -> (marker: String, language: String?)? {
        for marker in ["```", "~~~"] where line.hasPrefix(marker) {
            let rest = line.drop { $0 == marker.first }.trimmingCharacters(in: .whitespaces)
            let opening = String(line.prefix { $0 == marker.first })
            return (opening, rest.isEmpty ? nil : String(rest.split(separator: " ").first ?? ""))
        }
        return nil
    }

    private static func heading(_ line: String) -> MarkdownBlock? {
        let hashes = line.prefix { $0 == "#" }.count
        guard (1...6).contains(hashes) else { return nil }
        let rest = line.dropFirst(hashes)
        guard rest.isEmpty || rest.hasPrefix(" ") else { return nil }
        var text = rest.trimmingCharacters(in: .whitespaces)
        while text.hasSuffix("#") { text.removeLast() }
        return .heading(level: min(hashes, 3), text: text.trimmingCharacters(in: .whitespaces))
    }

    private static func isRule(_ line: String) -> Bool {
        let marks = line.filter { $0 != " " }
        guard marks.count >= 3, let first = marks.first, "-*_".contains(first) else { return false }
        return marks.allSatisfy { $0 == first }
    }

    private static func isTableSeparator(_ line: String) -> Bool {
        let trimmed = line.trimmingCharacters(in: .whitespaces)
        return trimmed.hasPrefix("|") && trimmed.contains("-") && trimmed.allSatisfy { "|-: ".contains($0) }
    }

    private static func listItem(_ line: String) -> MarkdownListItem? {
        let indent = line.prefix { $0 == " " || $0 == "\t" }.reduce(0) { $0 + ($1 == "\t" ? 4 : 1) }
        let body = line.drop { $0 == " " || $0 == "\t" }
        let marker: String
        var rest: Substring
        if let first = body.first, "-*+".contains(first), body.dropFirst().hasPrefix(" ") {
            marker = "•"
            rest = body.dropFirst(2)
        } else {
            let digits = body.prefix { $0.isNumber }
            guard (1...9).contains(digits.count) else { return nil }
            let after = body.dropFirst(digits.count)
            guard let dot = after.first, dot == "." || dot == ")", after.dropFirst().hasPrefix(" ") else { return nil }
            marker = "\(digits)."
            rest = after.dropFirst(2)
        }
        var text = rest.trimmingCharacters(in: .whitespaces)
        if text.hasPrefix("[ ] ") {
            text = "☐ " + text.dropFirst(4)
        } else if text.lowercased().hasPrefix("[x] ") {
            text = "☑ " + text.dropFirst(4)
        }
        return MarkdownListItem(marker: marker, level: indent >= 2 ? 1 : 0, text: text)
    }

    /// `![alt](src "title")`, alone on its line.
    private static func image(_ line: String) -> MarkdownBlock? {
        guard line.hasPrefix("!["), line.hasSuffix(")"),
              let close = line.range(of: "](") else { return nil }
        let alt = String(line[line.index(line.startIndex, offsetBy: 2)..<close.lowerBound])
        guard !alt.contains("]") else { return nil }
        var inner = line[close.upperBound..<line.index(before: line.endIndex)].trimmingCharacters(in: .whitespaces)
        var title: String?
        if inner.hasSuffix("\""), let open = inner.dropLast().lastIndex(of: "\""), open > inner.startIndex,
           inner[inner.index(before: open)] == " " {
            title = String(inner[inner.index(after: open)..<inner.index(before: inner.endIndex)])
            inner = inner[..<open].trimmingCharacters(in: .whitespaces)
        }
        if inner.hasPrefix("<"), inner.hasSuffix(">") { inner = String(inner.dropFirst().dropLast()) }
        guard !inner.isEmpty, !inner.contains(" ") || line.contains("](<") else { return nil }
        return .image(alt: alt, src: inner, title: title)
    }
}
