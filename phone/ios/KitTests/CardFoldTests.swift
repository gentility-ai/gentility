import XCTest
@testable import DeskPhoneKit

final class CardFoldTests: XCTestCase {
    private let now = WireDate.parse("2026-06-01T00:00:00Z")!
    private let ids = Ids()
    private var fold = CardFold()

    private func row(_ attention: JSONValue?) { fold.row(Asks.attention(attention), now: now) }

    private func said(_ json: JSONValue) throws {
        fold.said(try json.decode())
    }

    private func answer(_ answer: Answer) -> DoorRequest? {
        fold.answer(answer, sessionID: "s1", now: now, op: ids.next)
    }

    func testARowsApprovalPinsTheCardWithItsOptions() {
        row(Asks.approvalAttention())
        guard case .approval(let request)? = fold.card?.ask else { return XCTFail("nothing pinned") }
        XCTAssertEqual(request.options.map(\.label), ["Allow once", "Always", "Deny"])
        XCTAssertNil(fold.cleared)
    }

    func testAnAskedMomentPinsWhenNothingIsPinned() throws {
        try said(.object(["kind": .string("asked"), "request": Asks.approvalJSON("r-2")]))
        XCTAssertEqual(fold.card?.ask.id, "r-2")
        try said(.object(["kind": .string("asked"), "request": Asks.approvalJSON("r-3")]))
        XCTAssertEqual(fold.card?.ask.id, "r-2", "one card at a time; the row says which is oldest")
    }

    func testAllowOnceIsTheSessionAnswerWithAFreshOp() {
        row(Asks.approvalAttention())
        XCTAssertEqual(answer(.option("allow")), .sessionAnswer(sessionID: "s1", requestID: "r-1", optionID: "allow", op: "op-1"))
        XCTAssertEqual(fold.card?.sending, true)
    }

    func testADoubleTapSendsOnce() {
        row(Asks.approvalAttention())
        XCTAssertNotNil(answer(.option("allow")))
        XCTAssertNil(answer(.option("deny")))
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    func testAnAnswerMustFitTheAsk() {
        row(Asks.approvalAttention())
        XCTAssertNil(answer(.decline), "an approval is answered with one of its options")
        XCTAssertNil(answer(.option("nope")), "an option it did not offer")
        row(nil)
        row(Asks.formAttention())
        XCTAssertNil(answer(.option("allow")))
        XCTAssertEqual(answer(.decline), .sessionElicitationAnswer(sessionID: "s1", requestID: "e-1", action: .decline, content: nil, op: "op-1"))
    }

    func testAServerQuestionIsAnsweredAsAQuestion() {
        row(Asks.questionAttention())
        XCTAssertEqual(
            answer(.accept(.object(["why": .string("tidy")]))),
            .sessionQuestionAnswer(sessionID: "s1", questionID: "q-1", action: .accept, content: .object(["why": .string("tidy")]), op: "op-1")
        )
    }

    func testAnExpiredQuestionIsNotPinned() {
        row(Asks.questionAttention(expires: "2026-05-31T00:00:00Z"))
        XCTAssertNil(fold.card)
    }

    /// Review focus 3.
    func testOurOkClearsTheCardAndALateRowDoesNotPinItAgain() {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        XCTAssertFalse(fold.replied(.ok, op: "op-1"))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
        row(Asks.approvalAttention())
        XCTAssertNil(fold.card, "a row said before the answer landed")
    }

    /// Review focus 3.
    func testOurOwnAnsweredEchoClearsSilently() throws {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        try said(Asks.answered("r-1", option: "allow"))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    /// Review focus 3.
    func testAnotherDevicesAnswerClearsWithANote() throws {
        row(Asks.approvalAttention())
        try said(Asks.answered("r-1", option: "deny"))
        XCTAssertNil(fold.card)
        XCTAssertEqual(fold.cleared, "Answered on another device")
        row(Asks.approvalAttention("r-2"))
        XCTAssertEqual(fold.card?.ask.id, "r-2")
        XCTAssertNil(fold.cleared, "a new card takes the line away")
    }

    func testUnansweredClearsSilently() throws {
        row(Asks.approvalAttention())
        try said(.object(["kind": .string("unanswered"), "request_id": .string("r-1")]))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    func testAnElicitationDecidedElsewhereSaysSoAndWithdrawnDoesNot() throws {
        row(Asks.formAttention("e-1"))
        try said(.object(["kind": .string("elicitation_decided"), "request_id": .string("e-1"), "decision": .object(["outcome": .string("declined")])]))
        XCTAssertEqual(fold.cleared, "Answered on another device")
        row(Asks.formAttention("e-2"))
        try said(.object(["kind": .string("elicitation_decided"), "request_id": .string("e-2"), "decision": .object(["outcome": .string("withdrawn")])]))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    func testAnAttentionFreeRowClearsAnUnansweredCardSilently() {
        row(Asks.approvalAttention())
        row(nil)
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    /// Ruling 2: a refusal is weighed against the row, never its words.
    func testARefusalAsksForTheRowAndAGoneQuestionSaysAnsweredElsewhere() {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "harness", message: "no permission request r-1 is waiting for an answer", reason: nil)), op: "op-1"))
        XCTAssertEqual(fold.card?.checking, true)
        row(nil)
        XCTAssertNil(fold.card)
        XCTAssertEqual(fold.cleared, "Answered on another device")
    }

    func testARefusalWhoseQuestionStandsShowsItsSentence() {
        row(Asks.formAttention())
        _ = answer(.accept(.object(["branch": .string("")])))
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "invalid_answer", message: "branch: Required.", reason: nil)), op: "op-1"))
        row(Asks.formAttention())
        XCTAssertEqual(fold.card?.note, "branch: Required.")
        XCTAssertEqual(fold.card?.checking, false)
        XCTAssertNil(fold.card?.answer, "the person may answer again")
        XCTAssertNotNil(answer(.decline))
    }

    func testARefusalWithNoRowToWeighItAgainstShowsItsSentence() {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        _ = fold.replied(.error(DoorRefusal(code: "harness", message: "busy", reason: nil)), op: "op-1")
        fold.rowUnread()
        XCTAssertEqual(fold.card?.note, "busy")
    }

    /// Review focus 2.
    func testAnUnsentAnswerIsOwedAndResentWithTheSameOp() {
        row(Asks.approvalAttention())
        let first = answer(.option("allow"))
        fold.unsent(op: "op-1", macLabel: "Morgan's MacBook Pro")
        XCTAssertEqual(fold.card?.owed, true)
        XCTAssertEqual(fold.card?.note, "Couldn't reach Morgan's MacBook Pro — trying again")
        row(nil)
        XCTAssertNotNil(fold.card, "an owed answer outlives the row; its resend decides")
        XCTAssertEqual(fold.resend(sessionID: "s1"), first)
        XCTAssertNil(fold.resend(sessionID: "s1"), "resent once per redial")
        XCTAssertEqual(ids.minted, ["op-1"])
    }

    /// After a redial the Mac answers the resend from its record, and says no
    /// row: the question the row named meanwhile must pin then.
    func testARowSaidWhileTheAnswerWasOwedPinsOnceItsOkLands() {
        row(Asks.approvalAttention("r-1"))
        _ = answer(.option("allow"))
        fold.unsent(op: "op-1", macLabel: "the Mac")
        row(Asks.approvalAttention("r-2"))
        XCTAssertEqual(fold.card?.ask.id, "r-1")
        XCTAssertNotNil(fold.resend(sessionID: "s1"))
        XCTAssertFalse(fold.replied(.ok, op: "op-1"))
        XCTAssertEqual(fold.card?.ask.id, "r-2")
        XCTAssertNil(fold.cleared)
    }

    /// A row read on the door that dropped may be stale by the time the
    /// answer lands: only a row read on the door after it pins.
    func testARowFromBeforeTheDropDoesNotPinOnceTheResentAnswerLands() {
        row(Asks.approvalAttention("r-1"))
        _ = answer(.option("allow"))
        row(Asks.approvalAttention("r-2"))
        fold.unsent(op: "op-1", macLabel: "the Mac")
        XCTAssertNotNil(fold.resend(sessionID: "s1"))
        XCTAssertFalse(fold.replied(.ok, op: "op-1"))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    func testARowNamingASettledIdLeavesAPendingCardAlone() throws {
        row(Asks.approvalAttention("r-1"))
        try said(Asks.answered("r-1", option: "deny"))
        row(Asks.approvalAttention("r-2"))
        row(Asks.approvalAttention("r-1"))
        XCTAssertEqual(fold.card?.ask.id, "r-2", "a stale row does not clear the card")
        _ = answer(.option("allow"))
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "harness", message: "busy", reason: nil)), op: "op-1"))
        row(Asks.approvalAttention("r-1"))
        XCTAssertEqual(fold.card?.ask.id, "r-2")
        XCTAssertEqual(fold.card?.checking, true, "a stale read is not the read the refusal waits on")
        XCTAssertNil(fold.cleared)
    }

    func testAReplyToAnEarlierAnswerIsNotThisCards() {
        row(Asks.formAttention())
        _ = answer(.accept(.object(["branch": .string("")])))
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "invalid_answer", message: "branch: Required.", reason: nil)), op: "op-1"))
        row(Asks.formAttention())
        XCTAssertNotNil(answer(.decline))
        XCTAssertFalse(fold.replied(.ok, op: "op-1"))
        fold.unsent(op: "op-1", macLabel: "the Mac")
        XCTAssertEqual(fold.card?.sending, true, "the earlier answer's reply and drop are not this one's")
        XCTAssertFalse(fold.replied(.ok, op: "op-2"))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    func testAnAnswerToAnExpiredQuestionIsNotSentAndSaysSo() {
        row(Asks.questionAttention(expires: "2026-06-02T00:00:00Z"))
        XCTAssertNotNil(fold.card)
        let later = WireDate.parse("2026-06-03T00:00:00Z")!
        XCTAssertNil(fold.answer(.accept(.object(["why": .string("tidy")])), sessionID: "s1", now: later, op: ids.next))
        XCTAssertEqual(ids.minted, [])
        XCTAssertNil(fold.card)
        XCTAssertEqual(fold.cleared, CardFold.expired)
    }

    func testAnotherOptionAnsweredWhileOursIsOutSaysAnotherDevice() throws {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        try said(Asks.answered("r-1", option: "deny"))
        XCTAssertNil(fold.card)
        XCTAssertEqual(fold.cleared, CardFold.answeredElsewhere)
    }

    /// The answer did reach the Mac: only its reply was lost.
    func testOurOptionsEchoWhileOwedIsOurs() throws {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        fold.unsent(op: "op-1", macLabel: "the Mac")
        try said(Asks.answered("r-1", option: "allow"))
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
        XCTAssertNil(fold.resend(sessionID: "s1"), "nothing is owed")
    }

    func testARefusalWhileOwedIsWeighedAgainstTheRowAndNotResent() {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        fold.unsent(op: "op-1", macLabel: "the Mac")
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "harness", message: "busy", reason: nil)), op: "op-1"))
        XCTAssertEqual(fold.card?.checking, true)
        XCTAssertEqual(fold.card?.owed, false)
        XCTAssertNil(fold.resend(sessionID: "s1"))
        row(nil)
        XCTAssertEqual(fold.cleared, CardFold.answeredElsewhere)
    }

    func testALateRowAfterOurEchoDoesNotPinAgain() throws {
        row(Asks.approvalAttention())
        _ = answer(.option("allow"))
        try said(Asks.answered("r-1", option: "allow"))
        row(Asks.approvalAttention())
        XCTAssertNil(fold.card)
        XCTAssertNil(fold.cleared)
    }

    func testALateAskedForASettledIdDoesNotPin() throws {
        row(Asks.approvalAttention())
        try said(Asks.answered("r-1", option: "deny"))
        try said(.object(["kind": .string("asked"), "request": Asks.approvalJSON("r-1")]))
        XCTAssertNil(fold.card)
        XCTAssertEqual(fold.cleared, CardFold.answeredElsewhere)
    }

    /// The ruling: a new card takes the line away.
    func testARefusalWhoseRowNamesAnotherQuestionPinsThatOne() {
        row(Asks.approvalAttention("r-1"))
        _ = answer(.option("allow"))
        XCTAssertTrue(fold.replied(.error(DoorRefusal(code: "harness", message: "busy", reason: nil)), op: "op-1"))
        row(Asks.approvalAttention("r-2"))
        XCTAssertEqual(fold.card?.ask.id, "r-2")
        XCTAssertNil(fold.cleared)
        row(Asks.approvalAttention("r-1"))
        XCTAssertEqual(fold.card?.ask.id, "r-2", "r-1 was settled when its row went")
    }
}
