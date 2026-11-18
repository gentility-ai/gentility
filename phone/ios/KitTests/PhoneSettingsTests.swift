import XCTest
@testable import DeskPhoneKit

final class PhoneSettingsTests: XCTestCase {
    private func settings(expires: String) -> PhoneSettings {
        PhoneSettings(name: "Morgan's iPhone", server: "mcp.gentility.test", expires: WireDate.parse(expires)!, connection: .connected, notificationsOff: false)
    }

    private let now = WireDate.parse("2026-06-01T12:00:00Z")!

    func testTheDaysLeftAreCountedUp() {
        XCTAssertEqual(settings(expires: "2026-06-30T12:00:00Z").left(now: now), "29 days left")
        XCTAssertEqual(settings(expires: "2026-06-03T11:00:00Z").left(now: now), "2 days left")
        XCTAssertEqual(settings(expires: "2026-06-01T13:00:00Z").left(now: now), "1 day left")
        XCTAssertEqual(settings(expires: "2026-06-01T12:00:00Z").left(now: now), "Run out")
    }

    func testTheLastWeekIsSoon() {
        XCTAssertFalse(settings(expires: "2026-06-30T12:00:00Z").endsSoon(now: now))
        XCTAssertTrue(settings(expires: "2026-06-08T12:00:00Z").endsSoon(now: now))
    }

    func testEachConnectionHasItsWords() {
        XCTAssertEqual(PhoneSettings.Connection.connected.words, "Connected")
        XCTAssertEqual(PhoneSettings.Connection.connecting.words, "Connecting…")
        XCTAssertEqual(PhoneSettings.Connection.off.words, "Not connected")
        XCTAssertEqual(PhoneSettings.Connection.failed.words, "Can't connect")
    }
}
