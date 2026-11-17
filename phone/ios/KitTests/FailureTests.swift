import XCTest
@testable import DeskPhoneKit

/// The storyboard's words, frame by frame (F1, F2, F3, F5).
final class FailureTests: XCTestCase {
    func testF1() {
        XCTAssertEqual(Failure.codeSpent.title, "Code expired")
        XCTAssertEqual(Failure.codeSpent.lines, [
            "That code is more than 5 minutes old or was already used.",
            "Choose New code in Desk, then scan again.",
        ])
        XCTAssertEqual(Failure.codeSpent.action, "Scan again")
    }

    func testF2() {
        XCTAssertEqual(Failure.denied.title, "Not allowed")
        XCTAssertEqual(Failure.denied.lines, [
            "Your Mac said no to this phone.",
            "If that was a mistake, add the phone again from Desk.",
        ])
    }

    func testF3SaysEachWordInItsOwnWords() {
        XCTAssertEqual(Failure.cannotJoin("no_gateway").title, "Can't join yet")
        XCTAssertEqual(Failure.cannotJoin("no_gateway").lines, [
            "Your organisation's network isn't set up for phones yet.",
            "Contact Gentility support. Nothing was changed.",
        ])
        XCTAssertEqual(Failure.cannotJoin("blank_name").lines.first, "This phone needs a name.")
        XCTAssertEqual(Failure.cannotJoin("server_error").lines.first, "The server couldn't finish pairing this phone.")
        XCTAssertEqual(Failure.cannotJoin("a_new_word").lines.first, "The server couldn't finish pairing this phone.")
        XCTAssertEqual(Failure.unreachable(host: "mcp.gentility.ai").lines.first, "This phone couldn't reach mcp.gentility.ai.")
    }

    func testF5() {
        XCTAssertEqual(Failure.removed.title, "Phone removed")
        XCTAssertEqual(Failure.removed.lines, ["This phone was removed from your account."])
        XCTAssertEqual(Failure.removed.action, "Pair again")
        XCTAssertEqual(Failure.lapsed.action, "Pair again")
        XCTAssertEqual(Failure.notJoined.title, "Pairing didn't finish")
        XCTAssertEqual(Failure.notJoined.action, "Pair again")
    }

    func testClaimRefusalsLandOnTheirScreens() {
        XCTAssertEqual(Failure(ClaimRefusal.unknown), .codeSpent)
        XCTAssertEqual(Failure(ClaimRefusal.expired), .codeSpent)
        XCTAssertEqual(Failure(ClaimRefusal.spent), .codeSpent)
        XCTAssertEqual(Failure(ClaimRefusal.blankName), .cannotJoin("blank_name"))
        XCTAssertEqual(Failure(ClaimRefusal.malformed), .cannotJoin("server_error"))
        XCTAssertEqual(Failure(ClaimRefusal.other("redirected")), .cannotJoin("server_error"))
    }

    func testEveryScreenSaysSomething() {
        let all: [Failure] = [.codeSpent, .denied, .cannotJoin("no_gateway"), .cannotJoin("blank_name"), .cannotJoin("this_phone"),
                              .cannotJoin("server_error"), .unreachable(host: "h"), .removed, .lapsed, .notJoined]
        for failure in all {
            XCTAssertFalse(failure.title.isEmpty, "\(failure)")
            XCTAssertFalse(failure.lines.isEmpty, "\(failure)")
            XCTAssertFalse(failure.lines.contains(where: \.isEmpty), "\(failure)")
        }
    }
}
