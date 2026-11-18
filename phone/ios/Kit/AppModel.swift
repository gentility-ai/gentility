import Foundation

/// The app's one root: which screen, and the node's life behind it.
///
/// The launch rule (slice 8.2): no bearer, the scanner; a bearer, Home at
/// once from the lobby, with the node brought up behind it. Home never
/// waits for 6net.
@MainActor
public final class AppModel: ObservableObject {
    public enum Route: Equatable {
        case launching
        case pairing
        case home
        /// F5. Pairing's own failures (F1–F3) are `pairing.state`'s.
        case failure(Failure)
    }

    public enum NodeState: Equatable {
        case down
        case joining
        case up(address: String)
        case failed(String)
    }

    /// The furthest apart a keychain read or a Rejoin is tried again.
    static let mostApart: Duration = .seconds(30)

    @Published public private(set) var route: Route = .launching
    @Published public private(set) var nodeState: NodeState = .down
    @Published public private(set) var home: HomeModel?
    /// Home's footer: notifications are denied in Settings.
    @Published public private(set) var notificationsOff = false
    /// Home's navigation; a tap on a notification sets it.
    @Published public var path: [HomeRow] = []
    public let scanner: ScannerModel
    public private(set) lazy var pairing = PairingFlow(
        store: store, nodeDirectory: nodeDirectory, transport: transport,
        join: { [weak self] credential in await self?.bringNodeUp(credential) },
        now: now
    )

    private let store: CredentialStore
    private let nodeDirectory: NodeDirectory
    private let node: NodeControl & DoorOpening
    private let makeLobby: (Credential) -> Lobby
    private let transport: (URL) -> ClaimTransport
    private let defaults: UserDefaults
    private let now: () -> Date
    private let protectedData: () async -> Void
    private let sleep: @Sendable (Duration) async throws -> Void
    /// When this model began: launch, for Home's first drawing; nil once
    /// that is said.
    private var launched: ContinuousClock.Instant? = .now
    private var credential: Credential?
    /// Moves on at every `forget`, so a join still out when the pairing was
    /// forgotten does not write into what comes after it.
    private var generation = 0
    /// The generation whose Join or Rejoin is out now, if one is.
    private var bringingUp: Int?
    /// A Rejoin asked again later: the node's state not readable yet.
    private var nodeRetry: Task<Void, Never>?
    private var nodeTries = 0
    /// A Status out to see how the node stands (`checkNode`), if one is.
    private var checkingNode = false
    /// Set while `forget` runs: a second one is a no-op, and "Pair again"
    /// waits for it (`againWhenForgotten`).
    private var forgetting = false
    private var againWhenForgotten = false
    /// Set by `background()`; `foreground()` acts only after one.
    private var away = false
    private weak var visible: ConversationModel?

    private let notifications: Notifications?
    /// The APNs host this build's tokens belong to (`PushEnvironment`).
    private let pushEnvironment: String?
    private var lobby: Lobby?
    private var personID: String?
    private var pushToken: String?
    /// What the server last took from `register`: each once, and again
    /// only once it changes.
    private var registered: Registration?
    /// One `register` out at a time, so a newer token never lands first.
    private var registering = false
    private var askedForToken = false
    /// A tap waiting for Home to list its row.
    private var pendingOpen: PushPayload?
    /// The tap Home is read again for, once.
    private var readAgainFor: PushPayload?

    private struct Registration: Equatable {
        let person: String
        let token: String?
        let environment: String?
    }

    /// `protectedData` returns once the keychain and the node's files can be
    /// read (the app's waits for the first unlock); `sleep` paces the
    /// tries again.
    public init(
        store: CredentialStore,
        nodeDirectory: NodeDirectory,
        node: NodeControl & DoorOpening,
        permission: CameraPermission,
        makeLobby: @escaping (Credential) -> Lobby = { ServerLobby(credential: $0) },
        transport: @escaping (URL) -> ClaimTransport = { ClaimClient(origin: $0) },
        notifications: Notifications? = nil,
        pushEnvironment: String? = nil,
        defaults: UserDefaults = .standard,
        now: @escaping () -> Date = Date.init,
        protectedData: @escaping () async -> Void = {},
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.store = store
        self.nodeDirectory = nodeDirectory
        self.node = node
        self.makeLobby = makeLobby
        self.transport = transport
        self.notifications = notifications
        self.pushEnvironment = pushEnvironment
        self.defaults = defaults
        self.now = now
        self.protectedData = protectedData
        self.sleep = sleep
        scanner = ScannerModel(permission: permission)
    }

    /// Once per process. A claim left waiting by a kill is resumed before
    /// anything can begin another.
    public func launch() async {
        guard route == .launching else { return }
        Log.app.info("launch: waiting for protected data")
        await protectedData()
        // A wait cut short says nothing about the keychain: the next launch
        // (the view's task again) waits and reads.
        guard !Task.isCancelled else {
            Log.app.info("launch called off while waiting for protected data")
            return
        }
        store.forgetIfReinstalled(defaults)
        Log.app.info("launch; node dir protection: \(self.nodeDirectory.protection()?.rawValue ?? "none yet", privacy: .public)")
        let kept: Credential?
        switch await readKept() {
        case .read(let credential):
            kept = credential
        case .forgotten:
            route = .pairing
            return
        case .calledOff:
            Log.app.info("launch called off while the keychain was refusing; the next one reads again")
            return
        }
        if (try? store.pendingClaim()) != nil {
            if kept == nil { route = .pairing }
            await pairing.resume()
            if pairing.state == .paired { return await arrive() }
            if kept == nil { return }
        }
        guard let credential = kept else {
            Log.app.info("no pairing kept: the scanner")
            route = .pairing
            return
        }
        Log.app.info("a pairing is kept: \(String(describing: credential), privacy: .public)")
        await open(credential)
    }

    /// The scanner read a Desk link.
    public func scanned(_ link: ClaimLink, phoneName: String) async {
        await pairing.begin(link, phoneName: phoneName)
        if pairing.state == .paired { await arrive() }
    }

    private enum Kept {
        /// What the keychain holds: a credential, or none.
        case read(Credential?)
        /// What it held no longer reads, and was forgotten.
        case forgotten
        /// The launch was cancelled while the keychain refused.
        case calledOff
    }

    /// The kept credential, asked of the keychain until it answers.
    private func readKept() async -> Kept {
        var tries = 0
        while true {
            do {
                return .read(try store.credential())
            } catch {
                if store.forgetIfUnreadable(error) { return .forgotten }
                tries += 1
                let delay = Self.backoff(tries)
                Log.app.error("the keychain refused a read (\(String(describing: error), privacy: .public)); asking again in \(String(describing: delay), privacy: .public)")
                do { try await sleep(delay) } catch { return .calledOff }
            }
        }
    }

    /// 1 s, 2 s, 4 s…, at most `mostApart`.
    static func backoff(_ tries: Int) -> Duration {
        min(.seconds(1 << min(max(tries - 1, 0), 10)), mostApart)
    }

    /// The pairing just finished, or a kept one was found by resume.
    private func arrive() async {
        let found: Credential?
        do {
            found = try store.credential()
        } catch {
            Log.app.error("paired, and the bearer does not read: \(String(describing: error), privacy: .public)")
            store.forgetIfUnreadable(error)
            found = nil
        }
        guard let found else {
            Log.app.error("paired with no bearer kept; back to the scanner")
            again()
            return
        }
        await open(found)
    }

    /// Home on `credential`, and the node behind it unless it is already
    /// on its way.
    private func open(_ credential: Credential) async {
        guard credential.expiresAt > now() else {
            Log.app.info("the kept bearer ran out at \(credential.expiresAt, privacy: .public)")
            return await forget(.lapsed)
        }
        showHome(credential)
        if nodeState == .down, bringingUp == nil {
            Task { await self.bringNodeUp(credential) }
        }
    }

    /// Join with the enrolment while it is held and the node's directory
    /// holds no identity yet; Rejoin on the identity otherwise. Nothing waits
    /// on this but the pairing's own few seconds.
    func bringNodeUp(_ credential: Credential) async {
        let started = generation
        guard bringingUp != started else {
            Log.node.info("the node is already on its way up")
            return
        }
        bringingUp = started
        defer { if bringingUp == started { bringingUp = nil } }
        nodeRetry?.cancel()
        nodeRetry = nil
        self.credential = credential
        nodeState = .joining
        do {
            try nodeDirectory.prepare()
        } catch {
            Log.node.error("the node's directory: \(String(describing: error), privacy: .public)")
        }
        let hostname = PhoneName.hostname(credential.phoneName)
        let (result, call) = await reach(hostname, enrollment: credential.enrollmentJSON)
        guard generation == started else {
            Log.node.info("a join finished after its pairing was forgotten; dropped")
            return
        }
        switch result {
        case .success(let answer):
            let address = answer.address ?? answer.addresses?.first ?? ""
            Log.node.info("the node is up at \(address, privacy: .public)")
            nodeState = .up(address: address)
            nodeTries = 0
            do {
                try nodeDirectory.prepare()
            } catch {
                Log.node.error("the node's directory after joining: \(String(describing: error), privacy: .public)")
            }
            if credential.enrollmentJSON != nil {
                var spent = credential
                spent.enrollmentJSON = nil
                do {
                    try store.save(spent)
                    Log.app.info("the spent enrolment is forgotten")
                } catch {
                    Log.app.error("the spent enrolment was not forgotten; the next launch rejoins on the node's identity: \(String(describing: error), privacy: .public)")
                }
                self.credential = spent
            }
        case .failure(let error):
            failed(error, call: call, credential: credential)
        }
    }

    private enum NodeCall { case join, rejoin }

    /// Go's Join writes the node's identity (node.json) in its exchange,
    /// before it answers, and spends the enrolment's key doing so. A kill
    /// between that and forgetting the enrolment, or a Join that fails after
    /// it, leaves both: the identity is the one to come back on, and the key
    /// only once it does not.
    private func reach(_ hostname: String, enrollment: String?) async -> (Result<NodeAnswer, NodeError>, NodeCall) {
        let dir = nodeDirectory.path
        guard let enrollment else {
            Log.node.info("rejoining 6net as \(hostname, privacy: .public)")
            return (await node.rejoin(dir: dir, hostname: hostname), .rejoin)
        }
        let rejoinedFirst = nodeDirectory.holdsIdentity
        if rejoinedFirst {
            Log.node.info("an enrolment is held and the node's directory holds an identity: rejoining 6net on it as \(hostname, privacy: .public)")
            let rejoined = await node.rejoin(dir: dir, hostname: hostname)
            guard case .failure(let error) = rejoined, error.code == "not_enrolled" else { return (rejoined, .rejoin) }
            Log.node.info("the identity there does not bring the node back; joining with the enrolment")
        } else {
            Log.node.info("joining 6net as \(hostname, privacy: .public)")
        }
        let joined = await node.join(dir: dir, hostname: hostname, enrollmentJSON: enrollment)
        if case .failure(let error) = joined, error.code != "malformed", !rejoinedFirst, nodeDirectory.holdsIdentity {
            Log.node.info("Join failed (\(error.code, privacy: .public)) after its exchange kept an identity; rejoining on it once")
            return (await node.rejoin(dir: dir, hostname: hostname), .rejoin)
        }
        return (joined, .join)
    }

    /// Go's words for a node that may come up if asked again later.
    private static let triedAgain: Set<String> = ["unreadable", "joining", "join_failed", "timed_out"]

    private func failed(_ error: NodeError, call: NodeCall, credential: Credential) {
        let code = error.code
        switch error {
        case .refused(_, let message):
            nodeState = .failed(message)
        case .unreadableAnswer(let name):
            nodeState = .failed("\(name) answered something unreadable")
        }
        let holding = credential.enrollmentJSON != nil
        if call == .rejoin, code == "revoked" || (code == "not_enrolled" && !holding) {
            Log.node.error("Rejoin says this phone is \(code, privacy: .public)")
            Task { await forget(.removed) }
            return
        }
        // Killed after the bearer was kept and not opened again until the
        // enrolment's ten-minute key died, with no identity that comes back:
        // nothing left can join, so say so and pair again. A Join that failed
        // with the key still live is tried again: soon when Go's word says
        // the moment was wrong (`triedAgain`), else on the next foreground.
        if holding, call == .join || code == "not_enrolled",
           let enrollment = credential.enrollmentJSON,
           let dies = (try? JSONValue.parse(enrollment))?["expires_at"]?.string.flatMap(WireDate.parse),
           dies <= now() {
            Log.node.error("\(call == .join ? "Join" : "Rejoin", privacy: .public) failed (\(code, privacy: .public)) and the enrolment's key died at \(dies, privacy: .public)")
            Task { await forget(.notJoined) }
            return
        }
        if Self.triedAgain.contains(code) {
            nodeState = .joining
            retryLater(credential)
        }
    }

    /// F7: the node's state does not read yet (before the first unlock), a
    /// join is under way, or one failed or timed out (a server deploy, a slow
    /// head). Asked again after 1 s, 2 s, 4 s…, at most `mostApart`.
    private func retryLater(_ credential: Credential) {
        nodeTries += 1
        let delay = Self.backoff(nodeTries)
        Log.node.info("the node is not ready; trying again in \(String(describing: delay), privacy: .public) (try \(self.nodeTries))")
        nodeRetry?.cancel()
        let sleep = sleep
        nodeRetry = Task { [weak self] in
            do { try await sleep(delay) } catch { return }
            guard let self, !Task.isCancelled else { return }
            self.nodeRetry = nil
            await self.bringNodeUp(credential)
        }
    }

    private func showHome(_ credential: Credential) {
        self.credential = credential
        guard route != .home else { return }
        let lobby = makeLobby(credential)
        self.lobby = lobby
        let home = HomeModel(lobby: lobby)
        home.onUnauthorized = { [weak self, weak home] in
            home?.disappear()
            Task { await self?.unauthorized() }
        }
        home.onPerson = { [weak self] person in
            self?.personID = person
            self?.registerPhone()
        }
        // A tap waiting on Home's rows opens once they are listed.
        home.onListed = { [weak self] in
            self?.openPending()
            guard let conversation = self?.visible else { return }
            if let row = self?.home?.rows.first(where: { $0.id == conversation.row.id && $0.organizationID == conversation.row.organizationID }) {
                conversation.row = row
            }
            Task { await conversation.listed() }
        }
        self.home = home
        route = .home
        if let launched {
            Log.home.info("home drawn \(String(describing: ContinuousClock.now - launched), privacy: .public) after launch")
            self.launched = nil
        }
        Log.app.info("home, paired with \(credential.origin.host ?? "?", privacy: .public)")
    }

    private func unauthorized() async {
        guard route == .home else { return }
        let ranOut = credential.map { $0.expiresAt <= now().addingTimeInterval(60) } ?? false
        await forget(ranOut ? .lapsed : .removed)
    }

    /// F5: the keychain, the node and its directory, all forgotten. The
    /// failure is on screen from the start; a second forget while one runs
    /// is a no-op.
    public func forget(_ why: Failure) async {
        guard !forgetting else {
            Log.app.info("already forgetting this pairing; \(String(describing: why), privacy: .public) changes nothing")
            return
        }
        forgetting = true
        Log.app.info("forgetting this pairing: \(String(describing: why), privacy: .public)")
        generation += 1
        nodeRetry?.cancel()
        nodeRetry = nil
        nodeTries = 0
        home?.disappear()
        home = nil
        kept = []
        credential = nil
        lobby = nil
        personID = nil
        registered = nil
        pendingOpen = nil
        readAgainFor = nil
        path = []
        route = .failure(why)
        do {
            try store.wipe()
        } catch {
            Log.app.error("could not wipe the keychain: \(String(describing: error), privacy: .public)")
        }
        await node.down()
        do {
            try nodeDirectory.wipe()
        } catch {
            Log.app.error("could not wipe the node's directory: \(String(describing: error), privacy: .public)")
        }
        nodeState = .down
        forgetting = false
        if againWhenForgotten {
            againWhenForgotten = false
            again()
        }
    }

    /// "Scan again" and "Pair again". While a forget is still taking the old
    /// node down, the scanner waits for it: a new pairing's Join must not
    /// land in a directory about to be wiped.
    public func again() {
        guard !forgetting else {
            Log.app.info("pair again: once the old node is down")
            againWhenForgotten = true
            return
        }
        pairing.reset()
        scanner.reset()
        route = .pairing
    }

    /// B5: the last `keeps` conversations by session id, oldest first.
    /// Reopening one shows its fold at once and reads only what changed.
    private var kept: [ConversationModel] = []
    static let keeps = 8

    public func conversation(for row: HomeRow) -> ConversationModel? {
        guard let home else { return nil }
        if let at = kept.firstIndex(where: { $0.row.id == row.id }) {
            let conversation = kept.remove(at: at)
            conversation.row = row
            conversation.reopened()
            kept.append(conversation)
            return conversation
        }
        let conversation = ConversationModel(
            row: row,
            reach: { [weak home] row in home?.reach(for: row) ?? .none },
            wake: { [weak home] in await home?.refresh() },
            doors: node
        )
        conversation.onRemoved = { [weak self] in
            Task { await self?.removed() }
        }
        conversation.onNodeDown = { [weak self] in
            Task { await self?.nodeWentDown() }
        }
        kept.append(conversation)
        if kept.count > Self.keeps {
            let gone = kept.removeFirst()
            Log.conversation.info("\(gone.row.id, privacy: .public) is let go: \(Self.keeps) conversations are kept")
        }
        return conversation
    }

    private func removed() async {
        guard route == .home else { return }
        await forget(.removed)
    }

    /// A conversation's dial found the node down: it ended on its own in the
    /// foreground, or a join failed for good. The node is asked how it
    /// stands and brought up, unless it is on its way up or set to be.
    func nodeWentDown() async {
        guard route == .home else { return }
        guard nodeRetry == nil else {
            Log.node.info("a dial found the node down; it is set to come up already")
            return
        }
        Log.node.info("a dial found the node down; asking how it stands")
        await checkNode()
    }

    /// The conversation on screen now, which `foreground()` dials again.
    /// Its delivered notifications come off the screen with it.
    public func showing(_ conversation: ConversationModel) {
        visible?.onPhaseChanged = nil
        visible = conversation
        conversation.onPhaseChanged = { [weak self] in self?.updateHomeRefresh() }
        updateHomeRefresh()
        guard let notifications else { return }
        let session = conversation.row.id
        Task { await notifications.removeDelivered(sessionID: session) }
    }

    public func hid(_ conversation: ConversationModel) {
        if visible === conversation {
            conversation.onPhaseChanged = nil
            visible = nil
            updateHomeRefresh()
        }
    }

    private func updateHomeRefresh() {
        guard let home else { return }
        let needed = !away && route == .home && (visible == nil || visible?.phase == .notOpen)
        if needed {
            if !home.periodicallyRefreshing { home.appear(refreshNow: visible == nil) }
        } else {
            home.disappear()
        }
    }

    /// Into the background: Home's minute, the node's tries again and the
    /// conversation's door all stop.
    public func background() {
        Log.app.info("into the background")
        away = true
        home?.disappear()
        nodeRetry?.cancel()
        nodeRetry = nil
        visible?.background()
    }

    /// Back from the background: the node is asked how it stands and
    /// brought up again if it went down; Home re-lists and its minute runs
    /// again, or the conversation on screen dials again.
    public func foreground() async {
        guard away else { return }
        away = false
        guard route == .home, let home else { return }
        Log.app.info("back from the background")
        let checking = Task { await self.checkNode() }
        // Notifications may have been turned on or off in Settings meanwhile.
        // Beside the dial, not before it: a permission prompt left open
        // answers only when the person gets to it.
        let pushing = Task { await self.homeAppeared() }
        if let visible {
            await notifications?.removeDelivered(sessionID: visible.row.id)
            // The conversation dials the door Home's rows name (`reach`),
            // and a Mac may have slept, woken or moved its door while the
            // app was away: Home re-lists first, so the dial is to where the
            // door is now, and a Mac now asleep shows F4 without a dial.
            await home.refresh()
            // A tap may have opened another conversation meanwhile, and
            // this one was left: the one on screen dials itself.
            if self.visible === visible {
                await visible.foreground()
            } else {
                Log.app.info("\(visible.row.id, privacy: .public) is no longer on screen; not dialling it")
            }
        } else {
            home.appear()
        }
        await checking.value
        await pushing.value
        updateHomeRefresh()
    }

    /// A6: `RouteMonitor` says which interface the phone's default route is
    /// on, at start and on every change. Handed to the node whatever state
    /// it is in — the library keeps the hint for the next socket it binds —
    /// and logged by Go with the interface's address counts. True when the
    /// node took it; one it refused is said again on the next change.
    @discardableResult
    public func defaultRoute(_ interface: String) async -> Bool {
        Log.node.info("default route is on \(interface, privacy: .public)")
        if case .failure(let error) = await node.defaultRoute(interface: interface) {
            Log.node.error("the node did not take the default route: \(String(describing: error), privacy: .public)")
            return false
        }
        return true
    }

    // MARK: push

    /// Home is on screen. Notifications are asked for while iOS still lets
    /// the app ask, which is the first time after pairing; a token is asked
    /// for once a launch while they are allowed. A register that was
    /// refused goes again.
    public func homeAppeared() async {
        registerPhone()
        guard let notifications else { return }
        var status = await notifications.authorization()
        if status == .notDetermined {
            Log.push.info("asking for notifications")
            status = await notifications.requestAuthorization() ? .allowed : .denied
        }
        notificationsOff = status == .denied
        Log.push.info("notifications are \(String(describing: status), privacy: .public)")
        guard status == .allowed, !askedForToken else { return }
        askedForToken = true
        Log.push.info("asking APNs for this launch's token")
        notifications.registerForRemote()
    }

    /// APNs gave this launch's token.
    public func gotPushToken(_ token: Data) {
        Log.push.info("APNs gave a token (\(token.count) bytes) for \(self.pushEnvironment ?? "no environment", privacy: .public)")
        pushToken = DeviceToken.hex(token)
        registerPhone()
    }

    /// APNs gave no token: Home's next appearance asks again.
    public func pushTokenFailed(_ error: Error) {
        let error = error as NSError
        Log.push.error("APNs gave no token: \(error.domain, privacy: .public) \(error.code); asked again when Home next appears")
        askedForToken = false
    }

    /// What `register` should carry now; nil until the person is known. The
    /// environment is the token's, so it goes only with one.
    private var wanted: Registration? {
        personID.map { Registration(person: $0, token: pushToken, environment: pushToken == nil ? nil : pushEnvironment) }
    }

    /// `register` on `phone:<person>`, once the person is known, and again
    /// whenever the token changes. One refused goes again when Home next
    /// appears.
    private func registerPhone() {
        guard !registering, let lobby, let credential, let registration = wanted, registration != registered else { return }
        registering = true
        let started = generation
        Task { [weak self] in
            let taken = await lobby.register(
                personID: registration.person, name: credential.phoneName, enrollmentID: credential.enrollmentID,
                pushToken: registration.token, environment: registration.environment
            )
            guard let self else { return }
            self.registering = false
            let current = self.generation == started
            if current, taken { self.registered = registration }
            if current, !taken { Log.push.info("register was refused; it goes again when Home next appears") }
            // A newer token, or a new pairing, waited on this one.
            if !current || taken || self.wanted != registration { self.registerPhone() }
        }
    }

    /// The new-thread sheet's model, over the Macs reachable now.
    public func newThread() -> NewThreadModel {
        let doors = node
        return NewThreadModel(macs: home?.macs() ?? [], open: { mac in
            try await doors.openDoor(address: mac.address, port: mac.port)
        })
    }

    /// A thread was started from here: its conversation opens once Home
    /// lists it. The Mac tells the server of a start a moment after it
    /// answers, so Home is read a few times before giving up.
    public func started(_ thread: StartedThread) async {
        guard let home else { return }
        for attempt in 0..<8 {
            await home.refresh()
            if let row = home.row(organizationID: thread.organizationID, sessionID: thread.sessionID) {
                Log.home.info("opening \(thread.sessionID, privacy: .public), just started")
                if route == .home { path = [row] }
                return
            }
            do { try await sleep(.milliseconds(attempt < 3 ? 600 : 1500)) } catch { return }
        }
        Log.home.notice("Home never listed \(thread.sessionID, privacy: .public); it will show when it does")
    }

    /// A notification was tapped: its conversation opens once Home lists it.
    public func opened(_ payload: PushPayload) {
        Log.push.info("a tap on \(payload.fact, privacy: .public) for \(payload.sessionID, privacy: .public) in \(payload.organizationID, privacy: .public)")
        pendingOpen = payload
        readAgainFor = nil
        openPending()
    }

    /// Opens the waiting tap's row if Home lists it. A row Home does not list
    /// is read for once more; still not listed, Home stays, as it does for a
    /// session the person cannot see.
    private func openPending() {
        guard let payload = pendingOpen, route == .home, let home, home.loaded else { return }
        if let row = home.row(organizationID: payload.organizationID, sessionID: payload.sessionID) {
            pendingOpen = nil
            readAgainFor = nil
            if let visible, visible.row.id == row.id, visible.row.organizationID == row.organizationID {
                Log.push.info("\(row.id, privacy: .public) is on screen already")
                return
            }
            Log.push.info("opening \(row.id, privacy: .public) from a tap")
            path = [row]
            return
        }
        // Its own read decides, once it is back.
        guard readAgainFor != payload else { return }
        readAgainFor = payload
        Log.push.info("Home does not list \(payload.sessionID, privacy: .public) in \(payload.organizationID, privacy: .public); reading it again")
        Task { [weak self] in
            await home.refresh()
            self?.readAgain(for: payload)
        }
    }

    private func readAgain(for payload: PushPayload) {
        guard pendingOpen == payload, readAgainFor == payload else { return }
        pendingOpen = nil
        readAgainFor = nil
        Log.push.notice("a tap named \(payload.sessionID, privacy: .public) in \(payload.organizationID, privacy: .public), which Home does not list; Home")
        path = []
    }

    /// Whether a notification arriving in the foreground shows its banner:
    /// not when that conversation is on screen.
    public func presents(_ payload: PushPayload) -> Bool {
        let onScreen = visible.map { $0.row.id == payload.sessionID && $0.row.organizationID == payload.organizationID } ?? false
        Log.push.info("\(payload.fact, privacy: .public) for \(payload.sessionID, privacy: .public) arrived in the foreground; \(onScreen ? "on screen, no banner" : "a banner", privacy: .public)")
        return !onScreen
    }

    /// Status, and the node brought up if it is down. One at a time: the
    /// foreground and a conversation's dial may both ask.
    private func checkNode() async {
        guard let credential else { return }
        guard bringingUp == nil, !checkingNode else {
            Log.node.info("the node is on its way up, or being asked, already")
            return
        }
        checkingNode = true
        defer { checkingNode = false }
        let started = generation
        let status = await node.status()
        guard generation == started, bringingUp == nil else { return }
        switch status {
        case .success(let answer) where answer.state == "up":
            if let address = answer.address ?? answer.addresses?.first {
                nodeState = .up(address: address)
            }
        case .success(let answer) where answer.state == "joining":
            Log.node.info("status: the node is joining")
        case .success(let answer):
            if let ended = answer.endedCode, ended == "revoked" || ended == "not_enrolled" {
                Log.node.error("status: the node ended, \(ended, privacy: .public)")
                return await forget(.removed)
            }
            Log.node.info("status: the node is \(answer.state ?? "?", privacy: .public); bringing it up")
            await bringNodeUp(credential)
        case .failure(let error):
            Log.node.error("status failed: \(String(describing: error), privacy: .public)")
            if case .up = nodeState { return }
            await bringNodeUp(credential)
        }
    }
}
