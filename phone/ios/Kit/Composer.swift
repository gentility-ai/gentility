import Foundation

/// A prompt queued behind the running turn, as the composer draws it.
public struct PendingLine: Equatable, Sendable, Identifiable {
    public let queueID: String
    public let text: String
    public var note: String?

    public var id: String { queueID }

    public init(queueID: String, text: String, note: String? = nil) {
        self.queueID = queueID
        self.text = text
        self.note = note
    }
}

/// The queue under the composer: the row's `queued` whenever a row is said,
/// and a prompt this phone queued since.
public struct PendingLines: Equatable, Sendable {
    public private(set) var lines: [PendingLine] = []
    /// The steer each line's row last said.
    private var steers: [String: String] = [:]
    /// The notes this phone set, newer than the row they were set under.
    private var notes: [String: String] = [:]

    public static let steering = "Steering…"
    public static let steerRefused = "Couldn't steer it in — it goes at the turn's end"
    /// `unknown`: Desk cannot say whether the agent heard it, and never sends
    /// the line by itself.
    public static let steerLost = "Desk lost track of this steer. Take it back, or send it from your Mac."

    public init() {}

    /// A row was said: its queue is the queue. A note this phone set stands
    /// until the row's steer for that line moves on; then the row's own says.
    public mutating func row(_ queued: [QueuedPrompt]) {
        var steers: [String: String] = [:]
        var notes: [String: String] = [:]
        lines = queued.map { prompt in
            let id = prompt.queueID
            steers[id] = prompt.steer
            if prompt.steer == self.steers[id] { notes[id] = self.notes[id] }
            return PendingLine(queueID: id, text: prompt.text, note: notes[id] ?? Self.note(prompt.steer))
        }
        self.steers = steers
        self.notes = notes
    }

    public mutating func queued(_ queueID: String, _ text: String) {
        guard !lines.contains(where: { $0.queueID == queueID }) else { return }
        lines.append(PendingLine(queueID: queueID, text: text))
    }

    public mutating func note(_ queueID: String, _ note: String?) {
        guard let at = lines.firstIndex(where: { $0.queueID == queueID }) else { return }
        lines[at].note = note
        notes[queueID] = note
    }

    @discardableResult
    public mutating func remove(_ queueID: String) -> PendingLine? {
        guard let at = lines.firstIndex(where: { $0.queueID == queueID }) else { return nil }
        steers[queueID] = nil
        notes[queueID] = nil
        return lines.remove(at: at)
    }

    static func note(_ steer: String?) -> String? {
        switch steer {
        case "asked", "claimed": steering
        case "refused": steerRefused
        case "unknown": steerLost
        default: nil
        }
    }
}

// MARK: the composer's verbs

/// The composer's verbs. Its state (`pending`, `composerNote`, `sending`,
/// `unsentPrompt`, `takingBack`) is declared on the model in
/// Conversation.swift. A prompt is never owed the way an answer is: words
/// that did not go stay in the composer for the person to send again, so
/// nothing here dials.
extension ConversationModel {
    public static let turnStarted = "A turn started on your Mac. Queue it, or steer it in."
    public static let alreadySent = "Already sent."
    public static let tooLateToTakeBack = "Being steered in — too late to take it back."
    public static func unreachableToSend(_ mac: String) -> String { "Couldn't reach \(mac). Your message is still here." }
    /// The Mac answered with something this app cannot read: it may have
    /// taken the words, or not.
    public static func unreadableReply(_ mac: String) -> String { "Couldn't read what Desk on \(mac) said." }

    private enum Took { case sent, queued(String) }

    /// The composer's ⏎ and Queue: a turn when the session is idle, queued
    /// behind the turn when one runs. True when the Mac took the words.
    public func send(_ text: String) async -> Bool {
        await prompt(text, queue: working) != nil
    }

    /// Steer now: queued, then steered into the turn. Nothing running by the
    /// time it lands, the prompt went at once and nothing is steered.
    public func steer(_ text: String) async -> Bool {
        guard working else { return await send(text) }
        switch await prompt(text, queue: true) {
        case .queued(let queueID)?:
            await steer(queueID: queueID)
            return true
        case .sent?:
            Log.conversation.info("Steer now: the words went as a turn of their own; nothing to steer")
            return true
        case nil:
            return false
        }
    }

    /// Take back: off the Mac's queue, and the words back to the composer.
    public func takeBack(_ queueID: String) async -> String? {
        guard !takingBack.contains(queueID) else {
            Log.conversation.notice("take back of \(queueID, privacy: .public) is already out")
            return nil
        }
        guard let door, door.isOpen else {
            Log.conversation.notice("take back of \(queueID, privacy: .public) with no door open")
            composerNote = Self.unreachableToSend(row.macLabel)
            return nil
        }
        // Read before asking: the row the Mac says as it withdraws can land
        // before the reply, and empty the queue the words are in.
        let words = pending.lines.first { $0.queueID == queueID }?.text
        takingBack.insert(queueID)
        defer { takingBack.remove(queueID) }
        do {
            let response = try await door.request(.sessionWithdraw(sessionID: row.id, queueID: queueID))
            Log.conversation.info("take back of \(queueID, privacy: .public) answered \(response.kind, privacy: .public)")
            switch response {
            case .ok:
                pending.remove(queueID)
                composerNote = nil
                return words
            case .error(let refusal) where refusal.code == "unknown_option":
                pending.remove(queueID)
                composerNote = Self.alreadySent
            case .error(let refusal) where refusal.code == "steering":
                pending.note(queueID, Self.tooLateToTakeBack)
            case .error(let refusal):
                composerNote = refusal.message
            default:
                composerNote = Self.unreadableReply(row.macLabel)
            }
        } catch {
            Log.conversation.error("take back of \(queueID, privacy: .public) did not reach the Mac: \(String(describing: error), privacy: .public)")
            composerNote = Self.unreachableToSend(row.macLabel)
        }
        return nil
    }

    /// A row of this session was said: its queue is the composer's. A row
    /// idle with nothing queued means the turn a lost prompt may have started
    /// is over, so the same words typed again are a new prompt, not that one.
    /// A turn's end is not that: the core starts the next queued prompt at
    /// once, and `working` does not count the queue. Not while a prompt is
    /// out: a row said before it lands says nothing of it.
    func composerHeard(_ session: DoorSession) {
        pending.row(session.queued)
        if session.requestedPrompt?.id != requested?.id {
            Log.conversation.info("the requested prompt: \(self.requested?.id ?? "none", privacy: .public) → \(session.requestedPrompt?.id ?? "none", privacy: .public)\(session.requestedPrompt?.compacting == true ? " (behind a compaction)" : "", privacy: .public)")
        }
        requested = session.requestedPrompt
        guard !session.working, session.queued.isEmpty, !sending, let held = unsentPrompt else { return }
        Log.conversation.info("the session is idle with nothing queued: prompt \(held.op ?? "", privacy: .public) is no longer held")
        unsentPrompt = nil
        if composerNote == Self.unreachableToSend(row.macLabel) { composerNote = nil }
    }

    private func prompt(_ text: String, queue: Bool) async -> Took? {
        let words = text.trimmingCharacters(in: .whitespacesAndNewlines)
        let attachments = tray.refs
        guard !words.isEmpty || !attachments.isEmpty, !sending else { return nil }
        guard tray.canSend(words) else {
            Log.conversation.notice("prompt while \(self.tray.busyCount) photo(s) are still going or one failed; nothing sent")
            return nil
        }
        guard let door, door.isOpen else {
            Log.conversation.notice("prompt (\(words.utf8.count) bytes) with no door open; the words stay")
            composerNote = Self.unreachableToSend(row.macLabel)
            return nil
        }
        let request = request(for: words, attachments: attachments, queue: queue)
        let op = request.op ?? ""
        sending = true
        defer { sending = false }
        Log.conversation.info("prompt \(op, privacy: .public) (\(words.utf8.count) bytes, \(attachments.count) attachment(s), queue \(request.body["queue"]?.bool ?? false))")
        do {
            let response = try await door.request(request)
            Log.conversation.info("prompt \(op, privacy: .public) answered \(response.kind, privacy: .public)")
            switch response {
            case .ok:
                unsentPrompt = nil
                composerNote = nil
                tray.clear()
                working = true
                return .sent
            case .queued(let queueID):
                unsentPrompt = nil
                composerNote = nil
                tray.clear()
                pending.queued(queueID, words.isEmpty ? Self.photosOnly(attachments.count) : words)
                return .queued(queueID)
            case .error(let refusal) where refusal.code == "turn_in_progress":
                // The Mac recorded the refusal under this op: Queue is a new one.
                unsentPrompt = nil
                working = true
                composerNote = Self.turnStarted
            case .error(let refusal) where refusal.code == "cannot_attach":
                // The words and the photos stay, for the person to remove them.
                unsentPrompt = nil
                composerNote = TrayError.cannotAttach(refusal.message).sentence
            case .error(let refusal):
                unsentPrompt = nil
                composerNote = refusal.message
            default:
                // The Mac may have taken the words: they stay held.
                composerNote = Self.unreadableReply(row.macLabel)
            }
        } catch {
            Log.conversation.error("prompt \(op, privacy: .public) did not reach the Mac: \(String(describing: error), privacy: .public); the words stay, under their op")
            composerNote = Self.unreachableToSend(row.macLabel)
        }
        return nil
    }

    /// The pending bubble at the feed's end: the row's requested prompt, but
    /// not while this phone's prompt of the same words is out. Those words
    /// are still in the composer, which empties once the Mac takes them; a
    /// row said before that reply would draw them twice.
    public var waitingPrompt: RequestedPrompt? {
        guard let requested else { return nil }
        if sending, case .sessionPrompt(_, let words, _, _, _)? = unsentPrompt, words == requested.text { return nil }
        return requested
    }

    /// A queued line that carries photos and no words.
    public static func photosOnly(_ count: Int) -> String { count == 1 ? "1 photo" : "\(count) photos" }

    /// What these words go as. Words whose reply was lost go again exactly
    /// as they went, `queue` and all, whatever the session does now, so the
    /// Mac answers them from its record: the core refuses the same op with
    /// other arguments. Other words are a new prompt under a new op.
    private func request(for words: String, attachments: [AttachmentRef], queue: Bool) -> DoorRequest {
        if let held = unsentPrompt, case .sessionPrompt(_, words, _, _, attachments) = held {
            Log.conversation.info("the same words again: prompt \(held.op ?? "", privacy: .public) goes again as it went")
            return held
        }
        let request = DoorRequest.sessionPrompt(sessionID: row.id, text: words, queue: queue, op: ids(), attachments: attachments)
        unsentPrompt = request
        return request
    }

    /// Stop the turn running. Prompts waiting behind it still run. True
    /// when the Mac took it.
    @discardableResult
    public func stop() async -> Bool {
        guard let door, door.isOpen else {
            Log.conversation.notice("stop with no door open")
            return false
        }
        do {
            let response = try await door.request(.sessionAbort(sessionID: row.id))
            Log.conversation.info("stop answered \(response.kind, privacy: .public)")
            return response == .ok
        } catch {
            Log.conversation.error("the stop did not reach the Mac: \(String(describing: error), privacy: .public)")
            return false
        }
    }

    private func steer(queueID: String) async {
        pending.note(queueID, PendingLines.steering)
        guard let door, door.isOpen else {
            Log.conversation.notice("steer of \(queueID, privacy: .public) with no door open")
            return pending.note(queueID, PendingLines.steerRefused)
        }
        do {
            let response = try await door.request(.sessionSteer(sessionID: row.id, queueID: queueID))
            Log.conversation.info("steer of \(queueID, privacy: .public) answered \(response.kind, privacy: .public)")
            if response != .ok { pending.note(queueID, PendingLines.steerRefused) }
        } catch {
            Log.conversation.error("the steer of \(queueID, privacy: .public) did not reach the Mac: \(String(describing: error), privacy: .public)")
            pending.note(queueID, PendingLines.steerRefused)
        }
    }
}
