import XCTest
@testable import DeskPhoneKit

final class FormDraftTests: XCTestCase {
    func testDefaultsArePrefilled() {
        let draft = FormDraft(fields: Asks.fields([
            Asks.field("branch", Asks.text(default: "main")),
            Asks.field("count", Asks.number(integer: true, default: 3)),
            Asks.field("force", Asks.boolean(default: true)),
            Asks.field("kind", Asks.choice(["fix", "feat"], default: "fix")),
        ]))
        XCTAssertEqual(draft.texts, ["branch": "main", "count": "3"])
        XCTAssertEqual(draft.flags, ["force": true])
        XCTAssertEqual(draft.picks, ["kind": "fix"])
    }

    func testARequiredFieldLeftEmptyIsRequired() {
        var draft = FormDraft(fields: Asks.fields([
            Asks.field("why", Asks.text(), required: true),
            Asks.field("kind", Asks.choice(["fix"]), required: true),
        ]))
        XCTAssertEqual(draft.problems(), ["why": "Required.", "kind": "Required."])
        XCTAssertNil(draft.content())
        draft.texts["why"] = "tidy"
        draft.picks["kind"] = "fix"
        XCTAssertEqual(draft.problems(), [:])
    }

    func testNumbersAreHeldToTheirKind() {
        var draft = FormDraft(fields: Asks.fields([Asks.field("n", Asks.number(integer: true, min: 1, max: 10))]))
        draft.texts["n"] = "ten"
        XCTAssertEqual(draft.problems(), ["n": "Not a number."])
        draft.texts["n"] = "2.5"
        XCTAssertEqual(draft.problems(), ["n": "A whole number."])
        draft.texts["n"] = "0"
        XCTAssertEqual(draft.problems(), ["n": "At least 1."])
        draft.texts["n"] = "11"
        XCTAssertEqual(draft.problems(), ["n": "At most 10."])
        draft.texts["n"] = " 7 "
        XCTAssertEqual(draft.content(), .object(["n": .number(7)]))
    }

    func testAWholeNumberTooLargeToCarryIsRefusedAsTheCoreRefusesIt() {
        var draft = FormDraft(fields: Asks.fields([Asks.field("n", Asks.number(integer: true))]))
        draft.texts["n"] = "9007199254740991"
        XCTAssertEqual(draft.problems(), [:])
        draft.texts["n"] = "9007199254740993"
        XCTAssertEqual(draft.problems(), ["n": "Too large to carry exactly."])
    }

    func testLengthsCountCodePointsAsTheCoreDoes() {
        var draft = FormDraft(fields: Asks.fields([Asks.field("t", Asks.text(min: 2, max: 3))]))
        draft.texts["t"] = "🙂🙂🙂"
        XCTAssertEqual(draft.problems(), [:])
        draft.texts["t"] = "abcd"
        XCTAssertEqual(draft.problems(), ["t": "At most 3 characters."])
        draft.texts["t"] = "a"
        XCTAssertEqual(draft.problems(), ["t": "At least 2 characters."])
    }

    func testTheContentHoldsWhatWasGivenAndEveryToggle() {
        var draft = FormDraft(fields: Asks.fields([
            Asks.field("branch", Asks.text()),
            Asks.field("note", Asks.text()),
            Asks.field("force", Asks.boolean()),
            Asks.field("kind", Asks.choice(["fix", "feat"])),
        ]))
        draft.texts["branch"] = "main"
        draft.picks["kind"] = "feat"
        XCTAssertEqual(draft.content(), .object(["branch": .string("main"), "force": .bool(false), "kind": .string("feat")]))
    }

    func testAPickManyFieldIsAnsweredOnTheMac() {
        XCTAssertFalse(FormDraft(fields: Asks.fields([Asks.field("m", Asks.multiChoice(["a", "b"]))])).answerableHere)
        XCTAssertTrue(FormDraft(fields: Asks.fields([Asks.field("t", Asks.text())])).answerableHere)
    }

    // The recorded form (`said_elicited`): its numbers are f64 on the wire,
    // `1.0` and `5.0` even for a whole-number field.

    func testTheRecordedFormIsPrefilledAndBoundedAsAPersonWritesIt() throws {
        var draft = FormDraft(fields: try recordedFields())
        XCTAssertEqual(draft.texts, ["count": "1"])
        XCTAssertEqual(draft.flags, ["notify": false])
        XCTAssertEqual(draft.picks, [:])
        XCTAssertEqual(draft.problems(), ["email": "Required."])
        draft.texts["email"] = "ada@example.test"
        draft.texts["count"] = "6"
        XCTAssertEqual(draft.problems(), ["count": "At most 5."])
        draft.texts["count"] = "0"
        XCTAssertEqual(draft.problems(), ["count": "At least 1."])
    }

    func testTheRecordedFormFilledInIsTheContentTheMacDecided() throws {
        var draft = FormDraft(fields: try recordedFields())
        draft.texts["count"] = "3"
        draft.texts["email"] = "ada@example.test"
        let decided = try Fixture.door("said_elicitation_decided")["event"]?["said"]?["decision"]?["content"]
        XCTAssertEqual(draft.content(), decided)
        // A JSON number, which the core reads as an f64; never the typed text.
        XCTAssertEqual(try draft.content()?.text(), #"{"count":3,"email":"ada@example.test","notify":false}"#)
    }

    private func recordedFields() throws -> [FormField] {
        guard case .event(.said(_, _, .elicited(let elicitation))) = try DoorFrame(Fixture.doorText("said_elicited")),
              case .form(_, let fields) = elicitation.mode else {
            throw FixtureError.missing("said_elicited")
        }
        return fields
    }
}
