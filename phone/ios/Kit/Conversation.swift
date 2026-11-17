import Foundation

/// One session, dialled on its Mac's door at `operate`.
///
/// The order an open runs in:
/// 1. `OpenDoor`.
/// 2. `hello {wants: ["operate", "unknown_words"]}`; a Desk that does not
///    accept `operate` is refused. `unknown_words` tells the core this app
///    reads a word it does not know as unknown; an older core logs it and
///    admits the hello.
/// 3. `session_transcript {session_id, last_turns: 20}`.
/// 4. Phase `open`, and the door's events for this session listened to.
/// 5. `environments_list` (once a conversation), then `sessions_list`: the
///    row applied, so a question parked before the dial pins its card.
/// 6. An answer owed from before a drop sent again, under its own op.
///
/// A failed dial is judged on Go's code, never its words.
@MainActor
public final class ConversationModel: ObservableObject {
    public enum Phase: Equatable {
        /// Dialling, or waiting on a node that is not up yet to try again.
        case connecting
        case open
        /// F4: unreachable, with no online Desk evidence.
        case asleep(macLabel: String)
        /// Desk was recently online, but the phone cannot reach its 6net link.
        case linkUnavailable(macLabel: String)
        case notOpen
        /// B6: the Mac is up, and its node holds no door in this row's
        /// organisation. No wake loop: a grant is what it waits on, and
        /// Retry asks Home again before it dials.
        case noDoor(organization: String)
        /// Any other refusal: a line under the title, and Retry.
        case refused(String)
        /// The door dropped. The turns on screen stay.
        case lost(String)

        /// Nothing dials again until the person taps Retry.
        var waitsForRetry: Bool {
            switch self {
            case .refused, .noDoor, .notOpen: return true
            default: return false
            }
        }

        /// Asleep or doorless: the screen shows what the server last heard.
        public var offline: Bool {
            switch self {
            case .asleep, .linkUnavailable, .noDoor, .notOpen: return true
            default: return false
            }
        }
    }

    public static let removed = "This phone was removed from your account."

    /// An owed answer's line follows it: every phase but a refusal or no
    /// door tries again by itself (a backoff, the asleep Mac's wake loop, an
    /// open under way); those two wait for Retry.
    @Published public private(set) var phase: Phase = .connecting {
        didSet {
            if oldValue != phase { onPhaseChanged?() }
            guard cards.card?.owed == true else { return }
            if phase == .notOpen {
                cards.waitingForOpen(macLabel: row.macLabel)
                return
            }
            let retrying = !phase.waitsForRetry
            Log.conversation.info("an answer is owed, \(retrying ? "and tried again by itself" : "until Retry", privacy: .public)")
            cards.owed(retrying: retrying, macLabel: row.macLabel)
        }
    }
    var onPhaseChanged: (() -> Void)?
    @Published public private(set) var fold = TranscriptFold()
    /// Written here and by the composer (Composer.swift).
    @Published public internal(set) var working: Bool {
        didSet {
            if !working { workingSince = nil } else if workingSince == nil { workingSince = now() }
        }
    }
    /// When the turn running began: the row's `turn_started_at`, else when
    /// this phone first saw it working.
    @Published public private(set) var workingSince: Date?
    @Published public private(set) var cards = CardFold()
    public var card: Card? { cards.card }
    /// A line where the card was: answered elsewhere, or expired.
    public var cleared: String? { cards.cleared }
    public internal(set) var row: HomeRow
    /// F5: Go says this phone is not enrolled, or the mothership revoked it.
    /// The caller wipes the phone's state and pairs again.
    public var onRemoved: (() -> Void)?
    /// A dial found the phone's node down: the caller brings it up. The
    /// screen keeps saying it is connecting and the door is tried again.
    public var onNodeDown: (() -> Void)?

    // The composer's state; its verbs are in Composer.swift.
    /// The prompts queued behind the turn, drawn under the composer.
    @Published public internal(set) var pending = PendingLines()
    /// The prompt the row says is asked for and not started; the feed draws
    /// `waitingPrompt`.
    @Published public internal(set) var requested: RequestedPrompt?
    /// A line under the composer: why the words did not go.
    @Published public internal(set) var composerNote: String?
    /// A prompt is out: the composer sends nothing more until it is answered.
    @Published public internal(set) var sending = false
    /// The prompt whose reply did not come, held until the session is seen
    /// idle: the same words sent again go as it went.
    var unsentPrompt: DoorRequest?
    /// The queued lines a Take back is out for.
    var takingBack: Set<String> = []
    /// The photos the next prompt carries (Attachments.swift).
    @Published public internal(set) var tray = AttachmentTray()
    /// hello offered `attachments`: the Mac takes photos.
    @Published public private(set) var canAttach = false
    /// The session's worktree root on the Mac, once a row is read.
    @Published public private(set) var worktreeRoot: String?
    /// The tray's puts under way.
    var uploads: Task<Void, Never>?
    /// The transcript's images, read through this door.
    public private(set) lazy var images = ImageStore { [weak self] request in
        guard let self else { throw ImageLoadError.asleep }
        return try await self.imageBytes(request)
    }

    /// Asked of the row as it is now, at every dial and wake.
    private let reach: (HomeRow) -> Reach
    private let wake: () async -> Void
    private let doors: DoorOpening
    private let sleep: @Sendable (Duration) async throws -> Void
    private let wakeEvery: Duration
    private(set) var door: DoorConnection?
    private var listening: Task<Void, Never>?
    /// The next try: the asleep Mac's wake loop, or a backoff on a node not up.
    private var later: Task<Void, Never>?
    /// Tries in a row that found the node not up, or redialled for an owed
    /// answer.
    private var notUp = 0
    private var retryNeedsHome = false
    /// Bumped by every open and leave; an open that finds it moved on was
    /// overtaken, and leaves the door and the phase to the newer one.
    private var generation = 0
    /// The person left the screen: no dial until it opens again.
    private var left = false
    private var paging = true
    /// B3: hello offered `session_get`, so the row is one frame, not a list.
    private var oneRow = false

    /// B2: where the cursor stands — the newest `through_seq` read, and the
    /// store it belongs to. A seq is one Mac's store's: a hello that names
    /// another store, or no longer offers `transcript_seq`, drops it.
    struct Cursor: Equatable {
        let storeID: String
        let throughSeq: Int
    }

    /// hello offered `transcript_brief`: every read asks the phone's shape.
    private var shape: TranscriptShape?
    /// hello offered `transcript_seq`, and named its store.
    private var storeID: String?
    private(set) var cursor: Cursor?
    let ids: () -> String
    private let now: () -> Date
    /// The Mac's environment that holds this row, once read.
    private var environmentID: String?
    /// Rows of this session the door has said: a row read answered after a
    /// newer one was said is older than what is on screen.
    private var rowsHeard = 0
    /// B1: the open under way, measured; said as one line once the row is
    /// read. Cleared by a failed dial.
    private(set) var report: OpenReport?
    /// B1: the open door's Mac and the node's path to it then, asked again
    /// when the door closes.
    private var openedPath: (address: String, path: PeerPath?)?

    public static let inputTooLong = "Too long to show here. See it on your Mac."

    public init(
        row: HomeRow,
        reach: @escaping (HomeRow) -> Reach,
        wake: @escaping () async -> Void,
        doors: DoorOpening,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) },
        wakeEvery: Duration = .seconds(30),
        ids: @escaping () -> String = { UUID().uuidString.lowercased() },
        now: @escaping () -> Date = Date.init
    ) {
        self.row = row
        self.reach = reach
        self.wake = wake
        self.doors = doors
        self.sleep = sleep
        self.wakeEvery = wakeEvery
        self.ids = ids
        self.now = now
        working = row.state == "working"
    }

    /// F4's and B6's words, with the row's last status from the lobby, and
    /// what it waits on.
    public var asleepLines: [String] {
        let first: String
        if case .notOpen = phase {
            first = Self.notOpenLine(machine: row.macLabel)
        } else if case .noDoor(let organization) = phase {
            first = "\(row.macLabel) can't be reached for \(organization). Ask an administrator to check access."
        } else if case .linkUnavailable = phase {
            first = "Desk recently heard from \(row.macLabel), but this phone can't reach it. Retrying."
        } else {
            first = "\(row.macLabel) is asleep. This session will open when that Mac wakes."
        }
        var lines = [first, "Showing what the server last heard.", "Status: \(row.state) · last turn \(row.ago)"]
        if let waiting = row.attentionLine ?? row.waitingLine { lines.append("Waiting on you: \(waiting)") }
        return lines
    }

    public static func notOpenLine(machine: String) -> String {
        "The Desk running this session on \(machine) is online but not yet open to your phone."
    }

    /// Reconsider only this waiting state when Home's existing list refresh lands.
    public func listed() async {
        guard !left, phase == .notOpen else { return }
        await dial()
    }

    /// The screen's own open, as it appears: no longer left, it dials the
    /// row's Mac and reads the last turns. The turns already on screen stay
    /// while it dials.
    public func open() async {
        left = false
        report = OpenReport(session: row.id)
        if !fold.turns.isEmpty {
            Log.conversation.info("\(self.row.id, privacy: .public) opens from memory: \(self.fold.turns.count) turn(s) held")
        }
        await dial()
    }

    /// B5: a kept conversation shown again draws no stale open or drop
    /// before its `open()` dials. Asleep, no door and a refusal stay: the
    /// open asks again. One still on screen is left alone: Home's body
    /// asks for it on every evaluation.
    func reopened() {
        guard left else { return }
        switch phase {
        case .open, .lost: phase = .connecting
        case .connecting, .asleep, .linkUnavailable, .noDoor, .notOpen, .refused: break
        }
    }

    /// Every dial goes through here, and none for a conversation the person
    /// left: a way back from the background, or a try again set before the
    /// leave, finds it left and dials nothing.
    private func dial() async {
        guard !left else {
            Log.conversation.info("\(self.row.id, privacy: .public) was left; not dialling")
            return
        }
        retryNeedsHome = false
        later?.cancel()
        later = nil
        close()
        generation += 1
        let attempt = generation
        let target: DoorTarget
        let available = reach(row)
        switch available {
        case .door(let door) where door.live:
            target = door
        case .door, .none, .linkUnavailable:
            Log.conversation.info("\(self.row.macLabel, privacy: .public) has no live door")
            // Offline, the screen shows the row as the server last heard
            // it, not what the last visit's door said.
            working = row.state == "working"
            return unavailable(available)
        case .noDoor(let organization):
            Log.conversation.info("\(self.row.macLabel, privacy: .public) is up but holds no door in \(organization, privacy: .public); not dialled, not woken")
            working = row.state == "working"
            return noDoor(organization)
        case .notOpen:
            report = nil
            notUp = 0
            working = row.state == "working"
            phase = .notOpen
            return
        }
        phase = .connecting
        Log.conversation.info("dialling \(target.macLabel, privacy: .public) at [\(target.address, privacy: .public)]:\(target.port) for \(self.row.id, privacy: .public)")
        do {
            let dialled = ContinuousClock.now
            let door = try await doors.openDoor(address: target.address, port: target.port)
            let dial = ContinuousClock.now - dialled
            guard attempt == generation else {
                Log.conversation.notice("a dial was overtaken by a newer one; closing its door")
                return door.close()
            }
            report?.dial = dial
            self.door = door
            // The node's path to the Mac, asked beside the hello: a status
            // read must not add to the open.
            async let asked = doors.peerPath(address: target.address)
            // Asked into locals: `report?.x = try await …` skips the ask
            // when no open is being measured (a redial).
            let hello = try await greet(door, target)
            let transcript = try await reread(.last, on: door)
            let path = await asked
            guard attempt == generation else { return }
            openedPath = (target.address, path)
            Log.conversation.info("door to \(target.macLabel, privacy: .public) open; path \(path?.description ?? "unknown", privacy: .public)")
            report?.path = path
            report?.hello = hello
            report?.transcript = transcript
            phase = .open
            if let began = report?.began { report?.shown = ContinuousClock.now - began }
            composerNote = nil
            Log.conversation.info("open on \(target.macLabel, privacy: .public): \(self.fold.turns.count) turn(s)")
            listen(door)
            let rowCost = await readRow(on: door, attempt)
            guard attempt == generation else { return }
            report?.row = rowCost
            if let line = report?.line { Log.conversation.info("\(line, privacy: .public)") }
            report = nil
            await resendOwed()
            // The tries in a row start again only once nothing is owed: a
            // door that opens and drops again waits longer each time.
            if attempt == generation, cards.card?.owed != true { notUp = 0 }
        } catch {
            guard attempt == generation else {
                Log.conversation.notice("an overtaken open ended: \(String(describing: error), privacy: .public)")
                return
            }
            failed(error, target)
        }
    }

    /// Closes the door and dials again.
    /// With no door here, Home is asked again first, the screen connecting
    /// meanwhile: its Macs are otherwise read once a minute, and a grant may
    /// have just landed.
    public func retry() async {
        Log.conversation.info("retry")
        let noDoor: Bool = if case .noDoor = phase { true } else { false }
        if noDoor || retryNeedsHome {
            phase = .connecting
            await wake()
        }
        report = OpenReport(session: row.id)
        await dial()
    }

    /// Back from the background (v1 W10): the door may have gone while the
    /// app was away, so it is closed and dialled again, as Retry does. The
    /// turns stay on screen, and the re-read keeps what it does not cover.
    public func foreground() async {
        Log.conversation.info("back from the background: dialling again")
        report = OpenReport(session: row.id)
        await dial()
    }

    /// Into the background: the door closes, and `foreground()` dials again.
    public func background() {
        Log.conversation.info("\(self.row.id, privacy: .public) into the background")
        stop()
    }

    /// The person left the screen: the door closes, and nothing dials it
    /// again until the screen's own `open()`.
    public func leave() {
        Log.conversation.info("leaving \(self.row.id, privacy: .public)")
        left = true
        stop()
    }

    private func stop() {
        generation += 1
        later?.cancel()
        later = nil
        close()
    }

    /// What a failed open means: Go's code for a dial, the core's for a
    /// refusal.
    private func failed(_ error: Error, _ target: DoorTarget) {
        report = nil
        let couldNot = "Couldn't open this session on \(target.macLabel)."
        switch error {
        case NodeError.refused(let code, let message):
            Log.conversation.error("the dial was refused: \(code, privacy: .public): \(message, privacy: .public)")
            switch code {
            case "joining", "node_down":
                close()
                phase = .connecting
                tryAgainLater()
                if code == "node_down" { onNodeDown?() }
                return
            case "not_enrolled", "revoked":
                close()
                phase = .refused(Self.removed)
                onRemoved?()
            case "timed_out", "unreachable":
                unavailable(reach(row))
            default:
                close()
                phase = .refused(couldNot)
            }
        case NodeError.unreadableAnswer(let call):
            Log.conversation.error("\(call, privacy: .public) answered something unreadable")
            close()
            phase = .refused(couldNot)
        case DoorError.refusal(let refusal):
            Log.conversation.error("the door refused: \(refusal.code, privacy: .public): \(refusal.message, privacy: .public)")
            close()
            phase = .refused(refusal.code == "protocol" ? refusal.message : couldNot)
        case StoreMismatch.named:
            close()
            retryNeedsHome = true
            phase = .refused(Self.wrongStoreLine(machine: target.macLabel))
        case DoorError.closed(let reason):
            Log.conversation.error("the door closed while opening: \(reason, privacy: .public)")
            close()
            phase = .lost(Self.dropped(target.macLabel))
            if cards.card?.owed == true {
                tryAgainLater(because: "an answer is owed")
                return
            }
        default:
            Log.conversation.error("could not open: \(String(describing: error), privacy: .public)")
            close()
            phase = .refused(couldNot)
        }
        notUp = 0
    }

    /// The hello, and how long its reply was waited for.
    private enum StoreMismatch: Error { case named }

    public static func wrongStoreLine(machine: String) -> String {
        "This connection reaches a different Desk on \(machine). Retry checks Home again and reopens it."
    }

    private func greet(_ door: DoorConnection, _ target: DoorTarget) async throws -> Duration {
        let reply = try await door.weigh(.hello(wants: ["operate", "unknown_words"]))
        switch reply.response {
        case .hello(let hello):
            guard hello.protocolVersion?.major == 1 else {
                let spoken = hello.protocolVersion.map { "\($0.major).\($0.minor)" } ?? "none"
                Log.conversation.error("Desk \(hello.version, privacy: .public) speaks protocol \(spoken, privacy: .public)")
                throw DoorError.refusal(DoorRefusal(code: "protocol", message: "Desk on \(target.macLabel) speaks a protocol this app does not. Update both.", reason: nil))
            }
            guard hello.accepted.contains("operate") else {
                Log.conversation.error("Desk \(hello.version, privacy: .public) on \(target.macLabel, privacy: .public) did not accept operate (accepted \(hello.accepted.joined(separator: ","), privacy: .public))")
                throw DoorError.refusal(DoorRefusal(code: "not_accepted", message: "operate was not accepted", reason: nil))
            }
            if hello.features.contains("transcript_seq"), let actual = hello.host?.storeID,
               let expected = row.machineID, actual != expected {
                throw StoreMismatch.named
            }
            canAttach = hello.capabilities.contains("attachments")
            paging = hello.features.contains("transcript_paging")
            oneRow = hello.features.contains("session_get")
            shape = hello.features.contains("transcript_brief") ? .brief : nil
            storeID = hello.features.contains("transcript_seq") ? hello.host?.storeID : nil
            if let cursor, cursor.storeID != storeID {
                Log.conversation.info("the cursor at \(cursor.throughSeq) belongs to store \(cursor.storeID, privacy: .public); this hello names \(self.storeID ?? "none", privacy: .public): dropped")
                self.cursor = nil
            }
            Log.conversation.info("hello from Desk \(hello.version, privacy: .public), accepted \(hello.accepted.joined(separator: ","), privacy: .public), paging \(self.paging), brief \(self.shape != nil), seq \(self.storeID != nil), one row \(self.oneRow)")
        case .error(let refusal):
            throw DoorError.refusal(refusal)
        default:
            throw DoorError.unexpected("hello")
        }
        return reply.cost.waited
    }

    /// The transcript read again, folded; what its reply cost. With a
    /// cursor, every read is the rows changed since it, whatever `refetch`
    /// asked: smaller than any page, and complete. Without one (the first
    /// read, an older Desk, a dropped cursor), a page as before. A reply
    /// marked `reset` is a page: the fold is replaced.
    private func reread(_ refetch: Refetch, on door: DoorConnection) async throws -> Cost {
        let request: DoorRequest
        if let cursor {
            request = .sessionTranscript(sessionID: row.id, fromTurn: nil, lastTurns: nil, shape: shape, afterSeq: cursor.throughSeq)
        } else {
            request = switch refetch {
            case .last: .sessionTranscript(sessionID: row.id, fromTurn: nil, lastTurns: paging ? 20 : nil, shape: shape)
            case .from(let turn): .sessionTranscript(sessionID: row.id, fromTurn: paging ? turn : nil, lastTurns: nil, shape: shape)
            }
        }
        let reply = try await door.weigh(request)
        switch reply.response {
        case .transcript(let transcript):
            let changes = cursor != nil && !transcript.reset
            if changes {
                fold.update(with: transcript)
            } else {
                fold.replace(with: transcript)
            }
            if let storeID, let through = transcript.throughSeq {
                cursor = Cursor(storeID: storeID, throughSeq: through)
            }
            let read = changes ? "changes" : String(describing: refetch)
            let through = transcript.throughSeq.map(String.init) ?? "none"
            let took = String(describing: reply.cost.waited)
            let decode = String(describing: reply.cost.decoded)
            Log.conversation.info("read \(read, privacy: .public)\(transcript.reset ? " (reset)" : "", privacy: .public): \(transcript.items.count) item(s) from turn \(transcript.fromTurn.map(String.init) ?? "the first", privacy: .public) through seq \(through, privacy: .public) in \(took, privacy: .public) (\(reply.cost.bytes) bytes, decode \(decode, privacy: .public))")
            return reply.cost
        case .error(let refusal):
            throw DoorError.refusal(refusal)
        default:
            throw DoorError.unexpected("session_transcript")
        }
    }

    private func listen(_ door: DoorConnection) {
        listening?.cancel()
        listening = Task { [weak self] in
            for await event in door.events {
                await self?.heard(event, on: door)
            }
            self?.ended(door)
        }
    }

    private func heard(_ event: DoorEvent, on door: DoorConnection) async {
        switch event {
        case .said(let sessionID, let turn, let said) where sessionID == row.id:
            let before = cards.card?.ask.id
            cards.said(said)
            logCard(before)
            switch said {
            case .turnEnded:
                working = false
            case .text, .toolCall, .continued, .heard, .compaction:
                if turn >= (fold.lastTurn ?? turn) { working = true }
            case .handoff, .lagged, .other, .asked, .answered, .unanswered, .elicited, .elicitationDecided:
                break
            }
            guard let refetch = fold.apply(said, turn: turn) else { return }
            do {
                _ = try await reread(refetch, on: door)
            } catch {
                Log.conversation.error("re-read \(String(describing: refetch), privacy: .public): \(String(describing: error), privacy: .public)")
            }
        case .session(let session) where session.id == row.id:
            rowsHeard += 1
            apply(session)
        case .lagged(let missed):
            Log.conversation.notice("the door lagged (\(missed) missed): reading what changed, and the row")
            do {
                _ = try await reread(.last, on: door)
            } catch {
                Log.conversation.error("re-read after a lag: \(String(describing: error), privacy: .public)")
            }
            _ = await readRow(on: door, generation)
        default:
            break
        }
    }

    private func ended(_ closed: DoorConnection) {
        guard door === closed else { return }
        logPathAtClose()
        door = nil
        listening = nil
        if phase == .open {
            Log.conversation.notice("the door to \(self.row.macLabel, privacy: .public) dropped; the turns stay")
            phase = .lost(Self.dropped(row.macLabel))
        }
        if cards.card?.answer != nil { redialSoon() }
    }

    // MARK: what the session waits on

    /// The session's row, read once the door is open: what it waits on now.
    /// The door says a row only when it changes, so an approval parked before
    /// this phone dialled (every push's tap) is read here or not at all. One
    /// frame by `session_get` where the Desk offers it; the environment's
    /// whole list where it does not. Nil when no row was applied, the card
    /// then coming from the events; else what the row's read cost.
    private func readRow(on door: DoorConnection, _ attempt: Int) async -> Cost? {
        do {
            let heard = rowsHeard
            let read = try await oneRow ? readOneRow(on: door) : readListedRow(on: door)
            guard attempt == generation, let read else { return nil }
            let (mine, cost) = read
            let took = String(describing: cost.waited)
            guard rowsHeard == heard else {
                Log.conversation.notice("read the row in \(took, privacy: .public), but the door said a newer one meanwhile: dropped")
                return nil
            }
            Log.conversation.info("read the row in \(took, privacy: .public) (\(cost.bytes) bytes); it waits on \(mine.waiting)")
            apply(mine)
            return cost
        } catch {
            Log.conversation.error("the row read failed: \(String(describing: error), privacy: .public); the card comes from the events")
            return nil
        }
    }

    /// The row by `session_get`. A Mac that does not hold it (a lobby-only
    /// row) says `unknown_session`, and no row is read.
    private func readOneRow(on door: DoorConnection) async throws -> (DoorSession, Cost)? {
        let reply = try await door.weigh(.sessionGet(sessionID: row.id))
        switch reply.response {
        case .session(let mine):
            guard mine.id == row.id else {
                Log.conversation.notice("session_get for \(self.row.id, privacy: .public) answered \(mine.id, privacy: .public); the card comes from the events")
                return nil
            }
            return (mine, reply.cost)
        case .error(let refusal) where refusal.code == "unknown_session":
            Log.conversation.notice("\(self.row.macLabel, privacy: .public) does not hold \(self.row.id, privacy: .public) (a lobby-only row); the card comes from the events")
            return nil
        case .error(let refusal):
            throw DoorError.refusal(refusal)
        default:
            Log.conversation.error("session_get answered \(reply.response.kind, privacy: .public); the card comes from the events")
            return nil
        }
    }

    /// The row from its environment's `sessions_list`, for a Desk without
    /// `session_get`.
    private func readListedRow(on door: DoorConnection) async throws -> (DoorSession, Cost)? {
        guard let environment = try await environment(on: door) else { return nil }
        let reply = try await door.weigh(.sessionsList(environmentID: environment))
        guard case .sessions(let rows) = reply.response else {
            Log.conversation.error("sessions_list answered \(reply.response.kind, privacy: .public); the card comes from the events")
            return nil
        }
        guard let mine = rows.first(where: { $0.id == row.id }) else {
            Log.conversation.notice("read \(rows.count) row(s); none is \(self.row.id, privacy: .public)")
            return nil
        }
        Log.conversation.info("read \(rows.count) row(s) for this one")
        return (mine, reply.cost)
    }

    /// The environment whose organisations hold this row's, the mothership
    /// first where two do. Asked once a conversation; a refusal is asked
    /// again on the next open.
    private func environment(on door: DoorConnection) async throws -> String? {
        if let environmentID { return environmentID }
        let response = try await door.request(.environmentsList)
        guard case .environments(let all) = response else {
            Log.conversation.error("environments_list answered \(response.kind, privacy: .public); no row read")
            return nil
        }
        let holding = all.filter { $0.organizations.contains { $0.id == row.organizationID } }
        environmentID = (holding.first { $0.mothership } ?? holding.first)?.id
        if let environmentID {
            Log.conversation.info("\(all.count) environment(s); organisation \(self.row.organizationID, privacy: .public) is in \(environmentID, privacy: .public)")
        } else {
            Log.conversation.error("\(all.count) environment(s) on \(self.row.macLabel, privacy: .public); none holds organisation \(self.row.organizationID, privacy: .public)")
        }
        return environmentID
    }

    private func apply(_ session: DoorSession) {
        working = session.working
        if let started = session.turnStartedAt, session.working { workingSince = started }
        if !session.working { fold.lastEnded(session.lastOutcome) }
        if let root = session.worktreePath { worktreeRoot = root }
        composerHeard(session)
        let before = cards.card?.ask.id
        cards.row(session.attention, now: now())
        logCard(before)
    }

    private func logCard(_ before: String?) {
        let after = cards.card?.ask.id
        guard after != before else { return }
        Log.conversation.info("the card: \(before ?? "none", privacy: .public) → \(after ?? "none", privacy: .public)\(self.cards.cleared.map { " (\($0))" } ?? "", privacy: .public)")
    }

    // MARK: acting

    /// The person answered the pinned card.
    public func answer(_ answer: Answer) async {
        guard let request = cards.answer(answer, sessionID: row.id, now: now(), op: ids) else {
            Log.conversation.notice("an answer with nothing to answer, one already out, or a question expired; nothing sent\(self.cards.cleared.map { " (\($0))" } ?? "", privacy: .public)")
            return
        }
        Log.conversation.info("answering \(self.cards.card?.ask.id ?? "none", privacy: .public) as \(request.kind, privacy: .public)")
        await send(answer: request)
    }

    /// Sends an answer under the op it carries, and weighs the reply.
    private func send(answer request: DoorRequest) async {
        guard let op = request.op else {
            Log.conversation.error("\(request.kind, privacy: .public) carries no op; not sent")
            return
        }
        guard let door, door.isOpen else {
            Log.conversation.notice("\(request.kind, privacy: .public) \(op, privacy: .public) with no door open: owed")
            cards.unsent(op: op, macLabel: row.macLabel)
            redialSoon()
            return
        }
        let attempt = generation
        do {
            let response = try await door.request(request)
            Log.conversation.info("\(request.kind, privacy: .public) \(op, privacy: .public) answered \(response.kind, privacy: .public)")
            if cards.replied(response, op: op), (await readRow(on: door, attempt)) == nil {
                Log.conversation.notice("no row read after the refusal; the refusal stands")
                cards.rowUnread()
            }
        } catch {
            Log.conversation.error("\(request.kind, privacy: .public) \(op, privacy: .public) did not reach the Mac: \(String(describing: error), privacy: .public); owed")
            cards.unsent(op: op, macLabel: row.macLabel)
            // Left, or overtaken by a newer open: the next open resends it.
            if attempt == generation {
                redialSoon()
            } else {
                Log.conversation.info("\(op, privacy: .public) is owed to the next open; no redial from a door left behind")
            }
        }
    }

    /// The door is back: an answer owed from before it dropped goes again,
    /// under its own op.
    private func resendOwed() async {
        guard let request = cards.resend(sessionID: row.id) else { return }
        Log.conversation.info("the door is back: resending \(request.kind, privacy: .public) \(request.op ?? "", privacy: .public)")
        await send(answer: request)
    }

    /// An answer is owed: dial again by itself, 1 s, 2 s, 4 s… apart. A
    /// try already waiting, or an open under way, resends it.
    private func redialSoon() {
        guard later == nil, phase != .connecting else { return }
        tryAgainLater(because: "an answer is owed")
    }

    /// What the approval card draws under its heading: the call's line as
    /// the transcript has it, else the request's own title, else the agent's
    /// input, cut short. A title is drawn whole: it is what is approved.
    public func detail(for request: PermissionRequest) -> String? {
        if let title = fold.toolTitle(callID: request.callID) { return title }
        if let title = request.title { return title }
        if request.inputDropped { return Self.inputTooLong }
        guard let input = request.input, input != .null, let text = try? input.text() else { return nil }
        return text.count > 280 ? String(text.prefix(280)) + "…" : text
    }

    private func unavailable(_ available: Reach) {
        report = nil
        close()
        notUp = 0
        updateUnavailablePhase(available)
        later?.cancel()
        let sleep = sleep
        let every = wakeEvery
        later = Task { [weak self] in
            while !Task.isCancelled {
                do { try await sleep(every) } catch { return }
                guard let self, !Task.isCancelled else { return }
                await self.wake()
                guard !Task.isCancelled else { return }
                // Up with no door here is dialled too: the dial says so,
                // and the loop ends.
                let available = self.reach(self.row)
                switch available {
                case .door(let door) where door.live:
                    Log.conversation.info("\(self.row.macLabel, privacy: .public) woke")
                case .noDoor, .notOpen:
                    Log.conversation.info("\(self.row.macLabel, privacy: .public) woke with no door here")
                case .door, .none, .linkUnavailable:
                    self.updateUnavailablePhase(available)
                    continue
                }
                self.later = nil
                await self.dial()
                return
            }
        }
    }

    private func updateUnavailablePhase(_ available: Reach) {
        phase = available.deskOnline
            ? .linkUnavailable(macLabel: row.macLabel)
            : .asleep(macLabel: row.macLabel)
    }

    /// B6: no wake loop, and nothing tried again by itself: the next open,
    /// the way back from the background, or Retry asks again.
    private func noDoor(_ organization: String) {
        report = nil
        close()
        notUp = 0
        later?.cancel()
        later = nil
        phase = .noDoor(organization: organization)
    }

    /// F7: the node is still joining, or not up, or an answer is owed. The
    /// door is tried again after 1 s, 2 s, 4 s…, at most `wakeEvery` apart.
    private func tryAgainLater(because why: String = "the node is not up") {
        notUp += 1
        let delay = min(Duration.seconds(1 << min(notUp - 1, 10)), wakeEvery)
        Log.conversation.info("\(why, privacy: .public); trying again in \(String(describing: delay), privacy: .public) (try \(self.notUp))")
        later?.cancel()
        let sleep = sleep
        later = Task { [weak self] in
            do { try await sleep(delay) } catch { return }
            guard let self, !Task.isCancelled else { return }
            self.later = nil
            await self.dial()
        }
    }

    private func close() {
        listening?.cancel()
        listening = nil
        if door != nil { logPathAtClose() }
        door?.close()
        door = nil
    }

    /// B1: the node's path to the Mac again as its door closes, logged when
    /// the node can say and it is not the path the door opened on.
    private func logPathAtClose() {
        guard let opened = openedPath else { return }
        openedPath = nil
        let doors = doors
        Task {
            // The node cannot say (down, joining, tearing down): nothing to compare.
            guard let now = await doors.peerPath(address: opened.address), now != opened.path else { return }
            Log.conversation.info("path to \(opened.address, privacy: .public) at close: \(now.description, privacy: .public) (was \(opened.path?.description ?? "unknown", privacy: .public))")
        }
    }

    static func dropped(_ mac: String) -> String { "The link to \(mac) dropped." }
}
