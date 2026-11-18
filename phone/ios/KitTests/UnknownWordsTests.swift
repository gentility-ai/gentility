import XCTest
@testable import DeskPhoneKit

/// every vocabulary the phone decodes into a Swift enum, held to the
/// released protocol (`contract/released.json`). A word the phone
/// does not know reads as the fallback inside a whole frame, and the rest of
/// the frame still reads: never a crash, never a failed frame, never a known
/// value.
final class UnknownWordsTests: XCTestCase {
    /// What a frame carrying a word read as; nil when the frame around the
    /// word did not read whole.
    private enum Reading: Equatable { case known, fallback }

    private struct Vocabulary {
        /// The type's name in released.json.
        let name: String
        /// The words the phone has a case for.
        let named: Set<String>
        /// Whether the phone mirrors every released word, or a chosen subset.
        let mirrorsAll: Bool
        /// A whole frame carrying `word`, with every field any case needs.
        let frame: (String) -> String
        let read: (DoorFrame) -> Reading?
    }

    private static let unseen = "t161_unseen_word"
    /// The core's own word for a word it did not know: the fallback.
    private static let fallbackWord = "unknown"

    private static let row = #""id":"s","title":"t","status":"idle","working":false,"turns":1"#
    /// Both a permission request and an elicitation: each reader takes its keys.
    private static let request = #"{"request_id":"r","call_id":"c","tool":"t","kind":"execute","options":[],"message":"m","mode":{"type":"url","url":"u"}}"#

    private static let vocabularies: [Vocabulary] = [
        Vocabulary(
            name: "Response",
            named: ["hello", "transcript", "ok", "queued", "environments", "sessions", "session", "error", "attachment", "attachment_bytes",
                    "projects", "worktree_layout", "harness_config", "readiness", "session_started"],
            mirrorsAll: false,
            frame: { #"{"id":1,"response":{"type":"\#($0)","version":"0","build":"0","items":[],"queue_id":"q","environments":[],"sessions":[],"session":{\#(row)},"code":"c","message":"m","attachment":{"path":"/p","name":"p","kind":"file","size":null},"bytes_b64":"","session_id":"s"}}"# },
            read: { frame in
                guard case .reply(1, let response) = frame else { return nil }
                if case .other = response { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "Event",
            named: ["said", "session", "lagged"],
            mirrorsAll: false,
            frame: { #"{"event":{"type":"\#($0)","session_id":"s","turn":1,"said":{"kind":"text"},"session":{\#(row)},"missed":1}}"# },
            read: { frame in
                guard case .event(let event) = frame else { return nil }
                if case .other = event { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "Said",
            named: ["text", "tool_call", "asked", "answered", "unanswered", "elicited", "elicitation_decided", "turn_ended", "heard", "handoff", "continued", "compaction", "lagged"],
            mirrorsAll: false,
            frame: { #"{"event":{"type":"said","session_id":"s","turn":2,"said":{"kind":"\#($0)","id":"i","status":"in_progress","request":\#(request)}}}"# },
            read: { frame in
                guard case .event(.said("s", 2, let said)) = frame else { return nil }
                if case .other = said { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "Attention",
            named: ["approval", "elicitation", "question"],
            mirrorsAll: false,
            frame: { #"{"event":{"type":"session","session":{\#(row),"attention":{"type":"\#($0)","question_id":"q","request":\#(request)}}}}"# },
            read: { frame in
                guard case .event(.session(let row)) = frame, row.title == "t", let attention = row.attention else { return nil }
                if case .other = attention { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "ElicitationMode",
            named: ["url", "form"],
            mirrorsAll: true,
            frame: { #"{"event":{"type":"said","session_id":"s","turn":2,"said":{"kind":"elicited","request":{"request_id":"r","message":"m","mode":{"type":"\#($0)","url":"u","fields":[]}}}}}"# },
            read: { frame in
                guard case .event(.said(_, _, .elicited(let asked))) = frame, asked.message == "m" else { return nil }
                if case .other = asked.mode { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "FieldKind",
            named: ["text", "number", "boolean", "choice", "multi_choice"],
            mirrorsAll: true,
            frame: { #"{"event":{"type":"said","session_id":"s","turn":2,"said":{"kind":"elicited","request":{"request_id":"r","message":"m","mode":{"type":"form","fields":[{"name":"f","kind":{"type":"\#($0)"}}]}}}}}"# },
            read: { frame in
                guard case .event(.said(_, _, .elicited(let asked))) = frame,
                      case .form(_, let fields) = asked.mode, let field = fields.first, field.name == "f" else { return nil }
                if case .unknown = field.kind { return .fallback }
                return .known
            }
        ),
        Vocabulary(
            name: "AttachmentKind",
            named: ["image", "file", "folder"],
            mirrorsAll: true,
            frame: { #"{"id":1,"response":{"type":"attachment","attachment":{"path":"/p","name":"p","kind":"\#($0)","size":null}}}"# },
            read: { frame in
                guard case .reply(1, .attachment(let ref)) = frame, ref.path == "/p" else { return nil }
                return ref.kind == .unknown ? .fallback : .known
            }
        ),
    ]

    /// The words released.json lists for `type`: each `<type>.<word>` that is a `variant`.
    private func released(_ type: String, in shape: [String: String]) -> Set<String> {
        Set(shape.compactMap { key, value in
            let parts = key.split(separator: ".", omittingEmptySubsequences: false)
            return parts.count == 2 && parts[0] == type && value == "variant" ? String(parts[1]) : nil
        })
    }

    private func reading(_ vocabulary: Vocabulary, _ word: String) -> Reading? {
        guard let frame = try? DoorFrame(vocabulary.frame(word)) else { return nil }
        return vocabulary.read(frame)
    }

    func testEveryVocabularyIsStillReleased() throws {
        let shape = try Fixture.released()
        for vocabulary in Self.vocabularies {
            XCTAssertFalse(released(vocabulary.name, in: shape).isEmpty, "\(vocabulary.name) is gone from released.json: renamed? Follow it here and in DoorWire.swift")
        }
    }

    func testThePhoneNamesOnlyReleasedWordsAndEveryWordWhereItMirrorsAll() throws {
        let shape = try Fixture.released()
        for vocabulary in Self.vocabularies {
            let words = released(vocabulary.name, in: shape)
            XCTAssertEqual(vocabulary.named.subtracting(words), [], "\(vocabulary.name): the phone names words released.json does not")
            if vocabulary.mirrorsAll {
                XCTAssertEqual(words.subtracting(vocabulary.named).subtracting([Self.fallbackWord]), [], "\(vocabulary.name): released words with no case in DoorWire.swift")
            }
        }
    }

    func testEveryReleasedWordReadsAsItsCaseOrTheFallback() throws {
        let shape = try Fixture.released()
        for vocabulary in Self.vocabularies {
            for word in released(vocabulary.name, in: shape).sorted() {
                let expected: Reading = vocabulary.named.contains(word) ? .known : .fallback
                XCTAssertEqual(reading(vocabulary, word), expected, "\(vocabulary.name).\(word)")
            }
        }
    }

    func testAnUnseenWordReadsAsTheFallbackAndTheFrameStillReads() {
        for vocabulary in Self.vocabularies {
            XCTAssertEqual(reading(vocabulary, Self.unseen), .fallback, vocabulary.name)
        }
    }
}
