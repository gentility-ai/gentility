import Foundation

/// One tool call of a run: how it went, and its title.
public struct ToolStep: Equatable, Sendable, Identifiable {
    public enum State: Equatable, Sendable {
        case running, done, failed
        /// Stopped, declined, or a word this build has not heard of.
        case other
    }

    public let id: String
    public let mark: String
    public let title: String

    public var state: State {
        switch mark {
        case "…": .running
        case "✓": .done
        case "✗": .failed
        default: .other
        }
    }
}

/// The tool calls an agent made back to back, drawn as one row that opens:
/// a turn's forty reads and edits are not forty lines of the conversation.
public struct ToolRun: Equatable, Sendable, Identifiable {
    public let steps: [ToolStep]
    /// The first call's id: a run keeps it as calls join its end, so a run
    /// the person opened stays open while the agent works.
    public var id: String { "run-" + (steps.first?.id ?? "") }

    public var failed: Int { steps.filter { $0.state == .failed }.count }
    /// The call going now, the last of them where there are several.
    public var running: ToolStep? { steps.last { $0.state == .running } }
    /// Whether the row draws a spinner. A call the phone last heard running
    /// is only known to be running while the conversation is open: through
    /// a redial or a drop the strip or the banner speaks, and the row waits.
    public func spins(live: Bool) -> Bool { live && running != nil }

    /// The row's words: the call going now, the one call there is, or how
    /// many there were and how many of them failed.
    public var headline: String {
        if let running { return running.title }
        if steps.count == 1, let only = steps.first { return only.title }
        let count = "\(steps.count) tool calls"
        return failed == 0 ? count : "\(count), \(failed) failed"
    }
}

/// What a turn draws, top to bottom: its lines, with each run of tool calls
/// as one row.
public enum TranscriptRow: Equatable, Sendable, Identifiable {
    case line(Line)
    case tools(ToolRun)

    public var id: String {
        switch self {
        case .line(let line): line.id
        case .tools(let run): run.id
        }
    }

    public static func rows(_ lines: [Line]) -> [TranscriptRow] {
        var rows: [TranscriptRow] = []
        var steps: [ToolStep] = []
        func flush() {
            if !steps.isEmpty { rows.append(.tools(ToolRun(steps: steps))) }
            steps = []
        }
        for line in lines {
            if case .tool(let id, let text) = line {
                steps.append(step(id: id, text: text))
            } else {
                flush()
                rows.append(.line(line))
            }
        }
        flush()
        return rows
    }

    /// A tool line is its mark, a space, then its title (`Transcript.mark`).
    static func step(id: String, text: String) -> ToolStep {
        guard let space = text.firstIndex(of: " ") else { return ToolStep(id: id, mark: "", title: text) }
        return ToolStep(id: id, mark: String(text[..<space]), title: String(text[text.index(after: space)...]))
    }
}

/// A Markdown table's cells, from its source lines.
public struct MarkdownTable: Equatable, Sendable {
    public let header: [String]
    public let rows: [[String]]

    /// Nil where the source has no header row and separator: the caller
    /// draws it as it came.
    public init?(source: String) {
        let lines = source.split(separator: "\n").map(String.init)
        guard lines.count >= 2, Self.isSeparator(Self.cells(lines[1])) else { return nil }
        let header = Self.cells(lines[0])
        guard !header.isEmpty else { return nil }
        self.header = header
        // Every row as wide as the header: a short row is padded, a long one cut.
        self.rows = lines.dropFirst(2).map { line in
            let cells = Self.cells(line)
            return (0..<header.count).map { $0 < cells.count ? cells[$0] : "" }
        }
    }

    /// A row's cells: split on pipes a backslash does not escape, the outer
    /// pipes dropped.
    static func cells(_ line: String) -> [String] {
        var cells: [String] = []
        var cell = ""
        var escaped = false
        for character in line.trimmingCharacters(in: .whitespaces) {
            if escaped {
                cell.append(character)
                escaped = false
            } else if character == "\\" {
                escaped = true
                cell.append(character)
            } else if character == "|" {
                cells.append(cell)
                cell = ""
            } else {
                cell.append(character)
            }
        }
        cells.append(cell)
        if cells.first?.trimmingCharacters(in: .whitespaces).isEmpty == true { cells.removeFirst() }
        if cells.last?.trimmingCharacters(in: .whitespaces).isEmpty == true { cells.removeLast() }
        return cells.map { $0.trimmingCharacters(in: .whitespaces).replacingOccurrences(of: "\\|", with: "|") }
    }

    static func isSeparator(_ cells: [String]) -> Bool {
        !cells.isEmpty && cells.allSatisfy { cell in
            let dashes = cell.trimmingCharacters(in: CharacterSet(charactersIn: ":"))
            return !dashes.isEmpty && dashes.allSatisfy { $0 == "-" }
        }
    }
}
