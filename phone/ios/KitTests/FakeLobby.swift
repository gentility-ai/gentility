import Foundation
@testable import DeskPhoneKit

final class FakeLobby: Lobby, @unchecked Sendable {
    var person = Organizations.Person(id: Build.morgan, email: "morgan@example.com", name: "Morgan")
    var organizations: [Organizations.Organization] = [.init(id: "o-1", slug: "northwind", name: "Northwind")]
    var lists: [String: [LobbyList]] = [:]
    var machineReplies: [String: Result<MachinesReply, Error>] = [:]
    var unauthorized = false
    var unreachableNext = false
    var listErrors: [String: Error] = [:]
    private(set) var sinces: [String: [String?]] = [:]
    private(set) var registered: [String] = []
    private(set) var refreshes = 0
    private(set) var machinesAsked: [String: Int] = [:]

    /// Runs before each organisations read answers: a test holds it here.
    var refreshing: @MainActor () async -> Void = {}

    func organizations() async throws -> Organizations {
        await refreshing()
        if unreachableNext { unreachableNext = false; throw LobbyError.unreachable("offline") }
        refreshes += 1
        if unauthorized { throw LobbyError.unauthorized }
        return Organizations(organizations: organizations, person: person)
    }

    func list(organizationID: String, since: String?) async throws -> LobbyList {
        sinces[organizationID, default: []].append(since)
        if let error = listErrors[organizationID] { throw error }
        guard var queued = lists[organizationID], !queued.isEmpty else {
            return LobbyList(sessions: [], now: "2026-01-01T00:00:00.000000Z")
        }
        let next = queued.removeFirst()
        lists[organizationID] = queued.isEmpty ? [next] : queued
        return next
    }

    func machines(organizationID: String) async throws -> MachinesReply {
        machinesAsked[organizationID, default: 0] += 1
        return try (machineReplies[organizationID] ?? .success(MachinesReply(machines: [], now: "2026-01-01T00:00:00.000000Z"))).get()
    }

    private(set) var inboxes: [(id: String, field: InboxField, value: Date?)] = []
    /// What the next inboxes answer; with none left, the server takes the field.
    var inboxResults: [Result<InboxReply, LobbyError>] = []

    func inbox(organizationID: String, id: String, field: InboxField, value: Date?) async throws -> InboxReply {
        inboxes.append((id, field, value))
        if !inboxResults.isEmpty { return try inboxResults.removeFirst().get() }
        return InboxReply(applied: [field.rawValue], stale: [])
    }

    struct Registration: Equatable {
        let personID: String
        let pushToken: String?
        let environment: String?
    }

    private(set) var registrations: [Registration] = []
    /// How many registers are refused before they answer.
    var registerRefusals = 0
    /// How many of the next registers wait for `releaseRegister()`.
    var holdRegisters = 0
    /// The most registers out at once.
    private(set) var mostRegistersOut = 0
    private var registersOut = 0
    private var heldRegister: CheckedContinuation<Void, Never>?
    private let lock = NSLock()

    /// A register is waiting for `releaseRegister()`.
    var holdingRegister: Bool { lock.withLock { heldRegister != nil } }

    func releaseRegister() {
        lock.withLock {
            defer { heldRegister = nil }
            return heldRegister
        }?.resume()
    }

    func register(personID: String, name: String, enrollmentID: String?, pushToken: String?, environment: String?) async -> Bool {
        let held: Bool = lock.withLock {
            registered.append(personID)
            registrations.append(Registration(personID: personID, pushToken: pushToken, environment: environment))
            registersOut += 1
            mostRegistersOut = max(mostRegistersOut, registersOut)
            guard holdRegisters > 0 else { return false }
            holdRegisters -= 1
            return true
        }
        if held {
            await withCheckedContinuation { continuation in lock.withLock { heldRegister = continuation } }
        }
        return lock.withLock {
            registersOut -= 1
            guard registerRefusals == 0 else {
                registerRefusals -= 1
                return false
            }
            return true
        }
    }
}

enum Build {
    static let now = "2026-01-01T12:00:00.000000Z"
    static let morgan = "u-morgan"

    static func row(_ id: String, title: String, mac: String?, host: String?, owner: String = morgan, lastEventAt: String = now, working: Bool = false, attention: String = "none", attentionLine: String? = nil, archivedAt: String? = nil, settledAt: String? = nil, snoozedUntil: String? = nil, branch: String? = nil, repository: String? = nil, looper: String? = nil, waitingOn: String? = nil, waitingReason: String? = nil) throws -> LobbyRow {
        let fields: [String: JSONValue] = [
            "id": .string(id), "title": .string(title), "status": .string("open"),
            "attention": .string(attention), "attention_line": attentionLine.map(JSONValue.string) ?? .null, "working": .bool(working), "turns": .number(3),
            "owner_id": .string(owner), "last_event_at": .string(lastEventAt),
            "machine_id": mac.map(JSONValue.string) ?? .null, "host": host.map(JSONValue.string) ?? .null,
            "archived_at": archivedAt.map(JSONValue.string) ?? .null, "recap_line": .null,
            "settled_at": settledAt.map(JSONValue.string) ?? .null, "snoozed_until": snoozedUntil.map(JSONValue.string) ?? .null,
            "branch": branch.map(JSONValue.string) ?? .null, "repository": repository.map(JSONValue.string) ?? .null,
            "looper_id": looper.map(JSONValue.string) ?? .null,
            "waiting_on": waitingOn.map(JSONValue.string) ?? .null,
            "waiting_reason": waitingReason.map(JSONValue.string) ?? .null,
        ]
        return try JSONValue.object(fields).decode()
    }

    static func machine(desk: String?, owner: (kind: String, id: String, name: String) = ("user", morgan, "morgan@example.com"), address: String = "fd06:6e7:0:2::4", seen: String? = now, door: String? = "approved", port: Int = 6446, deskPresence: String? = nil, installs: [JSONValue]? = nil) throws -> NetMachine {
        let services: [JSONValue] = door.map { [.object(["id": .string("svc-1"), "kind": .string("desk"), "port": .number(Double(port)), "state": .string($0)])] } ?? []
        var fields: [String: JSONValue] = [
            "node_id": .string("n-\(address)"), "name": .string("node"), "address": .string(address),
            "owner": .object(["kind": .string(owner.kind), "id": .string(owner.id), "name": .string(owner.name)]),
            "presence": .object(["path": .string("relayed"), "last_seen_at": seen.map(JSONValue.string) ?? .null]),
            "services": .array(services), "status": .string("active"),
            "desk_machine_id": desk.map(JSONValue.string) ?? .null,
            "desk_presence": deskPresence.map(JSONValue.string) ?? .null,
        ]
        if let installs { fields["desk_installs"] = .array(installs) }
        return try JSONValue.object(fields).decode()
    }

    static func phone() throws -> NetMachine {
        try machine(desk: nil, address: "fd06:6e7:0:2::9", door: nil)
    }
}
