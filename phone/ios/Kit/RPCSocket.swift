import Foundation

/// Header-authenticated WebSockets need an explicit request; they must never
/// silently fall back to the URL-only transport used by Phoenix.
public protocol RPCTransport: WebSocketTransport {
    func open(_ request: URLRequest) async throws
    func ping() async throws
    var closeCode: Int? { get }
}

public enum RPCError: Error, Equatable, Sendable {
    case closed(Int?)
    case timeout
    case malformed
    case tooLarge
    case busy
    case refused(code: Int, reason: String, message: String)
    case revoked
}

/// One connection, without replay or transport selection. A lost reply is
/// uncertain, including a send that failed. The caller owns retry policy.
public actor RPCSocket: CustomStringConvertible, CustomDebugStringConvertible, CustomReflectable {
    public static let largestRequest = 16 * 1024 * 1024
    private let transport: RPCTransport
    private let request: URLRequest
    private let replyWithin: Duration
    private let pingEvery: Duration
    private let sleep: @Sendable (Duration) async throws -> Void
    private let maximumPending: Int
    private var counter = 0
    private var waiting: [String: CheckedContinuation<JSONValue, Error>] = [:]
    private var organizations: [String: String] = [:]
    private var timers: [String: Task<Void, Never>] = [:]
    private var opening: CheckedContinuation<Void, Error>?
    private var openingTimer: Task<Void, Never>?
    private var reader: Task<Void, Never>?
    private var heartbeat: Task<Void, Never>?
    private var pingTimer: Task<Void, Never>?
    private var ended = false
    private var terminal: RPCError?
    public private(set) var isOpen = false
    public private(set) var revokedOrganizations: Set<String> = []

    public init(transport: RPCTransport, origin: URL, bearer: String,
                replyWithin: Duration = .seconds(30), pingEvery: Duration = .seconds(20),
                maximumPending: Int = 64,
                sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }) {
        self.transport = transport
        self.replyWithin = replyWithin
        self.pingEvery = pingEvery
        self.maximumPending = maximumPending
        self.sleep = sleep
        var parts = URLComponents(url: origin, resolvingAgainstBaseURL: false)!
        parts.scheme = parts.scheme == "https" ? "wss" : "ws"
        parts.path = "/desk/rpc"
        parts.query = nil
        parts.fragment = nil
        var request = URLRequest(url: parts.url!)
        request.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization")
        self.request = request
    }

    public nonisolated var description: String { "RPCSocket(bearer redacted)" }
    public nonisolated var debugDescription: String { description }
    public nonisolated var customMirror: Mirror { Mirror(self, children: [:]) }

    public var failure: RPCError? { terminal }

    /// Called only after an authenticated organizations response begun after
    /// revocation confirms access again. Every server call checks it anew.
    func restoreOrganizations(_ organizations: Set<String>) {
        revokedOrganizations.subtract(organizations)
    }

    public func connect() async throws {
        guard !ended, !isOpen, opening == nil else { throw terminal ?? .closed(nil) }
        do {
            try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
                opening = continuation
                openingTimer = Task {
                    do { try await sleep(replyWithin) } catch { return }
                    settleOpening(.failure(RPCError.timeout))
                }
                Task {
                    do {
                        try await transport.open(request)
                        settleOpening(.success(()))
                    } catch { settleOpening(.failure(error)) }
                }
            }
        } catch {
            close(.closed(transport.closeCode))
            throw error
        }
        guard !ended else { throw terminal ?? .closed(nil) }
        isOpen = true
        reader = Task { await read() }
        heartbeat = Task { await beat() }
    }

    private func settleOpening(_ result: Result<Void, Error>) {
        openingTimer?.cancel()
        openingTimer = nil
        let pending = opening
        opening = nil
        pending?.resume(with: result)
    }

    public func call(_ method: String, _ params: JSONValue) async throws -> JSONValue {
        guard isOpen else { throw terminal ?? .closed(transport.closeCode) }
        guard waiting.count < maximumPending else { throw RPCError.busy }
        if let org = params["org"]?.string, revokedOrganizations.contains(org) {
            throw RPCError.refused(code: -32001, reason: "forbidden", message: "")
        }
        counter += 1
        let id = String(counter)
        let frame = JSONValue.object(["jsonrpc": .string("2.0"), "id": .string(id),
                                      "method": .string(method), "params": params])
        let text = try frame.text()
        guard text.utf8.count <= Self.largestRequest else { throw RPCError.tooLarge }
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                waiting[id] = continuation
                organizations[id] = params["org"]?.string
                timers[id] = Task {
                    do { try await sleep(replyWithin) } catch { return }
                    answer(id, .failure(RPCError.timeout))
                }
                Task {
                    // Cancellation before dispatch must not send a write.
                    guard waiting[id] != nil else { return }
                    do { try await transport.send(text) }
                    catch { close(.closed(transport.closeCode)) }
                }
            }
        } onCancel: {
            Task { await self.answer(id, .failure(CancellationError())) }
        }
    }

    public func disconnect() { close(.closed(nil)) }

    private func answer(_ id: String, _ result: Result<JSONValue, Error>) {
        organizations.removeValue(forKey: id)
        timers.removeValue(forKey: id)?.cancel()
        waiting.removeValue(forKey: id)?.resume(with: result)
    }

    private func read() async {
        while isOpen {
            do {
                let text = try await transport.receive()
                guard text.utf8.count <= DoorLimits.largestFrame,
                      case .object(let fields) = try JSONValue.parse(text),
                      fields["jsonrpc"] == .string("2.0") else {
                    close(.malformed)
                    return
                }
                if let id = fields["id"]?.string {
                    guard (fields["result"] == nil) != (fields["error"] == nil) else {
                        close(.malformed)
                        return
                    }
                    if let result = fields["result"] { answer(id, .success(result)) }
                    else if let error = fields["error"], let code = error["code"]?.int,
                            let message = error["message"]?.string {
                        answer(id, .failure(RPCError.refused(code: code, reason: error["data"]?["reason"]?.string ?? "refused", message: message)))
                    } else { close(.malformed) }
                } else if fields["id"] == nil, let method = fields["method"]?.string {
                    if method == "rpc.revoked" {
                        let params = fields["params"]
                        if params?["scope"]?.string == "connection" { close(.revoked) }
                        else if params?["scope"]?.string == "org", let org = params?["org"]?.string {
                            revokedOrganizations.insert(org)
                            for id in organizations.filter({ $0.value == org }).map(\.key) {
                                answer(id, .failure(RPCError.refused(code: -32001, reason: "forbidden", message: "")))
                            }
                        } else { close(.malformed) }
                    }
                    // Unknown notifications are deliberately ignored.
                } else { close(.malformed) }
            } catch {
                let code = transport.closeCode
                close(code == 4401 || code == 4409 ? .revoked : .closed(code))
            }
        }
    }

    private func beat() async {
        while isOpen {
            do { try await sleep(pingEvery) } catch { return }
            guard isOpen else { return }
            pingTimer = Task {
                do { try await sleep(.seconds(10)) } catch { return }
                close(.timeout)
            }
            do { try await transport.ping() }
            catch { close(.closed(transport.closeCode)) }
            pingTimer?.cancel()
            pingTimer = nil
        }
    }

    private func close(_ error: RPCError) {
        guard !ended else { return }
        let error: RPCError = transport.closeCode == 4401 || transport.closeCode == 4409 ? .revoked : error
        ended = true
        isOpen = false
        terminal = error
        settleOpening(.failure(error))
        reader?.cancel()
        heartbeat?.cancel()
        pingTimer?.cancel()
        for id in Array(waiting.keys) { answer(id, .failure(error)) }
        transport.close()
    }
}
