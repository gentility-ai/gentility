import XCTest
@testable import DeskPhoneKit

final class AttachmentTrayTests: XCTestCase {
    static func prepared(_ name: String = "IMG_1.jpg") -> PreparedImage {
        PreparedImage(name: name, mime: "image/jpeg", data: Data(name.utf8), thumbnail: Data([1]), width: 10, height: 10)
    }

    static func ref(_ name: String) -> AttachmentRef {
        AttachmentRef(path: "/att/\(name)", name: name, kind: .image, size: 4, mime: "image/jpeg", hash: name)
    }

    private let a = UUID(), b = UUID(), c = UUID()

    func testAPhotoAddedIsPreparedThenWaitsToGo() {
        var tray = AttachmentTray()
        XCTAssertTrue(tray.add(a))
        XCTAssertEqual(tray.items.first?.state, .preparing)
        XCTAssertNil(tray.nextUpload, "nothing to put while it is read")
        tray.prepared(a, Self.prepared())
        XCTAssertEqual(tray.items.first?.state, .uploading)
        XCTAssertEqual(tray.nextUpload?.id, a)
        XCTAssertEqual(tray.uploadingLine, "Uploading 1 photo…")
    }

    func testUploadsGoInTheTraysOrder() {
        var tray = AttachmentTray()
        tray.add(a); tray.add(b)
        tray.prepared(b, Self.prepared("b"))
        tray.prepared(a, Self.prepared("a"))
        XCTAssertEqual(tray.nextUpload?.id, a)
        tray.uploaded(a, Self.ref("a"))
        XCTAssertEqual(tray.nextUpload?.id, b)
    }

    func testARefOfAKindThisAppDoesNotKnowIsNotSentOn() {
        var tray = AttachmentTray()
        tray.add(a)
        tray.prepared(a, Self.prepared())
        tray.uploaded(a, AttachmentRef(path: "/att/a", name: "a", kind: .unknown, size: 4))
        XCTAssertEqual(tray.items.first?.state, .failed(.notSaved))
        XCTAssertEqual(tray.refs, [])
    }

    func testAFailedPutIsTriedAgain() {
        var tray = AttachmentTray()
        tray.add(a)
        tray.prepared(a, Self.prepared())
        tray.failed(a, .unreachable)
        XCTAssertEqual(tray.items.first?.state, .failed(.unreachable))
        XCTAssertNil(tray.nextUpload)
        tray.retry(a)
        XCTAssertEqual(tray.nextUpload?.id, a)
    }

    func testAPhotoThatNeverReadCannotBeTriedAgain() {
        var tray = AttachmentTray()
        tray.add(a)
        tray.failed(a, .unreadable)
        tray.retry(a)
        XCTAssertEqual(tray.items.first?.state, .failed(.unreadable))
    }

    func testRemovedDuringItsPutStaysRemoved() {
        var tray = AttachmentTray()
        tray.add(a)
        tray.prepared(a, Self.prepared())
        tray.remove(a)
        tray.uploaded(a, Self.ref("a"))
        XCTAssertTrue(tray.isEmpty)
        XCTAssertEqual(tray.refs, [])
    }

    func testTheTwentyFirstIsRefused() {
        var tray = AttachmentTray()
        for _ in 0..<20 { XCTAssertTrue(tray.add(UUID())) }
        XCTAssertFalse(tray.add(UUID()))
        XCTAssertEqual(tray.count, 20)
        XCTAssertEqual(tray.room, 0)
        XCTAssertEqual(AttachmentTray.full, "Up to 20 photos per message.")
    }

    func testCanSendAcrossStates() {
        var tray = AttachmentTray()
        XCTAssertFalse(tray.canSend("  "))
        XCTAssertTrue(tray.canSend("hi"))
        tray.add(a)
        XCTAssertFalse(tray.canSend("hi"), "a photo still being read")
        tray.prepared(a, Self.prepared())
        XCTAssertFalse(tray.canSend("hi"), "a photo still going")
        tray.uploaded(a, Self.ref("a"))
        XCTAssertTrue(tray.canSend(""), "a ready photo alone sends")
        tray.add(b)
        tray.prepared(b, Self.prepared("b"))
        tray.failed(b, .notSaved)
        XCTAssertFalse(tray.canSend("hi"), "a failed photo blocks send")
        tray.remove(b)
        XCTAssertTrue(tray.canSend("hi"))
    }

    func testTheRefsGoInTheTraysOrder() {
        var tray = AttachmentTray()
        tray.add(a); tray.add(b); tray.add(c)
        for (id, name) in [(c, "c"), (a, "a"), (b, "b")] {
            tray.prepared(id, Self.prepared(name))
            tray.uploaded(id, Self.ref(name))
        }
        XCTAssertEqual(tray.refs.map(\.name), ["a", "b", "c"])
    }

    func testTheUploadingLineCounts() {
        var tray = AttachmentTray()
        XCTAssertNil(tray.uploadingLine)
        tray.add(a); tray.add(b)
        XCTAssertEqual(tray.uploadingLine, "Uploading 2 photos…")
    }

    func testEachPhotoSaysItsPlaceAndState() {
        var tray = AttachmentTray()
        tray.add(a); tray.add(b); tray.add(c)
        tray.prepared(b, Self.prepared())
        tray.uploaded(b, Self.ref("b"))
        tray.failed(c, .tooLarge)
        XCTAssertEqual(tray.accessibilityLabel(a), "Photo 1 of 3, uploading")
        XCTAssertEqual(tray.accessibilityLabel(b), "Photo 2 of 3, ready")
        XCTAssertEqual(tray.accessibilityLabel(c), "Photo 3 of 3, failed, double tap for options")
    }

    func testEachFailureHasItsSentence() {
        XCTAssertEqual(TrayError.tooLarge.sentence, "This photo is too big to send (20 MB max).")
        XCTAssertEqual(TrayError.cannotAttach(nil).sentence, "This agent can't read images. Remove the photo to send.")
        XCTAssertEqual(TrayError.cannotAttach("Codex reads no images.").sentence, "Codex reads no images.")
        XCTAssertEqual(TrayError.unreachable.sentence, "Couldn't reach your Mac. Try again when it's awake.")
        XCTAssertEqual(TrayError.notSaved.sentence, "Your Mac couldn't save this photo. Try again.")
        XCTAssertEqual(TrayError.unreadable.sentence, "Couldn't read this photo.")
        XCTAssertEqual(TrayError.refused(DoorRefusal(code: "too_large", message: "x", reason: nil)), .tooLarge)
        XCTAssertEqual(TrayError.refused(DoorRefusal(code: "io", message: "x", reason: nil)), .notSaved)
    }
}
