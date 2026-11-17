import Foundation

/// `GET /oauth/organizations` (MCP host), including the authenticated
/// person's id in `person.id`.
public struct Organizations: Decodable, Equatable, Sendable {
    public struct Organization: Decodable, Equatable, Sendable, Identifiable {
        public let id: String
        public let slug: String
        public let name: String
    }

    public struct Person: Decodable, Equatable, Sendable {
        /// The person's user id: `phone:<id>`, and whose lobby rows are theirs.
        public let id: String
        public let email: String
        public let name: String?
    }

    public let organizations: [Organization]
    public let person: Person
}

/// `list` on `work_sessions:<org>`.
public struct LobbyList: Decodable, Equatable, Sendable {
    public let sessions: [LobbyRow]
    public let now: String
}

public struct LobbyRow: Decodable, Equatable, Sendable, Identifiable {
    public let id: String
    public let title: String
    public let status: String
    /// `none`, `approval` or `question` (the session contract).
    public let attention: String?
    /// The first line of what it waits on, as the push said it ("Wants to
    /// execute"); nil when it waits on nothing, and on a teammate's row.
    public let attentionLine: String?
    public let working: Bool
    public let turns: Int
    /// The Desk store id of the Mac it runs on; `desk_machine_id` in `machines`.
    public let machineID: String?
    /// That Mac's label.
    public let host: String?
    public let ownerID: String?
    public let recapLine: String?
    public let lastEventAt: String?
    public let archivedAt: String?
    public let settledAt: String?
    /// Asleep until then; a time past is no snooze.
    public let snoozedUntil: String?
    public let branch: String?
    /// `host/owner/repo`; nil off GitHub.
    public let repository: String?
    /// Set when the row is a looper's run.
    public let looperID: String?
    /// Who the thread waits on, `"person"` or nil: the server inbox's tag
    /// which the finished-or-waiting reflex sets and a visible turn
    /// clears. Absent from a server older than the tag.
    public let waitingOn: String?
    /// Why, in one sentence; nil on a teammate's row, as `attentionLine` is.
    public let waitingReason: String?

    enum CodingKeys: String, CodingKey {
        case id, title, status, attention, working, turns, host, branch, repository
        case attentionLine = "attention_line"
        case waitingOn = "waiting_on"
        case waitingReason = "waiting_reason"
        case machineID = "machine_id"
        case ownerID = "owner_id"
        case recapLine = "recap_line"
        case lastEventAt = "last_event_at"
        case archivedAt = "archived_at"
        case settledAt = "settled_at"
        case snoozedUntil = "snoozed_until"
        case looperID = "looper_id"
    }
}

/// `machines` on `work_sessions:<org>` (the machine-list contract).
public struct MachinesReply: Decodable, Equatable, Sendable {
    public let machines: [NetMachine]
    public let now: String
}

struct InstallReach {
    let node: NetMachine
    let port: Int?
    let deskPresence: String?
}

public struct NetMachine: Decodable, Equatable, Sendable {
    /// The list is authoritative; only an old reply without it uses services.
    func install(_ machineID: String) -> InstallReach? {
        if let installs = deskInstalls {
            guard let found = installs.first(where: { $0.deskMachineID == machineID }) else { return nil }
            return InstallReach(node: self, port: found.doorPort, deskPresence: found.deskPresence)
        }
        guard deskMachineID == machineID else { return nil }
        let port = services.first { $0.kind == "desk" && $0.state == "approved" }?.port
        return InstallReach(node: self, port: port, deskPresence: deskPresence)
    }

    public struct DeskInstall: Decodable, Equatable, Sendable {
        public let deskMachineID: String
        public let doorPort: Int?
        public var deskPresence: String? {
            switch deskPresenceWord {
            case "online", "offline": return deskPresenceWord
            default: return nil
            }
        }
        private let deskPresenceWord: String?

        enum CodingKeys: String, CodingKey {
            case deskMachineID = "desk_machine_id"
            case doorPort = "door_port"
            case deskPresenceWord = "desk_presence"
        }
    }

    public struct Owner: Decodable, Equatable, Sendable {
        /// `user`, `machine` or `case_runtime`.
        public let kind: String
        public let id: String?
        /// A user's email, a machine's slug.
        public let name: String?
    }

    public struct Presence: Decodable, Equatable, Sendable {
        /// `direct`, `relayed` or `offline`.
        public let path: String
        public let lastSeenAt: String?

        enum CodingKeys: String, CodingKey {
            case path
            case lastSeenAt = "last_seen_at"
        }
    }

    public struct Service: Decodable, Equatable, Sendable {
        public let id: String
        public let kind: String
        public let port: Int
        /// `declared` or `approved`.
        public let state: String
    }

    public let nodeID: String
    public let name: String
    public let owner: Owner
    /// This organisation's address for the node: the one to dial it on here.
    public let address: String
    public let presence: Presence
    public let services: [Service]
    public let status: String
    public let deskMachineID: String?
    /// Authoritative when present, including an empty list. Older servers omit it.
    public let deskInstalls: [DeskInstall]?
    /// Unknown and absent words are no Desk evidence, not offline.
    public var deskPresence: String? {
        switch deskPresenceWord {
        case "online", "offline": return deskPresenceWord
        default: return nil
        }
    }
    private let deskPresenceWord: String?

    enum CodingKeys: String, CodingKey {
        case name, owner, address, presence, services, status
        case nodeID = "node_id"
        case deskMachineID = "desk_machine_id"
        case deskInstalls = "desk_installs"
        case deskPresenceWord = "desk_presence"
    }
}

/// `POST /oauth/phone/claim`'s 202.
public struct ClaimReply: Decodable, Equatable, Sendable {
    public let claimID: String
    public let machineLabel: String
    public let expiresAt: String
    public let pollSecret: String
    /// Two decimal digits, "10" to "99", that the person types on the Mac.
    public let match: String

    enum CodingKeys: String, CodingKey {
        case match
        case claimID = "claim_id"
        case machineLabel = "machine_label"
        case expiresAt = "expires_at"
        case pollSecret = "poll_secret"
    }
}

/// What a claim refusal's `error` word says.
public enum ClaimRefusal: Error, Equatable, Sendable {
    case unknown, expired, spent, blankName, malformed
    case other(String)

    public init(word: String?) {
        switch word {
        case "unknown": self = .unknown
        case "expired": self = .expired
        case "spent": self = .spent
        case "blank_name": self = .blankName
        case "malformed": self = .malformed
        default: self = .other(word ?? "unreadable")
        }
    }
}

/// `POST /oauth/phone/poll`'s answer.
public enum PollAnswer: Equatable, Sendable {
    /// `pending`, `asking`, `allowed_pending`, or a word this app does not
    /// know yet: keep polling.
    case waiting(String)
    case denied
    case spent
    case expired
    /// The poll secret is not one the server handed out.
    case unknown
    case failed(String)
    case allowed(Granted)

    public init(_ body: JSONValue) throws {
        let status = body["status"]?.string
        switch status {
        case "denied": self = .denied
        case "spent": self = .spent
        case "expired": self = .expired
        case "failed": self = .failed(body["failure"]?.string ?? "server_error")
        case "allowed": self = .allowed(try Granted(body))
        default: self = .waiting(status ?? "unreadable")
        }
    }
}

/// The one answer that carries a bearer. It prints as redacted.
public struct Granted: Equatable, Sendable, CustomStringConvertible {
    public let accessToken: String
    public let expiresIn: Int
    public let scope: String
    /// The `enrollment` object, re-encoded with its keys as the server spelled
    /// them: what `phonenode.Join` takes.
    public let enrollmentJSON: String
    public let enrollmentID: String?

    public init(_ body: JSONValue) throws {
        guard let token = body["access_token"]?.string,
              let enrollment = body["enrollment"], case .object = enrollment else {
            throw ClaimRefusal.malformed
        }
        accessToken = token
        expiresIn = body["expires_in"]?.int ?? 0
        scope = body["scope"]?.string ?? ""
        enrollmentJSON = try enrollment.text()
        enrollmentID = enrollment["enrollment_id"]?.string
    }

    public var description: String { "Granted(redacted, expires in \(expiresIn) s)" }
}

/// A thread's inbox field the phone sets on the lobby's `inbox`.
public enum InboxField: String, Sendable {
    case settledAt = "settled_at"
    case snoozedUntil = "snoozed_until"
    case archivedAt = "archived_at"
}

/// The server's answer to an `inbox`: the fields it took, and the fields a
/// later write already beat, which the row snaps back to.
public struct InboxReply: Decodable, Equatable, Sendable {
    public let applied: [String]
    public let stale: [String]

    public init(applied: [String], stale: [String]) {
        self.applied = applied
        self.stale = stale
    }
}
