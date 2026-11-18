import XCTest
@testable import DeskPhoneKit

/// A Mac's door, scripted: each request's type answered from `answers`, and
/// every request kept.
private final class ScriptedDoor: Asking, @unchecked Sendable {
    private let lock = NSLock()
    private var _asked: [DoorRequest] = []
    private var _closed = false
    var answers: [String: [String]]

    init(_ answers: [String: [String]]) { self.answers = answers }

    var asked: [DoorRequest] { lock.withLock { _asked } }
    var kinds: [String] { asked.map(\.kind) }
    var closed: Bool { lock.withLock { _closed } }

    func request(_ request: DoorRequest, within: Duration?) async throws -> DoorResponse {
        let text: String? = lock.withLock {
            _asked.append(request)
            guard var queue = answers[request.kind], !queue.isEmpty else { return nil }
            let next = queue.count > 1 ? queue.removeFirst() : queue[0]
            answers[request.kind] = queue
            return next
        }
        guard let text else { throw DoorError.timeout }
        return try Wire.decoder.decode(DoorResponse.self, from: Data(text.utf8))
    }

    func close() { lock.withLock { _closed = true } }
}

private final class Memory: NewThreadMemory {
    var mac: String?
    var harness: String?
    var projects: [String: String] = [:]
    func project(on mac: String) -> String? { projects[mac] }
    func remember(project: String, on mac: String) { projects[mac] = project }
}

@MainActor
final class NewThreadTests: XCTestCase {
    private let mac = MacChoice(organizationID: "org-1", machineID: "m-1", label: "Morgan's MacBook Pro", address: "fd06::1", port: 7)

    private static let hello = #"{"type":"hello","version":"0.1.0","build":"b","accepted":["operate"],"features":["start_on_host"],"protocol":{"major":1,"minor":50}}"#
    private static let environments = #"{"type":"environments","environments":[{"id":"env-1","mothership":true,"organizations":[{"id":"org-1"}]}]}"#
    private static let projects = #"""
    {"type":"projects","projects":[
      {"project_path":"/Users/someone/old","name":"old","repository":null,"worktrees_dir":"","nested":false,"setup_command":null,"updated_at":"2026-01-01T00:00:00Z"},
      {"project_path":"/Users/someone/sample-app","name":"sample-app","repository":"github.com/acme/sample-app","worktrees_dir":"","nested":false,"setup_command":null,
       "organization_id":"org-1","new_threads":"new_worktree","agent":{"harness":"codex","model":"gpt-next","effort":"high"},"updated_at":"2026-02-01T00:00:00Z"},
      {"project_path":"/Users/someone/theirs","name":"theirs","organization_id":"org-2","updated_at":"2026-03-01T00:00:00Z"},
      {"name":"no path"}
    ]}
    """#
    private static let readiness = #"{"type":"readiness","path":"","harnesses":[{"harness":"claude","ready":true},{"harness":"codex","ready":true},{"harness":"omp","ready":false}]}"#
    private static let layout = #"{"type":"worktree_layout","worktrees_dir":"","nested":false,"setup_command":null,"default_base":"origin/main","branches":["main","dev"],"remembered":true}"#
    private static let config = #"""
    {"type":"harness_config","commands":[],"options":[
      {"id":"model","name":"Model","category":"model","current":"gpt-next","choices":[{"value":"gpt-next","name":"GPT Next","description":null},{"value":"gpt-mini","name":"GPT Mini","description":null}]},
      {"id":"effort","name":"Effort","category":"effort","current":"medium","choices":[{"value":"medium","name":"Medium","description":null},{"value":"high","name":"High","description":null}]},
      {"id":"x","name":"X","category":"something_new","current":null,"choices":[]}
    ]}
    """#

    private func script(_ changes: [String: [String]] = [:]) -> ScriptedDoor {
        var answers: [String: [String]] = [
            "hello": [Self.hello], "environments_list": [Self.environments], "projects_list": [Self.projects],
            "readiness": [Self.readiness], "worktree_layout": [Self.layout], "harness_config": [Self.config],
            "session_start": [#"{"type":"session_started","session_id":"id-1","url":"u","cwd":"/w","branch":"b","modes":[],"config":[]}"#],
            "session_prompt": [#"{"type":"ok"}"#],
        ]
        for (kind, replies) in changes { answers[kind] = replies }
        return ScriptedDoor(answers)
    }

    private func model(_ door: ScriptedDoor, memory: Memory = Memory()) -> NewThreadModel {
        NewThreadModel(macs: [mac], open: { _ in door }, memory: memory, ids: { "id-1" }, ops: { "op-1" })
    }

    // MARK: naming

    func testATitleIsThePromptsFirstLineCutToSeventyTwo() {
        XCTAssertEqual(ThreadNaming.title("\n  Fix the recap  \nand more"), "Fix the recap")
        XCTAssertEqual(ThreadNaming.title("  \n "), "Untitled thread")
        let long = ThreadNaming.title(String(repeating: "a", count: 100))
        XCTAssertEqual(long.count, 72)
        XCTAssertTrue(long.hasSuffix("…"))
    }

    func testASlugIsTheFirstFourWordsOnAWholeWordWithinForty() {
        XCTAssertEqual(ThreadNaming.slug("Find why the recap fires twice"), "find-why-the-recap")
        XCTAssertEqual(ThreadNaming.slug("Résumé: l'été, déjà!"), "résumé-l-été-déjà")
        XCTAssertEqual(ThreadNaming.slug("supercalifragilisticexpialidocious antidisestablishmentarianism x"), "supercalifragilisticexpialidocious")
        XCTAssertEqual(ThreadNaming.slug("?!"), "")
        XCTAssertEqual(ThreadNaming.name("?!", random: { 0xdeadbeef }), "desk-deadbeef")
    }

    func testAnIdIsAUUIDv7OfItsInstant() {
        let id = ThreadNaming.id(now: Date(timeIntervalSince1970: 1_700_000_000), random: { 0xff })
        XCTAssertEqual(id, "018bcfe5-6800-7fff-bfff-ffffffffffff")
        XCTAssertNotNil(UUID(uuidString: id))
    }

    // MARK: the wire

    func testAStartIsWrittenAsDesksWindowWritesOne() throws {
        let start = ThreadStart(sessionID: "id-1", environmentID: "env-1", organizationID: "org-1", cwd: "/p", harness: "codex",
                                model: nil, effort: "high", title: "T", make: .init(name: "fix-it", base: "origin/main"), namesDriver: true)
        let body = try JSONValue.parse(DoorRequest.sessionStart(start).frame(id: 3))["request"]
        XCTAssertEqual(body, .object([
            "type": .string("session_start"), "session_id": .string("id-1"), "environment_id": .string("env-1"),
            "organization_id": .string("org-1"), "cwd": .string("/p"), "harness": .string("codex"), "mode": .string("supervised"),
            "model": .null, "effort": .string("high"), "title": .string("T"), "rename_branch": .bool(true),
            "make": .object(["name": .string("fix-it"), "branch": .string("fix-it"), "base": .string("origin/main")]),
            "driver": .string("acp"), "package": .string("@agentclientprotocol/codex-acp"),
        ]))
        let plain = ThreadStart(sessionID: "id-1", environmentID: "env-1", organizationID: nil, cwd: "/p", harness: "claude",
                                model: nil, effort: nil, title: "T", make: nil, namesDriver: true)
        XCTAssertEqual(plain.json["rename_branch"], .bool(false))
        XCTAssertNil(plain.json["make"])
        XCTAssertNil(plain.json["driver"])
        XCTAssertEqual(DoorRequest.sessionAbort(sessionID: "s").body, .object(["type": .string("session_abort"), "session_id": .string("s")]))
    }

    func testAListSkipsWhatDoesNotReadAndAnOptionOfAnUnknownKindIsKept() throws {
        guard case .projects(let projects) = try Wire.decoder.decode(DoorResponse.self, from: Data(Self.projects.utf8)) else { return XCTFail() }
        XCTAssertEqual(projects.map(\.name), ["old", "sample-app", "theirs"])
        XCTAssertEqual(projects[1].newThreads, .newWorktree)
        XCTAssertEqual(projects[1].agent?.harness, "codex")
        let odd = try Wire.decoder.decode(DoorProject.self, from: Data(#"{"project_path":"/p","name":"p","new_threads":"somewhere_new"}"#.utf8))
        XCTAssertEqual(odd.newThreads, .unknown)
        guard case .harnessConfig(let options) = try Wire.decoder.decode(DoorResponse.self, from: Data(Self.config.utf8)) else { return XCTFail() }
        XCTAssertEqual(options.map(\.category), ["model", "effort", "something_new"])
    }

    // MARK: the sheet

    func testItOpensOnTheNewestProjectOfThisOrganisationWithItsOwnAgent() async {
        let door = script()
        let model = model(door)
        await model.appear()
        XCTAssertNil(model.line)
        XCTAssertEqual(model.projects.map(\.name), ["sample-app", "old"])
        XCTAssertEqual(model.project?.name, "sample-app")
        XCTAssertEqual(model.harnesses, ["claude", "codex"])
        XCTAssertEqual(model.harness, "codex")
        XCTAssertEqual(model.model, "gpt-next")
        XCTAssertEqual(model.effort, "high")
        XCTAssertTrue(model.newBranch)
        XCTAssertFalse(model.canSend, "no words yet")
    }

    func testTheRememberedProjectAndAgentComeBack() async {
        let memory = Memory()
        memory.projects[mac.id] = "/Users/someone/old"
        memory.harness = "codex"
        let model = model(script(), memory: memory)
        await model.appear()
        XCTAssertEqual(model.project?.name, "old")
        XCTAssertEqual(model.harness, "codex")
        XCTAssertNil(model.model, "no project choice: the agent's own")
    }

    func testSendStartsThenPromptsAndRemembers() async throws {
        let door = script()
        let memory = Memory()
        let model = model(door, memory: memory)
        await model.appear()
        model.text = "  Find why the recap fires twice\nand fix it "
        let started = await model.send()
        XCTAssertEqual(started, StartedThread(organizationID: "org-1", sessionID: "id-1"))
        XCTAssertEqual(Array(door.kinds.suffix(2)), ["session_start", "session_prompt"])
        guard case .sessionStart(let start) = door.asked[door.asked.count - 2] else { return XCTFail() }
        XCTAssertEqual(start.title, "Find why the recap fires twice")
        XCTAssertEqual(start.make, .init(name: "find-why-the-recap", base: "origin/main"))
        XCTAssertEqual(start.cwd, "/Users/someone/sample-app")
        XCTAssertEqual(door.asked.last, .sessionPrompt(sessionID: "id-1", text: "Find why the recap fires twice\nand fix it", queue: false, op: "op-1"))
        XCTAssertEqual(memory.projects[mac.id], "/Users/someone/sample-app")
        XCTAssertEqual(memory.harness, "codex")
        XCTAssertTrue(door.closed)
    }

    func testARefusedStartSaysWhyAndKeepsTheWords() async {
        let door = script(["session_start": [#"{"type":"error","code":"no_base","message":"The project has no base branch."}"#]])
        let model = model(door)
        await model.appear()
        model.text = "do it"
        let started = await model.send()
        XCTAssertNil(started)
        XCTAssertEqual(model.line, "The project has no base branch.")
        XCTAssertEqual(model.text, "do it")
        XCTAssertFalse(door.kinds.contains("session_prompt"))
    }

    func testAPromptThatFailsAfterTheStartIsSentAgainWithoutASecondStart() async {
        let door = script(["session_prompt": [#"{"type":"error","code":"prompt_refused","message":"Setup failed."}"#, #"{"type":"ok"}"#]])
        let model = model(door)
        await model.appear()
        model.text = "do it"
        let first = await model.send()
        XCTAssertNil(first)
        XCTAssertEqual(model.line, "Setup failed.")
        let second = await model.send()
        XCTAssertEqual(second?.sessionID, "id-1")
        XCTAssertEqual(door.kinds.filter { $0 == "session_start" }.count, 1)
        XCTAssertEqual(door.kinds.filter { $0 == "session_prompt" }.count, 2)
    }

    func testTheCheckoutNeedsNoBaseAndANewBranchWithoutOneIsNotSent() async {
        let door = script(["worktree_layout": [#"{"type":"worktree_layout","default_base":null,"branches":[]}"#]])
        let model = model(door)
        await model.appear()
        model.text = "do it"
        let refused = await model.send()
        XCTAssertNil(refused)
        XCTAssertNotNil(model.line)
        XCTAssertFalse(door.kinds.contains("session_start"))
        model.newBranch = false
        let started = await model.send()
        XCTAssertNotNil(started)
        guard case .sessionStart(let start)? = door.asked.first(where: { $0.kind == "session_start" }) else { return XCTFail() }
        XCTAssertNil(start.make)
    }

    func testADeskTooOldToTakeAnIdIsNotAsked() async {
        let old = #"{"type":"hello","version":"0.1.0","build":"b","accepted":["operate"],"features":[],"protocol":{"major":1,"minor":50}}"#
        let door = script(["hello": [old]])
        let model = model(door)
        await model.appear()
        XCTAssertEqual(model.line, "Desk on Morgan's MacBook Pro needs an update before this phone can start a thread there.")
        XCTAssertEqual(door.kinds, ["hello"])
        XCTAssertTrue(door.closed)
    }

    func testWithNoMacItSaysSo() async {
        let model = NewThreadModel(macs: [], open: { _ in throw DoorError.timeout }, memory: Memory())
        await model.appear()
        XCTAssertNotNil(model.line)
        XCTAssertFalse(model.canSend)
    }
}
