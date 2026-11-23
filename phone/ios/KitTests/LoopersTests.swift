import XCTest
@testable import DeskPhoneKit

final class FakeLooperLobby: LooperLobby, @unchecked Sendable {
    var rows: [String: [LooperRow]] = [:]
    var runs: [String: [LooperRunRow]] = [:]
    var reads: [String: LooperRunRead] = [:]
    var muted: Set<String> = []
    var refusal: Error?
    /// What every read throws, while set.
    var down: Error?
    private(set) var started: [(looper: String, message: String?)] = []
    private(set) var updates: [[String: JSONValue]] = []
    private(set) var answers: [(request: String, answer: String)] = []

    func organizations() async throws -> Organizations {
        Organizations(organizations: [.init(id: "o-1", slug: "northwind", name: "Northwind")],
                      person: .init(id: "p-1", email: "morgan@example.com", name: "Morgan"))
    }
    func loopers(organizationID: String) async throws -> [LooperRow] {
        if let down { throw down }
        return rows[organizationID] ?? []
    }
    func runs(organizationID: String, looperID: String) async throws -> [LooperRunRow] {
        if let down { throw down }
        return runs[looperID] ?? []
    }
    func run(organizationID: String, runID: String) async throws -> LooperRunRead {
        if let down { throw down }
        guard let read = reads[runID] else { throw LobbyError.refused("not_found") }
        return read
    }
    func start(organizationID: String, looperID: String, message: String?) async throws -> String {
        if let refusal { throw refusal }
        started.append((looperID, message))
        let run = LooperRunRow(id: "r-new", number: 9, status: "running")
        runs[looperID] = [run] + (runs[looperID] ?? [])
        return run.id
    }
    func update(organizationID: String, looperID: String, changes: [String: JSONValue]) async throws -> LooperRow {
        if let refusal { throw refusal }
        updates.append(changes)
        return LooperRow(id: looperID, name: changes["name"]?.string ?? "Triage",
                         schedule: .init(cron: "0 9 * * 1-5", enabled: changes["schedule_enabled"]?.bool ?? true, timezone: nil))
    }
    func answer(organizationID: String, requestID: String, answer: String) async throws -> LooperRequest {
        if let refusal { throw refusal }
        answers.append((requestID, answer))
        return LooperRequest(id: requestID, kind: "question", status: "answered")
    }
    func mutedLoopers(personID: String) async throws -> Set<String> { muted }
    func mute(personID: String, looperID: String, muted: Bool) async throws {
        if muted { self.muted.insert(looperID) } else { self.muted.remove(looperID) }
    }
}

@MainActor
final class LoopersTests: XCTestCase {
    private let now = WireDate.parse("2026-01-01T12:00:00.000000Z")!

    private func row(_ id: String, _ name: String, cron: String? = "0 9 * * 1-5", enabled: Bool = true, next: String? = nil, responder: Bool = false) -> LooperRow {
        LooperRow(id: id, name: name, schedule: .init(cron: cron, enabled: enabled, timezone: "Europe/London"), nextRunAt: next, responder: responder)
    }

    private let question = LooperRequest(id: "q-1", kind: "question", rendered: "Which repository owns billing?", status: "open", canResolve: true)

    // MARK: the schedule in words, as Desk reads it

    func testCronReadsAsDeskReadsIt() {
        XCTAssertEqual(CronReading.describe("0 9 * * 1-5"), "Weekdays at 09:00")
        XCTAssertEqual(CronReading.describe("* * * * *"), "Every minute")
        XCTAssertEqual(CronReading.describe("*/15 * * * *"), "Every 15 minutes")
        XCTAssertEqual(CronReading.describe("0 * * * *"), "Every hour")
        XCTAssertEqual(CronReading.describe("5 * * * *"), "Every hour at :05")
        XCTAssertEqual(CronReading.describe("0 */4 * * *"), "Every 4 hours")
        XCTAssertEqual(CronReading.describe("30 */4 * * 1-5"), "Every 4 hours at :30 on weekdays")
        XCTAssertEqual(CronReading.describe("30 18 * * *"), "Every day at 18:30")
        XCTAssertEqual(CronReading.describe("0 8 * * 1"), "Every Monday at 08:00")
        XCTAssertEqual(CronReading.describe("0 8 * * 7"), "Every Sunday at 08:00")
        XCTAssertEqual(CronReading.describe("0 8 1 * *"), "Monthly on day 1 at 08:00")
    }

    func testACronItWouldHaveToGuessAtIsNotRead() {
        for cron in ["0 9 * 1 *", "*/45 * * * *", "0 9 * * 1,3", "0 9 * * * *", "nonsense"] {
            XCTAssertNil(CronReading.describe(cron), cron)
        }
        XCTAssertEqual(CronReading.line(cron: "0 9 * * 1,3", responder: false), "A cron expression")
        XCTAssertEqual(CronReading.line(cron: "", responder: false), "No schedule — runs only when started")
        XCTAssertEqual(CronReading.line(cron: nil, responder: true), "No schedule — woken by its triggers")
    }

    // MARK: the list's one state

    func testALooperSaysOneStateInDesksWords() {
        func state(_ looper: Looper) -> String { looper.state(now: now).text }
        XCTAssertEqual(state(Looper(row: row("a", "A"), organizationID: "o", working: true, asking: true)), "Asking")
        XCTAssertEqual(state(Looper(row: row("a", "A"), organizationID: "o", working: true)), "Working")
        XCTAssertEqual(state(Looper(row: row("a", "A", next: "2026-01-01T15:00:00.000000Z"), organizationID: "o")), "Next run in 3 h")
        XCTAssertEqual(state(Looper(row: row("a", "A", next: "2026-01-01T12:20:00.000000Z"), organizationID: "o")), "Next run in 20 min")
        XCTAssertEqual(state(Looper(row: row("a", "A"), organizationID: "o")), "Not known yet")
        XCTAssertEqual(state(Looper(row: row("a", "A", enabled: false), organizationID: "o")), "Schedule off")
        XCTAssertEqual(state(Looper(row: row("a", "A", cron: nil), organizationID: "o")), "Manual")
        XCTAssertEqual(state(Looper(row: row("a", "A", cron: "", responder: true), organizationID: "o")), "Listening")
    }

    func testTheListPutsAskingFirstThenWorkingThenByName() async {
        let lobby = FakeLooperLobby()
        lobby.rows["o-1"] = [row("z", "Zebra"), row("b", "beta"), row("w", "Worker"), row("q", "Quiz")]
        lobby.runs["w"] = [LooperRunRow(id: "r1", status: "running")]
        lobby.runs["q"] = [LooperRunRow(id: "r2", status: "running", requests: [question])]
        lobby.runs["z"] = [LooperRunRow(id: "r3", status: "completed", requests: [question])]
        lobby.muted = ["b"]
        let model = LoopersModel(lobby: lobby, now: { [now] in now })
        await model.refresh()
        XCTAssertEqual(model.loopers.map(\.name), ["Quiz", "Worker", "beta", "Zebra"])
        XCTAssertEqual(model.loopers.map(\.asking), [true, false, false, false])
        XCTAssertEqual(model.loopers.first { $0.id == "b" }?.muted, true)
        XCTAssertTrue(model.loaded)
    }

    // MARK: a run's row

    func testARunRowSaysItsNumberWhyItWokeAndItsState() {
        let run = LooperRunRow(id: "r", number: 41, title: "Triage #41", status: "running", wake: .init(by: "scheduled", source: nil))
        XCTAssertEqual(RunFace(run).title, "Run #41")
        XCTAssertEqual(RunFace(run).woke, "Woken by its schedule")
        XCTAssertEqual(RunFace(run).word, "Running")
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "running", requests: [question])).word, "Asking")
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "failed")).word, "Failed")
        XCTAssertNil(RunFace(LooperRunRow(id: "r", status: "completed")).word)
        XCTAssertEqual(RunFace.wakeWords(.init(by: "trigger", source: "a new issue")), "Woken by a new issue")
        XCTAssertEqual(RunFace.wakeWords(.init(by: nil, source: nil)), "Woke")
    }

    // MARK: a looper's page

    func testRunNowStartsARunSaysSoAndOffersIt() async {
        let lobby = FakeLooperLobby()
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        await page.run()
        XCTAssertEqual(lobby.started.map(\.looper), ["a"])
        XCTAssertNil(lobby.started.first?.message)
        XCTAssertEqual(page.notice, "Run started")
        XCTAssertEqual(page.started?.id, "r-new")
        XCTAssertEqual(page.runBlocked, "A run is already going.")
        XCTAssertTrue(page.looper.working)
    }

    func testAMessageStartsARunWithThoseWordsAndABlankOneDoesNot() async {
        let lobby = FakeLooperLobby()
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        await page.run(message: "   ")
        XCTAssertTrue(lobby.started.isEmpty)
        XCTAssertEqual(page.failure, "Write a message first.")
        await page.run(message: " include the deploy queue ")
        XCTAssertEqual(lobby.started.first?.message, "include the deploy queue")
    }

    func testARefusedRunSaysWhyInTheWebsWords() async {
        let lobby = FakeLooperLobby()
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        lobby.refusal = LobbyError.refused("paused")
        await page.run()
        XCTAssertEqual(page.failure, "This looper is paused. Resume it on the web to run it.")
        lobby.refusal = LobbyError.refused("not_funded")
        await page.run()
        XCTAssertEqual(page.failure, "Its organisation is out of funds. Add funds on the web to run it.")
        XCTAssertNil(page.notice)
    }

    func testThePageListsTheRequestsWaiting() async {
        let lobby = FakeLooperLobby()
        lobby.runs["a"] = [LooperRunRow(id: "r1", status: "running", requests: [question]), LooperRunRow(id: "r0", status: "completed")]
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        await page.refresh()
        XCTAssertEqual(page.open.map(\.request.id), ["q-1"])
        XCTAssertTrue(page.looper.asking)
    }

    func testMutingSaysSoAndIsKept() async {
        let lobby = FakeLooperLobby()
        var heard: Looper?
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1") { heard = $0 }
        await page.setMuted(true)
        XCTAssertEqual(lobby.muted, ["a"])
        XCTAssertEqual(page.notice, "Muted on this phone")
        XCTAssertEqual(heard?.muted, true)
    }

    // MARK: Edit

    func testTheDraftSendsOnlyWhatChangedInTheServersKeys() {
        let was = LooperRow(id: "a", name: "Triage", systemPrompt: "Label issues.", schedule: .init(cron: "0 9 * * 1-5", enabled: true, timezone: "Europe/London"), notifyHandle: "@ops", visibility: "private")
        var draft = LooperDraft(was)
        XCTAssertTrue(draft.changes(from: was).isEmpty)
        draft.instructions = "Label issues, then stop."
        draft.scheduleOn = false
        draft.notify = "  "
        draft.isPublic = true
        XCTAssertEqual(draft.changes(from: was), [
            "system_prompt": .string("Label issues, then stop."),
            "schedule_enabled": .bool(false),
            "notify_handle": .null,
            "visibility": .string("public"),
        ])
        draft.cron = "0 8 * * 1"
        XCTAssertEqual(draft.cronLine, "Every Monday at 08:00")
    }

    func testSavingSendsTheChangesAndARefusalKeepsTheForm() async {
        let lobby = FakeLooperLobby()
        let page = LooperPageModel(looper: Looper(row: row("a", "Triage"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        var draft = LooperDraft(page.looper.row)
        draft.name = "Sorter"
        lobby.refusal = LobbyError.refused("invalid")
        let refused = await page.save(draft)
        XCTAssertFalse(refused)
        XCTAssertEqual(page.looper.name, "Triage")
        XCTAssertEqual(page.editFailure, "The server did not take that. Check the schedule and try again.")
        XCTAssertNil(page.failure, "a refused Save is said on the form, not under Run now")
        lobby.refusal = nil
        let saved = await page.save(draft)
        XCTAssertTrue(saved)
        XCTAssertNil(page.editFailure)
        XCTAssertEqual(page.notice, "Saved")
        XCTAssertEqual(lobby.updates, [["name": .string("Sorter")]])
        XCTAssertEqual(page.looper.name, "Sorter")
    }

    // MARK: a run

    func testARunFoldsItsTurnsAndItsToolCalls() {
        let turns = [
            LooperTurn(id: "t3", position: 3, role: "assistant", content: nil, toolCalls: [.init(id: "c2", name: "web_fetch", status: "failed")]),
            LooperTurn(id: "t1", position: 1, role: "user", content: "wake"),
            LooperTurn(id: "t2", position: 2, role: "assistant", content: "Reading the queue.", toolCalls: [.init(id: "c1", name: "task_list", status: "completed")]),
            LooperTurn(id: "t4", position: 4, role: "assistant", content: "Done."),
            LooperTurn(id: "t5", position: 5, role: "assistant", content: nil, isPrivate: true),
        ]
        XCTAssertEqual(LooperRunModel.fold(turns), [
            .said(id: "t1", text: "wake"),
            .words(id: "t2", text: "Reading the queue."),
            .tools(id: "t2-tools", names: ["task_list", "web_fetch"], failed: 1),
            .words(id: "t4", text: "Done."),
            .withheld(id: "t5"),
        ])
    }

    func testAnsweringARequestSendsTheWordsAndSaysSo() async {
        let lobby = FakeLooperLobby()
        let run = LooperRunRow(id: "r1", status: "running")
        lobby.reads["r1"] = LooperRunRead(run: run, turns: [], requests: [question])
        let model = LooperRunModel(run: run, organizationID: "o-1", lobby: lobby)
        await model.refresh()
        XCTAssertEqual(model.open.map(\.id), ["q-1"])
        await model.answer(question, with: "  ")
        XCTAssertTrue(lobby.answers.isEmpty)
        XCTAssertEqual(model.failure, "Write an answer first.")
        await model.answer(question, with: "acme/billing")
        XCTAssertEqual(lobby.answers.first?.answer, "acme/billing")
        XCTAssertEqual(model.notice, "Answered")
        XCTAssertNil(model.failure, "an answer that went clears the last failure")
    }

    func testARefusedAnswerSaysWhy() async {
        let lobby = FakeLooperLobby()
        let run = LooperRunRow(id: "r1", status: "running")
        lobby.reads["r1"] = LooperRunRead(run: run, turns: [], requests: [question])
        let model = LooperRunModel(run: run, organizationID: "o-1", lobby: lobby)
        await model.refresh()
        lobby.refusal = LobbyError.refused("not_open")
        await model.answer(question, with: "acme/billing")
        XCTAssertEqual(model.failure, "That request was already answered.")
        XCTAssertNil(model.notice)
    }

    // MARK: the words a refusal is said in

    func testEveryRefusalIsASentenceAndAnUnknownCodeIsNeverShown() {
        func said(_ code: String) -> String { LooperRefusal.sentence(LobbyError.refused(code)) }
        XCTAssertEqual(said("paused"), "This looper is paused. Resume it on the web to run it.")
        XCTAssertEqual(said("not_funded"), "Its organisation is out of funds. Add funds on the web to run it.")
        XCTAssertEqual(said("run_in_flight"), "A run is already going.")
        XCTAssertEqual(said("blank_message"), "Write a message first.")
        XCTAssertEqual(said("not_found"), "This looper is no longer yours to run.")
        XCTAssertEqual(said("not_editable"), "That setting is changed on the web.")
        XCTAssertEqual(said("invalid"), "The server did not take that. Check the schedule and try again.")
        XCTAssertEqual(said("answer_required"), "Write an answer first.")
        XCTAssertEqual(said("not_open"), "That request was already answered.")
        XCTAssertEqual(LooperRefusal.sentence(LobbyError.unauthorized), "This phone is no longer paired.")
        XCTAssertEqual(LooperRefusal.sentence(LobbyError.unreachable("timed out")), "Can't reach the server. Try again.")
        let unknown = said("quota_exceeded_v2")
        XCTAssertEqual(unknown, "That did not go through.")
        XCTAssertFalse(unknown.contains("quota"))
        for code in ["paused", "not_funded", "run_in_flight", "blank_message", "not_found", "not_editable", "invalid", "answer_required", "not_open", "anything"] {
            XCTAssertTrue(said(code).hasSuffix("."), code)
        }
    }

    // MARK: a read that fails is not an empty fact

    func testAListThatFailsToLoadSaysSoAndKeepsWhatItHad() async {
        let lobby = FakeLooperLobby()
        lobby.rows["o-1"] = [row("a", "Triage")]
        let model = LoopersModel(lobby: lobby)
        lobby.down = LobbyError.unreachable("timed out")
        await model.refresh()
        XCTAssertEqual(model.line, "Can't reach the server. Try again.")
        XCTAssertTrue(model.loopers.isEmpty)
        lobby.down = nil
        await model.refresh()
        XCTAssertNil(model.line, "a read that works clears the line")
        XCTAssertEqual(model.loopers.map(\.name), ["Triage"])
        lobby.down = LobbyError.unreachable("timed out")
        await model.refresh()
        XCTAssertEqual(model.line, "Can't reach the server. Try again.")
        XCTAssertEqual(model.loopers.map(\.name), ["Triage"], "the last list is kept")
    }

    func testAPageWhoseRunsFailToLoadSaysSoApartFromAPress() async {
        let lobby = FakeLooperLobby()
        lobby.runs["a"] = [LooperRunRow(id: "r1", number: 1, status: "completed")]
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        lobby.down = LobbyError.unreachable("timed out")
        await page.refresh()
        XCTAssertEqual(page.line, "Can't reach the server. Try again.")
        XCTAssertNil(page.failure, "a failed read is not a failed press")
        lobby.down = nil
        await page.refresh()
        XCTAssertNil(page.line)
        XCTAssertEqual(page.runs.map(\.id), ["r1"])
    }

    func testAPressClearsTheLastFailure() async {
        let lobby = FakeLooperLobby()
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        lobby.refusal = LobbyError.refused("not_funded")
        let refused = await page.run()
        XCTAssertFalse(refused)
        XCTAssertNotNil(page.failure)
        lobby.refusal = nil
        let started = await page.run(message: "Include the deploy queue")
        XCTAssertTrue(started, "the field is cleared on this, not on the absence of a failure")
        XCTAssertNil(page.failure)

        lobby.refusal = LobbyError.refused("invalid")
        var draft = LooperDraft(page.looper.row)
        draft.name = "B"
        _ = await page.save(draft)
        XCTAssertNotNil(page.editFailure)
        page.editing()
        XCTAssertNil(page.editFailure, "the form opens without an earlier refusal")

        lobby.refusal = LobbyError.refused("paused")
        await page.run()
        XCTAssertNotNil(page.failure)
        await page.setMuted(true)
        XCTAssertNil(page.failure)
        XCTAssertEqual(page.notice, "Muted on this phone")
        await page.setMuted(false)
        XCTAssertEqual(page.notice, "Notifications on")
    }

    func testARunThatFailsToLoadSaysSoAndAReadThatWorksClearsIt() async {
        let lobby = FakeLooperLobby()
        let run = LooperRunRow(id: "r1", status: "completed")
        lobby.reads["r1"] = LooperRunRead(run: run, turns: [], requests: [])
        let model = LooperRunModel(run: run, organizationID: "o-1", lobby: lobby)
        lobby.down = LobbyError.unreachable("timed out")
        await model.refresh()
        XCTAssertEqual(model.line, "Can't reach the server. Try again.")
        XCTAssertNil(model.failure)
        lobby.down = nil
        await model.refresh()
        XCTAssertNil(model.line)
    }

    // MARK: the rest of the words

    func testARunSaysQueuedAndStoppedAndEveryWayItWoke() {
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "queued")).word, "Queued")
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "pending")).word, "Queued")
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "stopped")).word, "Stopped")
        XCTAssertEqual(RunFace(LooperRunRow(id: "r", status: "cancelled")).word, "Stopped")
        XCTAssertNil(RunFace(LooperRunRow(id: "r", status: "completed")).word)
        func woke(_ by: String?, _ source: String? = nil) -> String { RunFace.wakeWords(by.map { .init(by: $0, source: source) }) }
        XCTAssertEqual(woke(nil), "Woke")
        XCTAssertEqual(woke("task"), "Woken by a task")
        XCTAssertEqual(woke("email"), "Woken by an email")
        XCTAssertEqual(woke("request"), "Woken by an answer")
        XCTAssertEqual(woke("manual"), "Started by hand")
        XCTAssertEqual(woke("trigger"), "Woken by a trigger")
        XCTAssertEqual(woke("event"), "Woken by an event")
        XCTAssertEqual(woke("event", "a new issue"), "Woken by a new issue")
        XCTAssertEqual(woke("something_new"), "Woke")
    }

    func testTheNextRunIsSaidNowInMinutesHoursAndDays() {
        func until(_ seconds: TimeInterval) -> String { Looper.until(now.addingTimeInterval(seconds), now: now) }
        XCTAssertEqual(until(-30), "now")
        XCTAssertEqual(until(30), "now")
        XCTAssertEqual(until(20 * 60), "in 20 min")
        XCTAssertEqual(until(47 * 3_600), "in 47 h")
        XCTAssertEqual(until(72 * 3_600), "in 3 d")
    }

    func testTheEditFormIsGivenANextRunOnlyWhereOneIsScheduled() {
        func next(_ looper: Looper) -> String? { looper.nextRun(now: now) }
        XCTAssertEqual(next(Looper(row: row("a", "A", next: "2026-01-01T15:00:00.000000Z"), organizationID: "o")), "in 3 h")
        XCTAssertEqual(next(Looper(row: row("a", "A", next: "2026-01-01T15:00:00.000000Z"), organizationID: "o", working: true, asking: true)), "in 3 h", "never the state word")
        XCTAssertNil(next(Looper(row: row("a", "A"), organizationID: "o")))
        XCTAssertNil(next(Looper(row: row("a", "A", enabled: false, next: "2026-01-01T15:00:00.000000Z"), organizationID: "o")))
        XCTAssertNil(next(Looper(row: row("a", "A", cron: nil), organizationID: "o")))
    }

    func testTheDraftSaysWhoSeesIt() {
        var draft = LooperDraft(row("a", "A"))
        XCTAssertEqual(draft.visibilityLine, "Only your organisation sees it and its runs")
        draft.isPublic = true
        XCTAssertEqual(draft.visibilityLine, "Anyone with its public link can read its brief and runs")
    }

    func testThePagesOwnSentences() {
        XCTAssertEqual(LooperCopy.running, "A run is already going.")
        XCTAssertEqual(LooperCopy.noRuns, "No runs yet. Run now starts one.")
        XCTAssertEqual(LooperCopy.answerOnWeb, "Answer this one on the web.")
        XCTAssertEqual(LooperCopy.notYours, "This run belongs to a looper that is not yours. It opens on the web.")
        XCTAssertEqual(LooperCopy.opensOnWeb, "This run opens on the web.")
        XCTAssertEqual(LooperCopy.nothingSaid(going: true), "Nothing said yet.")
        XCTAssertEqual(LooperCopy.nothingSaid(going: false), "This run said nothing.")
        XCTAssertEqual(LooperCopy.runFailed("timeout"), "This run failed. Reason: timeout")
        XCTAssertEqual(LooperCopy.tools(names: ["task_list"], failed: 0), "task_list")
        XCTAssertEqual(LooperCopy.tools(names: ["a", "b", "c"], failed: 0), "3 tool calls")
        XCTAssertEqual(LooperCopy.tools(names: ["a", "b", "c"], failed: 2), "3 tool calls, 2 failed")
    }

    func testTheFilterNamesLooperAndResponderRuns() {
        XCTAssertEqual(ThreadKind.looper.plural, "Looper runs")
        XCTAssertEqual(ThreadKind.responder.plural, "Responder runs")
    }

    func testAPageSaysARunIsGoingWhereRunNowCannotRun() async {
        let lobby = FakeLooperLobby()
        lobby.runs["a"] = [LooperRunRow(id: "r1", status: "running")]
        let page = LooperPageModel(looper: Looper(row: row("a", "A"), organizationID: "o-1"), lobby: lobby, personID: "p-1")
        XCTAssertNil(page.runBlocked)
        await page.refresh()
        XCTAssertEqual(page.runBlocked, "A run is already going.")
    }
}
