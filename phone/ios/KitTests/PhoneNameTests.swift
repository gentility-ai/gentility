import XCTest
@testable import DeskPhoneKit

final class PhoneNameTests: XCTestCase {
    func testTheDevicesNameIsTheDefaultAndAChosenOneIsRemembered() throws {
        let defaults = try XCTUnwrap(UserDefaults(suiteName: "test.\(UUID().uuidString)"))
        XCTAssertEqual(PhoneName.current(defaults: defaults, device: "iPhone"), "iPhone")
        PhoneName.remember("  Morgan's iPhone  ", defaults: defaults)
        XCTAssertEqual(PhoneName.current(defaults: defaults, device: "iPhone"), "Morgan's iPhone")
        PhoneName.remember("   ", defaults: defaults)
        XCTAssertEqual(PhoneName.current(defaults: defaults, device: "iPhone"), "iPhone", "a blank name is no name")
    }

    func testTheHostnameIsADNSLabel() {
        XCTAssertEqual(PhoneName.hostname("Morgan's iPhone"), "morgan-s-iphone")
        XCTAssertEqual(PhoneName.hostname("  ÉLAN 15 Pro!! "), "lan-15-pro")
        XCTAssertEqual(PhoneName.hostname("???"), "iphone")
        XCTAssertEqual(PhoneName.hostname(String(repeating: "a", count: 80)).count, 63)
        XCTAssertEqual(PhoneName.hostname(String(repeating: "a", count: 62) + " b"), String(repeating: "a", count: 62), "a label never ends in a hyphen")
    }
}
