import XCTest
@testable import DeskPhoneKit

@MainActor
final class HomeParkTests: XCTestCase {
    private var lobby: FakeLobby!
    private let now = WireDate.parse(Build.now)!

    override func setUp() async throws {
        lobby = FakeLobby()
    }

    /// A Home listing `rows`, refreshed once. Its undo never times out.
    private func home(_ rows: [LobbyRow]) async -> HomeModel {
        lobby.lists["o-1"] = [LobbyList(sessions: rows, now: Build.now)]
        let home = HomeModel(lobby: lobby, sleep: { _ in throw CancellationError() }, now: { [now] in now })
        await home.refresh()
        return home
    }

    private func row(snoozedUntil: String? = nil, attention: String = "none", looper: String? = nil) throws -> LobbyRow {
        try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp", attention: attention, snoozedUntil: snoozedUntil, looper: looper)
    }

    func testSettleMovesTheRowAtOnceAndOffersUndo() async throws {
        let home = await home([try row()])
        await home.act(.settle, on: home.rows[0])
        XCTAssertNotNil(home.rows[0].settledAt)
        XCTAssertEqual(lobby.inboxes.map(\.field), [.settledAt])
        XCTAssertNotNil(lobby.inboxes.last?.value)
        XCTAssertEqual(home.undo?.words, "Settled")
        XCTAssertEqual(home.undo?.inverse, .unsettle)

        await home.undoLast()
        XCTAssertNil(home.rows[0].settledAt)
        XCTAssertEqual(lobby.inboxes.map(\.field), [.settledAt, .settledAt])
        XCTAssertNil(lobby.inboxes.last?.value)
        XCTAssertNil(home.undo, "an undo offers no undo of its own")
    }

    func testARefusalPutsTheRowBackAndSaysWhy() async throws {
        let home = await home([try row()])
        lobby.inboxResults = [.failure(.refused("attention_pending"))]
        await home.act(.snooze(now.addingTimeInterval(3600)), on: home.rows[0])
        XCTAssertNil(home.rows[0].snoozedUntil)
        XCTAssertEqual(home.line, "Couldn't snooze: it is waiting on you.")
        XCTAssertNil(home.undo)
    }

    func testAStaleAnswerSnapsTheRowBackToTheServer() async throws {
        let home = await home([try row()])
        lobby.inboxResults = [.success(InboxReply(applied: [], stale: ["settled_at"]))]
        await home.act(.settle, on: home.rows[0])
        XCTAssertNil(home.rows[0].settledAt, "a later write elsewhere won")
        XCTAssertNil(home.undo)
        XCTAssertNil(home.line, "stale is not a refusal")
    }

    func testUnreachablePutsTheRowBack() async throws {
        let home = await home([try row()])
        lobby.inboxResults = [.failure(.unreachable("offline"))]
        await home.act(.settle, on: home.rows[0])
        XCTAssertNil(home.rows[0].settledAt)
        XCTAssertEqual(home.line, "Can't reach the server. Showing what it last said.")
        XCTAssertNil(home.undo)
    }

    func testUndoOfAMovedSnoozeWhoseOldWakeHasPassedWakes() async throws {
        let home = await home([try row(snoozedUntil: "2026-01-01T11:59:00.000000Z")])
        await home.act(.snooze(now.addingTimeInterval(7200)), on: home.rows[0])
        await home.undoLast()
        XCTAssertEqual(lobby.inboxes.last?.field, .snoozedUntil)
        XCTAssertNil(lobby.inboxes.last?.value)
        XCTAssertNil(home.rows[0].snoozedUntil)
    }

    func testUndoOfAMovedSnoozeStillAheadRestoresIt() async throws {
        let old = "2026-01-01T13:00:00.000000Z"
        let home = await home([try row(snoozedUntil: old)])
        await home.act(.snooze(now.addingTimeInterval(7200)), on: home.rows[0])
        XCTAssertEqual(home.undo?.words.hasPrefix("Snoozed until "), true)
        await home.undoLast()
        XCTAssertEqual(lobby.inboxes.last?.field, .snoozedUntil)
        XCTAssertEqual(lobby.inboxes.last?.value, WireDate.parse(old))
        XCTAssertEqual(home.rows[0].snoozedUntil, WireDate.parse(old))
    }

    func testArchiveHidesTheRowAndUndoUnarchives() async throws {
        let home = await home([try row()])
        await home.act(.archive, on: home.rows[0])
        XCTAssertTrue(home.rows.isEmpty)
        XCTAssertEqual(lobby.inboxes.map(\.field), [.archivedAt])
        XCTAssertNotNil(lobby.inboxes.last?.value)
        XCTAssertEqual(home.undo?.words, "Archived")
        await home.undoLast()
        XCTAssertEqual(lobby.inboxes.map(\.field), [.archivedAt, .archivedAt])
        XCTAssertNil(lobby.inboxes.last?.value)
        XCTAssertEqual(home.rows.map(\.id), ["s-1"])
    }

    func testALaterActionIsNotUndoneByAnEarlierRefusal() async throws {
        let home = await home([try row()])
        // A settle, then a snooze, then a second settle refused: the
        // refusal takes back the settle it made, not the earlier one's
        // field nor the snooze.
        let until = now.addingTimeInterval(3600)
        await home.act(.snooze(until), on: home.rows[0])
        lobby.inboxResults = [.failure(.refused("not_found"))]
        await home.act(.settle, on: home.rows[0])
        XCTAssertEqual(home.rows[0].snoozedUntil, until)
        XCTAssertNil(home.rows[0].settledAt)
    }

    func testAnOverrideIsDroppedAtTheNextListOnceAnswered() async throws {
        let home = await home([try row()])
        await home.act(.settle, on: home.rows[0])
        // The server's next word stands, whatever it says.
        lobby.lists["o-1"] = [LobbyList(sessions: [try row()], now: Build.now)]
        await home.refresh()
        XCTAssertNil(home.rows[0].settledAt)
    }

    func testAListReadBeforeTheAnswerDoesNotUndoTheMove() async throws {
        let home = await home([try row()])
        // The refresh starts, the person settles while it is out, the
        // server answers, and then the list read before the write arrives.
        lobby.refreshing = { [unowned home] in
            self.lobby.refreshing = {}
            await home.act(.settle, on: home.rows[0])
        }
        await home.refresh()
        XCTAssertNotNil(home.rows[0].settledAt, "a stale list put the settle back")
        await home.refresh()
        XCTAssertNil(home.rows[0].settledAt, "the next list, started after the answer, is truth")
    }

    func testALooperRunHasNoActionsAndAWaitingRowCannotSnooze() throws {
        func homeRow(_ lobbyRow: LobbyRow) -> HomeRow {
            HomeModel.collect(rows: ["o-1": ["s-1": lobbyRow]], machines: [:], personID: Build.morgan, now: now)[0]
        }
        let looper = HomeModel.actions(for: homeRow(try row(looper: "l-1")), now: now)
        XCTAssertEqual(looper.leading, [])
        XCTAssertEqual(looper.trailing, [])

        let waiting = HomeModel.actions(for: homeRow(try row(attention: "approval")), now: now)
        XCTAssertEqual(waiting.leading, [.settle])
        XCTAssertEqual(waiting.trailing, [.archive])

        let snoozed = HomeModel.actions(for: homeRow(try row(snoozedUntil: "2026-01-02T09:00:00.000000Z")), now: now)
        XCTAssertEqual(snoozed.trailing, [.wake, .archive])

        let plain = HomeModel.actions(for: homeRow(try row()), now: now)
        XCTAssertEqual(plain.leading, [.settle])
        XCTAssertEqual(plain.trailing, [.snooze(.distantFuture), .archive])
    }
}
