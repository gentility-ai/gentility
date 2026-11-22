import XCTest
@testable import DeskPhoneKit

final class FakeLooperLobby: LooperLobby, @unchecked Sendable {
    var rows: [String: [LooperRow]] = [:]
    var runs: [String: [LooperRunRow]] = [:]
    var reads: [String: LooperRunRead] = [:]
    var muted: Set<String> = []
    var refusal: Error?
    private(set) var started: [(looper: String, message: String?)] = []
    private(set) var updates: [[String: JSONValue]] = []
    private(set) var answers: [(request: String, answer: String)] = []

    func organizations() async throws -> Organizations {
        Organizations(organizations: [.init(id: "o-1", slug: "northwind", name: "Northwind")],
                      person: .init(id: "p-1", email: "morgan@example.com", name: "Morgan"))
    }
    func loopers(organizationID: String) async throws -> [LooperRow] { rows[organizationID] ?? [] }
    func runs(organizationID: String, looperID: String) async throws -> [LooperRunRow] { runs[looperID] ?? [] }
    func run(organizationID: String, runID: String) async throws -> LooperRunRead {
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
        XCTAssertEqual(state(Looper(row: row("a", "A", next: "2026-01-01T15:00:00.000000Z"), organizationID: "o")), "next run in 3 h")
        XCTAssertEqual(state(Looper(row: row("a", "A", next: "2026-01-01T12:20:00.000000Z"), organizationID: "o")), "next run in 20 min")
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
        XCTAssertEqual(page.runBlocked, "Running…")
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
        XCTAssertEqual(page.failure, "Paused: resume it to run")
        lobby.refusal = LobbyError.refused("not_funded")
        await page.run()
        XCTAssertEqual(page.failure, "Add funds to run")
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
        lobby.refusal = nil
        let saved = await page.save(draft)
        XCTAssertTrue(saved)
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
        await model.answer(question, with: "acme/billing")
        XCTAssertEqual(lobby.answers.first?.answer, "acme/billing")
        XCTAssertEqual(model.notice, "Answered")
    }
}
