import XCTest
@testable import DeskPhoneKit

@MainActor
final class HomeMenuTests: XCTestCase {
    private let now = WireDate.parse("2026-01-01T12:00:00.000000Z")!

    private func row(attention: RowAttention = .none, settled: Bool = false, snoozedFor: Double? = nil, branch: String? = nil, looper: Bool = false, tagged: Bool = false) -> HomeRow {
        HomeRow(id: "s", organizationID: "o", title: "T", state: "idle", ago: "", machineID: nil, macLabel: "mbp", recapLine: nil,
                lastEventAt: now, settledAt: settled ? now : nil, snoozedUntil: snoozedFor.map { now.addingTimeInterval($0 * 60) },
                branch: branch, looper: looper, attention: attention, waitingOn: tagged ? "person" : nil, waitingReason: tagged ? "Which branch?" : nil)
    }

    /// What every row without a branch offers to copy.
    private let copy = HomeMenuItem.copy([.threadID("s"), .title("T")])

    func testAnIdleRowOffersCopySettleSnoozeAndArchive() {
        XCTAssertEqual(HomeModel.menu(for: row(), now: now), [copy, .settle, .snooze(SnoozePresets.at(now)), .archive])
    }

    func testARowWithABranchOffersItBetweenTheIDAndTheTitle() {
        XCTAssertEqual(HomeModel.menu(for: row(branch: "fix-login"), now: now).first, .copy([.threadID("s"), .branch("fix-login"), .title("T")]))
    }

    func testACopyNamesWhatItCopiesAndCarriesTheText() {
        XCTAssertEqual([HomeCopy.threadID("s"), .branch("b"), .title("T")].map(\.label), ["Copy thread id", "Copy branch", "Copy title"])
        XCTAssertEqual([HomeCopy.threadID("s"), .branch("b"), .title("T")].map(\.text), ["s", "b", "T"])
    }

    func testASettledRowOffersUnsettle() {
        XCTAssertEqual(HomeModel.menu(for: row(settled: true), now: now).dropFirst().first, .unsettle)
    }

    func testASnoozedRowOffersWake() {
        XCTAssertEqual(HomeModel.menu(for: row(snoozedFor: 30), now: now), [copy, .settle, .wake, .archive])
    }

    func testAWaitingRowCannotBeSnoozed() {
        XCTAssertEqual(HomeModel.menu(for: row(attention: .question), now: now), [copy, .settle, .archive])
    }

    /// A thread the server tags as waiting on you sits on Needs you
    /// but is no ask: it can still be put off.
    func testATaggedRowCanStillBeSnoozed() {
        XCTAssertEqual(HomeModel.menu(for: row(tagged: true), now: now), [copy, .settle, .snooze(SnoozePresets.at(now)), .archive])
    }

    func testALooperRunOffersCopyAndSaysItIsManagedOnTheMac() {
        XCTAssertEqual(HomeModel.menu(for: row(looper: true), now: now), [copy, .managedOnMac])
    }

    func testTheMenuAgreesWithTheSwipes() {
        for row in [row(), row(settled: true), row(snoozedFor: 30), row(attention: .approval), row(looper: true), row(tagged: true)] {
            let swipes = HomeModel.actions(for: row, now: now)
            let fromSwipes = (swipes.leading + swipes.trailing).map(Self.kind).sorted()
            let fromMenu = HomeModel.menu(for: row, now: now).compactMap(\.kind).sorted()
            XCTAssertEqual(fromMenu, fromSwipes)
        }
    }

    private static func kind(_ action: HomeAction) -> String {
        switch action {
        case .settle: "settle"
        case .unsettle: "unsettle"
        case .snooze: "snooze"
        case .wake: "wake"
        case .archive: "archive"
        case .unarchive: "unarchive"
        }
    }

    func testCollectMapsApprovalAndQuestionToTheirAttention() async throws {
        let lobby = FakeLobby()
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "A", mac: nil, host: nil, lastEventAt: "2026-01-01T11:59:00.000000Z", attention: "approval"),
            try Build.row("s-2", title: "Q", mac: nil, host: nil, lastEventAt: "2026-01-01T11:58:00.000000Z", attention: "question"),
            try Build.row("s-3", title: "N", mac: nil, host: nil, lastEventAt: "2026-01-01T11:57:00.000000Z"),
        ], now: Build.now)]
        let home = HomeModel(lobby: lobby, sleep: { _ in throw CancellationError() }, now: { [now] in now })
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.attention), [.approval, .question, .none])
    }
}
