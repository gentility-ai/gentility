import Foundation

public enum Line: Equatable, Sendable, Identifiable {
    /// The person's words, and what they attached (the item's `attachments`).
    case you(id: String, text: String, attachments: [AttachmentRef] = [])
    case agent(id: String, text: String)
    /// One line per tool call: a mark for how it went, then its title. Its id
    /// is the provider's call id, which the live moment and the transcript
    /// item both carry, so the two are one line.
    case tool(id: String, text: String)
    /// A compaction of the agent's context, in the window's words:
    /// `Context compacted 213K → 14.6K tokens`.
    case compaction(id: String, text: String)

    public var id: String {
        switch self {
        case .you(let id, _, _), .agent(let id, _), .tool(let id, _), .compaction(let id, _): return id
        }
    }
}

public struct Turn: Equatable, Sendable, Identifiable {
    public let number: Int
    public var lines: [Line]
    /// How the turn ended, where it did not end as turns do: nil for one
    /// still running, one the agent finished, and one whose end was not heard.
    public var end: TurnEnd?
    public var id: Int { number }

    public init(number: Int, lines: [Line], end: TurnEnd? = nil) {
        self.number = number
        self.lines = lines
        self.end = end
    }

    /// The end drawn under the turn: none where a compaction's own line has
    /// already said it failed. A compaction that fails ends its turn with the
    /// core's sentence `compaction failed: …`, and the line reads "Compaction
    /// failed" above it; the window draws such a turn as its line alone.
    public var drawnEnd: TurnEnd? {
        guard let end, end.outcome.hasPrefix("compaction failed") else { return end }
        let compacted = lines.contains { if case .compaction = $0 { return true } else { return false } }
        return compacted ? nil : end
    }
}

/// A turn that was stopped, was cut short, or failed: the core's word for
/// how it ended (`Said.turnEnded`'s `outcome`, a `turn_ended` row's
/// `reason`, a row's `last_outcome`), and what more it said.
public struct TurnEnd: Equatable, Sendable {
    public enum Tone: Equatable, Sendable {
        /// Somebody stopped it: nothing went wrong.
        case stopped
        /// A limit or a refusal: the session goes on with another prompt.
        case limited
        case failed
    }

    public let outcome: String
    public let detail: String?

    /// Nil for `end_turn`, and for no word at all: a turn that ended as
    /// turns do says nothing under it.
    public init?(outcome: String?, detail: String? = nil) {
        guard let outcome, !outcome.isEmpty, outcome != "end_turn" else { return nil }
        self.outcome = outcome
        self.detail = detail.flatMap { $0.isEmpty ? nil : $0 }
    }

    /// The window's own words for a stop (`isStopped` in
    /// desk/ui/src/session/timeline.logic.ts): `cancelled`, or the stop's own
    /// sentence, which always begins "stopped ".
    var isStopped: Bool { outcome == "cancelled" || outcome.hasPrefix("stopped ") }

    public var tone: Tone {
        if isStopped { return .stopped }
        switch outcome {
        case "limited", "max_tokens", "refusal": return .limited
        default: return .failed
        }
    }

    /// The line under the turn. A stop with words of its own keeps them
    /// ("Stopped from outside by Morgan"); the person's own Stop says no more
    /// than "Stopped".
    public var words: String {
        switch outcome {
        case "cancelled", "stopped by the person": return "Stopped"
        case "limited", "max_tokens": return "Limited"
        case "refusal": return "Refused"
        default: return isStopped ? outcome.prefix(1).uppercased() + outcome.dropFirst() : "Failed"
        }
    }

    /// What more is drawn under the line: the limit's own words, or the
    /// failure's sentence, which is the error.
    public var more: String? {
        switch tone {
        case .stopped: return nil
        case .limited:
            if outcome == "max_tokens" { return detail ?? "This turn reached the model’s output limit. Send a message to continue." }
            return detail
        case .failed: return detail.map { "\(outcome)\n\($0)" } ?? outcome
        }
    }
}

/// Where the working caret is drawn: at the end of the agent's last words,
/// or alone after any other last line. Nowhere while a compaction runs: its
/// line is the live sign, and the agent is not about to write.
public enum Caret: Equatable, Sendable {
    case none
    /// At the end of this line, the agent's.
    case inLine(String)
    /// On its own after the last line.
    case alone

    public init(turns: [Turn], working: Bool, compacting: Bool) {
        guard working, !compacting else {
            self = .none
            return
        }
        if case .agent(let id, _)? = turns.last?.lines.last { self = .inLine(id) } else { self = .alone }
    }
}

public enum Refetch: Equatable, Sendable {
    /// The newest turns again: something was missed.
    case last
    /// This turn and those after it.
    case from(Int)
}

/// A session's turns as the phone draws them, from the transcript and the
/// moments said since. Thoughts and sub-agents' items are not drawn.
public struct TranscriptFold: Equatable, Sendable {
    public private(set) var turns: [Turn] = []
    /// Tool calls' titles by the provider's call id, sub-agents' too: what
    /// an approval about the call draws under its heading.
    private var titles: [String: String] = [:]
    /// Lines' ordinals as read, by line id: where `update` puts a new row.
    /// A line heard live and not read yet has none.
    private var ordinals: [String: Int] = [:]
    /// The compactions heard or read still under way, by item id.
    private var running: Set<String> = []

    public init() {}

    public var lastTurn: Int? { turns.last?.number }

    /// The last turn has a compaction under way: what the strip says while
    /// it runs.
    public var compacting: Bool { turns.last?.lines.contains { running.contains($0.id) } ?? false }

    /// A tool call's title, by the provider's call id; nil for a call with
    /// none, whose line is drawn by the tool's name.
    public func toolTitle(callID: String) -> String? { titles[callID] }

    /// A transcript answer. One that covers from a turn replaces that turn and
    /// those after (a whole transcript, with no `from_turn`, covers them all);
    /// a live line the read does not hold yet stays at the end of its turn, so
    /// a re-read never takes back words already on the screen, nor an end
    /// heard live that the read does not say. A page marked `reset` (the
    /// store was rebuilt) replaces every turn.
    public mutating func replace(with transcript: Transcript) {
        for case let call? in transcript.items.map(\.toolCall) {
            if let id = call.providerToolCallID, let title = call.title, !title.isEmpty { titles[id] = title }
        }
        for item in transcript.items { if let line = Self.line(item) { ordinals[line.id] = item.ordinal } }
        for item in transcript.items { if let compaction = item.compaction { heard(item.id, compaction) } }
        let read = Self.turns(from: transcript.items)
        guard !turns.isEmpty, !transcript.reset else {
            turns = read
            return
        }
        let from = transcript.fromTurn ?? Int.min
        let kept = turns.filter { $0.number < from }
        var fresh = read.filter { $0.number >= from }
        for old in turns where old.number >= from {
            guard let at = fresh.firstIndex(where: { $0.number == old.number }) else {
                fresh.append(old)
                continue
            }
            let held = Set(fresh[at].lines.map(\.id))
            fresh[at].lines += old.lines.filter { !held.contains($0.id) }
            fresh[at].end = fresh[at].end ?? old.end
        }
        turns = kept + fresh.sorted { $0.number < $1.number }
    }

    /// B5: the rows changed since the cursor, whatever their turn. A row whose
    /// line is on screen is redrawn in place; a new row goes after the rows
    /// read before it in its turn and before any line heard live and not read
    /// yet (a turn's prompt, never said live, comes before the words that
    /// streamed); a turn newer than the last is appended in order; a row for
    /// a turn older than the first on screen was paged out and is dropped.
    /// On an empty fold every row draws. Tool titles are kept for every call
    /// heard, as `replace` keeps them.
    public mutating func update(with transcript: Transcript) {
        for case let call? in transcript.items.map(\.toolCall) {
            if let id = call.providerToolCallID, let title = call.title, !title.isEmpty { titles[id] = title }
        }
        for item in transcript.items { if let compaction = item.compaction { heard(item.id, compaction) } }
        let first = turns.first?.number ?? Int.min
        for item in transcript.items.sorted(by: { $0.ordinal < $1.ordinal }) {
            let number = item.turn ?? 0
            guard number >= first else { continue }
            if let end = Self.end(item) {
                ended(number, end)
                continue
            }
            guard let line = Self.line(item) else { continue }
            if let at = turns.firstIndex(where: { $0.number == number }) {
                if let index = turns[at].lines.firstIndex(where: { $0.id == line.id }) {
                    turns[at].lines[index] = line
                } else {
                    let index = turns[at].lines.firstIndex { ordinals[$0.id].map { $0 > item.ordinal } ?? true }
                    turns[at].lines.insert(line, at: index ?? turns[at].lines.endIndex)
                }
            } else {
                turns.append(Turn(number: number, lines: [line]))
                turns.sort { $0.number < $1.number }
            }
            ordinals[line.id] = item.ordinal
        }
    }

    /// One moment, said live. Answers what to re-read, if anything.
    public mutating func apply(_ said: Said, turn: Int) -> Refetch? {
        var refetch: Refetch?
        if !turns.contains(where: { $0.number == turn }) {
            if let last = lastTurn, turn < last { return nil }
            // The person's words open a turn and are not said live: read them,
            // and any turn between the last on screen and this one, missed
            // whole.
            refetch = .from(lastTurn.map { $0 + 1 } ?? turn)
            turns.append(Turn(number: turn, lines: []))
        }
        guard let at = turns.firstIndex(where: { $0.number == turn }) else { return refetch }
        switch said {
        case .text(let id, let tail, let whole, let parent):
            guard parent == nil else { break }
            if let index = turns[at].lines.firstIndex(where: { $0.id == id }), case .agent(_, let text) = turns[at].lines[index] {
                turns[at].lines[index] = .agent(id: id, text: whole ?? text + tail)
            } else {
                turns[at].lines.append(.agent(id: id, text: whole ?? tail))
            }
        case .toolCall(let id, let name, let title, let status, _, let parent):
            if let title, !title.isEmpty { titles[id] = title }
            guard parent == nil else { break }
            let line = Line.tool(id: id, text: Self.mark(status) + " " + (title ?? name))
            if let index = turns[at].lines.firstIndex(where: { $0.id == id }) {
                turns[at].lines[index] = line
            } else {
                turns[at].lines.append(line)
            }
        case .compaction(let id, let compaction):
            heard(id, compaction)
            let line = Line.compaction(id: id, text: Self.words(compaction))
            if let index = turns[at].lines.firstIndex(where: { $0.id == id }) {
                turns[at].lines[index] = line
            } else {
                turns[at].lines.append(line)
            }
        case .turnEnded(let outcome, let detail):
            turns[at].end = TurnEnd(outcome: outcome, detail: detail)
            refetch = .from(turn)
        case .heard, .handoff:
            refetch = .from(turn)
        case .lagged:
            refetch = .last
        case .continued, .other, .asked, .answered, .unanswered, .elicited, .elicitationDecided:
            // A question is a card, not a line.
            break
        }
        return refetch
    }

    /// The session's row says how its last turn ended (`last_outcome`), and
    /// says it of the last turn only while none runs: the end of the last
    /// turn on screen, where nothing heard or read has said one. A turn that
    /// ended as turns do clears nothing.
    public mutating func lastEnded(_ outcome: String?) {
        guard let last = lastTurn, let end = TurnEnd(outcome: outcome) else { return }
        guard let at = turns.firstIndex(where: { $0.number == last }), turns[at].end == nil else { return }
        turns[at].end = end
    }

    /// A turn's end, read: onto its turn, or a turn of its own where none
    /// on screen has the number.
    private mutating func ended(_ number: Int, _ end: TurnEnd) {
        if let at = turns.firstIndex(where: { $0.number == number }) {
            turns[at].end = end
        } else {
            turns.append(Turn(number: number, lines: [], end: end))
            turns.sort { $0.number < $1.number }
        }
    }

    static func turns(from items: [TranscriptItem]) -> [Turn] {
        var byTurn: [Int: [Line]] = [:]
        var ends: [Int: TurnEnd] = [:]
        for item in items.sorted(by: { $0.ordinal < $1.ordinal }) {
            let number = item.turn ?? 0
            if let end = end(item) {
                ends[number] = end
                byTurn[number, default: []] += []
                continue
            }
            guard let line = line(item) else { continue }
            byTurn[number, default: []].append(line)
        }
        return byTurn.keys.sorted().map { Turn(number: $0, lines: byTurn[$0] ?? [], end: ends[$0]) }
    }

    /// A `turn_ended` row: the core writes one for every turn's end, its
    /// `reason` and `detail` as the fact that ended it carried them (no reason
    /// for a turn that ended as turns do, which draws nothing). A
    /// `turn_ended_late` is the same row filed after every row, for an end
    /// from a log older than its place; both go onto the turn they name.
    static func end(_ item: TranscriptItem) -> TurnEnd? {
        guard item.kind == "turn_ended" || item.kind == "turn_ended_late", let content = item.content,
              case .object(let body)? = try? JSONValue.parse(Data(content.utf8)) else { return nil }
        return TurnEnd(outcome: body["reason"]?.string, detail: body["detail"]?.string)
    }

    static func line(_ item: TranscriptItem) -> Line? {
        guard item.parentCallID == nil else { return nil }
        switch (item.kind, item.role) {
        case ("message", "user"):
            return .you(id: item.id, text: item.content ?? "", attachments: item.attachments ?? [])
        case ("message", "assistant"), ("message", nil):
            return .agent(id: item.id, text: item.content ?? "")
        case ("tool_call", _):
            let call = item.toolCall
            guard call?.parentCallID == nil else { return nil }
            let label = call?.title ?? call?.name ?? "A tool"
            return .tool(id: call?.providerToolCallID ?? item.id, text: mark(call?.status) + " " + label)
        case ("compaction", _):
            return item.compaction.map { .compaction(id: item.id, text: words($0)) }
        default:
            return nil
        }
    }

    /// A compaction's latest status: under way, or not any more.
    private mutating func heard(_ id: String, _ compaction: Compaction) {
        if Self.finished(compaction) { running.remove(id) } else { running.insert(id) }
    }

    /// Done, stopped or failed. A status this build has not heard of is
    /// under way, as its words say.
    static func finished(_ compaction: Compaction) -> Bool {
        ["completed", "cancelled", "failed"].contains(compaction.status)
    }

    /// A compaction in the window's words (`compactionWords` in its
    /// timeline): under way, done with the sizes measured, stopped, or failed.
    /// A status this build has not heard of reads as under way, as there.
    static func words(_ compaction: Compaction) -> String {
        switch compaction.status {
        case "completed":
            switch (compaction.before, compaction.after) {
            case let (before?, after?): return "Context compacted \(tokens(before)) → \(tokens(after)) tokens"
            case let (before?, nil): return "Context compacted from \(tokens(before)) tokens"
            case let (nil, after?): return "Context compacted to \(tokens(after)) tokens"
            case (nil, nil): return "Context compacted"
            }
        case "cancelled": return "Stopped compacting"
        case "failed": return "Compaction failed"
        default: return "Compacting context…"
        }
    }

    /// A token count as the compaction line says it (`compactTokens` in the
    /// window): exact under a thousand, the tenth kept under a hundred
    /// thousand and dropped where it is nought, whole above, then millions.
    static func tokens(_ count: Int) -> String {
        let n = max(0, count)
        if n < 1000 { return "\(n)" }
        let tenths = (Double(n) / 100).rounded() / 10
        if tenths < 100 { return "\(tenth(tenths))K" }
        let thousands = (Double(n) / 1000).rounded()
        if thousands < 1000 { return "\(Int(thousands))K" }
        return "\(tenth((Double(n) / 100_000).rounded() / 10))M"
    }

    private static func tenth(_ value: Double) -> String {
        value == value.rounded() ? "\(Int(value))" : String(format: "%.1f", value)
    }

    /// How a call went, drawn before its title ("✓ Write …").
    static func mark(_ status: String?) -> String {
        switch status {
        case "executed": return "✓"
        case "failed": return "✗"
        case "cancelled": return "⊘"
        // Refused at its question and not run. A circled mark, as a stopped
        // call's is, because the window draws the two alike, in the line's own
        // colour and not failure's: saying no is not the call going wrong. A
        // minus where a stop has a slash, so the two still read apart.
        case "declined": return "⊖"
        case "pending", "running", nil: return "…"
        // A word this build has not heard of says neither going nor finished.
        default: return "?"
        }
    }
}
