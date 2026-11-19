import XCTest
@testable import DeskPhoneKit

final class ShelvesTests: XCTestCase {
    private let now = WireDate.parse("2026-01-01T12:00:00.000000Z")!
    private let all = HomeFilter(query: "", project: nil, hidden: [])

    private func row(_ id: String, minutesAgo: Double = 0, waiting: Bool = false, settled: Bool = false, snoozedFor: Double? = nil,
                     title: String? = nil, branch: String? = nil, project: String? = nil, looper: Bool = false,
                     tagged: Bool = false, working: Bool = false) -> HomeRow {
        HomeRow(id: id, organizationID: "o", title: title ?? id, state: waiting ? HomeRow.waitingOnYou : working ? "working" : "idle", ago: "",
                machineID: nil, macLabel: "mbp", recapLine: nil, lastEventAt: now.addingTimeInterval(-minutesAgo * 60),
                settledAt: settled ? now : nil, snoozedUntil: snoozedFor.map { now.addingTimeInterval($0 * 60) },
                branch: branch, project: project, looper: looper,
                waitingOn: tagged ? "person" : nil, waitingReason: tagged ? "Which branch?" : nil)
    }

    private func shelves(_ rows: [HomeRow], _ filter: HomeFilter? = nil) -> [Shelf: [String]] {
        Dictionary(uniqueKeysWithValues: Shelves.arrange(rows, filter: filter ?? all, now: now).map { ($0.shelf, $0.rows.map(\.id)) })
    }

    func testEachRowLandsOnTheFirstShelfThatHoldsIt() {
        let s = shelves([
            row("live"), row("waiting", waiting: true), row("snoozed", snoozedFor: 30),
            row("settled", settled: true), row("snoozed-waiting", waiting: true, snoozedFor: 30),
            row("settled-waiting", waiting: true, settled: true), row("settled-snoozed", settled: true, snoozedFor: 30),
        ])
        XCTAssertEqual(s[.needsYou].map(Set.init), ["waiting", "snoozed-waiting", "settled-waiting"])
        XCTAssertEqual(s[.snoozed].map(Set.init), ["snoozed", "settled-snoozed"])
        XCTAssertEqual(s[.settled], ["settled"])
        XCTAssertEqual(s[.live], ["live"])
    }

    /// The server's tag puts a row on Needs you, below the shelves a
    /// person chose: a snoozed tagged row sleeps, a settled one waits on
    /// nobody, and a working one is the feed behind a turn that cleared it.
    func testATaggedRowNeedsYouUnlessSnoozedSettledOrWorking() {
        let s = shelves([
            row("tagged", tagged: true), row("snoozed-tagged", snoozedFor: 30, tagged: true),
            row("settled-tagged", settled: true, tagged: true), row("working-tagged", tagged: true, working: true),
        ])
        XCTAssertEqual(s[.needsYou], ["tagged"])
        XCTAssertEqual(s[.snoozed], ["snoozed-tagged"])
        XCTAssertEqual(s[.settled], ["settled-tagged"])
        XCTAssertEqual(s[.live], ["working-tagged"])
    }

    func testWaitingOnYouKeepsOnlyTheTaggedRowsAndOpensTheFolds() {
        let rows = [
            row("tagged", tagged: true), row("asks", waiting: true), row("idle"),
            row("snoozed-tagged", snoozedFor: 30, tagged: true), row("working-tagged", tagged: true, working: true),
        ]
        let waiting = HomeFilter(query: "", project: nil, hidden: [], waitingOnly: true)
        let s = shelves(rows, waiting)
        XCTAssertEqual(s[.needsYou], ["tagged"])
        XCTAssertEqual(s[.snoozed], ["snoozed-tagged"])
        XCTAssertNil(s[.live])
        XCTAssertTrue(waiting.narrowed)
        XCTAssertFalse(all.waitingOnly, "off unless asked")
    }

    func testWorkingKeepsOnlyTheWorkingRowsAndWithWaitingEither() {
        let rows = [row("tagged", tagged: true), row("busy", working: true), row("idle")]
        XCTAssertEqual(shelves(rows, HomeFilter(query: "", project: nil, hidden: [], workingOnly: true))[.live], ["busy"])
        let either = shelves(rows, HomeFilter(query: "", project: nil, hidden: [], waitingOnly: true, workingOnly: true))
        XCTAssertEqual(either[.needsYou], ["tagged"])
        XCTAssertEqual(either[.live], ["busy"])
    }

    func testStarredNarrowsWhateverElseIsAsked() {
        func starred(_ id: String, working: Bool = false) -> HomeRow {
            HomeRow(id: id, organizationID: "o", title: id, state: working ? "working" : "idle", ago: "", machineID: nil, macLabel: "mbp",
                    recapLine: nil, lastEventAt: now, starred: true)
        }
        let rows = [starred("star"), starred("busy-star", working: true), row("busy", working: true), row("idle")]
        let onlyStars = HomeFilter(query: "", project: nil, hidden: [], starredOnly: true)
        XCTAssertEqual(Set(shelves(rows, onlyStars)[.live] ?? []), ["star", "busy-star"])
        let busyStars = HomeFilter(query: "", project: nil, hidden: [], workingOnly: true, starredOnly: true)
        XCTAssertEqual(shelves(rows, busyStars)[.live], ["busy-star"])
        XCTAssertEqual(busyStars.showing, "starred, working")
        XCTAssertEqual(onlyStars.showing, "starred")
        XCTAssertNil(all.showing)
    }

    func testAHiddenKindIsLeftOutUnlessItAsks() {
        func kinded(_ id: String, _ kind: ThreadKind, waiting: Bool = false) -> HomeRow {
            HomeRow(id: id, organizationID: "o", title: id, state: waiting ? HomeRow.waitingOnYou : "idle", ago: "", machineID: nil,
                    macLabel: "mbp", recapLine: nil, lastEventAt: now, kind: kind)
        }
        let rows = [kinded("t", .thread), kinded("c", .coordinator), kinded("s", .supervisor), kinded("asks", .coordinator, waiting: true)]
        let s = shelves(rows, HomeFilter(query: "", project: nil, hidden: [.coordinator]))
        XCTAssertEqual(Set(s[.live] ?? []), ["t", "s"])
        XCTAssertEqual(s[.needsYou], ["asks"])
    }

    func testAKindIsTheServersRoleAndALooperRunWithNoneIsALooper() {
        XCTAssertEqual(ThreadKind(role: "coordinator", looper: false), .coordinator)
        XCTAssertEqual(ThreadKind(role: "responder", looper: true), .responder)
        XCTAssertEqual(ThreadKind(role: nil, looper: true), .looper)
        XCTAssertEqual(ThreadKind(role: "unknown", looper: false), .thread)
        XCTAssertEqual(ThreadKind(role: nil, looper: false), .thread)
    }

    func testTheMenuCountsWhatNarrowsAndTheKindsOnce() {
        XCTAssertEqual(HomeFilter(query: "x", project: nil).count, 0, "a search is not the menu's")
        XCTAssertEqual(HomeFilter(query: "", project: "a/b", hidden: [], waitingOnly: true, starredOnly: true).count, 4)
    }

    func testAnExpiredSnoozeIsNoSnooze() {
        let s = shelves([row("woke", snoozedFor: -1), row("woke-settled", settled: true, snoozedFor: -1)])
        XCTAssertEqual(s[.live], ["woke"])
        XCTAssertEqual(s[.settled], ["woke-settled"])
        XCTAssertNil(s[.snoozed])
    }

    func testShelvesComeInOrderAndEmptyOnesAreNotDrawn() {
        let order = Shelves.arrange([row("s", settled: true), row("w", waiting: true)], filter: all, now: now).map(\.shelf)
        XCTAssertEqual(order, [.needsYou, .settled])
    }

    func testSnoozedSoonestWakeFirstOthersNewestFirst() {
        let s = shelves([row("late", snoozedFor: 90), row("soon", snoozedFor: 5), row("old", minutesAgo: 60), row("new", minutesAgo: 1)])
        XCTAssertEqual(s[.snoozed], ["soon", "late"])
        XCTAssertEqual(s[.live], ["new", "old"])
    }

    func testSearchMatchesTitleBranchOrProjectIgnoringCaseAndSpaces() {
        let rows = [row("a", title: "Fix Recap"), row("b", branch: "feat-recap"), row("c", project: "sampleco/recapper"), row("d")]
        let s = shelves(rows, HomeFilter(query: "  RECAP ", project: nil, hidden: []))
        XCTAssertEqual(s[.live].map(Set.init), ["a", "b", "c"])
    }

    func testAProjectFilterKeepsOnlyThatProject() {
        let rows = [row("a", project: "sampleco/gentility"), row("b", project: "sampleco/sample-app"), row("c")]
        XCTAssertEqual(shelves(rows, HomeFilter(query: "", project: "sampleco/gentility", hidden: []))[.live], ["a"])
    }

    func testLooperRunsHideUnlessShown() {
        let rows = [row("thread"), row("run", looper: true)]
        XCTAssertEqual(shelves(rows, HomeFilter(query: "", project: nil))[.live], ["thread"])
        XCTAssertEqual(shelves(rows)[.live].map(Set.init), ["thread", "run"])
    }

    /// Needs you holds whatever waits on the person, a looper's run included.
    func testAHiddenLooperRunThatWaitsOnYouStillNeedsYou() {
        let rows = [row("run", waiting: true, looper: true), row("quiet", looper: true)]
        let s = shelves(rows, HomeFilter(query: "", project: nil))
        XCTAssertEqual(s[.needsYou], ["run"])
        XCTAssertNil(s[.live])
    }

    func testAProjectNoLongerListedFallsBackToAll() {
        let filter = HomeFilter(query: "", project: "sampleco/gone", hidden: [])
        XCTAssertNil(filter.effective(projects: ["sampleco/gentility"]).project)
        XCTAssertEqual(filter.effective(projects: ["sampleco/gone"]).project, "sampleco/gone")
    }

    func testProjectsAreDistinctAndSorted() {
        XCTAssertEqual(Shelves.projects([row("a", project: "z/b"), row("b", project: "a/c"), row("c", project: "z/b"), row("d")]), ["a/c", "z/b"])
    }

    func testNarrowedIsASearchOrAProject() {
        XCTAssertFalse(HomeFilter(query: " ", project: nil).narrowed)
        XCTAssertTrue(HomeFilter(query: "x", project: nil).narrowed)
        XCTAssertTrue(HomeFilter(query: "", project: "a/b").narrowed)
    }
}
