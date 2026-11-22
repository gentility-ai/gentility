import Foundation

public enum LobbyError: Error, Equatable, Sendable {
    /// The bearer is refused: revoked, or run out.
    case unauthorized
    case unreachable(String)
    case refused(String)
}

/// What Home asks the mothership.
public protocol Lobby: Sendable {
    func organizations() async throws -> Organizations
    func list(organizationID: String, since: String?) async throws -> LobbyList
    func machines(organizationID: String) async throws -> MachinesReply
    /// Sets one of a thread's inbox fields, or clears it with nil: the
    /// server owns the inbox, and the Mac holding the thread hears it.
    /// Refused as `inbox_unsupported` by a server that cannot.
    func inbox(organizationID: String, id: String, field: InboxField, value: Date?) async throws -> InboxReply
    /// Names this phone on `phone:<person>`, with its push token and the
    /// APNs environment the token belongs to when it has one. True once the
    /// server took it.
    @discardableResult
    func register(personID: String, name: String, enrollmentID: String?, pushToken: String?, environment: String?) async -> Bool
}

/// What the Loopers tab asks the mothership. Every check is the server's:
/// a refusal comes back as `LobbyError.refused` with its reason.
public protocol LooperLobby: Sendable {
    func organizations() async throws -> Organizations
    func loopers(organizationID: String) async throws -> [LooperRow]
    func runs(organizationID: String, looperID: String) async throws -> [LooperRunRow]
    func run(organizationID: String, runID: String) async throws -> LooperRunRead
    /// Run now, or with `message` a run that opens with the person's words.
    /// The run's id.
    func start(organizationID: String, looperID: String, message: String?) async throws -> String
    func update(organizationID: String, looperID: String, changes: [String: JSONValue]) async throws -> LooperRow
    func answer(organizationID: String, requestID: String, answer: String) async throws -> LooperRequest
    func mutedLoopers(personID: String) async throws -> Set<String>
    func mute(personID: String, looperID: String, muted: Bool) async throws
}

/// The mothership on the API origin: organisations over HTTP, the rest over
/// one socket, made again whenever the last one has closed. A topic is
/// joined when an ask finds it not joined on the socket, so a channel the
/// server ended is joined again by the next ask.
public actor ServerLobby: Lobby, LooperLobby {
    private let credential: Credential
    private let http: URLSession
    private let makeTransport: @Sendable () -> WebSocketTransport
    private var socket: PhoenixSocket?
    /// The socket being made, which every ask meanwhile waits on.
    private var connecting: Task<PhoenixSocket, Error>?
    /// Each topic's join in flight, which every ask on it meanwhile waits on.
    private var joining: [String: Task<Void, Error>] = [:]
    /// Each joined topic's features, from its join's reply.
    private var features: [String: [String]] = [:]
    private var listening: Task<Void, Never>?
    private let makeRPCTransport: @Sendable () -> RPCTransport
    private let forcePhoenix: Bool
    private let hello: JSONValue
    private var rpc: RPCSocket?
    private var rpcConnecting: Task<(RPCSocket, [String]), Error>?
    private var rpcFeatures: [String] = []
    private var phoenixPins: Set<String> = []
    private var rpcUnavailable = false
    private var unauthorized = false
    private var revokedOrganizations: Set<String> = []

    /// Developer launch environment only; sampled once per link, never a
    /// person-facing setting. Fleet rollback withdraws the server's words.
    public static var developerForcesPhoenix: Bool {
        ProcessInfo.processInfo.environment["DESK_PHONE_FORCE_PHOENIX"] == "1"
    }

    /// `http` is any session; whichever it is, no redirect is followed, since
    /// one would hand the bearer to wherever it points.
    public init(credential: Credential, http: URLSession = RefusingRedirects.shared,
                forcePhoenix: Bool = ServerLobby.developerForcesPhoenix,
                makeRPCTransport: @escaping @Sendable () -> RPCTransport = { URLSessionTransport() },
                makeTransport: @escaping @Sendable () -> WebSocketTransport = { URLSessionTransport() }) {
        self.credential = credential
        self.http = http
        self.makeTransport = makeTransport
        self.makeRPCTransport = makeRPCTransport
        self.forcePhoenix = forcePhoenix
        let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "development"
        hello = .object([
            "version": .string(version), "build": .string(PhoneBuild.stamp),
            "protocol": .object(["major": .number(1)]),
            "features": .array(["rpc_sessions", "rpc_hosts", "rpc_inbox", "rpc_phone", "rpc_loopers", "inbox"].map(JSONValue.string)),
        ])
    }

    deinit {
        listening?.cancel()
        connecting?.cancel()
        rpcConnecting?.cancel()
        if let rpc { Task { await rpc.disconnect() } }
        if let socket {
            Task { await socket.disconnect() }
        }
    }

    public func organizations() async throws -> Organizations {
        try await checkRevocation("")
        let revokedBeforeRequest = revokedOrganizations
        var request = URLRequest(url: credential.origin.appendingPathComponent("oauth/organizations"))
        request.timeoutInterval = 15
        request.setValue("Bearer \(credential.bearer)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let started = ContinuousClock.now
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await http.data(for: request, delegate: RefusingRedirects.delegate)
        } catch {
            let error = error as NSError
            Log.home.error("GET /oauth/organizations got no answer after \(String(describing: ContinuousClock.now - started), privacy: .public): \(error.domain, privacy: .public) \(error.code)")
            throw LobbyError.unreachable("\(error.domain) \(error.code)")
        }
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        Log.home.info("GET /oauth/organizations answered \(status) in \(String(describing: ContinuousClock.now - started), privacy: .public)")
        if status == 401 { throw LobbyError.unauthorized }
        if (300...399).contains(status) { throw LobbyError.refused("redirected") }
        guard status == 200 else { throw LobbyError.refused("organizations answered \(status)") }
        guard let organizations = try? Wire.decoder.decode(Organizations.self, from: data) else {
            Log.home.error("/oauth/organizations' 200 (\(data.count) bytes) does not read as organisations")
            throw LobbyError.refused("unreadable organizations")
        }
        try await checkRevocation("")
        // Only an authenticated HTTP read begun after the revocation can
        // restore access. The server rebuilds and checks org scope per call.
        let restored = revokedBeforeRequest.intersection(organizations.organizations.map(\.id))
        revokedOrganizations.subtract(restored)
        await rpc?.restoreOrganizations(restored)
        return organizations
    }

    public func list(organizationID: String, since: String?) async throws -> LobbyList {
        let payload: JSONValue = since.map { .object(["since": .string($0)]) } ?? .object([:])
        return try read(await ask("work_sessions:\(organizationID)", "list", payload), "list")
    }

    public func machines(organizationID: String) async throws -> MachinesReply {
        try read(await ask("work_sessions:\(organizationID)", "machines", .object([:])), "machines")
    }

    public func inbox(organizationID: String, id: String, field: InboxField, value: Date?) async throws -> InboxReply {
        let topic = "work_sessions:\(organizationID)"
        let payload: JSONValue = .object([
            "session_id": .string(id),
            "op_id": .string(UUID().uuidString.lowercased()),
            "at": .string(WireDate.say(Date())),
            "changes": .object([field.rawValue: value.map { .string(WireDate.say($0)) } ?? .null]),
        ])
        return try read(await ask(topic, "inbox", payload), "inbox")
    }

    @discardableResult
    public func register(personID: String, name: String, enrollmentID: String?, pushToken: String?, environment: String?) async -> Bool {
        var payload: [String: JSONValue] = ["name": .string(name), "platform": .string("ios")]
        if let enrollmentID { payload["enrollment_id"] = .string(enrollmentID) }
        // The environment is the token's: with no token, neither goes.
        let environment = pushToken == nil ? nil : environment
        if let pushToken { payload["push_token"] = .string(pushToken) }
        if let environment { payload["environment"] = .string(environment) }
        // What it carries: the token's length, never its bytes.
        let carrying = pushToken.map { "a push token, \($0.count / 2) bytes" } ?? "no push token"
        do {
            _ = try await ask("phone:\(personID)", "register", .object(payload))
            Log.home.info("registered this phone as \(name, privacy: .public) (\(carrying, privacy: .public), \(environment ?? "no environment", privacy: .public))")
            return true
        } catch {
            Log.home.error("register (\(carrying, privacy: .public), \(environment ?? "no environment", privacy: .public)) refused: \(String(describing: error), privacy: .public)")
            return false
        }
    }

    // MARK: loopers

    public func loopers(organizationID: String) async throws -> [LooperRow] {
        let reply: LoopersReply = try read(await ask("looper_threads:\(organizationID)", "loopers", .object([:])), "loopers")
        return reply.loopers
    }

    public func runs(organizationID: String, looperID: String) async throws -> [LooperRunRow] {
        let reply: LooperRunsReply = try read(await ask("looper_threads:\(organizationID)", "history", .object(["looper_id": .string(looperID)])), "history")
        return reply.runs
    }

    public func run(organizationID: String, runID: String) async throws -> LooperRunRead {
        try read(await ask("looper_threads:\(organizationID)", "read", .object(["run_id": .string(runID)])), "read")
    }

    public func start(organizationID: String, looperID: String, message: String?) async throws -> String {
        var payload: [String: JSONValue] = ["looper_id": .string(looperID)]
        if let message { payload["message"] = .string(message) }
        let reply: LooperRunStarted = try read(await ask("looper_threads:\(organizationID)", "looper_run", .object(payload)), "looper_run")
        return reply.runID
    }

    public func update(organizationID: String, looperID: String, changes: [String: JSONValue]) async throws -> LooperRow {
        let payload: JSONValue = .object(["looper_id": .string(looperID), "changes": .object(changes)])
        let reply: LooperUpdated = try read(await ask("looper_threads:\(organizationID)", "looper_update", payload), "looper_update")
        return reply.looper
    }

    public func answer(organizationID: String, requestID: String, answer: String) async throws -> LooperRequest {
        let payload: JSONValue = .object([
            "request_id": .string(requestID),
            "decision": .object(["status": .string("answered"), "answer": .string(answer)]),
        ])
        let reply: LooperResolved = try read(await ask("looper_threads:\(organizationID)", "resolve", payload), "resolve")
        return reply.request
    }

    public func mutedLoopers(personID: String) async throws -> Set<String> {
        let reply: PhoneMutes = try read(await ask("phone:\(personID)", "mutes", .object([:])), "mutes")
        return Set(reply.mutes.loopers)
    }

    public func mute(personID: String, looperID: String, muted: Bool) async throws {
        _ = try await ask("phone:\(personID)", "mute", .object(["looper_id": .string(looperID), "muted": .bool(muted)]))
    }

    /// Whether the socket is open now. For the tests.
    var isConnected: Bool {
        get async { await socket?.isOpen ?? false }
    }

    var isRPCConnected: Bool { get async { await rpc?.isOpen ?? false } }

    /// Whether `topic` is joined on the socket now. For the tests.
    func isJoined(_ topic: String) async -> Bool {
        await socket?.isJoined(topic) ?? false
    }

    private func read<T: Decodable>(_ reply: JSONValue, _ what: String) throws -> T {
        guard let read = try? reply.decode(T.self) else {
            Log.home.error("the \(what, privacy: .public) reply does not read")
            throw LobbyError.refused("unreadable \(what)")
        }
        return read
    }

    private func ask(_ topic: String, _ event: String, _ payload: JSONValue) async throws -> JSONValue {
        do {
            try await checkRevocation(topic)
            let domain: String
            let method: String
            switch event {
            case "list": (domain, method) = ("sessions", "sessions.list")
            case "machines": (domain, method) = ("hosts", "hosts.machines")
            case "inbox": (domain, method) = ("inbox", "inbox.command")
            case "register": (domain, method) = ("phone", "phone.register")
            case "mute": (domain, method) = ("phone", "phone.mute")
            case "mutes": (domain, method) = ("phone", "phone.mutes")
            case "loopers": (domain, method) = ("loopers", "loopers.list_owned")
            case "history": (domain, method) = ("loopers", "loopers.history")
            case "read": (domain, method) = ("loopers", "loopers.read_run")
            case "looper_run": (domain, method) = ("loopers", "loopers.run_now")
            case "looper_update": (domain, method) = ("loopers", "loopers.update")
            case "resolve": (domain, method) = ("loopers", "loopers.resolve_request")
            default: preconditionFailure("undeclared phone call")
            }
            let pin = "\(domain):\(topic)"
            if !forcePhoenix, !rpcUnavailable, !phoenixPins.contains(pin), let rpc = try await rpcConnected() {
                // Concurrent callers all await hello before deciding. Pins
                // are link-lifetime state, never cleared on reconnect.
                try await checkRevocation(topic)
                if rpcFeatures.contains("rpc_\(domain)") {
                    if event == "inbox", !rpcFeatures.contains("inbox") {
                        throw LobbyError.refused("inbox_unsupported")
                    }
                    guard case .object(var params) = payload else { preconditionFailure("phone params must be objects") }
                    for prefix in ["work_sessions:", "looper_threads:"] where topic.hasPrefix(prefix) {
                        params["org"] = .string(String(topic.dropFirst(prefix.count)))
                    }
                    return try await rpc.call(method, .object(params))
                }
                phoenixPins.insert(pin)
            }
            return try await askPhoenix(topic, event, payload)
        } catch RPCError.revoked {
            unauthorized = true
            throw LobbyError.unauthorized
        } catch let error as RPCError {
            switch error {
            case .refused(_, let reason, _): throw LobbyError.refused(reason)
            case .revoked: throw LobbyError.unauthorized
            default: throw LobbyError.unreachable(String(describing: error))
            }
        } catch PhoenixError.refused(let status) {
            // An intermediary can refuse the upgrade while the bearer is
            // valid. Only organizations' 401 or explicit revocation signs out.
            throw LobbyError.refused("socket \(status)")
        } catch let error as LobbyError { throw error }
        catch { throw LobbyError.unreachable("The connection ended.") }
    }

    private func checkRevocation(_ topic: String) async throws {
        if let rpc {
            if await rpc.failure == .revoked { unauthorized = true }
            revokedOrganizations.formUnion(await rpc.revokedOrganizations)
        }
        guard !unauthorized else { throw LobbyError.unauthorized }
        for prefix in ["work_sessions:", "looper_threads:"] where topic.hasPrefix(prefix) {
            if revokedOrganizations.contains(String(topic.dropFirst(prefix.count))) { throw LobbyError.refused("forbidden") }
        }
    }

    private func rpcConnected() async throws -> RPCSocket? {
        if let rpc, await rpc.isOpen { return rpc }
        if rpcConnecting == nil {
            let transport = makeRPCTransport()
            let credential = credential
            let hello = hello
            rpcConnecting = Task { () throws -> (RPCSocket, [String]) in
                let fresh = RPCSocket(transport: transport, origin: credential.origin, bearer: credential.bearer)
                do {
                    try await fresh.connect()
                    let answer = try await fresh.call("rpc.hello", hello)
                    guard answer["principal"]?["kind"]?.string == "person",
                          let words = answer["features"]?.array, words.allSatisfy({ $0.string != nil }) else {
                        throw RPCError.malformed
                    }
                    return (fresh, words.compactMap(\.string))
                } catch {
                    await fresh.disconnect()
                    throw error
                }
            }
        }
        let pending = rpcConnecting!
        defer { if rpcConnecting == pending { rpcConnecting = nil } }
        do {
            let (fresh, words) = try await pending.value
            rpc = fresh
            rpcFeatures = words
            return fresh
        } catch PhoenixError.refused(let status) where [404, 405, 426].contains(status) {
            // Only an absent endpoint before application dispatch can select
            // legacy framing. Auth, timeouts and lost writes never downgrade.
            rpcUnavailable = true
            return nil
        }
    }

    private func askPhoenix(_ topic: String, _ event: String, _ payload: JSONValue) async throws -> JSONValue {
        let socket = try await connected()
        do {
            try await joined(topic, on: socket)
            if event == "inbox", !features[topic, default: []].contains("inbox") {
                throw LobbyError.refused("inbox_unsupported")
            }
            do {
                return try await socket.push(topic, event, payload)
            } catch PhoenixError.closed {
                // F10: the server ended this topic's channel (a crash, a
                // deploy's tidy close) under the ask while the socket stayed
                // open. Join it again, and ask once more.
                guard event != "inbox", await socket.isOpen else { throw PhoenixError.closed }
                Log.socket.info("\(topic, privacy: .public) closed under \(event, privacy: .public); joining it again")
                try await joined(topic, on: socket)
                return try await socket.push(topic, event, payload)
            }
        } catch let error as PhoenixError {
            switch error {
            case .joinRefused(let response), .replied(let response):
                throw LobbyError.refused(response["reason"]?.string ?? "refused")
            case .refused(let status):
                throw LobbyError.refused("socket \(status)")
            case .closed, .timeout, .notAFrame:
                throw LobbyError.unreachable(String(describing: error))
            }
        }
    }

    /// Joins `topic` unless it is joined on `socket` now; an ask meanwhile on
    /// the same topic waits on the same join.
    private func joined(_ topic: String, on socket: PhoenixSocket) async throws {
        if await socket.isJoined(topic) { return }
        if let pending = joining[topic] {
            return try await pending.value
        }
        let pending = Task {
            let reply = try await socket.join(topic, .object(["desk": hello]))
            self.heard(topic, features: reply["features"]?.array?.compactMap(\.string) ?? [])
        }
        joining[topic] = pending
        defer { if joining[topic] == pending { joining[topic] = nil } }
        try await pending.value
    }

    private func heard(_ topic: String, features: [String]) {
        self.features[topic] = features
    }

    private func connected() async throws -> PhoenixSocket {
        if let socket, await socket.isOpen { return socket }
        if let connecting {
            return try await connecting.value
        }
        let transport = makeTransport()
        let url = PhoenixSocket.url(origin: credential.origin, bearer: credential.bearer)
        let attempt = Task { () throws -> PhoenixSocket in
            let fresh = PhoenixSocket(transport: transport, url: url)
            do {
                try await fresh.connect()
            } catch PhoenixError.refused(let status) {
                Log.socket.error("the socket was refused (\(status))")
                throw LobbyError.refused("socket \(status)")
            } catch {
                throw LobbyError.unreachable(String(describing: error))
            }
            return fresh
        }
        connecting = attempt
        defer { connecting = nil }
        let fresh = try await attempt.value
        socket = fresh
        joining = [:]
        listening?.cancel()
        listening = Task {
            for await push in fresh.pushes {
                if push.closesChannel {
                    Log.socket.info("\(push.topic, privacy: .public) ended (\(push.event, privacy: .public)); the next ask joins it again")
                } else {
                    // Not acted on in this slice: Home re-reads on its minute.
                    Log.socket.info("\(push.topic, privacy: .public) pushed \(push.event, privacy: .public)")
                }
            }
        }
        return fresh
    }
}
