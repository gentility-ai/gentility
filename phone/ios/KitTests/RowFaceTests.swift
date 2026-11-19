import XCTest
@testable import DeskPhoneKit

final class RowFaceTests: XCTestCase {
    private let now = WireDate.parse("2026-01-01T12:00:00.000000Z")!
    private var utc: Calendar {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar
    }

    private func row(
        title: String = "Fix the recap timing", state: String = "idle", attention: RowAttention? = nil,
        attentionLine: String? = nil, minutesAgo: Double? = 120, mac: String = "Morgan's MacBook Pro", live: Bool = true,
        settled: Bool = false, snoozedFor: Double? = nil, branch: String? = nil, project: String? = nil, looper: Bool = false,
        waitingReason: String? = nil
    ) -> HomeRow {
        HomeRow(id: "s", organizationID: "o", title: title, state: state, ago: "", machineID: "m", macLabel: mac, recapLine: nil,
                lastEventAt: minutesAgo.map { now.addingTimeInterval(-$0 * 60) }, attentionLine: attentionLine, live: live,
                settledAt: settled ? now : nil, snoozedUntil: snoozedFor.map { now.addingTimeInterval($0 * 60) },
                branch: branch, project: project, looper: looper, attention: attention,
                waitingOn: waitingReason == nil ? nil : "person", waitingReason: waitingReason)
    }

    // MARK: Waiting on you

    func testATaggedRowSaysWaitingOnYouBehindARaisedHandWithItsReason() {
        let f = face(row(waitingReason: "Which branch should the fix go on?"))
        XCTAssertEqual(f.word, StatusWord(text: "Waiting on you", symbol: "hand.raised", tone: .input))
        XCTAssertNotEqual(f.word?.symbol, "questionmark.bubble", "not the question card's glyph")
        XCTAssertTrue(f.needsYou)
        XCTAssertFalse(f.recedes)
        XCTAssertEqual(f.attentionLine, "Which branch should the fix go on?")
        XCTAssertTrue(f.accessibilityLabel.contains("Waiting on you: Which branch should the fix go on?."))
    }

    func testAnAsksLineWinsOverTheTagsReason() {
        let f = face(row(state: HomeRow.waitingOnYou, attention: .question, attentionLine: "Pick one", waitingReason: "Which branch?"))
        XCTAssertEqual(f.word?.text, "Input")
        XCTAssertEqual(f.attentionLine, "Pick one")
    }

    func testASnoozedTaggedRowSleepsAndSaysNoReason() {
        let f = face(row(snoozedFor: 30, waitingReason: "Which branch?"))
        XCTAssertFalse(f.needsYou)
        XCTAssertNil(f.attentionLine)
        XCTAssertEqual(f.word?.symbol, "moon.zzz")
    }

    func testAWorkingTaggedRowSaysWorking() {
        let f = face(row(state: "working", waitingReason: "Which branch?"))
        XCTAssertEqual(f.word?.text, "Working")
        XCTAssertFalse(f.needsYou)
        XCTAssertNil(f.attentionLine)
    }

    private func face(_ row: HomeRow, showsMacName: Bool = false) -> RowFace {
        RowFace(row, now: now, showsMacName: showsMacName, calendar: utc)
    }

    // MARK: Ago.compact, as the Mac's rowAge

    func testCompactAgeReadsAsTheMacsRowAge() {
        XCTAssertEqual(Ago.compact(now.addingTimeInterval(-30), now: now), "now")
        XCTAssertEqual(Ago.compact(now.addingTimeInterval(-15 * 60), now: now), "15m")
        XCTAssertEqual(Ago.compact(now.addingTimeInterval(-2 * 3_600), now: now), "2h")
        XCTAssertEqual(Ago.compact(now.addingTimeInterval(-9 * 86_400), now: now), "9d")
    }

    func testCompactAgeReadsAFutureInstantAsNowAndNothingAsEmpty() {
        XCTAssertEqual(Ago.compact(now.addingTimeInterval(300), now: now), "now")
        XCTAssertEqual(Ago.compact(nil, now: now), "")
    }

    // MARK: Attention

    func testWaitingFollowsAttention() {
        XCTAssertTrue(row(attention: .approval).waiting)
        XCTAssertTrue(row(attention: .question).waiting)
        XCTAssertFalse(row(attention: RowAttention.none).waiting)
    }

    // MARK: Status words

    func testApprovalIsAnOrangeWordWithItsLine() {
        let face = face(row(attention: .approval, attentionLine: "Wants to execute"))
        XCTAssertEqual(face.word, StatusWord(text: "Approval", symbol: "checkmark.shield", tone: .approval))
        XCTAssertEqual(face.attentionLine, "Wants to execute")
        XCTAssertTrue(face.needsYou)
    }

    func testQuestionIsInput() {
        XCTAssertEqual(face(row(attention: .question)).word, StatusWord(text: "Input", symbol: "questionmark.bubble", tone: .input))
    }

    func testWorkingIsWorking() {
        XCTAssertEqual(face(row(state: "working")).word, StatusWord(text: "Working", symbol: "circle.dashed", tone: .working))
    }

    func testIdleHasNoWord() {
        let face = face(row())
        XCTAssertNil(face.word)
        XCTAssertNil(face.attentionLine)
        XCTAssertFalse(face.needsYou)
    }

    func testSnoozedSaysWhenItWakes() {
        XCTAssertEqual(face(row(snoozedFor: 150)).word, StatusWord(text: "14:30", symbol: "moon.zzz", tone: .snoozed))
        // 2026-01-01 is a Thursday; two days on at 09:00 is Saturday.
        let saturday = (2 * 24 - 3) * 60.0
        XCTAssertEqual(face(row(snoozedFor: saturday)).word?.text, "Sat 09:00")
    }

    func testAnExpiredSnoozeIsNoWord() {
        XCTAssertNil(face(row(snoozedFor: -10)).word)
    }

    func testSettledIsSettled() {
        XCTAssertEqual(face(row(settled: true)).word, StatusWord(text: "Settled", symbol: "checkmark.circle", tone: .settled))
    }

    func testAttentionOutranksParkAndWork() {
        XCTAssertEqual(face(row(state: "working", attention: .question, settled: true)).word?.tone, .input)
    }

    // MARK: Place, kind, branch, Mac

    func testAProjectRowShowsTheRepositoryNameAndNoMark() {
        let face = face(row(branch: "fix-recap", project: "sampleco/gentility"))
        XCTAssertEqual(face.place, "gentility")
        XCTAssertNil(face.kind, "a plain thread carries no mark")
        XCTAssertFalse(face.starred)
        XCTAssertEqual(face.branch, "fix-recap")
    }

    func testALooperRunSaysLooperRunAndIsMarked() {
        let face = face(row(project: "sampleco/gentility", looper: true))
        XCTAssertEqual(face.place, "Looper run")
        XCTAssertEqual(face.kind, KindMark(symbol: "repeat", name: "Looper"))
    }

    func testNoProjectAndNoLooperHasNoPlace() {
        let face = face(row())
        XCTAssertNil(face.kind)
        XCTAssertNil(face.place)
        XCTAssertNil(face.branch)
    }

    func testEachKindButAThreadIsMarkedAndSaid() {
        func kinded(_ kind: ThreadKind, looper: Bool = false) -> RowFace {
            face(HomeRow(id: "s", organizationID: "o", title: "T", state: "idle", ago: "", machineID: "m", macLabel: "mbp", recapLine: nil,
                         lastEventAt: nil, live: true, project: "sampleco/gentility", looper: looper, kind: kind, starred: true))
        }
        XCTAssertEqual(kinded(.coordinator).kind?.name, "Coordinator")
        XCTAssertEqual(kinded(.supervisor).kind?.symbol, "eye")
        XCTAssertEqual(kinded(.responder, looper: true).place, "Responder run")
        XCTAssertNil(kinded(.thread).kind)
        XCTAssertTrue(kinded(.thread).starred)
        XCTAssertTrue(kinded(.coordinator).accessibilityLabel.hasPrefix("T. Starred. Coordinator."))
    }

    func testOnlyAWordThatAsksIsDrawnAtTheRowsEnd() {
        XCTAssertTrue(face(row(state: HomeRow.waitingOnYou, attention: .approval)).asks)
        XCTAssertTrue(face(row(state: HomeRow.waitingOnYou, attention: .question)).asks)
        XCTAssertFalse(face(row(state: "working")).asks)
        XCTAssertFalse(face(row(settled: true)).asks)
        XCTAssertFalse(face(row()).asks)
    }

    func testTheMacIsNamedOnlyWhenRowsSpanMoreThanOne() {
        XCTAssertEqual(face(row(), showsMacName: false).mac, MacMark(name: nil, asleep: false))
        XCTAssertEqual(face(row(live: false), showsMacName: true).mac, MacMark(name: "Morgan's MacBook Pro", asleep: true))
        XCTAssertFalse(RowFace.showsMacName([row(), row()]))
        XCTAssertTrue(RowFace.showsMacName([row(), row(mac: "cedar")]))
    }

    func testParkedAndAsleepRowsRecedeButANeedsYouRowDoesNot() {
        XCTAssertFalse(face(row()).recedes)
        XCTAssertTrue(face(row(live: false)).recedes)
        XCTAssertTrue(face(row(settled: true)).recedes)
        XCTAssertTrue(face(row(snoozedFor: 30)).recedes)
        XCTAssertFalse(face(row(attention: .approval, live: false)).recedes)
    }

    func testAgeIsCompact() {
        XCTAssertEqual(face(row(minutesAgo: 120)).age, "2h")
        XCTAssertEqual(face(row(minutesAgo: nil)).age, "")
    }

    // MARK: Accessibility

    func testTheAccessibilityLabelReadsTitleWordPlaceMacAge() {
        let face = face(row(attention: .approval, attentionLine: "Wants to execute", live: false, branch: "fix-recap", project: "sampleco/gentility"))
        XCTAssertEqual(face.accessibilityLabel, "Fix the recap timing. Approval: Wants to execute. gentility, branch fix-recap. Morgan's MacBook Pro, asleep. 2 hours ago.")
    }

    func testAnIdleRowsLabelLeavesOutWhatItLacks() {
        XCTAssertEqual(face(row(minutesAgo: 1)).accessibilityLabel, "Fix the recap timing. Morgan's MacBook Pro. 1 minute ago.")
        XCTAssertEqual(face(row(minutesAgo: 0)).accessibilityLabel, "Fix the recap timing. Morgan's MacBook Pro. Just now.")
    }
}
