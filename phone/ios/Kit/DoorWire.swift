import Foundation

/// One text frame from a Mac's door: a reply to a request this app sent, or
/// an event nobody asked for (`pairing::carry`'s `Replied` and `Happened`).
public enum DoorFrame: Decodable, Equatable, Sendable {
    case reply(id: Int, DoorResponse)
    case event(DoorEvent)

    enum CodingKeys: String, CodingKey { case id, response, event }

    /// One frame, from the bytes Go handed over, decoded once. A reply is an
    /// integral `id` beside `response`; an event is `event`; anything else is
    /// not a frame.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        if c.contains(.response) {
            guard let id = try c.decodeIfPresent(Int.self, forKey: .id) else { throw DoorWireError.notAFrame }
            self = .reply(id: id, try c.decode(DoorResponse.self, forKey: .response))
        } else if c.contains(.event) {
            self = .event(try c.decode(DoorEvent.self, forKey: .event))
        } else {
            throw DoorWireError.notAFrame
        }
    }

    public init(data: Data) throws {
        self = try Wire.decoder.decode(DoorFrame.self, from: data)
    }

    public init(_ text: String) throws {
        try self.init(data: Data(text.utf8))
    }
}

public enum DoorWireError: Error, Equatable {
    case notAFrame
    case missing(String)
}

public enum DoorResponse: Decodable, Equatable, Sendable {
    case hello(DoorHello)
    case transcript(Transcript)
    /// Done: an answer recorded, a prompt sent, a steer asked, a line taken back.
    case ok
    /// A prompt asked to `queue` waits behind the turn, under this id.
    case queued(queueID: String)
    case environments([DoorEnvironment])
    case sessions([DoorSession])
    case session(DoorSession)
    case error(DoorRefusal)
    /// What an `attachment_put` kept: the ref a prompt attaches it by.
    case attachment(AttachmentRef)
    /// An attached file's or a worktree image's bytes, standard base64.
    case attachmentBytes(mime: String, bytesB64: String)
    case other(type: String)

    enum CodingKeys: String, CodingKey {
        case type, environments, sessions, session, attachment, mime
        case queueID = "queue_id"
        case bytesB64 = "bytes_b64"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        guard let type = try c.decodeIfPresent(String.self, forKey: .type) else { throw DoorWireError.missing("type") }
        switch type {
        case "hello": self = .hello(try DoorHello(from: decoder))
        case "transcript": self = .transcript(try Transcript(from: decoder))
        case "ok": self = .ok
        case "queued":
            guard let id = try c.decodeIfPresent(String.self, forKey: .queueID) else { throw DoorWireError.missing("queue_id") }
            self = .queued(queueID: id)
        case "environments": self = .environments(try c.decodeIfPresent([DoorEnvironment].self, forKey: .environments) ?? [])
        case "sessions": self = .sessions(Self.rows(try c.decodeIfPresent([Lenient<DoorSession>].self, forKey: .sessions) ?? []))
        case "session":
            guard c.contains(.session) else { throw DoorWireError.missing("session") }
            self = .session(try c.decode(DoorSession.self, forKey: .session))
        case "error": self = .error(try DoorRefusal(from: decoder))
        case "attachment":
            guard c.contains(.attachment) else { throw DoorWireError.missing("attachment") }
            self = .attachment(try c.decode(AttachmentRef.self, forKey: .attachment))
        case "attachment_bytes":
            guard let bytes = try c.decodeIfPresent(String.self, forKey: .bytesB64) else { throw DoorWireError.missing("bytes_b64") }
            self = .attachmentBytes(mime: try c.decodeIfPresent(String.self, forKey: .mime) ?? "application/octet-stream", bytesB64: bytes)
        default: self = .other(type: type)
        }
    }

    /// What a reply is, for a log: its type, and a refusal's code. Never its
    /// content, which can carry a person's words (a row's queued prompts).
    public var kind: String {
        switch self {
        case .hello: "hello"
        case .transcript: "transcript"
        case .ok: "ok"
        case .queued: "queued"
        case .environments(let all): "environments (\(all.count))"
        case .sessions(let rows): "sessions (\(rows.count))"
        case .session: "session"
        case .error(let refusal): "error \(refusal.code)"
        case .attachment: "attachment"
        case .attachmentBytes(let mime, let bytes): "attachment_bytes \(mime) (\(bytes.utf8.count) bytes of base64)"
        case .other(let type): "other \(type)"
        }
    }

    /// The rows that read: one this app cannot read is skipped, and logged
    /// without a byte of it, so the others still apply.
    private static func rows(_ read: [Lenient<DoorSession>]) -> [DoorSession] {
        read.enumerated().compactMap { at, row in
            if let why = row.failed {
                Log.door.error("row \(at) of a session list does not read (\(why, privacy: .public)); skipped")
            }
            return row.value
        }
    }
}

/// One element of a list that must not fail the list: the value, or why it
/// did not read (as `DoorConnection.unread` says it, never a byte of it).
struct Lenient<Value: Decodable>: Decodable {
    let value: Value?
    let failed: String?

    init(from decoder: Decoder) throws {
        do {
            value = try Value(from: decoder)
            failed = nil
        } catch {
            value = nil
            failed = DoorConnection.unread(error)
        }
    }
}

public struct DoorHello: Decodable, Equatable, Sendable {
    public struct Version: Decodable, Equatable, Sendable {
        public let major: Int
        public let minor: Int
    }

    /// The Mac this hello came from (`hello_host`): what a `through_seq`
    /// belongs to. A seq is one store's; a cursor is dropped when the store
    /// changes.
    public struct Host: Decodable, Equatable, Sendable {
        public let storeID: String?

        enum CodingKeys: String, CodingKey { case storeID = "store_id" }
    }

    public let version: String
    public let build: String
    public let capabilities: [String]
    public let accepted: [String]
    public let features: [String]
    /// Absent from a core from before versions, which is 0.0.
    public let protocolVersion: Version?
    public let host: Host?

    enum CodingKeys: String, CodingKey {
        case version, build, capabilities, accepted, features, host
        case protocolVersion = "protocol"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        version = try container.decode(String.self, forKey: .version)
        build = try container.decode(String.self, forKey: .build)
        capabilities = try container.decodeIfPresent([String].self, forKey: .capabilities) ?? []
        accepted = try container.decodeIfPresent([String].self, forKey: .accepted) ?? []
        features = try container.decodeIfPresent([String].self, forKey: .features) ?? []
        protocolVersion = try container.decodeIfPresent(Version.self, forKey: .protocolVersion)
        host = try container.decodeIfPresent(Host.self, forKey: .host)
    }
}

public struct Transcript: Decodable, Equatable, Sendable {
    public let items: [TranscriptItem]
    /// The first turn the answer covers; nil when it is the whole transcript,
    /// and for an `after_seq` answer.
    public let fromTurn: Int?
    public let earlier: Int?
    /// B2: the session's newest seq at the read — the only cursor. Absent
    /// from a Desk before `transcript_seq`.
    public let throughSeq: Int?
    /// B2: the rows were rebuilt after the cursor asked: this is a page, and
    /// the reader replaces its fold with it.
    public let reset: Bool

    enum CodingKeys: String, CodingKey {
        case items, earlier, reset
        case fromTurn = "from_turn"
        case throughSeq = "through_seq"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // One item this build cannot read does not cost the page around it.
        items = try c.decode([Lenient<TranscriptItem>].self, forKey: .items).enumerated().compactMap { at, item in
            if let why = item.failed {
                Log.door.error("item \(at) of a transcript page does not read (\(why, privacy: .public)); skipped")
            }
            return item.value
        }
        fromTurn = try c.decodeIfPresent(Int.self, forKey: .fromTurn)
        earlier = try c.decodeIfPresent(Int.self, forKey: .earlier)
        throughSeq = try c.decodeIfPresent(Int.self, forKey: .throughSeq)
        reset = try c.decodeIfPresent(Bool.self, forKey: .reset) ?? false
    }
}

public struct TranscriptItem: Decodable, Equatable, Sendable, Identifiable {
    public let id: String
    public let sessionID: String
    public let ordinal: Int
    /// `message`, `tool_call`, `thought` or `compaction`.
    public let kind: String
    public let role: String?
    /// A message's text; a tool call's JSON (`name`, `title`, `kind`, `status`, …).
    public let content: String?
    public let turn: Int?
    public let parentCallID: String?
    public let createdAt: String
    /// A `tool_call` item's content, read once with the item; nil for every
    /// other kind, and for content that is not a JSON object (B4).
    public let toolCall: TranscriptToolCall?
    /// A `compaction` item's content; nil for every other kind.
    public let compaction: Compaction?
    /// What the person attached to their message; nil from a core before
    /// attachments, and on every other item.
    public let attachments: [AttachmentRef]?

    enum CodingKeys: String, CodingKey {
        case id, ordinal, kind, role, content, turn, attachments
        case sessionID = "session_id"
        case parentCallID = "parent_call_id"
        case createdAt = "created_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        sessionID = try c.decode(String.self, forKey: .sessionID)
        ordinal = try c.decode(Int.self, forKey: .ordinal)
        kind = try c.decode(String.self, forKey: .kind)
        role = try c.decodeIfPresent(String.self, forKey: .role)
        content = try c.decodeIfPresent(String.self, forKey: .content)
        turn = try c.decodeIfPresent(Int.self, forKey: .turn)
        parentCallID = try c.decodeIfPresent(String.self, forKey: .parentCallID)
        createdAt = try c.decode(String.self, forKey: .createdAt)
        attachments = try c.decodeIfPresent([AttachmentRef].self, forKey: .attachments)
        toolCall = kind == "tool_call"
            ? content.flatMap { try? Wire.decoder.decode(TranscriptToolCall.self, from: Data($0.utf8)) }
            : nil
        compaction = kind == "compaction"
            ? content.flatMap { try? Wire.decoder.decode(Compaction.self, from: Data($0.utf8)) }
            : nil
    }

    /// When the item was written; nil when the core spelled it some other way.
    public var created: Date? { WireDate.parse(createdAt) }
}

/// The parts of a recorded tool call this app reads. The live moment names
/// the call by `providerToolCallID` (`Said.toolCall`'s `callID`), not by the
/// item's own id, so a line drawn live and the same call read back off the
/// transcript are matched on it.
public struct TranscriptToolCall: Decodable, Equatable, Sendable {
    public let providerToolCallID: String?
    public let name: String?
    public let title: String?
    /// What the tool does (`edit`, `read`, …): the live moment's `tool_kind`.
    public let kind: String?
    public let status: String?
    public let parentCallID: String?
    public let startedAt: Date?
    public let endedAt: Date?

    enum CodingKeys: String, CodingKey {
        case name, title, kind, status
        case providerToolCallID = "provider_tool_call_id"
        case parentCallID = "parent_call_id"
        case startedAt = "started_at"
        case endedAt = "ended_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        providerToolCallID = try c.decodeIfPresent(String.self, forKey: .providerToolCallID)
        name = try c.decodeIfPresent(String.self, forKey: .name)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        kind = try c.decodeIfPresent(String.self, forKey: .kind)
        status = try c.decodeIfPresent(String.self, forKey: .status)
        parentCallID = try c.decodeIfPresent(String.self, forKey: .parentCallID)
        startedAt = try c.decodeIfPresent(String.self, forKey: .startedAt).flatMap(WireDate.parse)
        endedAt = try c.decodeIfPresent(String.self, forKey: .endedAt).flatMap(WireDate.parse)
    }
}

/// A compaction of the agent's context, as the `compaction` item's content and
/// the live moment both carry it: how it stands, and the tokens in context
/// before and after, where they were measured.
public struct Compaction: Decodable, Equatable, Sendable {
    /// `in_progress`, `completed`, `failed` or `cancelled`.
    public let status: String
    public let before: Int?
    public let after: Int?

    public init(status: String, before: Int? = nil, after: Int? = nil) {
        self.status = status
        self.before = before
        self.after = after
    }
}

public struct DoorRefusal: Decodable, Equatable, Sendable {
    public let code: String
    public let message: String
    public let reason: String?
}

public enum DoorEvent: Decodable, Equatable, Sendable {
    case said(sessionID: String, turn: Int, said: Said)
    case session(DoorSession)
    case lagged(missed: Int)
    case other(type: String)

    enum CodingKeys: String, CodingKey {
        case type, turn, said, session, missed
        case sessionID = "session_id"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        guard let type = try c.decodeIfPresent(String.self, forKey: .type) else { throw DoorWireError.missing("type") }
        switch type {
        case "said":
            guard let sessionID = try c.decodeIfPresent(String.self, forKey: .sessionID),
                  let turn = try c.decodeIfPresent(Int.self, forKey: .turn), c.contains(.said) else {
                throw DoorWireError.missing("said")
            }
            self = .said(sessionID: sessionID, turn: turn, said: try c.decode(Said.self, forKey: .said))
        case "session":
            guard c.contains(.session) else { throw DoorWireError.missing("session") }
            self = .session(try c.decode(DoorSession.self, forKey: .session))
        case "lagged":
            self = .lagged(missed: try c.decodeIfPresent(Int.self, forKey: .missed) ?? 0)
        default:
            self = .other(type: type)
        }
    }
}

/// The fields of a `SessionView` this app reads; the rest are ignored.
public struct DoorSession: Decodable, Equatable, Sendable {
    public let id: String
    public let title: String
    public let status: String
    public let working: Bool
    public let turns: Int
    public let organizationID: String?
    /// What the session waits on now: the oldest question parked. An
    /// attention this app cannot read is `.other("unreadable")`, so the rest
    /// of the row still applies.
    public let attention: Attention?
    /// How many questions are parked, of which `attention` is the oldest.
    public let waiting: Int
    /// The prompts queued behind the turn running, next first.
    public let queued: [QueuedPrompt]
    /// The session's worktree root on the Mac: an agent's Markdown image
    /// under it is read with `file_image_read`.
    public let worktreePath: String?
    /// A prompt asked for whose turn has not started: nil when none waits,
    /// and when it does not read.
    public let requestedPrompt: RequestedPrompt?
    /// How the last turn ended: `end_turn`, or the reason it was stopped or
    /// failed. Nil where no turn has ended yet.
    public let lastOutcome: String?
    /// When the turn running began; nil where none runs.
    public let turnStartedAt: Date?

    enum CodingKeys: String, CodingKey {
        case id, title, status, working, turns, attention, waiting, queued
        case organizationID = "organization_id"
        case worktreePath = "worktree_path"
        case requestedPrompt = "requested_prompt"
        case lastOutcome = "last_outcome"
        case turnStartedAt = "turn_started_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decode(String.self, forKey: .title)
        status = try c.decode(String.self, forKey: .status)
        working = try c.decode(Bool.self, forKey: .working)
        turns = try c.decode(Int.self, forKey: .turns)
        organizationID = try c.decodeIfPresent(String.self, forKey: .organizationID)
        do {
            attention = try c.decodeIfPresent(Attention.self, forKey: .attention)
        } catch {
            Log.door.error("a row's attention does not read (\(DoorConnection.unread(error), privacy: .public)); nothing is pinned from it")
            attention = .other(type: "unreadable")
        }
        waiting = try c.decodeIfPresent(Int.self, forKey: .waiting) ?? 0
        queued = try c.decodeIfPresent([QueuedPrompt].self, forKey: .queued) ?? []
        worktreePath = try c.decodeIfPresent(String.self, forKey: .worktreePath)
        do {
            requestedPrompt = try c.decodeIfPresent(RequestedPrompt.self, forKey: .requestedPrompt)
        } catch {
            Log.door.error("a row's requested prompt does not read (\(DoorConnection.unread(error), privacy: .public)); none is drawn")
            requestedPrompt = nil
        }
        lastOutcome = try c.decodeIfPresent(String.self, forKey: .lastOutcome)
        turnStartedAt = try c.decodeIfPresent(String.self, forKey: .turnStartedAt).flatMap(WireDate.parse)
    }
}

/// An environment of the Mac's Desk (`EnvironmentView`): the login a row
/// belongs to, which `sessions_list` is asked by.
public struct DoorEnvironment: Decodable, Equatable, Sendable {
    public struct Organization: Decodable, Equatable, Sendable {
        public let id: String
    }

    public let id: String
    public let organizations: [Organization]
    public let mothership: Bool

    enum CodingKeys: String, CodingKey { case id, organizations, mothership }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        organizations = try c.decodeIfPresent([Organization].self, forKey: .organizations) ?? []
        mothership = try c.decodeIfPresent(Bool.self, forKey: .mothership) ?? false
    }
}

/// A prompt the agent has not taken yet (`RequestedPrompt`): asked for, its
/// turn not started, as often as not behind a compaction the core runs first.
public struct RequestedPrompt: Decodable, Equatable, Sendable, Identifiable {
    /// The same for as long as it waits.
    public let id: String
    public let text: String
    /// How many photos ride with it.
    public let attachments: Int
    /// The compaction it was sent behind is the turn running now.
    public let compacting: Bool

    public static let waitingForCompaction = "Waiting for the compaction to finish"
    public static let waitingToStart = "Waiting to start"

    public init(id: String, text: String, attachments: Int = 0, compacting: Bool) {
        self.id = id
        self.text = text
        self.attachments = attachments
        self.compacting = compacting
    }

    /// The line under its bubble.
    public var caption: String { compacting ? Self.waitingForCompaction : Self.waitingToStart }

    enum CodingKeys: String, CodingKey { case id, text, attachments, compacting }

    /// The photos are counted, not read: one this app cannot read is still
    /// a photo the prompt carries.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        text = try c.decode(String.self, forKey: .text)
        attachments = try c.decodeIfPresent([JSONValue].self, forKey: .attachments)?.count ?? 0
        compacting = try c.decodeIfPresent(Bool.self, forKey: .compacting) ?? false
    }
}

/// One prompt waiting its turn (`QueuedPrompt`).
public struct QueuedPrompt: Decodable, Equatable, Sendable, Identifiable {
    public let queueID: String
    public let text: String
    public let at: String
    /// Where a Steer of it stands: `asked`, `claimed`, `refused` or `unknown`.
    public let steer: String?
    public let steerReason: String?

    public var id: String { queueID }

    public init(queueID: String, text: String, at: String, steer: String?, steerReason: String?) {
        self.queueID = queueID
        self.text = text
        self.at = at
        self.steer = steer
        self.steerReason = steerReason
    }

    enum CodingKeys: String, CodingKey {
        case text, at, steer
        case queueID = "queue_id"
        case steerReason = "steer_reason"
    }
}

// MARK: what a session waits on

/// What a session wants from the person (`api::Attention`), tagged `type`.
public enum Attention: Decodable, Equatable, Sendable {
    case approval(PermissionRequest)
    case elicitation(Elicitation)
    /// A server prompt's question: a form whose `request_id` is the question's id.
    case question(questionID: String, expiresAt: String, request: Elicitation)
    /// A looper run's `asked`, or a word this app does not know.
    case other(type: String)

    enum CodingKeys: String, CodingKey {
        case type, request
        case questionID = "question_id"
        case expiresAt = "expires_at"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        guard let type = try c.decodeIfPresent(String.self, forKey: .type) else { throw DoorWireError.missing("type") }
        switch type {
        case "approval":
            guard c.contains(.request) else { throw DoorWireError.missing("request") }
            self = .approval(try c.decode(PermissionRequest.self, forKey: .request))
        case "elicitation":
            guard c.contains(.request) else { throw DoorWireError.missing("request") }
            self = .elicitation(try c.decode(Elicitation.self, forKey: .request))
        case "question":
            guard let id = try c.decodeIfPresent(String.self, forKey: .questionID), c.contains(.request) else {
                throw DoorWireError.missing("question")
            }
            self = .question(questionID: id, expiresAt: try c.decodeIfPresent(String.self, forKey: .expiresAt) ?? "", request: try c.decode(Elicitation.self, forKey: .request))
        default:
            self = .other(type: type)
        }
    }
}

/// One answer the agent will take to a permission request
/// (`harness::PermissionOption`).
public struct PermissionOption: Decodable, Equatable, Sendable, Identifiable {
    public let id: String
    /// `allow_once`, `allow_always`, `reject_once` or `reject_always`.
    public let kind: String
    /// The agent's words for the button, where it gave any.
    public let name: String?

    /// The button's words: the agent's, else the kind's.
    public var label: String {
        if let name, !name.isEmpty { return name }
        switch kind {
        case "allow_once": return "Allow once"
        case "allow_always": return "Always"
        case "reject_once": return "Deny"
        case "reject_always": return "Never"
        default: return kind
        }
    }

    public var rejects: Bool { kind.hasPrefix("reject") }
}

/// A question the agent stopped to ask (`harness::PermissionRequest`).
/// `input` is drawn, never matched on; a row's copy may have left it off
/// for size (`inputDropped`).
public struct PermissionRequest: Decodable, Equatable, Sendable {
    public let requestID: String
    public let callID: String
    /// The call's ACP title (for Bash, the whole command); "a tool" where
    /// no title was said; an MCP call's tool name where `server` is set.
    public let tool: String
    /// The MCP server `tool` belongs to; nil for a harness's own tools.
    public let server: String?
    public let kind: String
    public let input: JSONValue?
    public let inputDropped: Bool
    public let options: [PermissionOption]

    enum CodingKeys: String, CodingKey {
        case tool, server, kind, input, options
        case requestID = "request_id"
        case callID = "call_id"
        case inputDropped = "input_dropped"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        requestID = try c.decode(String.self, forKey: .requestID)
        callID = try c.decode(String.self, forKey: .callID)
        tool = try c.decode(String.self, forKey: .tool)
        server = try c.decodeIfPresent(String.self, forKey: .server)
        kind = try c.decode(String.self, forKey: .kind)
        input = try c.decodeIfPresent(JSONValue.self, forKey: .input)
        inputDropped = try c.decodeIfPresent(Bool.self, forKey: .inputDropped) ?? false
        options = try c.decode([PermissionOption].self, forKey: .options)
    }

    /// The card's heading, in the server's words for it
    /// (the server's approval summary), which the push and
    /// Home say too: "Wants to execute" for an ACP kind that names an
    /// action, "Wants approval" for any other. Never the tool: the Desk
    /// sends the call's title there, and a Bash call's title is its whole
    /// command, which the card draws as its detail.
    public var summary: String {
        Self.actions.contains(kind) ? "Wants to \(kind)" : "Wants approval"
    }

    /// The call's ACP title, where `tool` is one: not the Desk's word for a
    /// call with none, and not an MCP tool's name, whose arguments say more.
    public var title: String? {
        guard server == nil, !tool.isEmpty, tool != "a tool" else { return nil }
        return tool
    }

    static let actions: Set<String> = ["read", "edit", "delete", "move", "search", "execute", "fetch"]
}

/// A structured question the agent asked (`harness::elicitation::Elicitation`).
public struct Elicitation: Decodable, Equatable, Sendable {
    public let requestID: String
    public let callID: String?
    /// The agent's words, verbatim.
    public let message: String
    public let mode: ElicitationMode

    enum CodingKeys: String, CodingKey {
        case message, mode
        case requestID = "request_id"
        case callID = "call_id"
    }
}

/// A link to open, or fields to fill.
public enum ElicitationMode: Decodable, Equatable, Sendable {
    /// `signIn` names the connector when the link is a sign-in.
    case url(url: String, signIn: String?)
    case form(title: String?, fields: [FormField])
    case other(type: String)

    enum CodingKeys: String, CodingKey {
        case type, url, title, fields
        case signIn = "sign_in"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        guard let type = try c.decodeIfPresent(String.self, forKey: .type) else { throw DoorWireError.missing("type") }
        switch type {
        case "url":
            guard let url = try c.decodeIfPresent(String.self, forKey: .url) else { throw DoorWireError.missing("url") }
            self = .url(url: url, signIn: try c.decodeIfPresent(String.self, forKey: .signIn))
        case "form":
            self = .form(title: try c.decodeIfPresent(String.self, forKey: .title), fields: try c.decodeIfPresent([FormField].self, forKey: .fields) ?? [])
        default:
            self = .other(type: type)
        }
    }
}

/// One field of a form (`harness::elicitation::Field`).
public struct FormField: Decodable, Equatable, Sendable, Identifiable {
    public let name: String
    public let title: String?
    public let description: String?
    public let required: Bool
    /// Drawn masked.
    public let secret: Bool
    public let kind: FieldKind

    public var id: String { name }
    /// The field's title, else its name.
    public var label: String {
        if let title, !title.isEmpty { return title }
        return name
    }

    enum CodingKeys: String, CodingKey { case name, title, description, required, secret, kind }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        description = try c.decodeIfPresent(String.self, forKey: .description)
        required = try c.decodeIfPresent(Bool.self, forKey: .required) ?? false
        secret = try c.decodeIfPresent(Bool.self, forKey: .secret) ?? false
        kind = try c.decode(FieldKind.self, forKey: .kind)
    }
}

public struct ChoiceOption: Decodable, Equatable, Sendable {
    public let value: String
    public let title: String
    public let description: String?

    public init(value: String, title: String, description: String?) {
        self.value = value
        self.title = title
        self.description = description
    }
}

/// What a field takes (`harness::elicitation::FieldKind`), tagged `type`.
/// Lengths count code points, as the core does. Numbers are f64 on the
/// wire (`1.0`, even for an `integer` field).
public enum FieldKind: Decodable, Equatable, Sendable {
    case text(format: String?, minLength: Int?, maxLength: Int?, pattern: String?, defaultValue: String?)
    case number(integer: Bool, min: Double?, max: Double?, defaultValue: Double?)
    case boolean(defaultValue: Bool?)
    case choice(options: [ChoiceOption], defaultValue: String?)
    /// Pick-many: not drawn on the phone in this slice.
    case multiChoice(options: [ChoiceOption])
    /// The core's `unknown`, or a type this app does not know: free text.
    case unknown(rawType: String)

    enum CodingKeys: String, CodingKey {
        case type, format, pattern, integer, min, max, options
        case minLength = "min_len"
        case maxLength = "max_len"
        case defaultValue = "default"
        case rawType = "raw_type"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        guard let type = try c.decodeIfPresent(String.self, forKey: .type) else { throw DoorWireError.missing("type") }
        switch type {
        case "text":
            self = .text(format: try c.decodeIfPresent(String.self, forKey: .format),
                         minLength: try c.decodeIfPresent(Int.self, forKey: .minLength),
                         maxLength: try c.decodeIfPresent(Int.self, forKey: .maxLength),
                         pattern: try c.decodeIfPresent(String.self, forKey: .pattern),
                         defaultValue: try c.decodeIfPresent(String.self, forKey: .defaultValue))
        case "number":
            self = .number(integer: try c.decodeIfPresent(Bool.self, forKey: .integer) ?? false,
                           min: try c.decodeIfPresent(Double.self, forKey: .min),
                           max: try c.decodeIfPresent(Double.self, forKey: .max),
                           defaultValue: try c.decodeIfPresent(Double.self, forKey: .defaultValue))
        case "boolean":
            self = .boolean(defaultValue: try c.decodeIfPresent(Bool.self, forKey: .defaultValue))
        case "choice":
            self = .choice(options: try c.decodeIfPresent([ChoiceOption].self, forKey: .options) ?? [],
                           defaultValue: try c.decodeIfPresent(String.self, forKey: .defaultValue))
        case "multi_choice":
            self = .multiChoice(options: try c.decodeIfPresent([ChoiceOption].self, forKey: .options) ?? [])
        case "unknown":
            self = .unknown(rawType: try c.decodeIfPresent(String.self, forKey: .rawType) ?? "unknown")
        default:
            self = .unknown(rawType: type)
        }
    }
}

/// A moment of a running session (`desk_core::session::Said`), tagged `kind`.
public enum Said: Decodable, Equatable, Sendable {
    /// `tail` is what has not been said yet; `whole`, when present, is all of it so far.
    case text(messageID: String, tail: String, whole: String?, parentCallID: String?)
    case toolCall(callID: String, name: String, title: String?, status: String, settled: Bool, parentCallID: String?)
    /// A permission request, with the options as offered.
    case asked(PermissionRequest)
    /// `by` is `person` or `desk`: never which device.
    case answered(requestID: String, optionID: String, by: String)
    /// Nobody answered; the turn was cancelled.
    case unanswered(requestID: String)
    case elicited(Elicitation)
    /// `accepted`, `declined`, `cancelled`, `withdrawn` or `lost`.
    case elicitationDecided(requestID: String, outcome: String)
    case turnEnded(outcome: String, detail: String?)
    /// A steer or a peer's words went into the running turn: re-read it.
    case heard(from: String)
    /// A fresh agent was told the history: re-read.
    case handoff
    /// A turn began that nobody prompted.
    case continued
    /// The agent's context is being compacted, or was: `id` is its item's.
    case compaction(id: String, Compaction)
    case lagged(missed: Int)
    case other(kind: String)

    enum CodingKeys: String, CodingKey {
        case kind, tail, whole, name, title, status, settled, request, by, decision, outcome, detail, from, missed, id
        case messageID = "message_id"
        case parentCallID = "parent_call_id"
        case callID = "call_id"
        case requestID = "request_id"
        case optionID = "option_id"
    }

    enum DecisionKeys: String, CodingKey { case outcome }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? ""
        switch kind {
        case "text":
            self = .text(
                messageID: try c.decodeIfPresent(String.self, forKey: .messageID) ?? "",
                tail: try c.decodeIfPresent(String.self, forKey: .tail) ?? "",
                whole: try c.decodeIfPresent(String.self, forKey: .whole),
                parentCallID: try c.decodeIfPresent(String.self, forKey: .parentCallID)
            )
        case "tool_call":
            self = .toolCall(
                callID: try c.decodeIfPresent(String.self, forKey: .callID) ?? "",
                name: try c.decodeIfPresent(String.self, forKey: .name) ?? "",
                title: try c.decodeIfPresent(String.self, forKey: .title),
                status: try c.decodeIfPresent(String.self, forKey: .status) ?? "",
                settled: try c.decodeIfPresent(Bool.self, forKey: .settled) ?? false,
                parentCallID: try c.decodeIfPresent(String.self, forKey: .parentCallID)
            )
        case "asked":
            guard c.contains(.request) else { throw DoorWireError.missing("request") }
            self = .asked(try c.decode(PermissionRequest.self, forKey: .request))
        case "answered":
            self = .answered(requestID: try c.decodeIfPresent(String.self, forKey: .requestID) ?? "",
                             optionID: try c.decodeIfPresent(String.self, forKey: .optionID) ?? "",
                             by: try c.decodeIfPresent(String.self, forKey: .by) ?? "")
        case "unanswered":
            self = .unanswered(requestID: try c.decodeIfPresent(String.self, forKey: .requestID) ?? "")
        case "elicited":
            guard c.contains(.request) else { throw DoorWireError.missing("request") }
            self = .elicited(try c.decode(Elicitation.self, forKey: .request))
        case "elicitation_decided":
            var outcome = ""
            if let decision = try? c.nestedContainer(keyedBy: DecisionKeys.self, forKey: .decision) {
                outcome = try decision.decodeIfPresent(String.self, forKey: .outcome) ?? ""
            }
            self = .elicitationDecided(requestID: try c.decodeIfPresent(String.self, forKey: .requestID) ?? "", outcome: outcome)
        case "turn_ended":
            self = .turnEnded(outcome: try c.decodeIfPresent(String.self, forKey: .outcome) ?? "", detail: try c.decodeIfPresent(String.self, forKey: .detail))
        case "heard":
            self = .heard(from: try c.decodeIfPresent(String.self, forKey: .from) ?? "")
        case "handoff":
            self = .handoff
        case "continued":
            self = .continued
        case "compaction":
            self = .compaction(id: try c.decodeIfPresent(String.self, forKey: .id) ?? "", try Compaction(from: decoder))
        case "lagged":
            self = .lagged(missed: try c.decodeIfPresent(Int.self, forKey: .missed) ?? 0)
        default:
            self = .other(kind: kind)
        }
    }
}

/// The word an answer to a structured question carries.
public enum AnswerAction: String, Equatable, Sendable {
    case accept, decline, cancel
}

/// B2: the transcript's shape a reader asks for, where hello offers `transcript_brief`.
public enum TranscriptShape: String, Equatable, Sendable {
    case brief
}

/// What this app asks a door. Every answer and prompt carries an `op` the
/// phone minted: the same one sent again after a lost reply is answered from
/// the Mac's record and never written twice.
public enum DoorRequest: Equatable, Sendable {
    case hello(wants: [String])
    case sessionTranscript(sessionID: String, fromTurn: Int?, lastTurns: Int?, shape: TranscriptShape? = nil, afterSeq: Int? = nil)
    case sessionGet(sessionID: String)
    case environmentsList
    case sessionsList(environmentID: String)
    case sessionAnswer(sessionID: String, requestID: String, optionID: String, op: String)
    case sessionElicitationAnswer(sessionID: String, requestID: String, action: AnswerAction, content: JSONValue?, op: String)
    case sessionQuestionAnswer(sessionID: String, questionID: String, action: AnswerAction, content: JSONValue?, op: String)
    /// `attachments` are refs an `attachment_put` answered; written only
    /// when there are any.
    case sessionPrompt(sessionID: String, text: String, queue: Bool, op: String, attachments: [AttachmentRef] = [])
    case sessionSteer(sessionID: String, queueID: String)
    case sessionWithdraw(sessionID: String, queueID: String)
    /// A photo's bytes, kept content-addressed on the Mac (at most 20 MiB).
    case attachmentPut(name: String, mime: String, bytesB64: String)
    /// The bytes of a file a person attached.
    case attachmentRead(path: String)
    /// A raster image inside the session's worktree, by its relative path.
    case fileImageRead(sessionID: String, path: String)

    var body: JSONValue {
        switch self {
        case .hello(let wants):
            return .object(["type": .string("hello"), "wants": .array(wants.map(JSONValue.string))])
        case .sessionTranscript(let sessionID, let fromTurn, let lastTurns, let shape, let afterSeq):
            var fields: [String: JSONValue] = ["type": .string("session_transcript"), "session_id": .string(sessionID)]
            if let fromTurn { fields["from_turn"] = .number(Double(fromTurn)) }
            if let lastTurns { fields["last_turns"] = .number(Double(lastTurns)) }
            if let shape { fields["shape"] = .string(shape.rawValue) }
            if let afterSeq { fields["after_seq"] = .number(Double(afterSeq)) }
            return .object(fields)
        case .sessionGet(let sessionID):
            return .object(["type": .string("session_get"), "session_id": .string(sessionID)])
        case .environmentsList:
            return .object(["type": .string("environments_list")])
        case .sessionsList(let environmentID):
            return .object(["type": .string("sessions_list"), "environment_id": .string(environmentID)])
        case .sessionAnswer(let sessionID, let requestID, let optionID, let op):
            return .object([
                "type": .string("session_answer"), "session_id": .string(sessionID),
                "request_id": .string(requestID), "option_id": .string(optionID), "op": .string(op),
            ])
        case .sessionElicitationAnswer(let sessionID, let requestID, let action, let content, let op):
            var fields: [String: JSONValue] = [
                "type": .string("session_elicitation_answer"), "session_id": .string(sessionID),
                "request_id": .string(requestID), "action": .string(action.rawValue), "op": .string(op),
            ]
            if let content { fields["content"] = content }
            return .object(fields)
        case .sessionQuestionAnswer(let sessionID, let questionID, let action, let content, let op):
            var fields: [String: JSONValue] = [
                "type": .string("session_question_answer"), "session_id": .string(sessionID),
                "question_id": .string(questionID), "action": .string(action.rawValue), "op": .string(op),
            ]
            if let content { fields["content"] = content }
            return .object(fields)
        case .sessionPrompt(let sessionID, let text, let queue, let op, let attachments):
            var fields: [String: JSONValue] = [
                "type": .string("session_prompt"), "session_id": .string(sessionID), "text": .string(text), "op": .string(op),
            ]
            if queue { fields["queue"] = .bool(true) }
            if !attachments.isEmpty { fields["attachments"] = .array(attachments.map(\.json)) }
            return .object(fields)
        case .sessionSteer(let sessionID, let queueID):
            return .object(["type": .string("session_steer"), "session_id": .string(sessionID), "queue_id": .string(queueID)])
        case .sessionWithdraw(let sessionID, let queueID):
            return .object(["type": .string("session_withdraw"), "session_id": .string(sessionID), "queue_id": .string(queueID)])
        case .attachmentPut(let name, let mime, let bytesB64):
            return .object(["type": .string("attachment_put"), "name": .string(name), "mime": .string(mime), "bytes_b64": .string(bytesB64)])
        case .attachmentRead(let path):
            return .object(["type": .string("attachment_read"), "path": .string(path)])
        case .fileImageRead(let sessionID, let path):
            return .object(["type": .string("file_image_read"), "session_id": .string(sessionID), "path": .string(path)])
        }
    }

    public func frame(id: Int) throws -> String {
        try JSONValue.object(["id": .number(Double(id)), "request": body]).text()
    }

    var kind: String { body["type"]?.string ?? "" }

    /// The op an answer or a prompt goes under; nil for every other request.
    var op: String? { body["op"]?.string }
}

/// The largest frame a door carries, 32 MiB: the Mac's Desk refuses a
/// larger one, and Go's door reads up to the same (`largestFrame` in the
/// Connector's `phonenode/door.go`). A photo read back is one frame of
/// base64.
public enum DoorLimits {
    public static let largestFrame = 32 * 1024 * 1024
}

/// A file attached to a prompt (`attachments::AttachmentRef`): what a put
/// kept, and what a person's transcript message carries. Sent back exactly
/// as received; the core stats the path again.
public struct AttachmentRef: Codable, Equatable, Sendable, Hashable {
    public enum Kind: String, Codable, Sendable, Hashable {
        /// `unknown` is a kind this app does not know: drawn as a plain
        /// attachment, never read as one it does.
        case image, file, folder, unknown

        public init(from decoder: Decoder) throws {
            self = Kind(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
        }
    }

    public let path: String
    public let name: String
    public let kind: Kind
    public let size: Int?
    public let mime: String?
    public let hash: String?

    public init(path: String, name: String, kind: Kind, size: Int?, mime: String? = nil, hash: String? = nil) {
        self.path = path
        self.name = name
        self.kind = kind
        self.size = size
        self.mime = mime
        self.hash = hash
    }

    enum CodingKeys: String, CodingKey { case path, name, kind, size, mime, hash }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decode(String.self, forKey: .path)
        name = try c.decode(String.self, forKey: .name)
        kind = try c.decode(Kind.self, forKey: .kind)
        size = try c.decodeIfPresent(Int.self, forKey: .size)
        mime = try c.decodeIfPresent(String.self, forKey: .mime)
        hash = try c.decodeIfPresent(String.self, forKey: .hash)
    }

    /// As the core writes it: `size` always (null where unknown), `mime`
    /// and `hash` only where known.
    var json: JSONValue {
        var fields: [String: JSONValue] = [
            "path": .string(path), "name": .string(name), "kind": .string(kind.rawValue),
            "size": size.map { .number(Double($0)) } ?? .null,
        ]
        if let mime { fields["mime"] = .string(mime) }
        if let hash { fields["hash"] = .string(hash) }
        return .object(fields)
    }
}
