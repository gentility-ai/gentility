import Foundation

/// A cron read in words, as Desk reads it, so the two never disagree about
/// one schedule. Anything it would have to guess at is nil.
public enum CronReading {
    static let days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]

    public static func describe(_ cron: String) -> String? {
        let fields = cron.split(whereSeparator: \.isWhitespace).map(String.init)
        guard fields.count == 5 else { return nil }
        let (minute, hour, day, month, weekday) = (fields[0], fields[1], fields[2], fields[3], fields[4])
        guard month == "*" else { return nil }

        let m = number(minute, 0, 59)
        let h = number(hour, 0, 23)
        let everyMinutes = step(minute, 60)
        let everyHours = step(hour, 24)
        let weekdays = weekday == "1-5"
        let oneDay = number(weekday, 0, 7)

        if day == "*", weekday == "*" {
            if hour == "*" {
                if minute == "*" { return "Every minute" }
                if let everyMinutes { return everyMinutes == 1 ? "Every minute" : "Every \(everyMinutes) minutes" }
                if m == 0 { return "Every hour" }
                if let m { return "Every hour at :\(pad(m))" }
                return nil
            }
            guard let m else { return nil }
            if let everyHours { return hours(everyHours, m) }
            if let h { return "Every day at \(pad(h)):\(pad(m))" }
            return nil
        }

        guard let m else { return nil }
        if day == "*", weekdays {
            if let everyHours { return "\(hours(everyHours, m)) on weekdays" }
            if let h { return "Weekdays at \(pad(h)):\(pad(m))" }
            return nil
        }
        guard let h else { return nil }
        if day == "*", let oneDay { return "Every \(days[oneDay % 7]) at \(pad(h)):\(pad(m))" }
        if let date = number(day, 1, 31), weekday == "*" { return "Monthly on day \(date) at \(pad(h)):\(pad(m))" }
        return nil
    }

    /// The schedule's line: the cron in words, or why not.
    public static func line(cron: String?, responder: Bool) -> String {
        let cron = (cron ?? "").trimmingCharacters(in: .whitespaces)
        if cron.isEmpty {
            return responder ? "No schedule — woken by its triggers" : "No schedule — runs only when started"
        }
        return describe(cron) ?? "A cron expression"
    }

    private static func hours(_ every: Int, _ minute: Int) -> String {
        let said = every == 1 ? "Every hour" : "Every \(every) hours"
        return minute == 0 ? said : "\(said) at :\(pad(minute))"
    }

    private static func pad(_ n: Int) -> String { n < 10 ? "0\(n)" : "\(n)" }

    private static func number(_ field: String, _ min: Int, _ max: Int) -> Int? {
        guard (1...2).contains(field.count), field.allSatisfy(\.isASCIIDigit), let n = Int(field), n >= min, n <= max else { return nil }
        return n
    }

    /// `*/N` for an N that divides `span`. A step that does not divide its
    /// field is not "every N".
    private static func step(_ field: String, _ span: Int) -> Int? {
        guard field.hasPrefix("*/") else { return nil }
        let digits = field.dropFirst(2)
        guard (1...2).contains(digits.count), digits.allSatisfy(\.isASCIIDigit), let n = Int(digits), n > 0, span % n == 0 else { return nil }
        return n
    }
}

private extension Character {
    var isASCIIDigit: Bool { isASCII && isNumber }
}

/// The colour a looper's state word is drawn in.
public enum LooperTone: Equatable, Sendable {
    case asking, working, faint, bad
}

/// One looper on the Loopers tab: its row from the server, whose it is, and
/// what its runs are doing.
public struct Looper: Identifiable, Hashable, Sendable {
    public let row: LooperRow
    public let organizationID: String
    /// A run is going.
    public var working: Bool
    /// A run has a request open.
    public var asking: Bool
    public var muted: Bool

    public var id: String { row.id }
    public var name: String { row.name }
    public var responder: Bool { row.responder ?? false }

    public init(row: LooperRow, organizationID: String, working: Bool = false, asking: Bool = false, muted: Bool = false) {
        self.row = row
        self.organizationID = organizationID
        self.working = working
        self.asking = asking
        self.muted = muted
    }

    public var scheduleLine: String { CronReading.line(cron: row.schedule.cron, responder: responder) }

    private var hasCron: Bool { !(row.schedule.cron ?? "").trimmingCharacters(in: .whitespaces).isEmpty }

    /// The one state the list says.
    public func state(now: Date) -> (text: String, tone: LooperTone) {
        if asking { return ("Asking", .asking) }
        if working { return ("Working", .working) }
        if !hasCron { return (responder ? "Listening" : "Manual", .faint) }
        if !row.schedule.enabled { return ("Schedule off", .faint) }
        guard let next = row.nextRunAt.flatMap(WireDate.parse) else { return ("Not known yet", .faint) }
        return ("Next run \(Self.until(next, now: now))", .faint)
    }

    /// When it next runs, for the Edit form: nil where no run is scheduled
    /// or the time is not known.
    public func nextRun(now: Date) -> String? {
        guard hasCron, row.schedule.enabled, let next = row.nextRunAt.flatMap(WireDate.parse) else { return nil }
        return Self.until(next, now: now)
    }

    /// `in 3 h`, `in 12 min`, `now`.
    static func until(_ date: Date, now: Date) -> String {
        let seconds = date.timeIntervalSince(now)
        if seconds < 60 { return "now" }
        let minutes = Int(seconds / 60)
        if minutes < 60 { return "in \(minutes) min" }
        let hours = minutes / 60
        if hours < 48 { return "in \(hours) h" }
        return "in \(hours / 24) d"
    }
}

/// A run's row on a looper's page, in Desk's words.
public struct RunFace: Equatable, Sendable {
    public let title: String
    public let woke: String
    /// Queued, Running, Asking, Failed or Stopped; a finished run says nothing.
    public let word: String?
    public let tone: LooperTone

    public init(_ run: LooperRunRow) {
        title = run.number.map { "Run #\($0)" } ?? run.title ?? "Run"
        woke = Self.wakeWords(run.wake)
        let asking = (run.requests ?? []).contains { $0.status == "open" }
        switch run.status {
        case _ where asking && Self.going(run.status): (word, tone) = ("Asking", .asking)
        case "queued", "pending": (word, tone) = ("Queued", .faint)
        case "running": (word, tone) = ("Running", .working)
        case "failed": (word, tone) = ("Failed", .bad)
        case "stopped", "cancelled": (word, tone) = ("Stopped", .faint)
        default: (word, tone) = (nil, .faint)
        }
    }

    public static func going(_ status: String) -> Bool { ["queued", "pending", "running", "waiting"].contains(status) }

    /// Why it woke, in Desk's words.
    public static func wakeWords(_ wake: LooperRunRow.Wake?) -> String {
        switch wake?.by {
        case nil: "Woke"
        case "scheduled": "Woken by its schedule"
        case "task": "Woken by a task"
        case "email": "Woken by an email"
        case "trigger": wake?.source.map { "Woken by \($0)" } ?? "Woken by a trigger"
        case "event": wake?.source.map { "Woken by \($0)" } ?? "Woken by an event"
        case "request": "Woken by an answer"
        case "manual": "Started by hand"
        default: "Woke"
        }
    }
}

/// Why the server refused, in the words a person reads.
public enum LooperRefusal {
    public static func sentence(_ error: Error) -> String {
        switch error as? LobbyError {
        case .refused("paused"): "This looper is paused. Resume it on the web to run it."
        case .refused("not_funded"): "Its organisation is out of funds. Add funds on the web to run it."
        case .refused("run_in_flight"): "A run is already going."
        case .refused("blank_message"): "Write a message first."
        case .refused("not_found"): "This looper is no longer yours to run."
        case .refused("not_editable"): "That setting is changed on the web."
        case .refused("invalid"): "The server did not take that. Check the schedule and try again."
        case .refused("answer_required"): "Write an answer first."
        case .refused("not_open"): "That request was already answered."
        case .unauthorized: "This phone is no longer paired."
        case .unreachable: "Can't reach the server. Try again."
        default: "That did not go through."
        }
    }
}

/// The sentences the looper pages say that no model holds.
public enum LooperCopy {
    public static let running = "A run is already going."
    public static let noRuns = "No runs yet. Run now starts one."
    public static let answerOnWeb = "Answer this one on the web."
    public static let notYours = "This run belongs to a looper that is not yours. It opens on the web."
    public static let opensOnWeb = "This run opens on the web."
    public static let tryAgain = "Try again"

    /// A run with nothing to show: still going, or finished without a word.
    public static func nothingSaid(going: Bool) -> String {
        going ? "Nothing said yet." : "This run said nothing."
    }

    /// A run's own failure, which the server sends as it has it.
    public static func runFailed(_ reason: String) -> String {
        "This run failed. Reason: \(reason)"
    }

    /// A fold of tool calls: one call's name, or how many and how many failed.
    public static func tools(names: [String], failed: Int) -> String {
        if names.count == 1 { return names[0] }
        return failed > 0 ? "\(names.count) tool calls, \(failed) failed" : "\(names.count) tool calls"
    }
}

/// The Loopers tab: every looper the person owns, across organisations.
@MainActor
public final class LoopersModel: ObservableObject {
    @Published public private(set) var loopers: [Looper] = []
    @Published public private(set) var loaded = false
    /// Why the last refresh failed, said under the title.
    @Published public private(set) var line: String?

    private let lobby: LooperLobby
    private let now: @Sendable () -> Date
    private(set) var personID: String?
    private var reading: Task<Void, Never>?

    public init(lobby: LooperLobby, now: @escaping @Sendable () -> Date = { Date() }) {
        self.lobby = lobby
        self.now = now
    }

    /// Asking first, then working, then by name. Stable, so a refresh that
    /// changes nothing moves nothing.
    static func ordered(_ loopers: [Looper]) -> [Looper] {
        func rank(_ looper: Looper) -> Int { looper.asking ? 0 : looper.working ? 1 : 2 }
        return loopers.sorted {
            (rank($0), $0.name.lowercased(), $0.id) < (rank($1), $1.name.lowercased(), $1.id)
        }
    }

    /// Reads the list. A call made while a read is going waits for that
    /// read, so whoever asked may trust the list when this returns.
    public func refresh() async {
        if let reading { await reading.value; return }
        let task = Task { await read() }
        reading = task
        await task.value
        reading = nil
    }

    private func read() async {
        do {
            let organizations = try await lobby.organizations()
            personID = organizations.person.id
            let muted = (try? await lobby.mutedLoopers(personID: organizations.person.id)) ?? []
            var found: [Looper] = []
            for organization in organizations.organizations {
                for row in try await lobby.loopers(organizationID: organization.id) {
                    // A looper with a run going or asking is known from its
                    // newest runs; one ask per looper, as Desk's pin makes.
                    let runs = (try? await lobby.runs(organizationID: organization.id, looperID: row.id)) ?? []
                    let live = runs.filter { RunFace.going($0.status) }
                    found.append(Looper(
                        row: row, organizationID: organization.id,
                        working: !live.isEmpty,
                        asking: live.contains { ($0.requests ?? []).contains { $0.status == "open" } },
                        muted: muted.contains(row.id)
                    ))
                }
            }
            loopers = Self.ordered(found)
            line = nil
        } catch {
            line = LooperRefusal.sentence(error)
        }
        loaded = true
    }

    /// The looper a thread row's `looper_id` names, when it is listed.
    public func looper(id: String) -> Looper? { loopers.first { $0.id == id } }

    func replace(_ looper: Looper) {
        guard let at = loopers.firstIndex(where: { $0.id == looper.id }) else { return }
        loopers[at] = looper
    }

    public func page(for looper: Looper) -> LooperPageModel {
        LooperPageModel(looper: looper, lobby: lobby, personID: personID) { [weak self] changed in self?.replace(changed) }
    }
}

/// One looper's page: its runs, and what a person does to it.
@MainActor
public final class LooperPageModel: ObservableObject {
    @Published public private(set) var looper: Looper
    @Published public private(set) var runs: [LooperRunRow] = []
    @Published public private(set) var loaded = false
    /// "Run started", "Muted on this phone": said once, then cleared.
    @Published public var notice: String?
    /// Why the last press failed; the next press clears it.
    @Published public private(set) var failure: String?
    /// Why the last read of its runs failed; a read that works clears it.
    @Published public private(set) var line: String?
    /// Why the last Save failed, said on the Edit form.
    @Published public private(set) var editFailure: String?
    @Published public private(set) var busy = false
    /// The run a press just started, for the page to open.
    @Published public var started: LooperRunRow?

    private let lobby: LooperLobby
    private let personID: String?
    private let changed: (Looper) -> Void

    public init(looper: Looper, lobby: LooperLobby, personID: String?, changed: @escaping (Looper) -> Void = { _ in }) {
        self.looper = looper
        self.lobby = lobby
        self.personID = personID
        self.changed = changed
    }

    /// The requests waiting on a person, across its runs that are going.
    public var open: [(run: LooperRunRow, request: LooperRequest)] {
        runs.flatMap { run in (run.requests ?? []).filter { $0.status == "open" }.map { (run, $0) } }
    }

    /// Why Run now cannot run, or nil when it can.
    public var runBlocked: String? {
        runs.contains { RunFace.going($0.status) } ? LooperCopy.running : nil
    }

    /// The Edit form opens with no failure of an earlier Save.
    public func editing() { editFailure = nil }

    public func refresh() async {
        do {
            runs = try await lobby.runs(organizationID: looper.organizationID, looperID: looper.id)
            var next = looper
            let live = runs.filter { RunFace.going($0.status) }
            next.working = !live.isEmpty
            next.asking = live.contains { ($0.requests ?? []).contains { $0.status == "open" } }
            set(next)
            line = nil
        } catch {
            line = LooperRefusal.sentence(error)
        }
        loaded = true
    }

    /// Run now, or a run that opens with `message`. True once a run started.
    @discardableResult
    public func run(message: String? = nil) async -> Bool {
        let message = message?.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !busy else { return false }
        failure = nil
        if let message, message.isEmpty { failure = "Write a message first."; return false }
        busy = true
        defer { busy = false }
        do {
            let id = try await lobby.start(organizationID: looper.organizationID, looperID: looper.id, message: message)
            notice = "Run started"
            await refresh()
            started = runs.first { $0.id == id }
            return true
        } catch {
            failure = LooperRefusal.sentence(error)
            return false
        }
    }

    public func setMuted(_ muted: Bool) async {
        failure = nil
        guard let personID else { failure = "That did not go through."; return }
        do {
            try await lobby.mute(personID: personID, looperID: looper.id, muted: muted)
            var next = looper
            next.muted = muted
            set(next)
            notice = muted ? "Muted on this phone" : "Notifications on"
        } catch {
            failure = LooperRefusal.sentence(error)
        }
    }

    /// Sends what `draft` changed. True once the server took it.
    public func save(_ draft: LooperDraft) async -> Bool {
        let changes = draft.changes(from: looper.row)
        editFailure = nil
        guard !changes.isEmpty else { return true }
        do {
            let row = try await lobby.update(organizationID: looper.organizationID, looperID: looper.id, changes: changes)
            var next = looper
            next = Looper(row: row, organizationID: next.organizationID, working: next.working, asking: next.asking, muted: next.muted)
            set(next)
            notice = "Saved"
            return true
        } catch {
            editFailure = LooperRefusal.sentence(error)
            return false
        }
    }

    public func runModel(for run: LooperRunRow) -> LooperRunModel {
        LooperRunModel(run: run, organizationID: looper.organizationID, lobby: lobby)
    }

    private func set(_ next: Looper) {
        looper = next
        changed(next)
    }
}

/// The Edit form: every editable setting as text or a flag, never nil.
public struct LooperDraft: Equatable, Sendable {
    public var name: String
    public var instructions: String
    public var cron: String
    public var timezone: String
    public var scheduleOn: Bool
    public var notify: String
    public var isPublic: Bool

    public init(_ row: LooperRow) {
        name = row.name
        instructions = row.systemPrompt ?? ""
        cron = row.schedule.cron ?? ""
        timezone = row.schedule.timezone ?? ""
        scheduleOn = row.schedule.enabled
        notify = row.notifyHandle ?? ""
        isPublic = row.visibility == "public"
    }

    /// The cron's reading, under its field.
    public var cronLine: String { CronReading.line(cron: cron, responder: false) }

    public var visibilityLine: String {
        isPublic ? "Anyone with its public link can read its brief and runs" : "Only your organisation sees it and its runs"
    }

    /// Only what differs from `row`, in the server's keys. An emptied text
    /// setting is sent as null.
    public func changes(from row: LooperRow) -> [String: JSONValue] {
        let was = LooperDraft(row)
        var changes: [String: JSONValue] = [:]
        func text(_ value: String) -> JSONValue {
            let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
            return trimmed.isEmpty ? .null : .string(trimmed)
        }
        if name != was.name { changes["name"] = .string(name.trimmingCharacters(in: .whitespacesAndNewlines)) }
        if instructions != was.instructions { changes["system_prompt"] = text(instructions) }
        if cron != was.cron { changes["schedule_cron"] = text(cron) }
        if timezone != was.timezone { changes["schedule_timezone"] = text(timezone) }
        if scheduleOn != was.scheduleOn { changes["schedule_enabled"] = .bool(scheduleOn) }
        if notify != was.notify { changes["notify_handle"] = text(notify) }
        if isPublic != was.isPublic { changes["visibility"] = .string(isPublic ? "public" : "private") }
        return changes
    }
}

/// One thing drawn in a run's transcript.
public enum RunItem: Equatable, Sendable, Identifiable {
    /// What woke it, or what a person said to it.
    case said(id: String, text: String)
    case words(id: String, text: String)
    /// Consecutive tool calls, folded: their names, and how many failed.
    case tools(id: String, names: [String], failed: Int)
    /// A turn this reader may not read.
    case withheld(id: String)

    public var id: String {
        switch self {
        case .said(let id, _), .words(let id, _), .tools(let id, _, _), .withheld(let id): id
        }
    }
}

/// One run, read whole: its transcript and its requests.
@MainActor
public final class LooperRunModel: ObservableObject {
    @Published public private(set) var run: LooperRunRow
    @Published public private(set) var items: [RunItem] = []
    @Published public private(set) var requests: [LooperRequest] = []
    @Published public private(set) var loaded = false
    /// Why the last answer failed; the next answer clears it.
    @Published public private(set) var failure: String?
    /// Why the last read failed; a read that works clears it.
    @Published public private(set) var line: String?
    @Published public private(set) var notice: String?
    /// An answer is being sent.
    @Published public private(set) var answering = false

    public let organizationID: String
    private let lobby: LooperLobby

    public init(run: LooperRunRow, organizationID: String, lobby: LooperLobby) {
        self.run = run
        self.organizationID = organizationID
        self.lobby = lobby
    }

    public var open: [LooperRequest] { requests.filter { $0.status == "open" } }

    public func refresh() async {
        do {
            let read = try await lobby.run(organizationID: organizationID, runID: run.id)
            run = read.run
            items = Self.fold(read.turns)
            requests = read.requests
            line = nil
        } catch {
            line = LooperRefusal.sentence(error)
        }
        loaded = true
    }

    public func answer(_ request: LooperRequest, with text: String) async {
        let text = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !answering else { return }
        failure = nil
        guard !text.isEmpty else { failure = "Write an answer first."; return }
        answering = true
        defer { answering = false }
        do {
            let answered = try await lobby.answer(organizationID: organizationID, requestID: request.id, answer: text)
            if let at = requests.firstIndex(where: { $0.id == answered.id }) { requests[at] = answered }
            notice = "Answered"
            await refresh()
        } catch {
            failure = LooperRefusal.sentence(error)
        }
    }

    /// Turns as rows: a person's turn, the looper's words, and its tool
    /// calls folded where they follow one another.
    static func fold(_ turns: [LooperTurn]) -> [RunItem] {
        var items: [RunItem] = []
        func tools(_ id: String, _ calls: [LooperTurn.ToolCall]) {
            let names = calls.map { $0.name ?? "tool" }
            let failed = calls.filter { $0.status == "failed" || $0.status == "error" }.count
            if case .tools(let first, let had, let hadFailed)? = items.last {
                items[items.count - 1] = .tools(id: first, names: had + names, failed: hadFailed + failed)
            } else {
                items.append(.tools(id: id, names: names, failed: failed))
            }
        }
        for turn in turns.sorted(by: { $0.position < $1.position }) {
            if turn.isPrivate == true, turn.content == nil {
                items.append(.withheld(id: turn.id))
                continue
            }
            let text = (turn.content ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
            switch turn.role {
            case "user":
                if !text.isEmpty { items.append(.said(id: turn.id, text: text)) }
            case "assistant":
                if !text.isEmpty { items.append(.words(id: turn.id, text: text)) }
                if let calls = turn.toolCalls, !calls.isEmpty { tools("\(turn.id)-tools", calls) }
            default:
                break
            }
        }
        return items
    }
}
