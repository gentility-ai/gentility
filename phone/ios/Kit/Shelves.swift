import Foundation

/// Home's shelves, after the Desk sidebar's (`desk/ui/src/sidebar/sidebar.logic.ts`,
/// `shelves()`): a row sits on the first that holds it.
public enum Shelf: String, CaseIterable, Sendable {
    case needsYou, live, snoozed, settled

    public var title: String {
        switch self {
        case .needsYou: "Needs you"
        case .live: "Threads"
        case .snoozed: "Snoozed"
        case .settled: "Settled"
        }
    }

    /// Snoozed and Settled fold away until opened.
    public var folds: Bool { self == .snoozed || self == .settled }
}

public struct ShelfGroup: Identifiable, Equatable, Sendable {
    public let shelf: Shelf
    public let rows: [HomeRow]
    public var id: Shelf { shelf }
    public var title: String { shelf.title }
}

/// What a thread is for, after the Desk sidebar's kinds
/// (`desk/ui/src/sidebar/row.logic.ts`, `kindOf`).
public enum ThreadKind: String, CaseIterable, Sendable {
    case thread, supervisor, coordinator, looper, responder

    /// The server's role for the row; a looper's run with no role is a looper.
    public init(role: String?, looper: Bool) {
        self = role.flatMap(ThreadKind.init(rawValue:)) ?? (looper ? .looper : .thread)
    }

    /// As the filter names them.
    public var plural: String {
        switch self {
        case .thread: "Threads"
        case .supervisor: "Supervisors"
        case .coordinator: "Coordinators"
        case .looper: "Looper runs"
        case .responder: "Responder runs"
        }
    }

    public var singular: String {
        switch self {
        case .thread: "Thread"
        case .supervisor: "Supervisor"
        case .coordinator: "Coordinator"
        case .looper: "Looper"
        case .responder: "Responder"
        }
    }

    /// The SF Symbol a row is marked with; a plain thread carries none.
    public var symbol: String? {
        switch self {
        case .thread: nil
        case .supervisor: "eye"
        case .coordinator: "point.3.connected.trianglepath.dotted"
        case .looper: "repeat"
        case .responder: "bolt"
        }
    }
}

/// What the person narrowed Home to.
public struct HomeFilter: Equatable, Sendable {
    public var query: String
    /// `owner/repo`; nil is every project.
    public var project: String?
    /// The kinds left out. A row waiting on an approval or a question shows
    /// whatever its kind.
    public var hidden: Set<ThreadKind>
    /// Only the threads waiting on you (`HomeRow.waitsOnYou`).
    public var waitingOnly: Bool
    /// Only the threads working. With `waitingOnly`, either.
    public var workingOnly: Bool
    public var starredOnly: Bool

    /// What Home leaves out until asked: the runs nobody started by hand.
    public static let quiet: Set<ThreadKind> = [.looper, .responder]

    public init(query: String, project: String?, hidden: Set<ThreadKind> = HomeFilter.quiet, waitingOnly: Bool = false, workingOnly: Bool = false, starredOnly: Bool = false) {
        self.query = query
        self.project = project
        self.hidden = hidden
        self.waitingOnly = waitingOnly
        self.workingOnly = workingOnly
        self.starredOnly = starredOnly
    }

    private var needle: String { query.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// How many things the menu narrows by; the kinds count once, when they
    /// are not as Home starts.
    public var count: Int {
        [project != nil, hidden != Self.quiet, waitingOnly, workingOnly, starredOnly].filter { $0 }.count
    }

    /// A search, a project or a status: the folds open, so a match never
    /// hides.
    public var narrowed: Bool { !needle.isEmpty || project != nil || waitingOnly || workingOnly || starredOnly }

    /// What the footer says Home is showing only; nil when no status narrows it.
    public var showing: String? {
        var words: [String] = []
        if waitingOnly { words.append("waiting on you") }
        if workingOnly { words.append("working") }
        guard !words.isEmpty || starredOnly else { return nil }
        let status = words.joined(separator: " or ")
        return starredOnly ? (status.isEmpty ? "starred" : "starred, \(status)") : status
    }

    /// A remembered project no row is in any more reads as every project.
    public func effective(projects: [String]) -> HomeFilter {
        var filter = self
        if let project, !projects.contains(project) { filter.project = nil }
        return filter
    }

    public func admits(_ row: HomeRow) -> Bool {
        if waitingOnly || workingOnly {
            let waits = waitingOnly && row.waitsOnYou
            let works = workingOnly && row.state == "working"
            if !waits && !works { return false }
        }
        if starredOnly && !row.starred { return false }
        if hidden.contains(row.kind) && !row.waiting { return false }
        if let project, row.project != project { return false }
        guard !needle.isEmpty else { return true }
        return [row.title, row.branch, row.project].contains { $0?.localizedCaseInsensitiveContains(needle) == true }
    }
}

public enum Shelves {
    public static func shelf(of row: HomeRow, now: Date) -> Shelf {
        if row.waiting { return .needsYou }
        if let until = row.snoozedUntil, until > now { return .snoozed }
        if row.settledAt != nil { return .settled }
        if row.waitsOnYou { return .needsYou }
        return .live
    }

    /// `rows` the filter admits, on their shelves in `Shelf` order; an empty
    /// shelf is left out.
    public static func arrange(_ rows: [HomeRow], filter: HomeFilter, now: Date) -> [ShelfGroup] {
        let held = Dictionary(grouping: rows.filter(filter.admits)) { shelf(of: $0, now: now) }
        return Shelf.allCases.compactMap { shelf in
            guard let rows = held[shelf] else { return nil }
            return ShelfGroup(shelf: shelf, rows: rows.sorted(by: shelf == .snoozed ? soonestWake : HomeModel.newestFirst))
        }
    }

    /// Every project a row is in, once, sorted.
    public static func projects(_ rows: [HomeRow]) -> [String] {
        Set(rows.compactMap(\.project)).sorted()
    }

    static func soonestWake(_ a: HomeRow, _ b: HomeRow) -> Bool {
        let (left, right) = (a.snoozedUntil ?? .distantFuture, b.snoozedUntil ?? .distantFuture)
        return left != right ? left < right : a.id < b.id
    }
}
