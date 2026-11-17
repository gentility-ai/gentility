import Foundation
@testable import DeskPhoneKit

/// The recorded protocol, copied into this bundle at build time from
/// `contract/` (project.yml, "Copy the recorded protocol").
enum Fixture {
    private final class Marker {}

    static var root: URL {
        Bundle(for: Marker.self).bundleURL.appendingPathComponent("protocol", isDirectory: true)
    }

    /// One server exchange: `work_sessions/list` is `server/work_sessions/list.json`,
    /// its rows read back whole, or for a refusal its line of
    /// `server/refused.jsonl`, `name` taken off.
    static func server(_ name: String) throws -> JSONValue {
        let file = root.appendingPathComponent("server/\(name).json")
        if FileManager.default.fileExists(atPath: file.path) {
            return try expand(load(file))
        }
        let text = String(decoding: try Data(contentsOf: root.appendingPathComponent("server/refused.jsonl")), as: UTF8.self)
        for line in text.split(separator: "\n") {
            guard case .object(var exchange) = try JSONValue.parse(Data(line.utf8)),
                  exchange["name"]?.string == name else { continue }
            exchange.removeValue(forKey: "name")
            return .object(exchange)
        }
        throw FixtureError.missing("\(name).json")
    }

    /// `value` with each session row whole: the recorder writes a row as
    /// `{"$row": kind}` and the fields where it differs from the canonical
    /// row `server/rows/<kind>.json`.
    static func expand(_ value: JSONValue) throws -> JSONValue {
        switch value {
        case .object(var fields):
            guard let kind = fields.removeValue(forKey: "$row") else {
                return .object(try fields.mapValues(expand))
            }
            guard let name = kind.string,
                  case .object(let canonical) = try load(root.appendingPathComponent("server/rows/\(name).json")) else {
                throw FixtureError.missing("rows/\(kind).json")
            }
            return .object(canonical.merging(fields) { _, written in written })
        case .array(let items):
            return .array(try items.map(expand))
        default:
            return value
        }
    }

    /// The reply's `response` of a server exchange.
    static func response(_ name: String) throws -> JSONValue {
        guard let response = try server(name)["reply"]?["response"] else {
            throw FixtureError.noReply(name)
        }
        return response
    }

    /// One recorded frame: `hello` is `door/hello.json`.
    static func door(_ name: String) throws -> JSONValue {
        try load(root.appendingPathComponent("door/\(name).json"))
    }

    /// The released protocol's shape (`released.json`): `<Type>.<word>` is
    /// `"variant"` for each word a vocabulary has.
    static func released() throws -> [String: String] {
        guard case .object(let shape)? = try load(root.appendingPathComponent("released.json"))["shape"] else {
            throw FixtureError.missing("released.json shape")
        }
        return shape.compactMapValues(\.string)
    }

    /// The same frame as the text Go hands Swift.
    static func doorText(_ name: String) throws -> String {
        try door(name).text()
    }

    private static func load(_ url: URL) throws -> JSONValue {
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw FixtureError.missing(url.lastPathComponent)
        }
        return try JSONValue.parse(Data(contentsOf: url))
    }
}

enum FixtureError: Error, Equatable {
    case missing(String)
    case noReply(String)
}
