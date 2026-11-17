import XCTest
@testable import DeskPhoneKit

final class OpenReportTests: XCTestCase {
    func testTheLineIsTheSpecs() {
        let line = OpenReport.line(
            session: "s-1", shown: .milliseconds(840), dial: .milliseconds(70), hello: .milliseconds(12),
            transcript: Cost(bytes: 136_192, waited: .milliseconds(610), decoded: .milliseconds(18)),
            row: Cost(bytes: 2_048, waited: .milliseconds(140), decoded: .milliseconds(1)),
            path: PeerPath(path: "direct", via: "[fd8c::1]:41641", relay: nil)
        )
        XCTAssertEqual(line, "opened s-1 in 840 ms - dial 70, hello 12, transcript 610 (133 KB, decode 18), row 140 (2 KB); path direct [fd8c::1]:41641")
    }

    func testWhatWasNotMeasuredIsLeftOut() {
        let line = OpenReport.line(session: "s-1", shown: .milliseconds(5), dial: nil, hello: nil, transcript: nil, row: nil, path: nil)
        XCTAssertEqual(line, "opened s-1 in 5 ms")
    }

    func testBytesRoundToTheNearestKB() {
        XCTAssertEqual(OpenReport.kb(1_023), 1)
        XCTAssertEqual(OpenReport.kb(1_536), 2)
        XCTAssertEqual(OpenReport.kb(300), 0)
        XCTAssertEqual(OpenReport.ms(.microseconds(1_499)), 1)
    }
}
