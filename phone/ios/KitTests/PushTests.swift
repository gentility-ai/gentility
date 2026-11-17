import XCTest
@testable import DeskPhoneKit

final class PushTests: XCTestCase {
    /// A provisioning profile as the bundle carries it: Apple's signed
    /// envelope (here, stand-in bytes either side) around the XML plist.
    private func profile(_ apsEnvironment: String?) -> Data {
        let entitlement = apsEnvironment.map { "<key>aps-environment</key><string>\($0)</string>" } ?? ""
        let plist = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0"><dict>
        <key>Name</key><string>iOS Team Provisioning Profile: ai.gentility.desk.phone</string>
        <key>TeamIdentifier</key><array><string>EXAMPLETEAM</string></array>
        <key>Entitlements</key><dict>
        <key>application-identifier</key><string>EXAMPLETEAM.ai.gentility.desk.phone</string>
        \(entitlement)
        </dict></dict></plist>
        """
        return Data([0x30, 0x82, 0x2a, 0x1f, 0x06, 0x09, 0xff, 0x00]) + Data(plist.utf8) + Data([0xa0, 0x82, 0x0b, 0x00, 0x00, 0xfe])
    }

    private func bundle() throws -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("bundle-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    func testADevelopmentProfileIsSandboxAndADistributionOneProduction() {
        XCTAssertEqual(PushEnvironment.read(profile: profile("development")), "sandbox")
        XCTAssertEqual(PushEnvironment.read(profile: profile("production")), "production")
    }

    /// The simulator carries no profile; App Store and TestFlight installs
    /// neither. No word is sent, and the server pushes through production.
    func testNoProfileIsNoEnvironment() throws {
        XCTAssertNil(PushEnvironment.read(profile: nil))
        XCTAssertNil(PushEnvironment.current(in: try bundle()))
    }

    func testAProfileInTheBundleIsRead() throws {
        let bundle = try bundle()
        try profile("development").write(to: bundle.appendingPathComponent("embedded.mobileprovision"))
        XCTAssertEqual(PushEnvironment.current(in: bundle), "sandbox")
    }

    func testAProfileThatDoesNotSayIsNoEnvironment() {
        XCTAssertNil(PushEnvironment.read(profile: profile(nil)), "no aps-environment: the push entitlement is missing")
        XCTAssertNil(PushEnvironment.read(profile: profile("staging")))
        XCTAssertNil(PushEnvironment.read(profile: Data("not a profile".utf8)))
        XCTAssertNil(PushEnvironment.read(profile: Data()))
        XCTAssertNil(PushEnvironment.read(profile: Data("<?xml version=\"1.0\"?><plist><dict><key>Entitlements".utf8)), "cut short: no </plist>")
        XCTAssertNil(PushEnvironment.read(profile: Data("</plist> before <?xml version=\"1.0\"?>".utf8)), "the end before the start")
        XCTAssertNil(PushEnvironment.read(profile: Data(#"<?xml version="1.0"?><plist version="1.0"><array><string>development</string></array></plist>"#.utf8)), "not a dictionary")
    }

    func testATokenIsLowerCaseHex() {
        XCTAssertEqual(DeviceToken.hex(Data([0x00, 0xab, 0x0f, 0xff])), "00ab0fff")
        XCTAssertEqual(DeviceToken.hex(Data()), "")
    }

    /// The notification body, as APNs hands
    /// it to the app. No exchange of it is recorded, so it is built here to
    /// that function's keys.
    private func userInfo(_ body: String) throws -> [AnyHashable: Any] {
        try XCTUnwrap(JSONSerialization.jsonObject(with: Data(body.utf8)) as? [AnyHashable: Any])
    }

    func testThePayloadNamesTheConversation() throws {
        let body = #"""
        {"aps":{"alert":{"title":"Fix the recap timing","body":"Wants to execute"},"sound":"default","thread-id":"s-1"},
         "org_id":"o-1","session_id":"s-1","desk_machine_id":null,"fact":"approval_requested"}
        """#
        XCTAssertEqual(PushPayload(userInfo: try userInfo(body)), PushPayload(organizationID: "o-1", sessionID: "s-1", deskMachineID: nil, fact: "approval_requested"))
        let placed = #"{"org_id":"o-1","session_id":"s-1","desk_machine_id":"m-1","fact":"turn_completed"}"#
        XCTAssertEqual(PushPayload(userInfo: try userInfo(placed))?.deskMachineID, "m-1")
    }

    func testAMalformedPayloadOpensNothing() throws {
        XCTAssertNil(PushPayload(userInfo: ["org_id": "o-1"]), "no session to open")
        XCTAssertNil(PushPayload(userInfo: ["session_id": "s-1"]), "no organisation to find it in")
        XCTAssertNil(PushPayload(userInfo: try userInfo(#"{"org_id":"o-1","session_id":7,"fact":"approval_requested"}"#)), "a session that is not a string")
        XCTAssertNil(PushPayload(userInfo: try userInfo(#"{"org_id":"","session_id":"s-1","fact":"approval_requested"}"#)), "an empty organisation")
        XCTAssertNil(PushPayload(userInfo: [:]))
    }
}
