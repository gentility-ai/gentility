import Foundation

/// What a new thread is called before a model has read its prompt, as
/// Desk's own window names one.
public enum ThreadNaming {
    /// The prompt's first line that says anything, cut to 72 characters.
    public static func title(_ prompt: String) -> String {
        let line = prompt.split(whereSeparator: \.isNewline)
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .first { !$0.isEmpty } ?? ""
        guard !line.isEmpty else { return "Untitled thread" }
        return line.count > 72 ? line.prefix(71).trimmingCharacters(in: .whitespaces) + "…" : line
    }

    /// A worktree and branch name from the prompt: its first four words, in
    /// lower case and joined by hyphens, at most 40 characters on a whole
    /// word. Empty when the prompt holds no letter or digit.
    public static func slug(_ prompt: String) -> String {
        let line = prompt.split(whereSeparator: \.isNewline).first.map(String.init) ?? ""
        let words = line.lowercased()
            .split(whereSeparator: { (c: Character) in !(c.isLetter || c.isNumber) })
            .prefix(4)
        var slug = ""
        for word in words {
            let next = slug.isEmpty ? String(word) : slug + "-" + word
            if next.count > 40 { break }
            slug = next
        }
        return slug
    }

    /// The slug, or `desk-` and eight hex digits where the prompt gives none.
    public static func name(_ prompt: String, random: () -> UInt32 = { .random(in: .min ... .max) }) -> String {
        let slug = slug(prompt)
        return slug.isEmpty ? "desk-" + String(format: "%08x", random()) : slug
    }

    /// A UUIDv7: the Mac refuses a thread id of any other kind.
    public static func id(now: Date = Date(), random: () -> UInt8 = { .random(in: .min ... .max) }) -> String {
        let ms = UInt64(max(0, now.timeIntervalSince1970 * 1000))
        var bytes = (0..<16).map { _ in random() }
        for at in 0..<6 { bytes[at] = UInt8((ms >> UInt64(8 * (5 - at))) & 0xff) }
        bytes[6] = (bytes[6] & 0x0f) | 0x70
        bytes[8] = (bytes[8] & 0x3f) | 0x80
        let hex = bytes.map { String(format: "%02x", $0) }.joined()
        let cuts = [8, 12, 16, 20]
        return hex.enumerated().map { at, digit in cuts.contains(at) ? "-\(digit)" : "\(digit)" }.joined()
    }
}

/// A Mac a thread can be started on: one this phone has a live door to.
public struct MacChoice: Identifiable, Hashable, Sendable {
    public let organizationID: String
    public let machineID: String
    public let label: String
    public let address: String
    public let port: Int

    public var id: String { organizationID + "/" + machineID }

    public init(organizationID: String, machineID: String, label: String, address: String, port: Int) {
        self.organizationID = organizationID
        self.machineID = machineID
        self.label = label
        self.address = address
        self.port = port
    }
}

/// A door, as far as starting a thread needs one.
public protocol Asking: Sendable {
    func request(_ request: DoorRequest, within: Duration?) async throws -> DoorResponse
    func close()
}

extension DoorConnection: Asking {}

/// What the last thread started here chose, so the next one opens on it.
public protocol NewThreadMemory: AnyObject {
    var mac: String? { get set }
    func project(on mac: String) -> String?
    func remember(project: String, on mac: String)
    var harness: String? { get set }
}

public final class DefaultsThreadMemory: NewThreadMemory {
    private let defaults: UserDefaults

    public init(_ defaults: UserDefaults = .standard) { self.defaults = defaults }

    public var mac: String? {
        get { defaults.string(forKey: "newThread.mac") }
        set { defaults.set(newValue, forKey: "newThread.mac") }
    }

    public var harness: String? {
        get { defaults.string(forKey: "newThread.harness") }
        set { defaults.set(newValue, forKey: "newThread.harness") }
    }

    public func project(on mac: String) -> String? { defaults.string(forKey: "newThread.project." + mac) }

    public func remember(project: String, on mac: String) { defaults.set(project, forKey: "newThread.project." + mac) }
}

/// A thread that was started: where Home will list it.
public struct StartedThread: Equatable, Sendable {
    public let organizationID: String
    public let sessionID: String
}

/// The new-thread sheet: which Mac, project, branch and agent, each
/// pre-filled so the common case is words and Send.
///
/// It dials the chosen Mac and asks what Desk's own window asks: the
/// environments, the projects, which agents are ready, and where a project's
/// new worktrees start. Send is `session_start` under an id minted here,
/// then the words as the thread's first prompt. The id is kept until both
/// are done, so sending again after a failure starts no second thread.
@MainActor
public final class NewThreadModel: ObservableObject {
    /// The agents this app names, in the order it offers them.
    public static let agents = ["claude", "codex", "omp"]

    public static func agentName(_ harness: String) -> String {
        switch harness {
        case "claude": "Claude"
        case "codex": "Codex"
        case "omp": "OMP"
        default: harness
        }
    }

    @Published public var text = ""
    @Published public private(set) var macs: [MacChoice]
    @Published public private(set) var mac: MacChoice?
    @Published public private(set) var projects: [DoorProject] = []
    @Published public private(set) var project: DoorProject?
    /// A new worktree on a new branch, or the project's own checkout.
    @Published public var newBranch = true
    @Published public private(set) var harnesses: [String] = []
    @Published public private(set) var harness = "claude"
    @Published public private(set) var models: [AgentOption.Choice] = []
    /// Nil is the agent's own choice.
    @Published public var model: String?
    @Published public private(set) var efforts: [AgentOption.Choice] = []
    @Published public var effort: String?
    @Published public private(set) var loading = false
    @Published public private(set) var sending = false
    /// What went wrong, in a sentence; the words typed are kept.
    @Published public private(set) var line: String?

    public var canSend: Bool {
        !sending && !loading && project != nil
            && !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private let open: @Sendable (MacChoice) async throws -> Asking
    private let memory: NewThreadMemory
    private let ids: () -> String
    private let ops: () -> String
    private var door: Asking?
    private var environmentID: String?
    private var namesDriver = false
    private var base: String?
    /// The id of a thread asked for and not yet started and prompted.
    private var minted: String?
    private var started = false
    private var generation = 0

    public init(
        macs: [MacChoice],
        open: @escaping @Sendable (MacChoice) async throws -> Asking,
        memory: NewThreadMemory = DefaultsThreadMemory(),
        ids: @escaping () -> String = { ThreadNaming.id() },
        ops: @escaping () -> String = { UUID().uuidString.lowercased() }
    ) {
        self.macs = macs
        self.open = open
        self.memory = memory
        self.ids = ids
        self.ops = ops
        mac = macs.first { $0.id == memory.mac } ?? macs.first
    }

    /// The sheet appeared: dial the Mac it opens on.
    public func appear() async {
        guard door == nil, !loading else { return }
        await load()
    }

    /// The sheet went away.
    public func close() {
        generation += 1
        door?.close()
        door = nil
    }

    public func pick(mac choice: MacChoice) async {
        guard choice != mac else { return }
        mac = choice
        close()
        projects = []
        project = nil
        harnesses = []
        await load()
    }

    public func pick(project choice: DoorProject) async {
        guard choice != project else { return }
        await settle(choice)
    }

    public func pick(harness choice: String) async {
        guard choice != harness else { return }
        harness = choice
        await readOptions()
    }

    // MARK: reading the Mac

    private func load() async {
        guard let mac else {
            line = "No Mac is reachable. Open Desk on your Mac and try again."
            return
        }
        generation += 1
        let attempt = generation
        loading = true
        line = nil
        defer { if attempt == generation { loading = false } }
        do {
            let door = try await open(mac)
            guard attempt == generation else { return door.close() }
            self.door = door
            try await greet(door, mac)
            guard case .environments(let all) = try await door.request(.environmentsList, within: nil) else {
                throw DoorError.unexpected("environments_list")
            }
            let holding = all.filter { $0.organizations.contains { $0.id == mac.organizationID } }
            guard let environment = holding.first(where: \.mothership) ?? holding.first else {
                throw DoorError.refusal(DoorRefusal(code: "unknown_environment", message: "Desk on \(mac.label) is not signed in to this organisation.", reason: nil))
            }
            environmentID = environment.id
            guard case .projects(let listed) = try await door.request(.projectsList, within: nil) else {
                throw DoorError.unexpected("projects_list")
            }
            guard attempt == generation else { return }
            // A project of another organisation is not this door's to start in.
            projects = listed
                .filter { $0.organizationID == nil || $0.organizationID == mac.organizationID }
                .sorted { ($0.updatedAt ?? "") > ($1.updatedAt ?? "") }
            guard !projects.isEmpty else {
                line = "Desk on \(mac.label) has no projects yet. Add one there first."
                return
            }
            if case .readiness(let all) = try await door.request(.readiness(environmentID: environment.id, organizationID: mac.organizationID), within: nil) {
                let ready = Set(all.filter(\.ready).map(\.harness))
                harnesses = Self.agents.filter(ready.contains)
            }
            if harnesses.isEmpty { harnesses = ["claude"] }
            guard attempt == generation else { return }
            let remembered = memory.project(on: mac.id)
            await settle(projects.first { $0.path == remembered } ?? projects[0])
        } catch {
            guard attempt == generation else { return }
            Log.home.error("new thread: reading \(mac.label, privacy: .public) failed: \(String(describing: error), privacy: .public)")
            line = Self.say(error, mac: mac.label)
            close()
        }
    }

    private func greet(_ door: Asking, _ mac: MacChoice) async throws {
        guard case .hello(let hello) = try await door.request(.hello(wants: ["operate", "unknown_words"]), within: nil) else {
            throw DoorError.unexpected("hello")
        }
        guard hello.protocolVersion?.major == 1, hello.accepted.contains("operate") else {
            throw DoorError.refusal(DoorRefusal(code: "protocol", message: "Desk on \(mac.label) speaks a protocol this app does not. Update both.", reason: nil))
        }
        guard hello.features.contains("start_on_host") else {
            throw DoorError.refusal(DoorRefusal(code: "protocol", message: "Desk on \(mac.label) needs an update before this phone can start a thread there.", reason: nil))
        }
        namesDriver = hello.features.contains("native_codex_selection")
    }

    /// A project chosen: where its threads start, its agent, and its base.
    private func settle(_ choice: DoorProject) async {
        project = choice
        base = nil
        newBranch = choice.newThreads != .checkout
        let wanted = choice.agent?.harness ?? memory.harness ?? "claude"
        harness = harnesses.contains(wanted) ? wanted : harnesses.first ?? "claude"
        guard let door else { return }
        let attempt = generation
        if case .worktreeLayout(let layout)? = try? await door.request(.worktreeLayout(projectPath: choice.path), within: nil),
           attempt == generation, project == choice {
            base = layout.base
        }
        await readOptions()
    }

    /// The agent's models and effort levels. Unread, the agent's own stand.
    private func readOptions() async {
        models = []
        efforts = []
        model = nil
        effort = nil
        guard let door, let environmentID, let mac else { return }
        let asked = harness
        let reply = try? await door.request(.harnessConfig(harness: asked, environmentID: environmentID, organizationID: mac.organizationID), within: nil)
        guard case .harnessConfig(let options)? = reply, asked == harness else { return }
        models = options.first { $0.category == "model" }?.choices ?? []
        efforts = options.first { $0.category == "effort" }?.choices ?? []
        // A project's own choice stands where the agent still offers it.
        if let agent = project?.agent, agent.harness == asked {
            model = models.contains { $0.value == agent.model } ? agent.model : nil
            effort = efforts.contains { $0.value == agent.effort } ? agent.effort : nil
        }
    }

    // MARK: sending

    /// Starts the thread and sends the words as its first prompt. Nil when
    /// it could not, with `line` saying why.
    public func send() async -> StartedThread? {
        guard canSend, let mac, let project, let environmentID else { return nil }
        guard let door else {
            await load()
            return nil
        }
        let words = text.trimmingCharacters(in: .whitespacesAndNewlines)
        var make: ThreadStart.Make?
        if newBranch {
            guard let base else {
                line = "\(project.name) has no branch to start a new one from. Start in the checkout instead."
                return nil
            }
            make = ThreadStart.Make(name: ThreadNaming.name(words), base: base)
        }
        sending = true
        line = nil
        defer { sending = false }
        let id = minted ?? ids()
        minted = id
        do {
            if !started {
                let start = ThreadStart(
                    sessionID: id, environmentID: environmentID, organizationID: mac.organizationID, cwd: project.path,
                    harness: harness, model: model, effort: effort, title: ThreadNaming.title(words),
                    make: make, namesDriver: namesDriver
                )
                switch try await door.request(.sessionStart(start), within: nil) {
                case .sessionStarted: started = true
                case .error(let refusal): throw DoorError.refusal(refusal)
                case let other: throw DoorError.unexpected(other.kind)
                }
                Log.home.info("new thread \(id, privacy: .public) started on \(mac.label, privacy: .public)")
            }
            switch try await door.request(.sessionPrompt(sessionID: id, text: words, queue: false, op: ops()), within: nil) {
            case .ok, .queued: break
            case .error(let refusal): throw DoorError.refusal(refusal)
            case let other: throw DoorError.unexpected(other.kind)
            }
        } catch {
            Log.home.error("new thread \(id, privacy: .public): \(String(describing: error), privacy: .public)")
            line = Self.say(error, mac: mac.label)
            return nil
        }
        memory.mac = mac.id
        memory.remember(project: project.path, on: mac.id)
        memory.harness = harness
        close()
        return StartedThread(organizationID: mac.organizationID, sessionID: id)
    }

    /// A failure, as a person reads it.
    static func say(_ error: Error, mac: String) -> String {
        switch error {
        case DoorError.refusal(let refusal):
            return refusal.message.isEmpty ? "\(mac) could not start the thread. Try again." : refusal.message
        case DoorError.timeout:
            return "\(mac) did not answer in time. Try again."
        case DoorError.closed:
            return "The link to \(mac) dropped. Try again."
        default:
            return "Couldn't reach \(mac). Check that it is awake and Desk is open."
        }
    }
}
