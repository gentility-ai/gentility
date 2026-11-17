import XCTest
@testable import DeskPhoneKit

final class FixturesTests: XCTestCase {
    func testTheRecordedServerContractIsInTheBundle() throws {
        let list = try Fixture.server("work_sessions/list")
        XCTAssertEqual(list["topic"]?.string, "work_sessions:<org>")
        XCTAssertEqual(list["event"]?.string, "list")
        XCTAssertEqual(list["reply"]?["status"]?.string, "ok")
    }

    func testAValueRoundTripsAndKeepsItsSnakeCaseKeys() throws {
        let register = try Fixture.server("phone/register")
        let again = try JSONValue.parse(register.data())
        XCTAssertEqual(again, register)
        XCTAssertEqual(register["payload"]?["push_token"]?.string, "tok")
        XCTAssertEqual(register["reply"]?["response"]?["device"]?["has_push"]?.bool, true)
    }

    func testNumbersBooleansAndNullsKeepTheirKinds() throws {
        let row = try XCTUnwrap(Fixture.response("work_sessions/list")["sessions"]?.array?.first)
        XCTAssertEqual(row["turns"]?.int, 0)
        XCTAssertEqual(row["working"]?.bool, false)
        XCTAssertEqual(row["archived_at"], .null)
    }
}
