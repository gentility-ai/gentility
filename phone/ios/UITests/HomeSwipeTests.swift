import XCTest

/// Home's swipes against a real development server, seeded with a known
/// world. The end-to-end harness that seeds it runs these by hand: it hands
/// the runner `DESK_PHONE_CREDENTIAL` and `DESK_PHONE_E2E_DIR` (the directory
/// the Mac watches), and each test leaves the threads as it found them.
final class HomeSwipeTests: XCTestCase {
    private let app = XCUIApplication()
    private let plain = "Tidy the release notes"
    private let waiting = "Wants your approval"
    private let asks = "Asks mid-test"

    override func setUpWithError() throws {
        continueAfterFailure = false
        let environment = ProcessInfo.processInfo.environment
        let credential = try XCTUnwrap(environment["DESK_PHONE_CREDENTIAL"], "run through the end-to-end harness")
        app.launchEnvironment["DESK_PHONE_CREDENTIAL"] = credential
        app.launch()
        // A first list the dev server is too slow to answer is asked again
        // on Home's next minute.
        XCTAssertTrue(row(plain).waitForExistence(timeout: 90), "Home never listed \(plain)")
    }

    func test1SettleThenUnsettle() {
        shoot("before settle")
        swipe(plain, from: .leading)
        app.buttons["Settle"].tap()
        XCTAssertTrue(gone(row(plain)), "a settled row leaves Threads")
        shoot("settled")

        open("Settled")
        swipe(plain, from: .leading)
        app.buttons["Unsettle"].tap()
        XCTAssertTrue(gone(shelf("Settled")), "the Settled shelf empties")
        XCTAssertTrue(row(plain).waitForExistence(timeout: 10))
        shoot("unsettled")
    }

    func test2SnoozeThenWake() {
        swipe(plain, from: .trailing)
        app.buttons["Snooze"].tap()
        preset().tap()
        XCTAssertTrue(gone(row(plain)), "a snoozed row leaves Threads")
        shoot("snoozed")

        open("Snoozed")
        swipe(plain, from: .trailing)
        app.buttons["Wake"].tap()
        XCTAssertTrue(gone(shelf("Snoozed")), "the Snoozed shelf empties")
        XCTAssertTrue(row(plain).waitForExistence(timeout: 10))
        shoot("woken")
    }

    func test3ArchiveThenUndo() {
        swipe(plain, from: .trailing)
        app.buttons["Archive"].tap()
        XCTAssertTrue(gone(row(plain)), "an archived row leaves Home")
        shoot("archived")

        app.buttons["Undo"].tap()
        XCTAssertTrue(row(plain).waitForExistence(timeout: 10), "Undo brings the row back")
        shoot("unarchived")
    }

    /// A row waiting on the person offers no snooze; one that began waiting
    /// after the phone listed it still offers one, and the server refuses.
    func test4SnoozingAWaitingThreadIsRefused() throws {
        XCTAssertTrue(row(waiting).exists)
        swipe(waiting, from: .trailing)
        XCTAssertTrue(app.buttons["Archive"].waitForExistence(timeout: 5))
        XCTAssertFalse(app.buttons["Snooze"].exists, "a waiting row offers no snooze")
        shoot("waiting row's swipe")
        app.navigationBars["Home"].tap()

        try askTheMac()
        swipe(asks, from: .trailing)
        app.buttons["Snooze"].tap()
        preset().tap()
        let refusal = app.staticTexts["Couldn't snooze: it is waiting on you."]
        XCTAssertTrue(refusal.waitForExistence(timeout: 10), "the server's refusal is said")
        XCTAssertTrue(row(asks).waitForExistence(timeout: 5), "the refused row stays")
        shoot("snooze refused")
    }

    // MARK: - Helpers

    private enum Edge { case leading, trailing }

    private func row(_ title: String) -> XCUIElement {
        app.descendants(matching: .any)["home.row.\(title)"].firstMatch
    }

    private func shelf(_ title: String) -> XCUIElement {
        app.staticTexts[title].firstMatch
    }

    /// Half a swipe, so the buttons show and none is taken by a full swipe.
    private func swipe(_ title: String, from edge: Edge) {
        let target = row(title)
        XCTAssertTrue(target.waitForExistence(timeout: 10), "no row \(title)")
        let (start, end) = edge == .leading ? (0.1, 0.6) : (0.9, 0.4)
        target.coordinate(withNormalizedOffset: CGVector(dx: start, dy: 0.5))
            .press(forDuration: 0.05, thenDragTo: target.coordinate(withNormalizedOffset: CGVector(dx: end, dy: 0.5)))
    }

    private func open(_ title: String) {
        // The undo bar covers the Later shelves until it goes, after 5 s.
        XCTAssertTrue(gone(app.buttons["Undo"]), "the undo bar never went")
        let label = shelf(title)
        XCTAssertTrue(label.waitForExistence(timeout: 10), "no \(title) shelf")
        app.swipeUp()
        label.tap()
        app.swipeUp()
    }

    private func preset() -> XCUIElement {
        let button = app.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "In an hour")).firstMatch
        XCTAssertTrue(button.waitForExistence(timeout: 5), "no snooze presets")
        return button
    }

    private func gone(_ element: XCUIElement) -> Bool {
        let gone = XCTNSPredicateExpectation(predicate: NSPredicate(format: "exists == false"), object: element)
        return XCTWaiter.wait(for: [gone], timeout: 10) == .completed
    }

    /// The stand-in Mac raises the approval once asked; the phone, which
    /// polls once a minute, has not heard it by the time it swipes.
    private func askTheMac() throws {
        let dir = try XCTUnwrap(ProcessInfo.processInfo.environment["DESK_PHONE_E2E_DIR"])
        let asked = URL(fileURLWithPath: dir).appendingPathComponent("asked")
        try? FileManager.default.removeItem(at: asked)
        try Data().write(to: URL(fileURLWithPath: dir).appendingPathComponent("ask"))
        let answered = XCTNSPredicateExpectation(predicate: NSPredicate { _, _ in
            FileManager.default.fileExists(atPath: asked.path)
        }, object: nil)
        XCTAssertEqual(XCTWaiter.wait(for: [answered], timeout: 30), .completed, "the stand-in Mac never raised the approval")
    }

    private func shoot(_ name: String) {
        let shot = XCTAttachment(screenshot: app.screenshot())
        shot.name = name
        shot.lifetime = .keepAlways
        add(shot)
    }
}
