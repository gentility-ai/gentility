import Foundation

public struct HomeRow: Identifiable, Hashable, Sendable {
    public let id: String
    public let organizationID: String
    public let title: String
    /// `working`, `waiting on you` or `idle`.
    public let state: String
    /// `now`, `2 min`, `3 h`, `yesterday`, `4 d`.
    public let ago: String
    public let machineID: String?
    public let macLabel: String
    public let recapLine: String?
    public let lastEventAt: Date?
    /// What it waits on, drawn after `‼`; nil when it waits on nothing.
    public let attentionLine: String?
    /// Its Mac answers now.
    public let live: Bool
    /// The server recently heard Desk online, independently of 6net.
    public let deskOnline: Bool
    public let settledAt: Date?
    public let snoozedUntil: Date?
    public let branch: String?
    /// `owner/repo`; nil off GitHub.
    public let project: String?
    /// A looper's run.
    public let looper: Bool
    /// What it waits on the person for: an approval, a question, or nothing.
    public let attention: RowAttention
    /// The server inbox's tag: `"person"` when the thread waits on
    /// the person, and why in one sentence. Nil while it waits on nobody.
    public let waitingOn: String?
    public let waitingReason: String?

    /// `attention` defaults from `state`: a row built as waiting on you,
    /// with no kind named, waits on an approval.
    public init(id: String, organizationID: String, title: String, state: String, ago: String, machineID: String?, macLabel: String, recapLine: String?, lastEventAt: Date?, attentionLine: String? = nil, live: Bool = false, deskOnline: Bool = false, settledAt: Date? = nil, snoozedUntil: Date? = nil, branch: String? = nil, project: String? = nil, looper: Bool = false, attention: RowAttention? = nil, waitingOn: String? = nil, waitingReason: String? = nil) {
        self.attention = attention ?? (state == Self.waitingOnYou ? .approval : .none)
        self.waitingOn = waitingOn
        self.waitingReason = waitingReason
        self.id = id
        self.organizationID = organizationID
        self.title = title
        self.state = state
        self.ago = ago
        self.machineID = machineID
        self.macLabel = macLabel
        self.recapLine = recapLine
        self.lastEventAt = lastEventAt
        self.attentionLine = attentionLine
        self.live = live
        self.deskOnline = deskOnline
        self.settledAt = settledAt
        self.snoozedUntil = snoozedUntil
        self.branch = branch
        self.project = project
        self.looper = looper
    }

    public static let waitingOnYou = "waiting on you"

    /// Waiting on the person for an approval or a question: on the Needs
    /// you shelf, whatever else it is.
    public var waiting: Bool { attention != .none }

    /// Tagged by the server as waiting on the person, and reading so:
    /// an approval or a question says its own thing, a running turn is the
    /// person already answered (the server clears the tag when a visible turn
    /// starts, and Home hears it on its next list), and a settled thread
    /// waits on nobody. On the Needs you shelf unless snoozed: unlike an ask,
    /// the tag may be put off for later.
    public var waitsOnYou: Bool {
        attention == .none && waitingOn == "person" && state != "working" && settledAt == nil
    }

    /// What the row says it waits on: the ask's line, or else the tag's reason.
    public var waitingLine: String? {
        if waiting { return attentionLine }
        return waitsOnYou ? waitingReason : nil
    }

    public var statusLine: String { "\(state) · \(ago) · \(macLabel)" }

    /// This row with its park state replaced.
    func parked(settledAt: Date?, snoozedUntil: Date?) -> HomeRow {
        HomeRow(id: id, organizationID: organizationID, title: title, state: state, ago: ago, machineID: machineID, macLabel: macLabel, recapLine: recapLine, lastEventAt: lastEventAt, attentionLine: attentionLine, live: live, deskOnline: deskOnline, settledAt: settledAt, snoozedUntil: snoozedUntil, branch: branch, project: project, looper: looper, attention: attention, waitingOn: waitingOn, waitingReason: waitingReason)
    }
}

/// What a row waits on the person for, from the lobby row's `attention`.
public enum RowAttention: Hashable, Sendable {
    case none, approval, question

    init(wire: String?) {
        switch wire {
        case "approval": self = .approval
        case "question": self = .question
        default: self = .none
        }
    }
}

/// What a person does to a row from Home, as the Desk sidebar does it.
public enum HomeAction: Hashable, Sendable {
    case settle, unsettle, snooze(Date), wake, archive, unarchive
}

/// The last action, for the few seconds it can be taken back.
public struct Undo: Equatable, Sendable {
    public let words: String
    public let rowID: String
    public let organizationID: String
    public let inverse: HomeAction

    public init(words: String, rowID: String, organizationID: String, inverse: HomeAction) {
        self.words = words
        self.rowID = rowID
        self.organizationID = organizationID
        self.inverse = inverse
    }
}

/// Where to dial a row's Mac: its address in the row's organisation, and the
/// port its `desk` service declared.
public struct DoorTarget: Equatable, Sendable {
    public let address: String
    public let port: Int
    public let macLabel: String
    public let live: Bool
    public let deskPresence: String?

    public init(address: String, port: Int, macLabel: String, live: Bool, deskPresence: String? = nil) {
        self.address = address
        self.port = port
        self.macLabel = macLabel
        self.live = live
        self.deskPresence = deskPresence
    }
}

/// Where a row's Mac can be reached from this phone (B6).
public enum Reach: Equatable, Sendable {
    /// A door in the row's organisation, live or not (`DoorTarget.live`).
    case door(DoorTarget)
    /// The Mac is up on 6net — seen now in another organisation's machines —
    /// but its node holds no entry in the row's organisation: no grant, so no
    /// door. A wake will not change that; a grant will. Named by the
    /// organisation's slug, the word the person sees in Desk.
    case noDoor(organization: String)
    /// Desk was online, but there is no approved target here to dial.
    case linkUnavailable
    /// This online install is listed authoritatively without an open endpoint.
    case notOpen
    /// Nothing reaches it anywhere: asleep.
    case none

    var deskOnline: Bool {
        switch self {
        case .door(let target): return target.deskPresence == "online"
        case .linkUnavailable, .notOpen: return true
        case .noDoor, .none: return false
        }
    }
}

/// Network presence belongs to the node; the selected endpoint belongs to
/// one install. An online install without an endpoint is handled separately.
extension InstallReach {
    func isLive(serverNow: Date) -> Bool {
        port != nil && Liveness.isPresent(node, serverNow: serverNow)
    }
}

/// The existing network freshness and legacy service rules.
public enum Liveness {
    /// Two missed 30-second presence beats.
    public static let window: TimeInterval = 60

    /// The port of the node's approved `desk` service; nil while it is only
    /// declared, or absent.
    public static func doorPort(_ machine: NetMachine) -> Int? {
        machine.services.first { $0.kind == "desk" && $0.state == "approved" }?.port
    }

    /// Up on 6net: active and seen within `window` of the reply's `now`,
    /// door or no door.
    public static func isPresent(_ machine: NetMachine, serverNow: Date) -> Bool {
        guard machine.status == "active", let seen = machine.presence.lastSeenAt.flatMap(WireDate.parse) else { return false }
        return serverNow.timeIntervalSince(seen) <= window
    }

    /// Present, with an approved door.
    public static func isLive(_ machine: NetMachine, serverNow: Date) -> Bool {
        isPresent(machine, serverNow: serverNow) && doorPort(machine) != nil
    }
}

public enum Ago {
    public static func say(_ then: Date?, now: Date) -> String {
        guard let then else { return "a while ago" }
        let seconds = max(0, now.timeIntervalSince(then))
        if seconds < 60 { return "now" }
        if seconds < 3_600 { return "\(Int(seconds / 60)) min" }
        if seconds < 86_400 { return "\(Int(seconds / 3_600)) h" }
        if seconds < 172_800 { return "yesterday" }
        return "\(Int(seconds / 86_400)) d"
    }
}

/// The person's sessions from every organisation, shelved by `Shelves`.
@MainActor
public final class HomeModel: ObservableObject {
    /// The person's rows, unfiltered, newest first.
    @Published public private(set) var rows: [HomeRow] = []
    @Published public private(set) var line: String?
    @Published public private(set) var loaded = false
    @Published public private(set) var undo: Undo?
    public var onUnauthorized: (() -> Void)?
    public var onPerson: ((String) -> Void)?
    /// Said at the end of every refresh that listed: a tap waiting on Home's
    /// rows is opened then.
    public var onListed: (() -> Void)?

    public var isEmpty: Bool { loaded && rows.isEmpty && line == nil }

    private let lobby: Lobby
    private let every: Duration
    private let sleep: @Sendable (Duration) async throws -> Void
    private let now: () -> Date
    private var lobbyRows: [String: [String: LobbyRow]] = [:]
    private var since: [String: String] = [:]
    private var machines: [String: MachinesReply] = [:]
    /// Each organisation's slug, by id: the word a doorless row names.
    private var slugs: [String: String] = [:]
    /// Organisations whose `machines` was refused: not asked again until
    /// Home appears again.
    private var machinesRefused: Set<String> = []
    private var personID: String?
    private var ticking: Task<Void, Never>?
    var periodicallyRefreshing: Bool { ticking != nil }
    /// Each row's park state as this phone last set it, laid over the listed
    /// row until the server has answered and listed again.
    private var pending: [String: [Field: Pending]] = [:]
    private var serial = 0
    /// Counts refreshes begun: an answer is stamped with it, so a list read
    /// before the answer does not drop what the answer confirmed.
    private var refreshes = 0
    /// The serial of the action `undo` takes back.
    private var undoSerial = 0

    private enum Field {
        case settled, snoozed, archived

        var wire: InboxField {
            switch self {
            case .settled: .settledAt
            case .snoozed: .snoozedUntil
            case .archived: .archivedAt
            }
        }
    }

    private struct Pending {
        /// When settled, snoozed until, or archived; nil for none.
        let value: Date?
        let serial: Int
        /// The refresh count when the server answered; nil while unanswered.
        var answeredAt: Int?
    }

    public init(
        lobby: Lobby,
        every: Duration = .seconds(60),
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) },
        now: @escaping () -> Date = Date.init
    ) {
        self.lobby = lobby
        self.every = every
        self.sleep = sleep
        self.now = now
    }

    /// Re-asked when Home appears and every minute while it is visible.
    public func appear(refreshNow: Bool = true) {
        ticking?.cancel()
        machinesRefused = []
        let every = every
        let sleep = sleep
        ticking = Task { [weak self] in
            if !refreshNow {
                do { try await sleep(every) } catch { return }
            }
            while !Task.isCancelled {
                await self?.refresh()
                do { try await sleep(every) } catch { return }
            }
        }
    }

    public func disappear() {
        ticking?.cancel()
        ticking = nil
    }

    public func refresh() async {
        let began = ContinuousClock.now
        refreshes += 1
        let generation = refreshes
        let organizations: Organizations
        do {
            organizations = try await lobby.organizations()
        } catch LobbyError.unauthorized {
            Log.home.info("the bearer was refused")
            onUnauthorized?()
            return
        } catch {
            Log.home.error("organisations: \(String(describing: error), privacy: .public)")
            line = Self.unreachable
            return
        }
        let reached = Set(organizations.organizations.map(\.id))
        // An organisation the person has left takes its rows with it; one
        // they come back to is read whole again.
        lobbyRows = lobbyRows.filter { reached.contains($0.key) }
        since = since.filter { reached.contains($0.key) }
        machines = machines.filter { reached.contains($0.key) }
        machinesRefused = machinesRefused.filter { reached.contains($0) }
        slugs = Dictionary(organizations.organizations.map { ($0.id, $0.slug) }, uniquingKeysWith: { first, _ in first })
        var unreached = false
        for organization in organizations.organizations {
            do {
                let answered = try await refresh(organization.id)
                unreached = unreached || !answered
            } catch LobbyError.unauthorized {
                onUnauthorized?()
                return
            } catch {
                line = Self.unreachable
                return
            }
        }
        line = unreached ? Self.unreachable : nil
        if personID != organizations.person.id {
            personID = organizations.person.id
            onPerson?(organizations.person.id)
        }
        // A list begun after an action's answer has heard it: its word stands.
        pending = pending.compactMapValues { fields in
            let held = fields.filter { ($0.value.answeredAt ?? generation) >= generation }
            return held.isEmpty ? nil : held
        }
        publish()
        loaded = true
        Log.home.info("home: \(self.rows.count) session(s) in \(String(describing: ContinuousClock.now - began), privacy: .public)")
        onListed?()
    }

    /// The listed rows with this phone's pending park state laid over them.
    private func publish() {
        rows = Self.collect(rows: lobbyRows, machines: machines, personID: personID, now: now()).compactMap { row in
            guard let fields = pending[row.id] else { return row }
            if let archived = fields[.archived], archived.value != nil { return nil }
            return row.parked(
                settledAt: fields[.settled].map(\.value) ?? row.settledAt,
                snoozedUntil: fields[.snoozed].map(\.value) ?? row.snoozedUntil
            )
        }
    }

    /// What `row` offers, by its state; a looper run offers nothing, and a
    /// row waiting on the person cannot be snoozed: nobody hides their own
    /// blocker. Snooze carries a placeholder; the view asks the presets.
    public static func actions(for row: HomeRow, now: Date = Date()) -> (leading: [HomeAction], trailing: [HomeAction]) {
        if row.looper { return ([], []) }
        let leading: [HomeAction] = [row.settledAt == nil ? .settle : .unsettle]
        let snooze: [HomeAction] = (row.snoozedUntil.map { $0 > now } ?? false) ? [.wake] : row.waiting ? [] : [.snooze(.distantFuture)]
        return (leading, snooze + [.archive])
    }

    /// The long-press menu for `row`, in order: settle or unsettle, snooze
    /// (its presets) or wake, archive. The same rule as `actions(for:)`; a
    /// looper run says it is managed on the Mac, so a press never does
    /// nothing.
    public static func menu(for row: HomeRow, now: Date = Date()) -> [HomeMenuItem] {
        if row.looper { return [.managedOnMac] }
        let swipes = actions(for: row, now: now)
        return (swipes.leading + swipes.trailing).compactMap { action in
            switch action {
            case .settle: .settle
            case .unsettle: .unsettle
            case .snooze: .snooze(SnoozePresets.at(now))
            case .wake: .wake
            case .archive: .archive
            case .unarchive: nil
            }
        }
    }

    /// Moves `row` at once, asks the server, and offers to take it back; a
    /// refusal puts the row back and says why.
    public func act(_ action: HomeAction, on row: HomeRow) async {
        await perform(action, id: row.id, organizationID: row.organizationID, prior: row, offeringUndo: true)
    }

    /// Takes back the last action. A snooze whose old wake has passed since
    /// is taken back by waking, since the server refuses a wake in the past.
    public func undoLast() async {
        guard let undo else { return }
        self.undo = nil
        var inverse = undo.inverse
        if case .snooze(let old) = inverse, old <= now() { inverse = .wake }
        let prior = rows.first { $0.id == undo.rowID }
        await perform(inverse, id: undo.rowID, organizationID: undo.organizationID, prior: prior, offeringUndo: false)
    }

    private func perform(_ action: HomeAction, id: String, organizationID: String, prior: HomeRow?, offeringUndo: Bool) async {
        serial += 1
        let mine = serial
        let (field, value): (Field, Date?) = switch action {
        case .settle: (.settled, now())
        case .unsettle: (.settled, nil)
        case .snooze(let until): (.snoozed, until)
        case .wake: (.snoozed, nil)
        case .archive: (.archived, now())
        case .unarchive: (.archived, nil)
        }
        pending[id, default: [:]][field] = Pending(value: value, serial: mine)
        publish()
        if offeringUndo, let prior {
            undo = Undo(words: Self.words(action), rowID: id, organizationID: organizationID, inverse: Self.inverse(of: action, prior: prior))
            undoSerial = mine
            let sleep = sleep
            Task { [weak self] in
                do { try await sleep(.seconds(5)) } catch { return }
                if self?.undoSerial == mine { self?.undo = nil }
            }
        }
        do {
            let reply = try await lobby.inbox(organizationID: organizationID, id: id, field: field.wire, value: value)
            guard pending[id]?[field]?.serial == mine else { return }
            if reply.stale.contains(field.wire.rawValue) {
                // A later write from elsewhere beat this one: the row snaps
                // back to what the server holds.
                Log.home.info("\(Self.verb(action), privacy: .public) \(id, privacy: .public) was stale; snapping back")
                pending[id]?[field] = nil
                if undoSerial == mine { undo = nil }
                publish()
                await refresh()
            } else {
                pending[id]?[field]?.answeredAt = refreshes
            }
        } catch {
            Log.home.error("\(Self.verb(action), privacy: .public) \(id, privacy: .public): \(String(describing: error), privacy: .public)")
            // Only what this action set goes back; a later one stands.
            if pending[id]?[field]?.serial == mine { pending[id]?[field] = nil }
            if undoSerial == mine { undo = nil }
            if case LobbyError.unauthorized = error {
                onUnauthorized?()
                return
            }
            line = Self.refusal(action, error)
            publish()
        }
    }

    private static func inverse(of action: HomeAction, prior: HomeRow) -> HomeAction {
        switch action {
        case .settle: .unsettle
        case .unsettle: .settle
        case .snooze, .wake: prior.snoozedUntil.map(HomeAction.snooze) ?? .wake
        case .archive: .unarchive
        case .unarchive: .archive
        }
    }

    private static func verb(_ action: HomeAction) -> String {
        switch action {
        case .settle: "settle"
        case .unsettle: "unsettle"
        case .snooze: "snooze"
        case .wake: "wake"
        case .archive: "archive"
        case .unarchive: "unarchive"
        }
    }

    private static func words(_ action: HomeAction) -> String {
        switch action {
        case .settle: "Settled"
        case .unsettle: "Unsettled"
        case .snooze(let until): "Snoozed until \(wake.string(from: until))"
        case .wake: "Woken"
        case .archive: "Archived"
        case .unarchive: "Unarchived"
        }
    }

    private static let wake: DateFormatter = {
        let formatter = DateFormatter()
        formatter.setLocalizedDateFormatFromTemplate("EEE HH:mm")
        return formatter
    }()

    private static func refusal(_ action: HomeAction, _ error: Error) -> String {
        let why: String
        switch error as? LobbyError {
        case .refused("attention_pending"): why = "it is waiting on you"
        case .refused("session_running"): why = "it is still running"
        case .refused("inbox_unsupported"): why = "the server can't yet"
        case .refused("not_found"): why = "the server has no such thread"
        case .refused("forbidden"): why = "it isn't yours"
        case .unauthorized: why = "you are signed out"
        case .unreachable, nil: return unreachable
        case .refused(let reason): why = reason.replacingOccurrences(of: "_", with: " ")
        }
        return "Couldn't \(verb(action)): \(why)."
    }

    /// The row a notification names, if Home lists it.
    public func row(organizationID: String, sessionID: String) -> HomeRow? {
        rows.first { $0.id == sessionID && $0.organizationID == organizationID }
    }

    private static let unreachable = "Can't reach the server. Showing what it last said."

    /// Reads one organisation's rows and Macs; false when its rows went
    /// unanswered.
    private func refresh(_ organization: String) async throws -> Bool {
        var reached = true
        do {
            let listed = try await lobby.list(organizationID: organization, since: since[organization])
            var held = lobbyRows[organization] ?? [:]
            for row in listed.sessions {
                held[row.id] = row.archivedAt == nil ? row : nil
            }
            lobbyRows[organization] = held
            since[organization] = listed.now
        } catch LobbyError.unauthorized {
            throw LobbyError.unauthorized
        } catch LobbyError.refused("forbidden") {
            // An organization whose list is no longer readable must not
            // retain rows from before access was revoked.
            lobbyRows[organization] = nil
            since[organization] = nil
            machines[organization] = nil
            return true
        } catch {
            Log.home.error("list in \(organization, privacy: .public): \(String(describing: error), privacy: .public)")
            if case LobbyError.unreachable = error { reached = false }
        }
        guard !machinesRefused.contains(organization) else {
            Log.home.debug("machines in \(organization, privacy: .public) was refused; not asked again until Home appears")
            return reached
        }
        do {
            machines[organization] = try await lobby.machines(organizationID: organization)
        } catch LobbyError.unauthorized {
            throw LobbyError.unauthorized
        } catch LobbyError.refused(let reason) {
            Log.home.error("machines in \(organization, privacy: .public) refused: \(reason, privacy: .public); not asked again until Home appears")
            machines[organization] = nil
            machinesRefused.insert(organization)
        } catch {
            Log.home.error("machines in \(organization, privacy: .public): \(String(describing: error), privacy: .public)")
            machines[organization] = nil
        }
        return reached
    }

    /// The Macs a thread can be started on: each one that holds a thread of
    /// this person's and has a live door, the one heard from last first.
    public func macs() -> [MacChoice] {
        var seen: Set<String> = []
        return rows.compactMap { row in
            guard let machineID = row.machineID, case .door(let target) = reach(for: row), target.live else { return nil }
            let choice = MacChoice(organizationID: row.organizationID, machineID: machineID, label: row.macLabel,
                                   address: target.address, port: target.port)
            return seen.insert(choice.id).inserted ? choice : nil
        }
    }

    /// Where to dial `row`'s Mac, in the row's organisation; nil when it has
    /// no approved door there.
    public func target(for row: HomeRow) -> DoorTarget? {
        guard case .door(let target) = reach(for: row) else { return nil }
        return target
    }

    /// How to reach `row`'s Mac: its door here; else, if its node is present
    /// in another organisation and this one's machines, read, hold no entry
    /// for it, no door here; else asleep. A node listed here with a door
    /// only declared is unreachable; online Desk evidence keeps it from
    /// being called asleep.
    public func reach(for row: HomeRow) -> Reach {
        // Absent here only when here was read: an unread list says nothing.
        guard let machineID = row.machineID, let here = machines[row.organizationID] else { return .none }
        let now = now()
        if let machine = Self.mac(of: machineID, in: here, now: now) {
            if let port = machine.port {
                let serverNow = WireDate.parse(here.now) ?? now
                return .door(DoorTarget(address: machine.node.address, port: port, macLabel: row.macLabel,
                                        live: machine.isLive(serverNow: serverNow), deskPresence: machine.deskPresence))
            }
            if machine.node.deskInstalls != nil && machine.deskPresence == "online" { return .notOpen }
            return machine.deskPresence == "online" ? .linkUnavailable : .none
        }
        let presentElsewhere = machines.contains { organization, reply in
            guard organization != row.organizationID else { return false }
            let serverNow = WireDate.parse(reply.now) ?? now
            return reply.machines.contains { $0.install(machineID) != nil && Liveness.isPresent($0, serverNow: serverNow) }
        }
        return presentElsewhere ? .noDoor(organization: slugs[row.organizationID] ?? row.organizationID) : .none
    }

    /// The entry linked to the Desk store id `machineID`: a live one first,
    /// then one with a door, then any.
    nonisolated static func mac(of machineID: String?, in reply: MachinesReply, now: Date) -> InstallReach? {
        guard let machineID else { return nil }
        let serverNow = WireDate.parse(reply.now) ?? now
        let linked = reply.machines.compactMap { $0.install(machineID) }
        return linked.first { $0.isLive(serverNow: serverNow) }
            ?? linked.first { $0.port != nil }
            ?? linked.first
    }

    /// The person's rows across every organisation, newest first.
    nonisolated static func collect(rows: [String: [String: LobbyRow]], machines: [String: MachinesReply], personID: String?, now: Date) -> [HomeRow] {
        guard let personID else { return [] }
        var collected: [HomeRow] = []
        for (organization, held) in rows {
            let reply = machines[organization]
            let serverNow = reply.flatMap { WireDate.parse($0.now) } ?? now
            for row in held.values where row.ownerID == personID {
                let machine = reply.flatMap { mac(of: row.machineID, in: $0, now: now) }
                let lastEvent = row.lastEventAt.flatMap(WireDate.parse)
                let attention = RowAttention(wire: row.attention)
                let state = attention != .none ? HomeRow.waitingOnYou : row.working ? "working" : "idle"
                collected.append(HomeRow(
                    id: row.id, organizationID: organization, title: row.title, state: state,
                    ago: Ago.say(lastEvent, now: now), machineID: row.machineID,
                    macLabel: row.host ?? machine?.node.name ?? "Another Mac",
                    recapLine: row.recapLine, lastEventAt: lastEvent, attentionLine: row.attentionLine,
                    live: machine.map { $0.isLive(serverNow: serverNow) } ?? false,
                    deskOnline: machine?.deskPresence == "online",
                    settledAt: row.settledAt.flatMap(WireDate.parse),
                    snoozedUntil: row.snoozedUntil.flatMap(WireDate.parse),
                    branch: row.branch,
                    project: row.repository.map(project),
                    looper: row.looperID != nil,
                    attention: attention,
                    waitingOn: row.waitingOn,
                    waitingReason: row.waitingReason
                ))
            }
        }
        return collected.sorted(by: newestFirst)
    }

    /// `github.com/owner/repo` as the person reads it: `owner/repo`.
    nonisolated static func project(_ repository: String) -> String {
        repository.hasPrefix("github.com/") ? String(repository.dropFirst("github.com/".count)) : repository
    }

    /// Newest first; a tie falls to the id.
    nonisolated static func newestFirst(_ a: HomeRow, _ b: HomeRow) -> Bool {
        let (left, right) = (a.lastEventAt ?? .distantPast, b.lastEventAt ?? .distantPast)
        return left != right ? left > right : a.id < b.id
    }
}
