import XCTest
@testable import DeskPhoneKit

/// A photo put through the door, and the prompt that carries it.
@MainActor
final class AttachmentSendTests: XCTestCase {
    private var bridge: FakeBridge!
    private var ids: Ids!
    private var opening: Opening { Opening(bridge: bridge) }

    override func setUp() async throws {
        bridge = FakeBridge()
        ids = Ids()
    }

    private func opened(working: Bool = false) async throws -> ConversationModel {
        let conversation = Conversations.model(bridge, ids: ids)
        async let open: Void = conversation.open()
        try await opening.answer(working: working)
        await open
        XCTAssertEqual(conversation.phase, .open)
        return conversation
    }

    private func attachment(_ ref: AttachmentRef) -> JSONValue {
        .object(["response": .object(["type": .string("attachment"), "attachment": ref.json])])
    }

    func testTheRecordedHelloOffersAttachments() async throws {
        let conversation = try await opened()
        XCTAssertTrue(conversation.canAttach)
    }

    func testAPhotoIsPutThenSentWithTheWords() async throws {
        let conversation = try await opened()
        let image = AttachmentTrayTests.prepared()
        conversation.attach(prepared: image)
        let put = await bridge.nextRequest()
        XCTAssertEqual(put.request, .object([
            "type": .string("attachment_put"), "name": .string("IMG_1.jpg"), "mime": .string("image/jpeg"),
            "bytes_b64": .string(image.data.base64EncodedString()),
        ]))
        XCTAssertFalse(conversation.tray.canSend("look"), "send waits for the put")
        let ref = AttachmentTrayTests.ref("IMG_1.jpg")
        bridge.say(attachment(ref), id: put.id)
        await conversation.uploadsSettled()
        XCTAssertEqual(conversation.tray.refs, [ref])

        async let took = conversation.send("look at this")
        let prompt = await bridge.nextRequest()
        XCTAssertEqual(prompt.request["attachments"], .array([ref.json]))
        XCTAssertEqual(prompt.request["text"], .string("look at this"))
        bridge.say(try Fixture.door("prompt"), id: prompt.id)
        let sent = await took
        XCTAssertTrue(sent)
        XCTAssertTrue(conversation.tray.isEmpty, "the tray clears once the Mac took it")
    }

    func testAPhotoAloneSends() async throws {
        let conversation = try await opened()
        conversation.attach(prepared: AttachmentTrayTests.prepared())
        let put = await bridge.nextRequest()
        bridge.say(attachment(AttachmentTrayTests.ref("IMG_1.jpg")), id: put.id)
        await conversation.uploadsSettled()
        async let took = conversation.send("  ")
        let prompt = await bridge.nextRequest()
        XCTAssertEqual(prompt.request["text"], .string(""))
        bridge.say(try Fixture.door("prompt"), id: prompt.id)
        let sent = await took
        XCTAssertTrue(sent)
    }

    func testPutsGoOneAtATimeInOrder() async throws {
        let conversation = try await opened()
        conversation.attach(prepared: AttachmentTrayTests.prepared("one.jpg"))
        conversation.attach(prepared: AttachmentTrayTests.prepared("two.jpg"))
        let first = await bridge.nextRequest()
        XCTAssertEqual(first.request["name"], .string("one.jpg"))
        XCTAssertEqual(bridge.sent.filter { $0.contains("attachment_put") }.count, 1, "the second waits")
        bridge.say(attachment(AttachmentTrayTests.ref("one.jpg")), id: first.id)
        let second = await bridge.nextRequest()
        XCTAssertEqual(second.request["name"], .string("two.jpg"))
        bridge.say(attachment(AttachmentTrayTests.ref("two.jpg")), id: second.id)
        await conversation.uploadsSettled()
        XCTAssertEqual(conversation.tray.refs.map(\.name), ["one.jpg", "two.jpg"])
    }

    func testARefusedPutFailsAndIsTriedAgain() async throws {
        let conversation = try await opened()
        conversation.attach(prepared: AttachmentTrayTests.prepared())
        let put = await bridge.nextRequest()
        bridge.say(.object(["response": .object(["type": .string("error"), "code": .string("too_large"), "message": .string("too big")])]), id: put.id)
        await conversation.uploadsSettled()
        let id = try XCTUnwrap(conversation.tray.items.first?.id)
        XCTAssertEqual(conversation.tray.items.first?.state, .failed(.tooLarge))

        conversation.retryAttachment(id)
        let again = await bridge.nextRequest()
        XCTAssertEqual(again.request["type"], .string("attachment_put"))
        let ref = AttachmentTrayTests.ref("IMG_1.jpg")
        bridge.say(attachment(ref), id: again.id)
        await conversation.uploadsSettled()
        XCTAssertEqual(conversation.tray.items.first?.state, .ready(ref))
    }

    func testCannotAttachKeepsTheWordsAndThePhotos() async throws {
        let conversation = try await opened()
        conversation.attach(prepared: AttachmentTrayTests.prepared())
        let put = await bridge.nextRequest()
        bridge.say(attachment(AttachmentTrayTests.ref("IMG_1.jpg")), id: put.id)
        await conversation.uploadsSettled()
        async let took = conversation.send("look")
        let prompt = await bridge.nextRequest()
        bridge.say(.object(["response": .object([
            "type": .string("error"), "code": .string("cannot_attach"), "message": .string("This agent reads no images."),
        ])]), id: prompt.id)
        let sent = await took
        XCTAssertFalse(sent)
        XCTAssertEqual(conversation.tray.count, 1)
        XCTAssertEqual(conversation.composerNote, "This agent reads no images.")
    }

    func testAnImageIsReadFromTheWorktreeOrAsAnAttachment() async throws {
        let conversation = try await opened()
        async let worktree = conversation.imageBytes(.worktree(sessionID: "s", path: "shot.png"))
        let read = await bridge.nextRequest()
        XCTAssertEqual(read.request, .object(["type": .string("file_image_read"), "session_id": .string("s"), "path": .string("shot.png")]))
        bridge.say(.object(["response": .object(["type": .string("attachment_bytes"), "mime": .string("image/png"), "bytes_b64": .string("AAEC")])]), id: read.id)
        let bytes = try await worktree
        XCTAssertEqual(bytes, Data([0, 1, 2]))

        async let attached = conversation.imageBytes(.attachment(path: "/att/a.jpg"))
        let second = await bridge.nextRequest()
        XCTAssertEqual(second.request, .object(["type": .string("attachment_read"), "path": .string("/att/a.jpg")]))
        bridge.say(.object(["response": .object(["type": .string("error"), "code": .string("not_found"), "message": .string("gone")])]), id: second.id)
        do {
            _ = try await attached
            XCTFail("not found")
        } catch {
            XCTAssertEqual(error as? ImageLoadError, .notFound)
        }
    }
}
