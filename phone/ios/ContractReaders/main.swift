// What the phone reads each contract message it sends with, run over every
// answer the server's suite recorded for it. Compiled with the phone's wire types,
// `Kit/ServerWire.swift` and `Kit/JSONValue.swift`, which need only
// Foundation, so it runs on Linux in desk-test.yml's core job: the KitTests
// need a Mac, and no job runs them.
//
// It fails when a recorded answer will not decode as the phone decodes it,
// when a message here is not one the contract types, and when a message here
// has no recorded answer read. Exits 1 naming each failure, 2 on bad usage.

import Foundation

guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write(Data("usage: readers <contract/server>\n".utf8))
    exit(2)
}
let root = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)

typealias Read = (JSONValue) throws -> Void
var presenceCompatibilityChecks = 0
var installChecks = 0
var portlessInstallChecks = 0

/// The phone's reader for each message it sends, by contract name.
let readers: [String: Read] = [
    // `Lobby.joined`: the topic's feature words, an absent list read as none.
    "lobby.join": { reply in
        if let features = reply["features"], features.array?.allSatisfy({ $0.string != nil }) != true {
            throw Unread("features is \(try features.text())")
        }
    },
    "sessions.list": { _ = try $0.decode(LobbyList.self) },
    "hosts.machines": { reply in
        let listed = try reply.decode(MachinesReply.self)
        for node in listed.machines {
            for install in node.deskInstalls ?? [] {
                guard let selected = node.install(install.deskMachineID),
                      selected.port == install.doorPort,
                      selected.deskPresence == install.deskPresence else {
                    throw Unread("install selection borrowed another install's port or presence")
                }
                installChecks += 1
                if install.doorPort == nil { portlessInstallChecks += 1 }
            }
            if let installs = node.deskInstalls, installs.isEmpty,
               node.install(node.deskMachineID ?? "absent-install") != nil {
                throw Unread("an empty install list used the legacy service")
            }
        }
        guard case .object(var fields) = reply,
              let machines = fields["machines"]?.array else {
            throw Unread("machines is not an array")
        }
        for word: String? in ["online", "offline", "future_presence", nil] {
            for omit in [false, true] {
                fields["machines"] = .array(try machines.map { machine in
                    guard case .object(var row) = machine else { throw Unread("machine is not an object") }
                    row["desk_presence"] = omit ? nil : word.map(JSONValue.string) ?? .null
                    return .object(row)
                })
                let decoded = try JSONValue.object(fields).decode(MachinesReply.self)
                let expected = !omit && (word == "online" || word == "offline") ? word : nil
                guard decoded.machines.allSatisfy({ $0.deskPresence == expected }) else {
                    throw Unread("Desk presence did not preserve known evidence or ignore missing/unknown evidence")
                }
                presenceCompatibilityChecks += decoded.machines.count
            }
        }
    },
    "inbox.command": { _ = try $0.decode(InboxReply.self) },
    // `Lobby.register` and the join of `phone:<user>` read the status alone.
    "phone.join": { reply in guard case .object = reply else { throw Unread("not an object") } },
    "phone.register": { reply in guard case .object = reply else { throw Unread("not an object") } },
]

struct Unread: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

func load(_ url: URL) throws -> JSONValue {
    try JSONValue.parse(Data(contentsOf: url))
}

/// `value` with each row whole: the recorder writes a row as `{"$row": kind}`
/// and the fields where it differs from the canonical row `rows/<kind>.json`.
func expand(_ value: JSONValue) throws -> JSONValue {
    switch value {
    case .object(var fields):
        guard let kind = fields.removeValue(forKey: "$row") else {
            return .object(try fields.mapValues(expand))
        }
        guard let name = kind.string,
              case .object(let canonical) = try load(root.appendingPathComponent("rows/\(name).json")) else {
            throw Unread("no canonical row \(try kind.text())")
        }
        return .object(canonical.merging(fields) { _, written in written })
    case .array(let items):
        return .array(try items.map(expand))
    default:
        return value
    }
}

var failures: [String] = []

// Each message's Phoenix address, `channel/event`, for the typed ones.
var typed: [String: String] = [:]
for method in try load(root.appendingPathComponent("contract.json"))["methods"]?.array ?? [] {
    guard let name = method["name"]?.string,
          method["x-untyped"]?.bool != true,
          let channel = method["x-phoenix"]?["channel"]?.string,
          let event = method["x-phoenix"]?["event"]?.string else { continue }
    typed["\(channel)/\(event)"] = name
}
let names = Set(typed.values)
for name in readers.keys.sorted() where !names.contains(name) {
    failures.append("\(name): not a typed message of the contract")
}

var read: [String: Int] = [:]
let files = FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil)?
    .compactMap { $0 as? URL }
    .filter { $0.pathExtension == "json" && $0.deletingLastPathComponent().deletingLastPathComponent().standardizedFileURL == root.standardizedFileURL }
    .sorted { $0.path < $1.path } ?? []
for file in files {
    let channel = file.deletingLastPathComponent().lastPathComponent
    let recorded = "\(channel)/\(file.deletingPathExtension().lastPathComponent)"
    let exchange = try expand(load(file))
    guard let event = exchange["event"]?.string ?? (exchange["join"] != nil ? "join" : nil),
          let name = typed["\(channel)/\(event)"],
          let reader = readers[name],
          exchange["reply"]?["status"]?.string == "ok",
          let response = exchange["reply"]?["response"] else { continue }
    do {
        try reader(response)
        read[name, default: 0] += 1
    } catch {
        failures.append("\(recorded), a \(name), does not read: \(error)")
    }
}
for name in readers.keys.sorted() where read[name, default: 0] == 0 {
    failures.append("\(name): no recorded answer was read")
}
if presenceCompatibilityChecks == 0 {
    failures.append("lobby.machines: no machines checked for presence compatibility")
}
if installChecks == 0 || portlessInstallChecks == 0 {
    failures.append("hosts.machines: missing recorded install or null-port selection coverage")
}

if failures.isEmpty {
    for name in readers.keys.sorted() { print("\(name): \(read[name]!) recorded answers read") }
} else {
    for failure in failures { FileHandle.standardError.write(Data("\(failure)\n".utf8)) }
    exit(1)
}
