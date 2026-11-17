import Foundation

/// Any JSON, as the wire carries it. Payloads and replies are held as this
/// until a typed reader takes the part it needs, and the enrolment is handed
/// to Go as this, re-encoded, with every key as the server spelled it.
public enum JSONValue: Equatable, Sendable {
    case null
    case bool(Bool)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])
}

extension JSONValue: Codable {
    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let value = try? container.decode(Bool.self) {
            self = .bool(value)
        } else if let value = try? container.decode(Double.self) {
            self = .number(value)
        } else if let value = try? container.decode(String.self) {
            self = .string(value)
        } else if let value = try? container.decode([JSONValue].self) {
            self = .array(value)
        } else {
            self = .object(try container.decode([String: JSONValue].self))
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .null: try container.encodeNil()
        case .bool(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .object(let value): try container.encode(value)
        }
    }
}

extension JSONValue {
    public subscript(key: String) -> JSONValue? {
        guard case .object(let object) = self else { return nil }
        return object[key]
    }

    public var string: String? {
        guard case .string(let value) = self else { return nil }
        return value
    }

    /// The number as an `Int`, or nil when it is fractional or out of range.
    public var int: Int? {
        guard case .number(let value) = self else { return nil }
        return Int(exactly: value)
    }

    public var number: Double? {
        guard case .number(let value) = self else { return nil }
        return value
    }

    public var bool: Bool? {
        guard case .bool(let value) = self else { return nil }
        return value
    }

    public var array: [JSONValue]? {
        guard case .array(let value) = self else { return nil }
        return value
    }

    /// A typed reader over this value.
    public func decode<T: Decodable>(_ type: T.Type = T.self) throws -> T {
        try Wire.decoder.decode(T.self, from: data())
    }

    public func data() throws -> Data { try Wire.encoder.encode(self) }

    public func text() throws -> String { String(decoding: try data(), as: UTF8.self) }

    public static func parse(_ data: Data) throws -> JSONValue {
        try Wire.decoder.decode(JSONValue.self, from: data)
    }

    public static func parse(_ text: String) throws -> JSONValue {
        try parse(Data(text.utf8))
    }
}

/// The one decoder and encoder. Plain: no key strategy, because a key
/// strategy also rewrites the keys inside a nested `JSONValue`.
public enum Wire {
    public static let decoder = JSONDecoder()
    public static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return encoder
    }()
}
