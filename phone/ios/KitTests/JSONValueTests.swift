import XCTest
@testable import DeskPhoneKit

final class JSONValueTests: XCTestCase {
    func testAWholeNumberDecodesAsANumberAndReadsAsAnInt() throws {
        let one = try JSONValue.parse("1")
        XCTAssertEqual(one, .number(1))
        XCTAssertEqual(one.int, 1)
    }

    func testAFractionalSpellingStillDecodesAsANumber() throws {
        XCTAssertEqual(try JSONValue.parse("1.0"), .number(1))
    }

    func testTrueDecodesAsABoolNotANumber() throws {
        let yes = try JSONValue.parse("true")
        XCTAssertEqual(yes, .bool(true))
        XCTAssertNil(yes.int)
    }

    func testANumberTooLargeForAnIntReadsAsNoInt() throws {
        let huge = try JSONValue.parse("1e30")
        XCTAssertEqual(huge, .number(1e30))
        XCTAssertNil(huge.int)
    }
}
