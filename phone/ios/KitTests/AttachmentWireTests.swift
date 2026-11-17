import XCTest
@testable import DeskPhoneKit

/// The attachment frames, in the shapes the Mac's Desk writes and reads: an
/// attachment put, an attachment read and a file image read, answered with an
/// attachment reference or its bytes.
final class AttachmentWireTests: XCTestCase {
    static let refJSON = #"{"path":"/Users/someone/Library/Application Support/Desk/attachments/ab12/IMG_1.jpg","name":"IMG_1.jpg","kind":"image","size":48213,"mime":"image/jpeg","hash":"ab12"}"#
    static let ref = AttachmentRef(
        path: "/Users/someone/Library/Application Support/Desk/attachments/ab12/IMG_1.jpg", name: "IMG_1.jpg",
        kind: .image, size: 48213, mime: "image/jpeg", hash: "ab12"
    )

    func testTheLargestFrameIsTheCores() {
        XCTAssertEqual(DoorLimits.largestFrame, 32 * 1024 * 1024)
    }

    func testTheServerSocketTakesFramesUpToTheLargest() {
        let session = URLSession(configuration: .ephemeral)
        defer { session.invalidateAndCancel() }
        let task = URLSessionTransport.task(session, URL(string: "wss://example.invalid/socket")!)
        XCTAssertEqual(task.maximumMessageSize, DoorLimits.largestFrame)
    }

    func testThePutReadAndImageReadAreTheFramesTheCoreReads() throws {
        XCTAssertEqual(
            try DoorRequest.attachmentPut(name: "IMG_1.jpg", mime: "image/jpeg", bytesB64: "AAEC").frame(id: 1),
            #"{"id":1,"request":{"bytes_b64":"AAEC","mime":"image/jpeg","name":"IMG_1.jpg","type":"attachment_put"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.attachmentRead(path: "/a/b.png").frame(id: 2),
            #"{"id":2,"request":{"path":"/a/b.png","type":"attachment_read"}}"#
        )
        XCTAssertEqual(
            try DoorRequest.fileImageRead(sessionID: "s1", path: "docs/shot.png").frame(id: 3),
            #"{"id":3,"request":{"path":"docs/shot.png","session_id":"s1","type":"file_image_read"}}"#
        )
    }

    func testAPromptWithoutAttachmentsIsTheFrameItWasBefore() throws {
        XCTAssertEqual(
            try DoorRequest.sessionPrompt(sessionID: "s1", text: "hi", queue: false, op: "op-1", attachments: []).frame(id: 4),
            #"{"id":4,"request":{"op":"op-1","session_id":"s1","text":"hi","type":"session_prompt"}}"#
        )
    }

    func testAPromptCarriesItsRefsAsReceived() throws {
        let other = AttachmentRef(path: "/x/y.png", name: "y.png", kind: .image, size: nil)
        let frame = try DoorRequest.sessionPrompt(sessionID: "s1", text: "", queue: true, op: "op-2", attachments: [Self.ref, other]).frame(id: 5)
        let sent = try JSONValue.parse(Data(frame.utf8))["request"]
        XCTAssertEqual(sent?["attachments"], .array([
            try JSONValue.parse(Data(Self.refJSON.utf8)),
            .object(["path": .string("/x/y.png"), "name": .string("y.png"), "kind": .string("image"), "size": .null]),
        ]))
        XCTAssertEqual(sent?["queue"], .bool(true))
    }

    func testTheAttachmentReplyDecodesToItsRef() throws {
        let frame = try DoorFrame(#"{"id":7,"response":{"type":"attachment","attachment":\#(Self.refJSON)}}"#)
        XCTAssertEqual(frame, .reply(id: 7, .attachment(Self.ref)))
    }

    func testTheBytesReplyDecodes() throws {
        let frame = try DoorFrame(#"{"id":8,"response":{"type":"attachment_bytes","mime":"image/png","bytes_b64":"iVBO"}}"#)
        XCTAssertEqual(frame, .reply(id: 8, .attachmentBytes(mime: "image/png", bytesB64: "iVBO")))
    }

    func testAnUnknownKindIsUnknownNotAFileAndHasNoSize() throws {
        let ref = try JSONValue.parse(Data(#"{"path":"/p","name":"p","kind":"socket","size":null}"#.utf8)).decode(AttachmentRef.self)
        XCTAssertEqual(ref, AttachmentRef(path: "/p", name: "p", kind: .unknown, size: nil))
    }

    func testATranscriptItemCarriesItsAttachmentsOrNone() throws {
        let with = try JSONValue.parse(Data(#"""
        {"id":"u1","session_id":"s","ordinal":1,"kind":"message","role":"user","content":"look","turn":1,"parent_call_id":null,"created_at":"2026-01-01T00:00:00Z","attachments":[\#(Self.refJSON)]}
        """#.utf8)).decode(TranscriptItem.self)
        XCTAssertEqual(with.attachments, [Self.ref])
        XCTAssertEqual(TranscriptFold.line(with), .you(id: "u1", text: "look", attachments: [Self.ref]))

        let without = try JSONValue.parse(Data(#"""
        {"id":"u2","session_id":"s","ordinal":2,"kind":"message","role":"user","content":"plain","turn":1,"created_at":"2026-01-01T00:00:00Z"}
        """#.utf8)).decode(TranscriptItem.self)
        XCTAssertNil(without.attachments)
        XCTAssertEqual(TranscriptFold.line(without), .you(id: "u2", text: "plain"))
    }

    func testARowCarriesItsWorktree() throws {
        let row = try JSONValue.parse(Data(#"{"id":"s","title":"t","status":"idle","working":false,"turns":1,"worktree_path":"/Users/someone/wt/fix"}"#.utf8)).decode(DoorSession.self)
        XCTAssertEqual(row.worktreePath, "/Users/someone/wt/fix")
    }

    func testAPutWaitsAMinuteAndAReadHalfOne() {
        XCTAssertEqual(DoorConnection.patience(for: .attachmentPut(name: "n", mime: "image/jpeg", bytesB64: "")), .seconds(60))
        XCTAssertEqual(DoorConnection.patience(for: .attachmentRead(path: "/p")), .seconds(30))
        XCTAssertEqual(DoorConnection.patience(for: .fileImageRead(sessionID: "s", path: "p")), .seconds(30))
    }
}
