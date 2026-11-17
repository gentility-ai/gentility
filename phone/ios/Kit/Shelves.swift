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

/// What the person narrowed Home to.
public struct HomeFilter: Equatable, Sendable {
    public var query: String
    /// `owner/repo`; nil is every project.
    public var project: String?
    public var showLoopers: Bool
    /// Only the threads waiting on you (`HomeRow.waitsOnYou`).
    public var waitingOnly: Bool

    public init(query: String, project: String?, showLoopers: Bool, waitingOnly: Bool = false) {
        self.query = query
        self.project = project
        self.showLoopers = showLoopers
        self.waitingOnly = waitingOnly
    }

    private var needle: String { query.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// A search, a project or Waiting on you: the folds open, so a match
    /// never hides.
    public var narrowed: Bool { !needle.isEmpty || project != nil || waitingOnly }

    /// A remembered project no row is in any more reads as every project.
    public func effective(projects: [String]) -> HomeFilter {
        var filter = self
        if let project, !projects.contains(project) { filter.project = nil }
        return filter
    }

    public func admits(_ row: HomeRow) -> Bool {
        if waitingOnly && !row.waitsOnYou { return false }
        if row.looper && !showLoopers && !row.waiting { return false }
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
