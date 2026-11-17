import Foundation

/// The phone's failure screens that end pairing or a pairing's life: F1, F2,
/// F3 and F5 of the storyboard, in its words. F4 is a conversation's, not a
/// route's (`ConversationModel.Phase.asleep`).
public enum Failure: Equatable, Sendable {
    /// F1: claim refused, or poll `expired`/`spent`/`unknown`, or the code's
    /// five minutes ran out while the Mac was asked.
    case codeSpent
    /// F2: poll `denied`, a wrong number included.
    case denied
    /// F3: poll `failed` with its word, a refusal the server gave in no word
    /// this app knows (`server_error`), or a local one (`this_phone`).
    case cannotJoin(String)
    /// F3's family: the server did not answer at all.
    case unreachable(host: String)
    /// F5: the bearer is refused.
    case removed
    /// F5, when the 30-day bearer has simply run out.
    case lapsed
    /// The bearer was kept but Join never ran before the enrolment's
    /// ten-minute key died: the phone holds nothing it can join with.
    case notJoined

    public init(_ refusal: ClaimRefusal) {
        switch refusal {
        case .unknown, .expired, .spent: self = .codeSpent
        case .blankName: self = .cannotJoin("blank_name")
        case .malformed, .other: self = .cannotJoin("server_error")
        }
    }

    public var title: String {
        switch self {
        case .codeSpent: return "Code expired"
        case .denied: return "Not allowed"
        case .cannotJoin: return "Can't join yet"
        case .unreachable: return "Can't reach the server"
        case .removed: return "Phone removed"
        case .lapsed: return "Pairing ran out"
        case .notJoined: return "Pairing didn't finish"
        }
    }

    public var lines: [String] {
        switch self {
        case .codeSpent:
            return ["That code is more than 5 minutes old or was already used.", "Choose New code in Desk, then scan again."]
        case .denied:
            return ["Your Mac said no to this phone.", "If that was a mistake, add the phone again from Desk."]
        case .cannotJoin("no_gateway"):
            return ["Your organisation's network isn't set up for phones yet.", "Contact Gentility support. Nothing was changed."]
        case .cannotJoin("blank_name"):
            return ["This phone needs a name.", "Give it one under the camera, then scan a new code. Nothing was changed."]
        case .cannotJoin("this_phone"):
            return ["This phone couldn't keep its pairing safe.", "Restart the app and scan a new code."]
        case .cannotJoin:
            return ["The server couldn't finish pairing this phone.", "Nothing was changed. Choose New code in Desk and try again."]
        case .unreachable(let host):
            return ["This phone couldn't reach \(host).", "Check the connection, then scan again."]
        case .removed:
            return ["This phone was removed from your account."]
        case .lapsed:
            return ["This phone's pairing lasts 30 days and has run out."]
        case .notJoined:
            return ["This phone was allowed but never finished joining your network.", "Remove it in Desk → Settings → Phone, then pair again."]
        }
    }

    public var action: String {
        switch self {
        case .removed, .lapsed, .notJoined: return "Pair again"
        default: return "Scan again"
        }
    }
}
