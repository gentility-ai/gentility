import Foundation

/// From a scanned link to a paired phone.
@MainActor
public final class PairingFlow: ObservableObject {
    public enum State: Equatable {
        case scanning
        case claiming(host: String)
        /// "Check on your Mac".
        case number(machineLabel: String, match: String, host: String)
        /// "Joining your network".
        case joining(machineLabel: String)
        case paired
        case failed(Failure)
    }

    /// How long a code lives when its `expires_at` does not read
    /// (the claim endpoint's five-minute lifetime, also used by the Go harness).
    static let claimLifetime: TimeInterval = 5 * 60
    static let pollEvery: Duration = .seconds(2)

    @Published public private(set) var state: State = .scanning
    private var running: Task<Void, Never>?

    private let store: CredentialStore
    private let nodeDirectory: NodeDirectory
    private let transport: (URL) -> ClaimTransport
    private let join: (Credential) async -> Void
    private let sleep: @Sendable (Duration) async throws -> Void
    private let now: () -> Date
    private let joinWait: Duration

    /// `sleep` paces the poll; `joinWait` is the most Home waits on Join, by
    /// the wall clock.
    public init(
        store: CredentialStore,
        nodeDirectory: NodeDirectory,
        transport: @escaping (URL) -> ClaimTransport = { ClaimClient(origin: $0) },
        join: @escaping (Credential) async -> Void,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) },
        now: @escaping () -> Date = Date.init,
        joinWait: Duration = .seconds(5)
    ) {
        self.store = store
        self.nodeDirectory = nodeDirectory
        self.transport = transport
        self.join = join
        self.sleep = sleep
        self.now = now
        self.joinWait = joinWait
    }

    /// A scanned link. Whatever this phone held before is forgotten first, so
    /// a QR from another deployment pairs with that deployment and nothing of
    /// the last one is left to mix with it. A pairing already running is
    /// called off first.
    public func begin(_ link: ClaimLink, phoneName: String) async {
        await run { await $0.pair(link, phoneName: phoneName) }
    }

    /// At launch: a claim redeemed before the app was killed is polled again,
    /// unless its bearer was already kept (a kill, or a failed clear, after
    /// the save): then the claim is spent and the phone is paired; polling
    /// would only hear `spent`, and Scan again would wipe a good bearer.
    public func resume() async {
        await run { await $0.resumeKept() }
    }

    /// Back to the scanner, calling off any pairing still running.
    public func reset() {
        Log.pairing.info("back to the scanner")
        running?.cancel()
        running = nil
        state = .scanning
    }

    /// One pairing at a time: the one running is cancelled, and a cancelled
    /// one changes nothing after its next suspension.
    private func run(_ body: @escaping (PairingFlow) async -> Void) async {
        running?.cancel()
        let task = Task { await body(self) }
        running = task
        await task.value
        if running == task { running = nil }
    }

    private func pair(_ link: ClaimLink, phoneName: String) async {
        Log.pairing.info("pairing with \(link.host, privacy: .public) as \(phoneName, privacy: .public)")
        do {
            try store.wipe()
            try nodeDirectory.wipe()
        } catch {
            Log.pairing.error("could not forget an earlier pairing: \(String(describing: error), privacy: .public)")
            state = .failed(.cannotJoin("this_phone"))
            return
        }
        state = .claiming(host: link.host)
        let reply: ClaimReply
        do {
            reply = try await transport(link.origin).claim(code: link.code, phoneName: phoneName)
        } catch let refusal as ClaimRefusal {
            guard !Task.isCancelled else { return }
            Log.pairing.info("the claim was refused: \(String(describing: refusal), privacy: .public)")
            state = .failed(Failure(refusal))
            return
        } catch {
            guard !Task.isCancelled else { return }
            Log.pairing.error("the claim did not reach \(link.host, privacy: .public): \(String(describing: error), privacy: .public)")
            state = .failed(.unreachable(host: link.host))
            return
        }
        guard !Task.isCancelled else {
            Log.pairing.info("a claim on \(reply.machineLabel, privacy: .public) was called off")
            return
        }
        Log.pairing.info("claimed on \(reply.machineLabel, privacy: .public); the code expires \(reply.expiresAt, privacy: .public)")
        let pending = PendingClaim(
            origin: link.origin, host: link.host, pollSecret: reply.pollSecret,
            machineLabel: reply.machineLabel, match: reply.match, expiresAt: reply.expiresAt, phoneName: phoneName
        )
        do {
            try store.savePending(pending)
        } catch {
            Log.pairing.error("the poll secret was not kept; a kill now loses this claim: \(String(describing: error), privacy: .public)")
        }
        await poll(pending)
    }

    private func resumeKept() async {
        let found: PendingClaim?
        do {
            found = try store.pendingClaim()
        } catch {
            if !store.forgetIfUnreadable(error) {
                Log.pairing.error("could not read a kept claim: \(String(describing: error), privacy: .public)")
            }
            return
        }
        guard let pending = found else {
            Log.pairing.info("no claim was waiting at launch")
            return
        }
        let kept: Credential?
        do {
            kept = try store.credential()
        } catch {
            if !store.forgetIfUnreadable(error) {
                Log.pairing.error("could not read the kept bearer; the claim waits for a later launch: \(String(describing: error), privacy: .public)")
            }
            return
        }
        if kept != nil {
            Log.pairing.info("the claim for \(pending.machineLabel, privacy: .public) was already allowed and its bearer kept; clearing it")
            clearPending()
            state = .paired
            return
        }
        Log.pairing.info("resuming the claim for \(pending.machineLabel, privacy: .public)")
        await poll(pending)
    }

    /// Until the Mac decides or the code dies. Only a poll that got no answer
    /// or a 5xx is asked again; any other refusal ends it (F5).
    private func poll(_ pending: PendingClaim) async {
        state = .number(machineLabel: pending.machineLabel, match: pending.match, host: pending.host)
        let transport = transport(pending.origin)
        let deadline: Date
        if let expires = WireDate.parse(pending.expiresAt) {
            deadline = expires
        } else {
            deadline = now().addingTimeInterval(Self.claimLifetime)
            Log.pairing.error("the code's expiry \(pending.expiresAt, privacy: .public) does not read; polling for \(Int(Self.claimLifetime)) s")
        }
        var heard = ""
        polling: while !Task.isCancelled {
            guard now() < deadline else {
                Log.pairing.info("the code expired before the Mac decided")
                return finish(.codeSpent)
            }
            do {
                let answer = try await transport.poll(secret: pending.pollSecret)
                // Called off while asking: a newer pairing owns the state and
                // the store, and an allowed here is spent unheard, as a kill
                // would spend it.
                guard !Task.isCancelled else { break polling }
                switch answer {
                case .waiting(let word):
                    if word != heard {
                        Log.pairing.info("the poll says \(word, privacy: .public)")
                        heard = word
                    }
                case .denied:
                    return finish(.denied)
                case .spent, .expired, .unknown:
                    return finish(.codeSpent)
                case .failed(let word):
                    return finish(.cannotJoin(word))
                case .allowed(let granted):
                    return await allowed(granted, pending)
                }
            } catch _ where Task.isCancelled {
                break polling
            } catch let unanswered as PollUnanswered {
                Log.pairing.error("a poll to \(pending.host, privacy: .public) got no answer (\(unanswered.reason, privacy: .public)); asking again")
            } catch let refusal as ClaimRefusal {
                Log.pairing.error("the poll was refused: \(String(describing: refusal), privacy: .public)")
                return finish(Failure(refusal))
            } catch {
                Log.pairing.error("the poll failed: \(String(describing: error), privacy: .public)")
                return finish(.cannotJoin("server_error"))
            }
            do { try await sleep(Self.pollEvery) } catch { break polling }
        }
        Log.pairing.info("the poll for \(pending.machineLabel, privacy: .public) was called off")
    }

    private func finish(_ failure: Failure) {
        clearPending()
        Log.pairing.info("pairing ended: \(String(describing: failure), privacy: .public)")
        state = .failed(failure)
    }

    private func clearPending() {
        do {
            try store.clearPending()
        } catch {
            Log.pairing.error("could not clear the spent claim: \(String(describing: error), privacy: .public)")
        }
    }

    private func allowed(_ granted: Granted, _ pending: PendingClaim) async {
        // The bearer exists nowhere else now: the server spent the claim in
        // the transaction that minted it. It is saved before anything else is
        // done with it.
        let credential = Credential(
            origin: pending.origin, bearer: granted.accessToken,
            expiresAt: now().addingTimeInterval(TimeInterval(granted.expiresIn)),
            enrollmentJSON: granted.enrollmentJSON, enrollmentID: granted.enrollmentID,
            phoneName: pending.phoneName
        )
        do {
            try store.save(credential)
        } catch {
            Log.pairing.error("the bearer could not be kept: \(String(describing: error), privacy: .public)")
            return finish(.cannotJoin("this_phone"))
        }
        clearPending()
        Log.pairing.info("allowed on \(pending.machineLabel, privacy: .public), enrolment \(granted.enrollmentID ?? "?", privacy: .public); joining 6net")
        state = .joining(machineLabel: pending.machineLabel)
        let joining = Task { await self.join(credential) }
        let joined = await Self.first(of: joining, orAfter: joinWait)
        if !joined {
            Log.pairing.info("6net is still joining after \(String(describing: self.joinWait), privacy: .public); Home goes on")
        }
        guard !Task.isCancelled else { return }
        state = .paired
    }

    /// Whether the join finished before the wait ran out; it goes on either
    /// way (Home is never held for 6net past five seconds).
    private static func first(of task: Task<Void, Never>, orAfter wait: Duration) async -> Bool {
        await withCheckedContinuation { (done: CheckedContinuation<Bool, Never>) in
            let once = Once(done)
            Task { await task.value; once.resume(true) }
            Task { try? await Task.sleep(for: wait); once.resume(false) }
        }
    }
}

/// A continuation resumed by whichever of several callers comes first.
final class Once<Value: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Value, Never>?

    init(_ continuation: CheckedContinuation<Value, Never>) {
        self.continuation = continuation
    }

    func resume(_ value: Value) {
        let taken: CheckedContinuation<Value, Never>? = lock.withLock {
            defer { continuation = nil }
            return continuation
        }
        taken?.resume(returning: value)
    }
}
