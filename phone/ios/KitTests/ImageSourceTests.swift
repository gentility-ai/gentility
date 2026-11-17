import XCTest
@testable import DeskPhoneKit

final class ImageSourceTests: XCTestCase {
    private let root = "/Users/someone/wt/fix-login"

    private func request(_ src: String, root: String? = "/Users/someone/wt/fix-login") -> ImageRequest {
        ImageSource.request(for: src, sessionID: "s", worktreeRoot: root)
    }

    func testAPathUnderTheWorktreeIsReadRelativeToIt() {
        XCTAssertEqual(request("/Users/someone/wt/fix-login/shots/a.png"), .worktree(sessionID: "s", path: "shots/a.png"))
        XCTAssertEqual(request("/Users/someone/wt/fix-login/shots/a.png", root: "/Users/someone/wt/fix-login/"), .worktree(sessionID: "s", path: "shots/a.png"))
    }

    func testARelativePathIsTheWorktrees() {
        XCTAssertEqual(request("docs/flow.png"), .worktree(sessionID: "s", path: "docs/flow.png"))
    }

    func testAnyOtherAbsolutePathIsAnAttachment() {
        XCTAssertEqual(request("/Users/someone/Library/Desk/attachments/ab/IMG_1.jpg"), .attachment(path: "/Users/someone/Library/Desk/attachments/ab/IMG_1.jpg"))
        XCTAssertEqual(request("/Users/someone/wt/fix-login-other/a.png"), .attachment(path: "/Users/someone/wt/fix-login-other/a.png"))
    }

    func testWithNoRootKnownAPathIsAnAttachment() {
        XCTAssertEqual(request("/tmp/a.png", root: nil), .attachment(path: "/tmp/a.png"))
    }

    func testAFileURLIsItsPath() {
        XCTAssertEqual(request("file:///Users/someone/wt/fix-login/a.png"), .worktree(sessionID: "s", path: "a.png"))
    }

    func testPercentEncodingIsDecoded() {
        XCTAssertEqual(request("/tmp/a%20b.png"), .attachment(path: "/tmp/a b.png"))
    }

    func testTheWebWaitsForATap() {
        XCTAssertEqual(request("https://example.com/a.png"), .remote(URL(string: "https://example.com/a.png")!))
        XCTAssertEqual(request("HTTP://example.com/a.png"), .remote(URL(string: "HTTP://example.com/a.png")!))
    }

    func testADataURLIsItsBytes() {
        XCTAssertEqual(request("data:image/png;base64,AAEC"), .inline(Data([0, 1, 2])))
        XCTAssertEqual(request("data:image/png,raw"), .unsupported)
    }

    func testAnythingElseIsTheAltText() {
        XCTAssertEqual(request("ftp://example.com/a.png"), .unsupported)
        XCTAssertEqual(request("//cdn.example.com/a.png"), .unsupported)
        XCTAssertEqual(request(""), .unsupported)
    }
}

final class ImageStoreTests: XCTestCase {
    actor Calls {
        var asked: [ImageRequest] = []
        func add(_ request: ImageRequest) { asked.append(request) }
    }

    private static let png = ImagePrepTests.encode(ImagePrepTests.image(64, 32), .png)!

    func testTwoAsksAtOnceReadOnce() async throws {
        let calls = Calls()
        let store = ImageStore { request in
            await calls.add(request)
            try await Task.sleep(for: .milliseconds(50))
            return Self.png
        }
        let request = ImageRequest.attachment(path: "/a.png")
        async let one = store.data(for: request)
        async let two = store.data(for: request)
        let (a, b) = try await (one, two)
        XCTAssertEqual(a, Self.png)
        XCTAssertEqual(b, Self.png)
        let asked = await calls.asked
        XCTAssertEqual(asked, [request])
    }

    func testAFailureIsKeptUntilForgotten() async throws {
        let calls = Calls()
        let store = ImageStore { request in
            await calls.add(request)
            throw ImageLoadError.notFound
        }
        let request = ImageRequest.attachment(path: "/gone.png")
        for _ in 0..<2 {
            do {
                _ = try await store.data(for: request)
                XCTFail("gone")
            } catch {
                XCTAssertEqual(error as? ImageLoadError, .notFound)
            }
        }
        var asked = await calls.asked
        XCTAssertEqual(asked.count, 1, "a scroll does not ask again")
        await store.forget(request)
        _ = try? await store.data(for: request)
        asked = await calls.asked
        XCTAssertEqual(asked.count, 2)
    }

    func testAsleepIsNotKept() async throws {
        let calls = Calls()
        let store = ImageStore { request in
            await calls.add(request)
            throw ImageLoadError.asleep
        }
        let request = ImageRequest.attachment(path: "/a.png")
        _ = try? await store.data(for: request)
        _ = try? await store.data(for: request)
        let asked = await calls.asked
        XCTAssertEqual(asked.count, 2)
    }

    func testTheLineGetsADownscaledImage() async throws {
        let store = ImageStore { _ in Self.png }
        let image = try await store.image(for: .attachment(path: "/a.png"), edge: 16)
        XCTAssertEqual(image.cgImage.map { max($0.width, $0.height) }, 16)
        let whole = try await store.image(for: .attachment(path: "/a.png"), edge: nil)
        XCTAssertEqual(whole.cgImage?.width, 64)
    }

    func testAtMostTwoReadsAtOnce() async throws {
        actor Gauge {
            var now = 0, most = 0
            func enter() { now += 1; most = max(most, now) }
            func leave() { now -= 1 }
        }
        let gauge = Gauge()
        let store = ImageStore { _ in
            await gauge.enter()
            try await Task.sleep(for: .milliseconds(30))
            await gauge.leave()
            return Self.png
        }
        try await withThrowingTaskGroup(of: Data.self) { group in
            for at in 0..<5 { group.addTask { try await store.data(for: .attachment(path: "/\(at).png")) } }
            for try await _ in group {}
        }
        let most = await gauge.most
        XCTAssertEqual(most, 2)
    }
}
