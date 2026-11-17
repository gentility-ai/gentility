import Foundation

/// What the pinned card asks: the session's oldest question.
public enum Ask: Equatable, Sendable {
    case approval(PermissionRequest)
    case elicitation(Elicitation)
    /// A server prompt's question, drawn as an elicitation is and answered
    /// with `session_question_answer`.
    case question(questionID: String, expiresAt: Date?, request: Elicitation)

    /// The id an answer names.
    public var id: String {
        switch self {
        case .approval(let request): request.requestID
        case .elicitation(let request): request.requestID
        case .question(let id, _, _): id
        }
    }

    /// What a session's attention pins: nothing for none, a looper's, an
    /// unknown word, or a question past its `expires_at`.
    public init?(_ attention: Attention?, now: Date) {
        switch attention {
        case .approval(let request)?:
            self = .approval(request)
        case .elicitation(let request)?:
            self = .elicitation(request)
        case .question(let id, let expires, let request)?:
            let at = WireDate.parse(expires)
            if let at, at <= now { return nil }
            self = .question(questionID: id, expiresAt: at, request: request)
        case .other?, nil:
            return nil
        }
    }

    /// A question past its `expires_at`, which the Mac refuses an answer to.
    public func expired(at now: Date) -> Bool {
        if case .question(_, let at?, _) = self { return at <= now }
        return false
    }
}

/// What the person answered.
public enum Answer: Equatable, Sendable {
    /// An approval's option.
    case option(String)
    /// A form's content, or a link's Done (no content).
    case accept(JSONValue?)
    case decline
}

/// The pinned card: what is asked, and where its answer stands.
public struct Card: Equatable, Sendable {
    public let ask: Ask
    /// Out and not yet answered, or owed after a drop.
    public fileprivate(set) var answer: Answer?
    /// The op the answer went with; the same one on a resend.
    public fileprivate(set) var op: String?
    /// The answer did not reach the Mac: resent once the door is back.
    public fileprivate(set) var owed = false
    /// The Mac refused the answer, and the row is being read to see why.
    public fileprivate(set) var checking = false
    fileprivate var refusal: DoorRefusal?
    public fileprivate(set) var note: String?

    public init(ask: Ask, note: String? = nil) {
        self.ask = ask
        self.note = note
    }

    /// The answer is on its way now.
    public var sending: Bool { answer != nil && !owed }
}

/// The card's state machine: fed rows,
/// moments and replies; asked for the request an answer or a resend sends.
public struct CardFold: Equatable, Sendable {
    public private(set) var card: Card?
    /// A line where the card was.
    public private(set) var cleared: String?
    /// Ids answered or decided: a row from before that never pins them again.
    private var settled: Set<String> = []
    /// What the latest row asks, kept while a card holds the place: it pins
    /// once that card settles, since nothing else may say it again.
    private var latest: Ask?

    public static let answeredElsewhere = "Answered on another device"
    public static let expired = "This question has expired"
    public static let unreadable = "Desk on your Mac didn't take that answer. Answer it on your Mac."

    public static func unreachable(_ mac: String) -> String { "Couldn't reach \(mac) — trying again" }
    /// An owed answer nothing will try again by itself: the open was refused.
    public static let waitingForRetry = "Your answer hasn't gone yet. Retry sends it."
    public static func waitingForOpen(_ mac: String) -> String {
        "Your answer hasn't gone yet. It will send when Desk on \(mac) is open to your phone."
    }

    public init() {}

    /// A row of the session: from the row read, or the door's events. A row
    /// naming an id already settled was said before that answer landed, and
    /// says nothing now.
    public mutating func row(_ attention: Attention?, now: Date) {
        let asked = Ask(attention, now: now)
        if let asked, settled.contains(asked.id) { return }
        latest = asked
        if var pinned = card {
            if pinned.answer != nil { return }
            if pinned.checking {
                if asked?.id == pinned.ask.id {
                    pinned.checking = false
                    pinned.note = pinned.refusal?.message
                    card = pinned
                    return
                }
                settle(pinned.ask.id, saying: Self.answeredElsewhere)
                return
            } else if asked?.id == pinned.ask.id {
                return
            } else {
                card = nil
            }
        }
        if let asked { pin(asked) }
    }

    /// The row read after a refusal found no row: the refusal stands.
    public mutating func rowUnread() {
        guard var pinned = card, pinned.checking else { return }
        pinned.checking = false
        pinned.note = pinned.refusal?.message
        card = pinned
    }

    public mutating func said(_ said: Said) {
        switch said {
        case .asked(let request):
            pinIfFree(.approval(request))
        case .elicited(let request):
            pinIfFree(.elicitation(request))
        case .answered(let id, let option, _):
            settle(id, saying: card?.answer == .option(option) ? nil : Self.answeredElsewhere)
        case .unanswered(let id):
            settle(id, saying: nil)
        case .elicitationDecided(let id, let outcome):
            switch (card?.answer, outcome) {
            case (.accept?, "accepted"), (.decline?, "declined"): settle(id, saying: nil)
            case (_, "accepted"), (_, "declined"): settle(id, saying: Self.answeredElsewhere)
            default: settle(id, saying: nil)
            }
        default:
            break
        }
    }

    /// The person answered at `now`: the request to send, under an op minted
    /// now. Nil sends nothing: an answer already out, one the ask did not
    /// offer, or a question expired, which clears saying so.
    public mutating func answer(_ answer: Answer, sessionID: String, now: Date, op mint: () -> String) -> DoorRequest? {
        guard var pinned = card, pinned.answer == nil, !pinned.checking,
              Self.fits(pinned.ask, answer) else { return nil }
        if pinned.ask.expired(at: now) {
            settle(pinned.ask.id, saying: Self.expired)
            return nil
        }
        let op = mint()
        pinned.answer = answer
        pinned.op = op
        pinned.owed = false
        pinned.note = nil
        pinned.refusal = nil
        card = pinned
        cleared = nil
        return Self.request(pinned.ask, answer, sessionID: sessionID, op: op)
    }

    /// The Mac's reply to the answer sent under `op`; a reply to any other
    /// answer is not this card's. True when the row must be read to weigh a
    /// refusal.
    public mutating func replied(_ response: DoorResponse, op: String) -> Bool {
        guard var pinned = card, pinned.answer != nil, pinned.op == op else { return false }
        switch response {
        case .ok:
            settle(pinned.ask.id, saying: nil)
            return false
        case .error(let refusal):
            pinned.answer = nil
            pinned.op = nil
            pinned.owed = false
            pinned.checking = true
            pinned.refusal = refusal
            card = pinned
            return true
        default:
            pinned.answer = nil
            pinned.op = nil
            pinned.owed = false
            pinned.note = Self.unreadable
            card = pinned
            return false
        }
    }

    /// The answer sent under `op` did not reach the Mac. The latest row is
    /// forgotten too: it was read on a door now gone, so only a row read on
    /// the next one may pin after this card.
    public mutating func unsent(op: String, macLabel: String) {
        guard var pinned = card, pinned.answer != nil, pinned.op == op else { return }
        latest = nil
        pinned.owed = true
        pinned.note = Self.unreachable(macLabel)
        card = pinned
    }

    /// Whether anything tries the owed answer again by itself; its line
    /// says so, and says "trying again" only while something does.
    public mutating func owed(retrying: Bool, macLabel: String) {
        guard var pinned = card, pinned.owed else { return }
        pinned.note = retrying ? Self.unreachable(macLabel) : Self.waitingForRetry
        card = pinned
    }

    public mutating func waitingForOpen(macLabel: String) {
        guard var pinned = card, pinned.owed else { return }
        pinned.note = Self.waitingForOpen(macLabel)
        card = pinned
    }

    /// The door is back: the owed answer, under the op it first went with.
    public mutating func resend(sessionID: String) -> DoorRequest? {
        guard var pinned = card, pinned.owed, let answer = pinned.answer, let op = pinned.op else { return nil }
        pinned.owed = false
        card = pinned
        return Self.request(pinned.ask, answer, sessionID: sessionID, op: op)
    }

    private mutating func pin(_ ask: Ask) {
        card = Card(ask: ask)
        cleared = nil
    }

    private mutating func pinIfFree(_ ask: Ask) {
        guard card == nil, !settled.contains(ask.id) else { return }
        pin(ask)
    }

    /// The id is done with. Its card clears with `line` (nil: silently), and
    /// the latest row's ask, if it still stands, pins in its place.
    private mutating func settle(_ id: String, saying line: String?) {
        settled.insert(id)
        guard card?.ask.id == id else { return }
        card = nil
        cleared = line
        if let next = latest, !settled.contains(next.id) { pin(next) }
    }

    static func fits(_ ask: Ask, _ answer: Answer) -> Bool {
        switch (ask, answer) {
        case (.approval(let request), .option(let option)): request.options.contains { $0.id == option }
        case (.elicitation, .accept), (.elicitation, .decline), (.question, .accept), (.question, .decline): true
        default: false
        }
    }

    static func request(_ ask: Ask, _ answer: Answer, sessionID: String, op: String) -> DoorRequest? {
        switch (ask, answer) {
        case (.approval(let request), .option(let option)):
            .sessionAnswer(sessionID: sessionID, requestID: request.requestID, optionID: option, op: op)
        case (.elicitation(let request), .accept(let content)):
            .sessionElicitationAnswer(sessionID: sessionID, requestID: request.requestID, action: .accept, content: content, op: op)
        case (.elicitation(let request), .decline):
            .sessionElicitationAnswer(sessionID: sessionID, requestID: request.requestID, action: .decline, content: nil, op: op)
        case (.question(let id, _, _), .accept(let content)):
            .sessionQuestionAnswer(sessionID: sessionID, questionID: id, action: .accept, content: content, op: op)
        case (.question(let id, _, _), .decline):
            .sessionQuestionAnswer(sessionID: sessionID, questionID: id, action: .decline, content: nil, op: op)
        default:
            nil
        }
    }
}
