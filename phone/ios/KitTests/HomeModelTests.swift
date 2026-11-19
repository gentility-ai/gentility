import XCTest
@testable import DeskPhoneKit

@MainActor
final class HomeModelTests: XCTestCase {
    private var lobby: FakeLobby!

    override func setUp() async throws {
        lobby = FakeLobby()
    }

    private func home(sleep: @escaping @Sendable (Duration) async throws -> Void = { _ in throw CancellationError() }) -> HomeModel {
        HomeModel(lobby: lobby, sleep: sleep, now: { WireDate.parse(Build.now)! })
    }

    private func install(_ id: String, port: Int?, presence: String) -> JSONValue {
        .object(["desk_machine_id": .string(id), "door_port": port.map { .number(Double($0)) } ?? .null,
                 "desk_presence": .string(presence)])
    }

    func testSiblingInstallsUseTheirOwnPortsAndPresence() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "First", mac: "m-first", host: "willow"),
            try Build.row("s-2", title: "Second", mac: "m-second", host: "willow"),
        ], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
            try Build.machine(desk: "m-first", deskPresence: "offline", installs: [
                install("m-first", port: 6446, presence: "offline"),
                install("m-second", port: 6447, presence: "online"),
            ]),
        ], now: Build.now))
        let home = home()
        await home.refresh()
        let first = try XCTUnwrap(home.rows.first { $0.id == "s-1" })
        let second = try XCTUnwrap(home.rows.first { $0.id == "s-2" })
        XCTAssertEqual(home.target(for: first)?.port, 6446)
        XCTAssertEqual(home.target(for: second)?.port, 6447)
        XCTAssertFalse(first.deskOnline)
        XCTAssertTrue(second.deskOnline)
        XCTAssertTrue(second.live)
    }

    func testRecordedSiblingReplySelectsEachInstall() async throws {
        let reply: MachinesReply = try Fixture.response("work_sessions/machines_two_installs").decode()
        let node = try XCTUnwrap(reply.machines.first)
        let installs = try XCTUnwrap(node.deskInstalls)
        XCTAssertEqual(installs.count, 2)
        lobby.machineReplies["o-1"] = .success(reply)
        lobby.lists["o-1"] = [LobbyList(sessions: try installs.enumerated().map { index, install in
            try Build.row("recorded-\(index)", title: "Recorded", mac: install.deskMachineID, host: node.name)
        }, now: reply.now)]
        let home = home()
        await home.refresh()
        for (index, install) in installs.enumerated() {
            let row = try XCTUnwrap(home.rows.first { $0.id == "recorded-\(index)" })
            XCTAssertEqual(home.target(for: row)?.port, install.doorPort)
            XCTAssertEqual(row.deskOnline, install.deskPresence == "online")
        }
    }

    func testAuthoritativeEmptyInstallListDoesNotUseLegacyDoor() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-first", host: "willow")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
            try Build.machine(desk: "m-first", deskPresence: "online", installs: []),
        ], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertNil(home.target(for: home.rows[0]))
        XCTAssertFalse(home.rows[0].deskOnline)
        XCTAssertFalse(home.rows[0].live)
    }

    func testSiblingWithoutApprovedPortNeverBorrowsTheLegacyService() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-second", host: "willow")], now: Build.now)]
        for presence in ["online", "offline", "future_presence"] {
            lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
                try Build.machine(desk: "m-first", deskPresence: "online", installs: [
                    install("m-first", port: 6446, presence: "online"),
                    install("m-second", port: nil, presence: presence),
                ]),
            ], now: Build.now))
            let home = home()
            await home.refresh()
            let row = home.rows[0]
            XCTAssertNil(home.target(for: row))
            XCTAssertFalse(row.live)
            XCTAssertEqual(row.deskOnline, presence == "online")
            if presence == "online" {
                XCTAssertEqual(home.reach(for: row), .notOpen)
                XCTAssertNotEqual(home.reach(for: row), .linkUnavailable, "An unopened install is not a failed network link")
                XCTAssertNotEqual(home.reach(for: row), .none, "An online install is not asleep")
            } else {
                XCTAssertEqual(home.reach(for: row), .none)
            }
        }
    }

    func testWithdrawingSiblingRemovesOnlyItsReachability() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "First", mac: "m-first", host: "willow"),
            try Build.row("s-2", title: "Second", mac: "m-second", host: "willow"),
        ], now: Build.now)]
        let first = install("m-first", port: 6446, presence: "online")
        let home = home()
        for installs in [[first, install("m-second", port: 6447, presence: "online")], [first]] {
            lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-first", installs: installs)], now: Build.now))
            await home.refresh()
            let a = try XCTUnwrap(home.rows.first { $0.id == "s-1" })
            let b = try XCTUnwrap(home.rows.first { $0.id == "s-2" })
            XCTAssertEqual(home.target(for: a)?.port, 6446)
            XCTAssertEqual(home.target(for: b)?.port, installs.count == 2 ? 6447 : nil)
            XCTAssertEqual(b.deskOnline, installs.count == 2)
        }
    }

    func testSiblingPresenceElsewhereMatchesTheSameInstallOnly() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "paper-kites", name: "Paper Kites"), .init(id: "o-2", slug: "northwind", name: "Northwind")]
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-second", host: "willow")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [], now: Build.now))
        for linked in ["m-first", "m-second"] {
            lobby.machineReplies["o-2"] = .success(MachinesReply(machines: [
                try Build.machine(desk: "m-first", installs: [install(linked, port: 6447, presence: "online")]),
            ], now: Build.now))
            let home = home()
            await home.refresh()
            XCTAssertEqual(home.reach(for: home.rows[0]), linked == "m-second" ? .noDoor(organization: "paper-kites") : .none)
        }
    }

    func testRevokedOrganizationDropsCachedRowsWithoutDroppingOtherOrganizations() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "Gone", mac: nil, host: nil)], now: Build.now)]
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-2", title: "Kept", mac: nil, host: nil)], now: Build.now)]
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.count, 2)
        lobby.listErrors["o-1"] = LobbyError.refused("forbidden")
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.title), ["Kept"])
    }

    func testCredentialRevocationDuringListReachesSignedOutHandling() async {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        let home = home()
        var signedOut = 0
        home.onUnauthorized = { signedOut += 1 }
        lobby.listErrors["o-1"] = LobbyError.unauthorized
        await home.refresh()
        XCTAssertEqual(signedOut, 1)
        XCTAssertNil(lobby.sinces["o-2"])
        XCTAssertNil(home.line)
    }

    func testCredentialRevocationDuringInboxDoesNotPublishARefusalLine() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "Thread", mac: nil, host: nil)], now: Build.now)]
        let home = home()
        await home.refresh()
        var signedOut = 0
        home.onUnauthorized = { signedOut += 1 }
        lobby.inboxResults = [.failure(.unauthorized)]
        await home.act(.settle, on: home.rows[0])
        XCTAssertEqual(signedOut, 1)
        XCTAssertNil(home.line)
    }

    func testRowsAreFlatNewestFirstAndCarryTheirMac() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Billing precision", mac: "m-cedar", host: "cedar", lastEventAt: "2025-12-31T09:00:00.000000Z"),
            try Build.row("s-2", title: "Flaky store test", mac: "m-mbp", host: "mbp", lastEventAt: "2026-01-01T11:46:00.000000Z", attention: "question"),
            try Build.row("s-3", title: "Phone pairing spec", mac: "m-mbp", host: "mbp", lastEventAt: "2026-01-01T11:58:00.000000Z", working: true),
        ], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
            try Build.machine(desk: "m-mbp"),
            try Build.machine(desk: "m-cedar", address: "fd06:6e7:0:2::5", seen: "2026-01-01T10:00:00.000000Z"),
            try Build.phone(),
        ], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.title), ["Phone pairing spec", "Flaky store test", "Billing precision"])
        XCTAssertEqual(home.rows.map(\.statusLine), ["working · 2 min · mbp", "waiting on you · 14 min · mbp", "idle · yesterday · cedar"])
        XCTAssertEqual(home.rows.map(\.live), [true, true, false])
    }

    func testARowCarriesItsShelfFields() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp", settledAt: "2026-01-01T10:00:00.000000Z",
                          snoozedUntil: "2026-01-02T10:00:00.000000Z", branch: "feat-x", repository: "github.com/sampleco/gentility", looper: "l-1"),
        ], now: Build.now)]
        let home = home()
        await home.refresh()
        let row = try XCTUnwrap(home.rows.first)
        XCTAssertEqual(row.settledAt, WireDate.parse("2026-01-01T10:00:00.000000Z"))
        XCTAssertEqual(row.snoozedUntil, WireDate.parse("2026-01-02T10:00:00.000000Z"))
        XCTAssertEqual(row.branch, "feat-x")
        XCTAssertEqual(row.project, "sampleco/gentility")
        XCTAssertTrue(row.looper)
    }

    func testAMacWhoseDoorIsOnlyDeclaredIsAsleep() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp", door: "declared"), try Build.phone()], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.first?.live, false)
        XCTAssertNil(home.target(for: home.rows[0]), "a declared door has no approved port to dial")
    }

    func testALiveMacIsDialledOnItsAddressInTheRowsOrganisation() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-9", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp", address: "fd06:6e7:0:1::4"), try Build.phone()], now: Build.now))
        lobby.machineReplies["o-2"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp", address: "fd06:6e7:0:2::4", port: 7000)], now: Build.now))
        let home = home()
        await home.refresh()
        let target = try XCTUnwrap(home.target(for: home.rows[0]))
        XCTAssertEqual(target, DoorTarget(address: "fd06:6e7:0:2::4", port: 7000, macLabel: "mbp", live: true))
    }

    /// Review focus 3.
    func testAnOrganisationWithNoMacsListsItsRowsAsleep() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-old", host: "old-mac")], now: Build.now)]
        lobby.machineReplies["o-2"] = .success(MachinesReply(machines: [], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.macLabel), ["old-mac"])
        XCTAssertEqual(home.rows.map(\.live), [false])
        XCTAssertNil(home.target(for: home.rows[0]))
    }

    func testMachinesRefusedStillListsTheRows() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-2"] = .failure(LobbyError.refused("forbidden"))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.macLabel), ["mbp"])
        XCTAssertEqual(home.rows.map(\.live), [false])
    }

    /// A refused `machines` is not asked again
    /// every minute, only when Home appears again.
    func testAMachinesRefusalIsRememberedUntilHomeAppearsAgain() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-2"] = .failure(LobbyError.refused("forbidden"))
        let home = home()
        await home.refresh()
        await home.refresh()
        XCTAssertEqual(lobby.machinesAsked["o-2"], 1, "a refusal is not asked again on the minute")
        XCTAssertEqual(lobby.machinesAsked["o-1"], 2)
        XCTAssertEqual(home.rows.map(\.macLabel), ["mbp"])
        XCTAssertEqual(home.rows.map(\.live), [false])
        lobby.machineReplies["o-2"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp")], now: Build.now))
        home.appear()
        try await until { self.lobby.machinesAsked["o-2"] == 2 }
        try await until { home.rows.map(\.live) == [true] }
        home.disappear()
    }

    func testAMachinesAnswerThatDidNotArriveIsAskedAgainOnTheMinute() async throws {
        lobby.machineReplies["o-1"] = .failure(LobbyError.unreachable("offline"))
        let home = home()
        await home.refresh()
        await home.refresh()
        XCTAssertEqual(lobby.machinesAsked["o-1"], 2)
    }

    /// Ties fall to the id, so the order holds
    /// from one refresh to the next.
    func testRowsThatTieAreOrderedByID() async throws {
        let ids = ["s-f", "s-c", "s-a", "s-e", "s-b", "s-d"]
        lobby.lists["o-1"] = [LobbyList(
            sessions: try ids.map { try Build.row($0, title: $0, mac: "m-mbp", host: "mbp") },
            now: Build.now
        )]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp"), try Build.phone()], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.id), ids.sorted())
    }

    func testNothingAnywhereIsAnEmptyHome() async throws {
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertTrue(home.loaded)
        XCTAssertTrue(home.isEmpty)
        XCTAssertNil(home.line)
    }

    func testAnotherPersonsRowsAreNotListed() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Mine", mac: "m-mbp", host: "mbp"),
            try Build.row("s-2", title: "A teammate's", mac: "m-theirs", host: "their-mac", owner: "u-teammate"),
        ], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp"), try Build.phone()], now: Build.now))
        var people: [String] = []
        let home = home()
        home.onPerson = { people.append($0) }
        await home.refresh()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.title), ["Mine"])
        XCTAssertEqual(people, [Build.morgan], "the person is told once, from /oauth/organizations")
    }

    func testSinceIsTheLastNowAndAnArchivedRowLeaves() async throws {
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp"), try Build.phone()], now: Build.now))
        lobby.lists["o-1"] = [
            LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: "2026-01-01T12:00:00.000000Z"),
            LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp", archivedAt: Build.now)], now: "2026-01-01T12:01:00.000000Z"),
        ]
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.count, 1)
        await home.refresh()
        XCTAssertEqual(lobby.sinces["o-1"], [nil, "2026-01-01T12:00:00.000000Z"])
        XCTAssertTrue(home.rows.isEmpty)
    }

    func testAnOrganisationThePersonLeftTakesItsRowsAndItsSinceWithIt() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.lists["o-2"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.count, 1)
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A")]
        await home.refresh()
        XCTAssertTrue(home.rows.isEmpty)
        lobby.organizations = [.init(id: "o-1", slug: "a", name: "A"), .init(id: "o-2", slug: "b", name: "B")]
        await home.refresh()
        XCTAssertEqual(lobby.sinces["o-2"], [nil, nil], "back again, it is read whole")
    }

    func testARowOnNoMacIsListedAsleepUnderAnotherMac() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: nil, host: nil)], now: Build.now)]
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.rows.map(\.macLabel), ["Another Mac"])
        XCTAssertEqual(home.rows.map(\.live), [false])
        XCTAssertNil(home.target(for: home.rows[0]))
    }

    func testARefusedBearerCallsOnUnauthorized() async {
        lobby.unauthorized = true
        var told = false
        let home = home()
        home.onUnauthorized = { told = true }
        await home.refresh()
        XCTAssertTrue(told)
    }

    func testAServerThatDoesNotAnswerKeepsWhatHomeHadAndSaysSo() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp"), try Build.phone()], now: Build.now))
        let home = home()
        await home.refresh()
        lobby.unreachableNext = true
        await home.refresh()
        XCTAssertEqual(home.rows.count, 1)
        XCTAssertEqual(home.line, "Can't reach the server. Showing what it last said.")
    }

    /// The organisations answered over HTTP, but the socket did not: Home
    /// is not empty, it is out of reach.
    func testALobbyThatDoesNotAnswerIsNotAnEmptyHome() async throws {
        lobby.listErrors["o-1"] = LobbyError.unreachable("closed")
        let home = home()
        await home.refresh()
        XCTAssertFalse(home.isEmpty)
        XCTAssertEqual(home.line, "Can't reach the server. Showing what it last said.")
        lobby.listErrors = [:]
        await home.refresh()
        XCTAssertNil(home.line)
        XCTAssertTrue(home.isEmpty)
    }

    func testHomeRefreshesOnAppearAndEveryMinuteWhileVisible() async throws {
        let ticks = Ticks()
        let home = home(sleep: { _ in try await ticks.wait() })
        home.appear()
        try await ticks.untilWaiting(1)
        XCTAssertEqual(lobby.refreshes, 1, "once on appear")
        await ticks.tick()
        try await ticks.untilWaiting(2)
        XCTAssertEqual(lobby.refreshes, 2, "again a minute later")
        home.disappear()
        await ticks.tick()
        try await Task.sleep(for: .milliseconds(50))
        XCTAssertEqual(lobby.refreshes, 2, "not while hidden")
    }

    func testAWaitingRowSaysWhatItWaitsOn() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Fix the recap timing", mac: "m-mbp", host: "mbp", attention: "approval", attentionLine: "Wants to execute"),
            try Build.row("s-2", title: "Port band", mac: "m-mbp", host: "mbp"),
        ], now: Build.now)]
        let home = home()
        await home.refresh()
        let rows = home.rows
        XCTAssertEqual(rows.map(\.waiting), [true, false])
        XCTAssertEqual(rows.map(\.attentionLine), ["Wants to execute", nil])
    }

    /// The star and the role, as the list carries them.
    func testARowCarriesItsStarAndItsKind() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Programme", mac: "m-mbp", host: "mbp", starredAt: "2026-01-01T10:00:00.000000Z", role: "coordinator"),
            try Build.row("s-2", title: "Sorter #4", mac: "m-mbp", host: "mbp", lastEventAt: "2026-01-01T11:00:00.000000Z", looper: "l-1", role: .some(nil)),
            try Build.row("s-3", title: "Old server", mac: "m-mbp", host: "mbp", lastEventAt: "2026-01-01T10:00:00.000000Z"),
        ], now: Build.now)]
        let home = home()
        await home.refresh()
        let rows = Dictionary(uniqueKeysWithValues: home.rows.map { ($0.id, $0) })
        XCTAssertEqual(rows["s-1"]?.kind, .coordinator)
        XCTAssertEqual(rows["s-1"]?.starred, true)
        XCTAssertEqual(rows["s-2"]?.kind, .looper)
        XCTAssertEqual(rows["s-2"]?.starred, false)
        XCTAssertEqual(rows["s-3"]?.kind, .thread, "no tags at all is a thread")
    }

    /// The server inbox's tag, as the list carries it.
    func testATaggedRowCarriesWhoItWaitsOnAndWhy() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [
            try Build.row("s-1", title: "Fix the recap timing", mac: "m-mbp", host: "mbp", waitingOn: "person", waitingReason: "Which branch should the fix go on?"),
            try Build.row("s-2", title: "Port band", mac: "m-mbp", host: "mbp", lastEventAt: "2026-01-01T11:00:00.000000Z"),
        ], now: Build.now)]
        let home = home()
        await home.refresh()
        let rows = home.rows
        XCTAssertEqual(rows.map(\.waitingOn), ["person", nil])
        XCTAssertEqual(rows.map(\.waitingReason), ["Which branch should the fix go on?", nil])
        XCTAssertEqual(rows.map(\.waitsOnYou), [true, false])
        XCTAssertEqual(rows.map(\.waiting), [false, false], "a tag is no ask: it can still be snoozed")
        XCTAssertEqual(rows.map(\.waitingLine), ["Which branch should the fix go on?", nil])
    }

    func testARowIsFoundByItsOrganisationAndSessionAndListingSaysSo() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        let home = home()
        var listed = 0
        home.onListed = { listed += 1 }
        XCTAssertNil(home.row(organizationID: "o-1", sessionID: "s-1"), "nothing before the first list")
        await home.refresh()
        XCTAssertEqual(home.row(organizationID: "o-1", sessionID: "s-1")?.title, "T")
        XCTAssertNil(home.row(organizationID: "o-2", sessionID: "s-1"))
        XCTAssertEqual(listed, 1)
    }

    /// B6: the Mac is live in another organisation's machines and absent
    /// from the row's: it has no door here, and the row says so.
    func testALiveMacAbsentFromTheRowsOrganisationHasNoDoorThere() async throws {
        lobby.organizations = [.init(id: "o-x", slug: "northwind", name: "northwind"), .init(id: "o-e", slug: "paper-kites", name: "Paper Kites")]
        lobby.lists["o-e"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-willow", host: "driftwood")], now: Build.now)]
        lobby.machineReplies["o-x"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-willow", address: "fd06:6e7:0:1::7")], now: Build.now))
        lobby.machineReplies["o-e"] = .success(MachinesReply(machines: [try Build.phone()], now: Build.now))
        let home = home()
        await home.refresh()
        let row = home.rows[0]
        XCTAssertNil(home.target(for: row))
        XCTAssertEqual(home.reach(for: row), .noDoor(organization: "paper-kites"))
    }

    func testAMacSeenNowhereIsAsleepNotDoorless() async throws {
        lobby.organizations = [.init(id: "o-x", slug: "northwind", name: "northwind"), .init(id: "o-e", slug: "paper-kites", name: "Paper Kites")]
        lobby.lists["o-e"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-willow", host: "driftwood")], now: Build.now)]
        lobby.machineReplies["o-x"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-willow", address: "fd06:6e7:0:1::7", seen: "2026-01-01T10:00:00.000000Z")], now: Build.now))
        lobby.machineReplies["o-e"] = .success(MachinesReply(machines: [], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.reach(for: home.rows[0]), .none, "seen two hours ago elsewhere is not live")
    }

    /// The row's organisation's machines went unread: absence there is not
    /// known, so a Mac up elsewhere is asleep here, not doorless.
    func testAMacUpElsewhereIsAsleepWhileTheRowsMachinesAreUnread() async throws {
        lobby.organizations = [.init(id: "o-x", slug: "northwind", name: "northwind"), .init(id: "o-e", slug: "paper-kites", name: "Paper Kites")]
        lobby.lists["o-e"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-willow", host: "driftwood")], now: Build.now)]
        lobby.machineReplies["o-x"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-willow", address: "fd06:6e7:0:1::7")], now: Build.now))
        lobby.machineReplies["o-e"] = .failure(LobbyError.unreachable("offline"))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.reach(for: home.rows[0]), .none)
    }

    /// Review focus 4.
    func testAMacLinkedHereWithADeclaredDoorIsStillAsleepNotDoorless() async throws {
        lobby.organizations = [.init(id: "o-1", slug: "northwind", name: "Northwind"), .init(id: "o-2", slug: "b", name: "B")]
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp", door: "declared"), try Build.phone()], now: Build.now))
        lobby.machineReplies["o-2"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp", address: "fd06:6e7:0:3::4")], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.reach(for: home.rows[0]), .none)
    }

    func testALiveDoorReachesAsADoor() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [try Build.machine(desk: "m-mbp")], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertEqual(home.reach(for: home.rows[0]), .door(DoorTarget(address: "fd06:6e7:0:2::4", port: 6446, macLabel: "mbp", live: true)))
    }

    func testDeskEvidenceSurvivesStale6netAndDoesNotMakeTheTargetLive() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        for presence: String? in ["online", "offline", "future_presence", nil] {
            lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
                try Build.machine(desk: "m-mbp", seen: "2026-01-01T10:00:00.000000Z", deskPresence: presence),
            ], now: Build.now))
            let home = home()
            await home.refresh()
            let target = try XCTUnwrap(home.target(for: home.rows[0]))
            XCTAssertFalse(target.live)
            XCTAssertEqual(home.reach(for: home.rows[0]).deskOnline, presence == "online")
            XCTAssertEqual(target.deskPresence, presence == "future_presence" ? nil : presence)
        }
    }

    func testOnlineDeskWithoutAnApprovedTargetIsNotAsleep() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
            try Build.machine(desk: "m-mbp", seen: nil, door: nil, deskPresence: "online"),
        ], now: Build.now))
        let home = home()
        await home.refresh()
        XCTAssertNil(home.target(for: home.rows[0]))
        XCTAssertEqual(home.reach(for: home.rows[0]), .linkUnavailable)
    }

    func testHomeDoesNotCallOnlineDeskAsleepWhen6netIsDown() async throws {
        lobby.lists["o-1"] = [LobbyList(sessions: [try Build.row("s-1", title: "T", mac: "m-mbp", host: "mbp")], now: Build.now)]
        for presence: String? in ["online", "offline", "future_presence", nil] {
            lobby.machineReplies["o-1"] = .success(MachinesReply(machines: [
                try Build.machine(desk: "m-mbp", seen: nil, deskPresence: presence),
            ], now: Build.now))
            let home = home()
            await home.refresh()
            let row = try XCTUnwrap(home.rows.first)
            XCTAssertFalse(row.live)
            XCTAssertEqual(row.deskOnline, presence == "online")
            let face = RowFace(row, now: WireDate.parse(Build.now)!, showsMacName: true)
            XCTAssertEqual(face.mac.asleep, presence != "online")
            XCTAssertEqual(face.recedes, presence != "online")
            XCTAssertEqual(face.accessibilityLabel.contains("mbp, asleep"), presence != "online")
            XCTAssertEqual(row.parked(settledAt: nil, snoozedUntil: nil).deskOnline, row.deskOnline)
        }
    }
}

/// A clock the test moves by hand.
actor Ticks {
    private var waiters: [CheckedContinuation<Void, Error>] = []
    /// How many waits have begun.
    private(set) var waited = 0

    func wait() async throws {
        waited += 1
        try await withCheckedThrowingContinuation { waiters.append($0) }
        try Task.checkCancellation()
    }

    func tick() {
        let now = waiters
        waiters = []
        now.forEach { $0.resume() }
    }

    func untilWaiting(_ count: Int) async throws {
        for _ in 0..<200 where waited < count { try await Task.sleep(for: .milliseconds(5)) }
        guard waited >= count else { throw CancellationError() }
    }
}
