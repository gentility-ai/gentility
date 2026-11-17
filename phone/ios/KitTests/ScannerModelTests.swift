import XCTest
@testable import DeskPhoneKit

final class FakePermission: CameraPermission, @unchecked Sendable {
    var current: CameraAccess
    var grants: Bool
    private(set) var asked = 0

    init(_ current: CameraAccess, grants: Bool = true) {
        self.current = current
        self.grants = grants
    }

    func status() -> CameraAccess { current }

    func request() async -> Bool {
        asked += 1
        current = grants ? .granted : .denied
        return grants
    }
}

@MainActor
final class ScannerModelTests: XCTestCase {
    private let good = "https://mcp.gentility.ai/phone/claim#q0Zr8Xr3n1mE5yJpS6sHhV0W8Ykz2bT9cLdQe4fGa7U"

    func testGrantedAccessScans() async {
        let scanner = ScannerModel(permission: FakePermission(.granted))
        await scanner.appear()
        XCTAssertEqual(scanner.state, .scanning)
    }

    func testDeniedAccessSaysHowToFixItAndNeverAsksAgain() async {
        let permission = FakePermission(.denied)
        let scanner = ScannerModel(permission: permission)
        await scanner.appear()
        XCTAssertEqual(scanner.state, .denied)
        XCTAssertEqual(permission.asked, 0, "iOS will not ask twice; Settings is the way")
    }

    func testRestrictedAccessIsDeniedToo() async {
        let scanner = ScannerModel(permission: FakePermission(.restricted))
        await scanner.appear()
        XCTAssertEqual(scanner.state, .denied)
    }

    func testAFirstLaunchAsksAndARefusalLandsOnDenied() async {
        let permission = FakePermission(.undetermined, grants: false)
        let scanner = ScannerModel(permission: permission)
        await scanner.appear()
        XCTAssertEqual(permission.asked, 1)
        XCTAssertEqual(scanner.state, .denied)
    }

    func testAJunkCodeSaysNotADeskCodeAndKeepsScanning() async {
        let scanner = ScannerModel(permission: FakePermission(.granted))
        var links: [ClaimLink] = []
        scanner.onLink = { links.append($0) }
        await scanner.appear()
        scanner.read("https://example.com/menu")
        XCTAssertEqual(scanner.state, .notADeskCode)
        scanner.read(good)
        XCTAssertEqual(links.count, 1)
    }

    func testOneCodeSeenManyTimesIsTakenOnce() async {
        let scanner = ScannerModel(permission: FakePermission(.granted))
        var links: [ClaimLink] = []
        scanner.onLink = { links.append($0) }
        await scanner.appear()
        scanner.read(good)
        scanner.read(good)
        scanner.read(good)
        XCTAssertEqual(links.count, 1)
        scanner.reset()
        scanner.read(good)
        XCTAssertEqual(links.count, 2, "scan again after a failure")
    }
}
