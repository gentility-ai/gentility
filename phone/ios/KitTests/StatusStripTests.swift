import XCTest
@testable import DeskPhoneKit

final class StatusStripTests: XCTestCase {
    private let now = Date(timeIntervalSince1970: 1_000_000)

    func testAWorkingTurnSaysHowLongAndWhatWaits() throws {
        let strip = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: true, since: now.addingTimeInterval(-65), queued: 2))
        XCTAssertEqual(strip.words(now: now), "Working · 1m 05s · 2 queued")
        let unstamped = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: true, since: nil, queued: 0))
        XCTAssertEqual(unstamped.words(now: now), "Working")
    }

    func testACompactionSaysSoInPlaceOfTheClock() throws {
        let strip = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: true, compacting: true, since: now.addingTimeInterval(-65), queued: 1))
        XCTAssertEqual(strip.words(now: now), "Compacting… · 1 queued")
        XCTAssertNil(StatusStrip(phase: .open, held: true, working: false, compacting: true, since: nil, queued: 0), "a turn's end ends it")
    }

    func testADialSaysConnectingOrReconnectingAndNothingQueued() throws {
        XCTAssertEqual(StatusStrip(phase: .connecting, held: false, working: false, since: nil, queued: 0)?.words(now: now), "Connecting…")
        let again = try XCTUnwrap(StatusStrip(phase: .connecting, held: true, working: true, since: now, queued: 3))
        XCTAssertEqual(again.words(now: now), "Reconnecting…", "the queue is the open door's to say")
    }

    func testAQuietSessionAndAnOfflineOneHaveNoStrip() {
        XCTAssertNil(StatusStrip(phase: .open, held: true, working: false, since: nil, queued: 0))
        XCTAssertEqual(StatusStrip(phase: .open, held: true, working: false, since: nil, queued: 1)?.words(now: now), "1 queued")
        XCTAssertNil(StatusStrip(phase: .asleep(macLabel: "cedar"), held: true, working: true, since: now, queued: 1))
        XCTAssertNil(StatusStrip(phase: .lost("The link dropped."), held: true, working: true, since: now, queued: 0))
    }

    func testTheClockReadsAsTheWindowsDoes() {
        XCTAssertEqual(StatusStrip.elapsed(-3), "0s", "a Mac's clock ahead of the phone's")
        XCTAssertEqual(StatusStrip.elapsed(45.9), "45s")
        XCTAssertEqual(StatusStrip.elapsed(185), "3m 05s")
        XCTAssertEqual(StatusStrip.elapsed(3_720), "1h 02m")
    }

    func testAWorkingStripSpinsOnlyOnceTheTranscriptsSignIsOutOfSight() throws {
        let working = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: true, since: nil, queued: 0))
        XCTAssertFalse(working.spins(atEnd: true), "the caret or the running call is the live sign")
        XCTAssertTrue(working.spins(atEnd: false))
        let compacting = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: true, compacting: true, since: nil, queued: 0))
        XCTAssertTrue(compacting.spins(atEnd: true), "a compaction's line does not move")
        XCTAssertFalse(working.spins(atEnd: false, stops: true), "the clock beside the stop is the sign")
        XCTAssertTrue(compacting.spins(atEnd: true, stops: true), "a compaction has no clock, so the stop does not take its spinner")
        let connecting = try XCTUnwrap(StatusStrip(phase: .connecting, held: true, working: false, since: nil, queued: 0))
        XCTAssertTrue(connecting.spins(atEnd: true))
        let queued = try XCTUnwrap(StatusStrip(phase: .open, held: true, working: false, since: nil, queued: 1))
        XCTAssertFalse(queued.spins(atEnd: false))
    }
}
