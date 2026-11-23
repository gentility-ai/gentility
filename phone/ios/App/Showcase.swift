#if DEBUG
import DeskPhoneKit
import SwiftUI

/// A row's long-press menu, drawn still: a menu cannot be opened from
/// state, so this lays the lifted row and its items over a dimmed Home.
private struct HomeMenuShowcase: View {
    let row: HomeRow
    let rows: [HomeRow]

    var body: some View {
        let face = RowFace(row, now: Date(), showsMacName: RowFace.showsMacName(rows))
        ZStack {
            NavigationStack {
                HomeList(rows: rows, line: nil, empty: false, joining: false, notificationsOff: false, preset: HomeListPreset(project: ""))
                    .navigationTitle("Home")
            }
            .blur(radius: 12)
            Color.black.opacity(0.18).ignoresSafeArea()
            VStack(alignment: .leading, spacing: 10) {
                HomeRowView(row: row, face: face)
                    .padding(.horizontal, 16).padding(.vertical, 12)
                    .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 14))
                VStack(spacing: 0) {
                    item("Settle", "checkmark.circle")
                    Divider()
                    item("Snooze", "moon.zzz", more: true)
                    Rectangle().fill(Color(.separator).opacity(0.5)).frame(height: 8)
                    item("Archive", "archivebox", destructive: true)
                }
                .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 14))
                .frame(width: 250)
            }
            .padding(.horizontal, 16)
        }
    }

    private func item(_ title: String, _ symbol: String, more: Bool = false, destructive: Bool = false) -> some View {
        HStack {
            Text(title)
            Spacer()
            if more { Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(.secondary) }
            Image(systemName: symbol)
        }
        .foregroundStyle(destructive ? AnyShapeStyle(.red) : AnyShapeStyle(.primary))
        .padding(.horizontal, 16).padding(.vertical, 12)
    }
}

/// A conversation whose turns land just after it draws, as an open's first
/// read does: the screen follows them to the end, as the app does.
private struct ArrivingTranscript: View {
    let turns: [Turn]
    let acting: Acting
    let strip: StatusStrip?
    @State private var shown: [Turn] = []

    var body: some View {
        NavigationStack(path: .constant(NavigationPath(["s"]))) {
            Color.clear
                .navigationTitle("Home")
                .navigationDestination(for: String.self) { _ in
                    TranscriptScreen(turns: shown, phase: .open, asleepLines: [], acting: acting, strip: strip, onRetry: {})
                        .navigationTitle("Phone pairing spec")
                        .navigationBarTitleDisplayMode(.inline)
                        .task {
                            try? await Task.sleep(for: .milliseconds(300))
                            shown = turns
                        }
                }
        }
    }
}

/// Each screen with canned values, for the pull request's screenshots
/// (`DESK_PHONE_SHOWCASE=<screen>`). Debug builds only; nothing here dials,
/// reads the keychain or opens the camera.
struct Showcase: View {
    static let screens = [
        "scanner", "camera", "number", "joining", "home", "home-siblings", "home-sibling-unavailable", "sibling-not-open", "home-off", "home-search", "home-settled", "home-snooze", "home-undo",
        "home-menu", "home-tip", "home-filtered", "home-waiting", "home-empty", "home-nomatch", "home-loopers", "home-xxl", "home-presence", "conversation", "f4", "f4-waiting", "link-unavailable", "no-door", "lost",
        "idle", "approval", "approval-three", "approval-short", "show-all", "question", "question-tall", "link", "queued", "steer-refused",
        "requested-compacting", "requested-waiting",
        "elsewhere", "redial", "unopened-answer", "wrong-store", "ended", "ended-words",
        "tools", "tools-running", "strip-working", "strip-scrolled", "strip-reconnecting", "strip-reconnecting-tools", "strip-tall", "strip-compacting", "strip-compacting-stop",
        "attach-menu", "attach-tray", "attach-failed", "attach-old-mac",
        "transcript-images", "transcript-markdown", "transcript-streaming", "transcript-long", "transcript-remote-image", "image-viewer",
        "f1", "f2", "f3", "f5", "lapsed", "unjoined", "new-thread", "new-thread-refused", "new-thread-blank", "settings", "settings-off",
    ]

    let screen: String
    @State private var name = "Morgan's iPhone"

    var body: some View {
        switch screen {
        case "scanner": ScannerScreen(state: .scanning, name: $name, camera: false, onCode: { _ in })
        case "camera": ScannerScreen(state: .denied, name: $name, camera: false, onCode: { _ in })
        case "number": NumberScreen(machineLabel: "Morgan's MacBook Pro", match: "47", host: "mcp.gentility.ai")
        case "joining": JoiningScreen(machineLabel: "Morgan's MacBook Pro")
        case "home": home(notificationsOff: false)
        case "sibling-not-open":
            conversation(TranscriptScreen(turns: [], phase: .notOpen,
                asleepLines: [ConversationModel.notOpenLine(machine: Self.siblingRows(secondReachable: false)[1].macLabel),
                              "Showing what the server last heard.", "Status: idle · last turn 3 min"],
                acting: Self.acting(open: false, working: false), onRetry: {}), "Phone pairing spec")
        case "home-siblings", "home-sibling-unavailable": NavigationStack {
            HomeList(rows: Self.siblingRows(secondReachable: screen == "home-siblings"), line: nil, empty: false, joining: false, notificationsOff: false, preset: HomeListPreset(project: ""))
                .navigationTitle("Home")
        }
        case "home-presence": NavigationStack {
            HomeList(rows: Self.presenceRows, line: nil, empty: false, joining: false, notificationsOff: false, preset: HomeListPreset(project: ""))
                .navigationTitle("Home")
        }
        case "home-off": home(notificationsOff: true)
        case "home-search": home(notificationsOff: false, query: "recap")
        case "home-settled": home(notificationsOff: false, open: [.settled])
        case "home-snooze": home(notificationsOff: false, snoozing: Self.rows[2])
        case "home-undo": home(notificationsOff: false, undo: Undo(words: "Snoozed until Tue 09:00", rowID: "s-2", organizationID: "o-1", inverse: .wake))
        case "home-menu": HomeMenuShowcase(row: Self.rows[2], rows: Self.rows)
        case "home-tip": home(notificationsOff: false)
        case "home-filtered": home(notificationsOff: false, preset: HomeListPreset(project: "sampleco/sample-app"))
        case "home-waiting": home(notificationsOff: false, preset: HomeListPreset(project: "", waitingOnly: true))
        case "home-empty": NavigationStack {
            HomeList(rows: [], line: nil, empty: true, joining: false, notificationsOff: false, preset: HomeListPreset(project: ""))
                .navigationTitle("Home")
        }
        case "home-nomatch": home(notificationsOff: false, query: "zebra")
        case "home-starred": home(notificationsOff: false, preset: HomeListPreset(project: "", hidden: [], starredOnly: true))
        case "home-working": home(notificationsOff: false, preset: HomeListPreset(project: "", hidden: [], workingOnly: true))
        case "home-loopers": home(notificationsOff: false, preset: HomeListPreset(project: "", hidden: []))
        case "home-xxl": home(notificationsOff: false).dynamicTypeSize(.accessibility2)
        case "conversation": conversation(Self.acting(working: true), "Phone pairing spec")
        case "idle": conversation(Self.acting(working: false), "Phone pairing spec")
        case "f4": conversation(TranscriptScreen(turns: [], phase: .asleep(macLabel: "cedar"), asleepLines: Self.asleep, acting: Self.acting(open: false, working: false), onRetry: {}), "Billing precision")
        case "link-unavailable": conversation(TranscriptScreen(turns: [], phase: .linkUnavailable(macLabel: "Morgan's MacBook Pro in the upstairs study"), asleepLines: Self.linkUnavailable, acting: Self.acting(open: false, working: false), onRetry: {}), "Billing precision")
        case "f4-waiting": conversation(TranscriptScreen(turns: [], phase: .asleep(macLabel: "cedar"), asleepLines: Self.asleep + ["Waiting on you: Wants to execute"], acting: Self.acting(open: false, working: false), onRetry: {}), "Billing precision")
        case "no-door": conversation(TranscriptScreen(turns: [], phase: .noDoor(organization: "paper-kites"), asleepLines: Self.noDoor, acting: Self.acting(open: false, working: false), onRetry: {}), "Billing precision")
        case "lost": conversation(TranscriptScreen(turns: Self.turns, phase: .lost("The link to Morgan's MacBook Pro dropped."), asleepLines: [], acting: Self.acting(open: false, working: false), onRetry: {}), "Phone pairing spec")
        case "approval": conversation(Self.acting(working: true, card: Card(ask: .approval(Self.approval)), detail: Self.command), "Fix the recap timing")
        case "approval-three": conversation(Self.acting(working: true, card: Card(ask: .approval(Self.approval)), detail: Self.threeLines), "Fix the recap timing")
        case "approval-short": conversation(Self.acting(working: true, card: Card(ask: .approval(Self.approval)), detail: "cargo test -p desk_core"), "Fix the recap timing")
        case "show-all": conversation(Self.acting(working: true, card: Card(ask: .approval(Self.approval)), detail: Self.command), "Fix the recap timing")
            .sheet(isPresented: .constant(true)) { WholeDetail(heading: Self.approval.summary, detail: Self.command) }
        case "question": conversation(Self.acting(working: true, card: Card(ask: .elicitation(Self.form))), "Fix the recap timing")
        case "question-tall": conversation(Self.acting(working: true, card: Card(ask: .elicitation(Self.tall))), "Fix the recap timing")
        case "link": conversation(Self.acting(working: true, card: Card(ask: .elicitation(Self.link))), "Connect Linear")
        case "queued": conversation(Self.acting(working: true, pending: [PendingLine(queueID: "q", text: "then run the store tests")]), "Phone pairing spec")
        case "steer-refused": conversation(Self.acting(working: true, pending: [PendingLine(queueID: "q", text: "stop and run the tests", note: PendingLines.steerRefused)]), "Phone pairing spec")
        case "requested-compacting": conversation(TranscriptScreen(turns: Self.turns, phase: .open, asleepLines: [], acting: Self.acting(working: true),
                                                                   requested: RequestedPrompt(id: "p", text: "now wire the pairing screen to the door", compacting: true), onRetry: {}), "Phone pairing spec")
        case "requested-waiting": conversation(TranscriptScreen(turns: Self.turns, phase: .open, asleepLines: [], acting: Self.acting(working: false),
                                                                requested: RequestedPrompt(id: "p", text: "now wire the pairing screen to the door", attachments: 1, compacting: false), onRetry: {}), "Phone pairing spec")
        case "elsewhere": conversation(Self.acting(working: true, cleared: CardFold.answeredElsewhere), "Fix the recap timing")
        case "redial": conversation(TranscriptScreen(turns: Self.turns, phase: .lost("The link to Morgan's MacBook Pro dropped."), asleepLines: [], acting: Self.acting(open: false, working: true, card: Self.owed, detail: Self.command), onRetry: {}), "Fix the recap timing")
        case "unopened-answer":
            conversation(TranscriptScreen(turns: [], phase: .notOpen,
                asleepLines: [ConversationModel.notOpenLine(machine: "Morgan's MacBook Pro"), "Showing what the server last heard.", "Status: idle · last turn 3 min"],
                acting: Self.acting(open: false, working: false, card: Self.owedToUnopened, detail: Self.threeLines), onRetry: {}), "Fix the recap timing")
        case "wrong-store":
            conversation(TranscriptScreen(turns: [], phase: .refused(ConversationModel.wrongStoreLine(machine: "Morgan's MacBook Pro")), asleepLines: [], acting: Self.acting(open: false, working: false), onRetry: {}), "Phone pairing spec")
        case "strip-working":
            conversation(TranscriptScreen(turns: Self.turns, phase: .open, asleepLines: [],
                                          acting: Self.acting(working: true, pending: [PendingLine(queueID: "q1", text: "then run the store tests"), PendingLine(queueID: "q2", text: "and open a PR")]),
                                          strip: StatusStrip(phase: .open, held: true, working: true, since: Date(timeIntervalSinceNow: -65), queued: 2), onStop: {}, onRetry: {}), "Phone pairing spec")
        case "strip-tall":
            ArrivingTranscript(turns: Self.tallTranscript, acting: Self.acting(working: true),
                               strip: StatusStrip(phase: .open, held: true, working: true, since: Date(timeIntervalSinceNow: -65), queued: 0))
        case "strip-compacting":
            conversation(TranscriptScreen(turns: Self.turns + [Turn(number: 2, lines: [.compaction(id: "k", text: "Compacting context…")])], phase: .open, asleepLines: [],
                                          acting: Self.acting(working: true, pending: [PendingLine(queueID: "q1", text: "then run the store tests")]),
                                          requested: RequestedPrompt(id: "p", text: "now wire the pairing screen to the door", compacting: true), compacting: true,
                                          strip: StatusStrip(phase: .open, held: true, working: true, compacting: true, since: Date(timeIntervalSinceNow: -65), queued: 1),
                                          onRetry: {}), "Phone pairing spec")
        case "strip-compacting-stop":
            conversation(TranscriptScreen(turns: Self.turns + [Turn(number: 2, lines: [.compaction(id: "k", text: "Compacting context…")])], phase: .open, asleepLines: [],
                                          acting: Self.acting(working: true), compacting: true,
                                          strip: StatusStrip(phase: .open, held: true, working: true, compacting: true, since: Date(timeIntervalSinceNow: -65), queued: 0),
                                          onStop: {}, onRetry: {}), "Phone pairing spec")
        case "strip-reconnecting":
            conversation(TranscriptScreen(turns: Self.turns, phase: .connecting, asleepLines: [], acting: Self.acting(open: false, working: true),
                                          strip: StatusStrip(phase: .connecting, held: true, working: true, since: nil, queued: 0), onRetry: {}), "Phone pairing spec")
        case "strip-reconnecting-tools":
            conversation(TranscriptScreen(turns: Self.toolTurns(running: true), phase: .connecting, asleepLines: [], acting: Self.acting(open: false, working: true),
                                          strip: StatusStrip(phase: .connecting, held: true, working: true, since: nil, queued: 0), onRetry: {}), "Phone pairing spec")
        case "tools": conversation(TranscriptScreen(turns: Self.toolTurns(running: false), phase: .open, asleepLines: [], acting: Self.acting(working: false), onRetry: {}), "Phone pairing spec")
        case "tools-running": conversation(TranscriptScreen(turns: Self.toolTurns(running: true), phase: .open, asleepLines: [], acting: Self.acting(working: true),
                                                           strip: StatusStrip(phase: .open, held: true, working: true, since: Date(timeIntervalSinceNow: -65), queued: 0), onStop: {}, onRetry: {}), "Phone pairing spec")
        case "ended": conversation(TranscriptScreen(turns: Self.ended, phase: .open, asleepLines: [], acting: Self.acting(working: false), onRetry: {}), "Phone pairing spec")
        case "ended-words": conversation(TranscriptScreen(turns: Self.endedWords, phase: .open, asleepLines: [], acting: Self.acting(working: false), onRetry: {}), "Phone pairing spec")
        case "attach-menu", "attach-tray", "attach-failed", "attach-old-mac",
             "transcript-images", "transcript-markdown", "transcript-streaming", "transcript-long", "transcript-remote-image", "image-viewer",
             "strip-scrolled":
            PictureShowcase(screen: screen)
        case "f1": FailureView(failure: .codeSpent, onAction: {})
        case "f2": FailureView(failure: .denied, onAction: {})
        case "f3": FailureView(failure: .cannotJoin("no_gateway"), onAction: {})
        case "f5": FailureView(failure: .removed, onAction: {})
        case "new-thread", "new-thread-refused": NewThreadShowcase(refused: screen == "new-thread-refused")
        case "new-thread-blank": NewThreadShowcase(refused: false, text: "")
        case "settings", "settings-off":
            NavigationStack {
                SettingsPage(
                    settings: PhoneSettings(
                        name: "Morgan's iPhone", server: "mcp.gentility.ai",
                        expires: Date(timeIntervalSinceNow: (screen == "settings" ? 26 : 5) * 86_400),
                        connection: screen == "settings" ? .connected : .connecting, notificationsOff: screen == "settings-off"
                    ),
                    version: "1.4 (212)", now: Date(), onPairAgain: {}
                )
            }
        case "lapsed": FailureView(failure: .lapsed, onAction: {})
        case "unjoined": FailureView(failure: .notJoined, onAction: {})
        default: Text("No showcase named \(screen)")
        }
    }

    private func home(notificationsOff: Bool, query: String = "", open: Set<Shelf> = [], snoozing: HomeRow? = nil, undo: Undo? = nil, preset: HomeListPreset = HomeListPreset(project: "")) -> some View {
        NavigationStack {
            HomeList(rows: Self.rows, line: nil, empty: false, joining: false, notificationsOff: notificationsOff, undo: undo, onCompose: {}, query: query, open: open, snoozing: snoozing, preset: preset)
                .navigationTitle("Home")
        }
    }

    /// An open conversation, with `acting` at its foot.
    private func conversation(_ acting: Acting, _ title: String) -> some View {
        conversation(TranscriptScreen(turns: Self.turns, phase: .open, asleepLines: [], acting: acting, onRetry: {}), title)
    }

    /// Pushed onto Home, as a conversation is, so the back button draws.
    private func conversation(_ screen: TranscriptScreen, _ title: String) -> some View {
        NavigationStack(path: .constant(NavigationPath(["s"]))) {
            Color.clear
                .navigationTitle("Home")
                .navigationDestination(for: String.self) { _ in
                    screen
                        .threadChrome(title: title, branch: "feat-phone-pairing", machine: "Morgan's MacBook Pro", menu: HomeModel.menu(for: Self.rows[2]), onAct: { _ in })
                }
        }
    }

    /// Presentation fixtures only: Kit tests prove that the two store ids select
    /// separate approved ports and that the second cannot borrow the first's.
    static func siblingRows(secondReachable: Bool) -> [HomeRow] {
        [
            HomeRow(id: "s-first", organizationID: "o", title: "Fix the recap timing", state: "idle", ago: "2 min", machineID: "m-first", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -120), live: true, deskOnline: true, project: "sampleco/gentility"),
            HomeRow(id: "s-second", organizationID: "o", title: "Phone pairing spec", state: "idle", ago: "3 min", machineID: "m-second", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -180), live: secondReachable, deskOnline: true, project: "sampleco/gentility"),
        ]
    }

    /// Stalled 6net with online Desk, both down, and a reachable Mac.
    static let presenceRows = [
        HomeRow(id: "s-1", organizationID: "o", title: "Phone pairing spec", state: "idle", ago: "2 min", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -120), live: false, deskOnline: true, branch: "feat-phone-pairing", project: "sampleco/gentility"),
        HomeRow(id: "s-3", organizationID: "o", title: "Billing precision", state: "idle", ago: "yesterday", machineID: "m-cedar", macLabel: "cedar", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -90_000), live: false, deskOnline: false, branch: "fix-billing-precision", project: "sampleco/gentility"),
        HomeRow(id: "s-9", organizationID: "o", title: "Onboarding copy pass", state: "idle", ago: "3 h", machineID: "m-willow", macLabel: "driftwood", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -10_000), live: true, deskOnline: true, branch: "copy-onboarding", project: "sampleco/sample-app"),
    ]

    /// Every face: an approval and a question (one on an asleep Mac), work,
    /// an idle row with a long branch, a looper run, two Macs, a snooze, two
    /// settled, two projects, and a thread the server tags as waiting on you.
    static let rows = [
        HomeRow(id: "s-2", organizationID: "o", title: "Fix the recap timing", state: HomeRow.waitingOnYou, ago: "now", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -40), attentionLine: "Wants to execute cargo test -p desk_core --lib recorded", live: true, branch: "fix-recap-timing", project: "sampleco/gentility", attention: .approval),
        HomeRow(id: "s-7", organizationID: "o", title: "Invite teammates to a channel", state: HomeRow.waitingOnYou, ago: "14 min", machineID: "m-cedar", macLabel: "cedar", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -840), attentionLine: "Which branch should the fix go on?", branch: "feat-invites", project: "sampleco/sample-app", attention: .question),
        HomeRow(id: "s-1", organizationID: "o", title: "Phone pairing spec", state: "working", ago: "2 min", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -120), live: true, branch: "feat-phone-pairing", project: "sampleco/gentility", starred: true),
        HomeRow(id: "s-8", organizationID: "o", title: "task-sorter #41", state: "working", ago: "6 min", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -360), live: true, project: "sampleco/gentility", looper: true),
        HomeRow(id: "s-11", organizationID: "o", title: "Phone app programme", state: "idle", ago: "20 min", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -1_200), live: true, branch: "main", project: "sampleco/gentility", kind: .coordinator, starred: true),
        HomeRow(id: "s-12", organizationID: "o", title: "Architecture review", state: "idle", ago: "1 h", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -3_600), live: true, project: "sampleco/gentility", kind: .supervisor),
        HomeRow(id: "s-13", organizationID: "o", title: "Support inbox: refund question", state: "idle", ago: "2 h", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -7_200), live: true, looper: true, kind: .responder),
        HomeRow(id: "s-3", organizationID: "o", title: "Billing precision: keep decimal dollars through the ledger and the invoice renderer", state: "idle", ago: "yesterday", machineID: "m-cedar", macLabel: "cedar", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -90_000), branch: "fix-billing-precision-decimal-dollars-through-ledger", project: "sampleco/gentility"),
        HomeRow(id: "s-9", organizationID: "o", title: "Onboarding copy pass", state: "idle", ago: "3 h", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -10_000), live: true, branch: "copy-onboarding", project: "sampleco/sample-app", starred: true),
        HomeRow(id: "s-4", organizationID: "o", title: "Recap line for loopers", state: "idle", ago: "3 h", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -10_800), live: true, snoozedUntil: Date(timeIntervalSinceNow: 7_200), project: "sampleco/gentility"),
        HomeRow(id: "s-5", organizationID: "o", title: "Make the welcome screen clearer", state: "idle", ago: "2 d", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -172_800), live: true, settledAt: Date(timeIntervalSinceNow: -100_000), project: "sampleco/sample-app"),
        HomeRow(id: "s-6", organizationID: "o", title: "Port band", state: "idle", ago: "4 d", machineID: "m-cedar", macLabel: "cedar", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -345_600), settledAt: Date(timeIntervalSinceNow: -300_000), project: "sampleco/gentility"),
        HomeRow(id: "s-10", organizationID: "o", title: "Waiting-on-you surfaces", state: "idle", ago: "5 min", machineID: "m-mbp", macLabel: "Morgan's MacBook Pro", recapLine: nil, lastEventAt: Date(timeIntervalSinceNow: -300), live: true, branch: "feat-waiting-on-you-surfaces", project: "sampleco/gentility", waitingOn: "person", waitingReason: "Should the web filter show only your own threads?"),
    ]

    static let turns = [Turn(number: 1, lines: [
        .you(id: "u", text: "draw this workflow out in storyboards"),
        .agent(id: "a", text: "Here are the frames…"),
        .tool(id: "t", text: "✓ Write tmp/phone-pairing.html"),
        .agent(id: "b", text: "Proposed: number matching…"),
    ])]

    /// Runs of tool calls between the agent's words: one clean, one with a
    /// failure, one call alone, and the last still going.
    static func toolTurns(running: Bool) -> [Turn] {
        [Turn(number: 1, lines: [
            .you(id: "u", text: "Find why the recap fires twice and fix it"),
            .agent(id: "a0", text: "I'll read the recap scheduler and its tests first."),
            .tool(id: "t0", text: "✓ Read src/recap.rs"),
            .tool(id: "t1", text: "✓ Read src/recap/schedule.rs"),
            .tool(id: "t2", text: "✓ Grep \"schedule_recap\""),
            .tool(id: "t3", text: "✓ Read tests/recap.rs"),
            .agent(id: "a1", text: "The timer is armed in two places: once when the turn ends and again when the row is read. I'll drop the second and pin it with a test."),
            .tool(id: "t4", text: "✓ Edit src/recap/schedule.rs"),
            .tool(id: "t5", text: "✗ Bash cargo test -p desk_core recap"),
            .tool(id: "t6", text: "✓ Edit tests/recap.rs"),
            .tool(id: "t7", text: "✓ Bash cargo test -p desk_core recap"),
            .agent(id: "a2", text: "Fixed. The first run failed on a stale assertion, which I updated."),
            .tool(id: "t8", text: "✓ Bash git diff --stat"),
            .agent(id: "a3", text: "Two files changed. Checking the wider suite now."),
        ] + (running ? [
            .tool(id: "t9", text: "✓ Bash cargo fmt --check"),
            .tool(id: "t10", text: "… Bash cargo test -p desk_core --lib"),
        ] : []))]
    }

    /// A transcript taller than the screen, its last line short: at latest,
    /// the strip's inset keeps it clear of the capsule.
    static let tallTranscript = [Turn(number: 1, lines: [
        .you(id: "u0", text: "first, the pairing code"),
        .agent(id: "a0", text: "The code is six digits, shown on both screens."),
        .tool(id: "t0", text: "✓ Read docs/pairing.md"),
        .you(id: "u", text: "draw this workflow out in storyboards"),
        .agent(id: "a1", text: "Frame 1: the phone shows the number."),
        .tool(id: "t1", text: "✓ Write tmp/frame-1.html"),
        .agent(id: "a2", text: "Frame 2: the Mac shows the same number."),
        .tool(id: "t2", text: "✓ Write tmp/frame-2.html"),
        .agent(id: "a3", text: "Frame 3: the person taps Allow."),
        .tool(id: "t3", text: "✓ Write tmp/frame-3.html"),
        .agent(id: "a4", text: "Frame 4: the phone joins."),
        .tool(id: "t4", text: "✓ Write tmp/frame-4.html"),
        .agent(id: "a5", text: "Frame 5: Home lists the threads."),
        .tool(id: "t5", text: "… Write tmp/frame-5.html"),
        .agent(id: "a6", text: "Last line"),
    ])]

    /// A stopped turn, a limited one and a failed one, each with its line.
    static let ended = [
        Turn(number: 1, lines: [
            .you(id: "u1", text: "draw this workflow out in storyboards"),
            .tool(id: "t1", text: "⊘ Write tmp/phone-pairing.html"),
        ], end: TurnEnd(outcome: "stopped by the person")),
        Turn(number: 2, lines: [
            .you(id: "u2", text: "carry on"),
            .agent(id: "a2", text: "Picking up from frame 4…"),
        ], end: TurnEnd(outcome: "limited", detail: "You've hit your limit · resets 4pm (Europe/London)")),
        Turn(number: 3, lines: [
            .you(id: "u3", text: "try again"),
            .tool(id: "t3", text: "✗ Bash cargo test -p desk_core"),
        ], end: TurnEnd(outcome: "the agent exited before the turn ended: Internal error: API Error: 529 Overloaded")),
    ]

    /// Stops in their own words, one long enough to wrap, and a refusal.
    static let endedWords = [
        Turn(number: 1, lines: [
            .you(id: "u1", text: "draw this workflow out in storyboards"),
            .agent(id: "a1", text: "Starting on frame 1…"),
        ], end: TurnEnd(outcome: "stopped from outside by Morgan")),
        Turn(number: 2, lines: [
            .you(id: "u2", text: "carry on"),
            .tool(id: "t2", text: "⊘ Bash cargo test -p desk_core"),
        ], end: TurnEnd(outcome: "stopped from outside while the next build was being prepared")),
        Turn(number: 3, lines: [
            .you(id: "u3", text: "keep going"),
        ], end: TurnEnd(outcome: "refusal", detail: "The model declined to continue this request.")),
    ]

    static func acting(open: Bool = true, working: Bool, card: Card? = nil, detail: String? = nil, cleared: String? = nil, pending: [PendingLine] = []) -> Acting {
        Acting(open: open, working: working, card: card, detail: detail, cleared: cleared, pending: pending, note: nil, sending: false)
    }

    /// A Bash call's title is its whole command, as the Desk sends it in
    /// `tool`: the card's detail, never its heading.
    static let command = """
    cd desk && cargo test -p desk_core --lib recorded -- --nocapture 2>&1 \\
      | tee /tmp/desk-core-test.log
    grep -c 'test result: ok' /tmp/desk-core-test.log
    rm -rf target/debug/incremental/desk_core-*
    echo done
    """

    /// About 135 characters on three short lines, which wrap past four.
    static let threeLines = """
    git fetch origin && git rebase origin/master
    cargo test -p desk_core --lib recorded --quiet
    rm -rf target/debug/incremental/desk_core-*
    """

    /// Allow once tapped, and the door gone before the Mac heard it.
    static var owed: Card? {
        var fold = CardFold()
        fold.row(.approval(approval), now: .now)
        _ = fold.answer(.option("a"), sessionID: "s", now: .now, op: { "op" })
        fold.unsent(op: "op", macLabel: "Morgan's MacBook Pro")
        return fold.card
    }

    static var owedToUnopened: Card? {
        var fold = CardFold()
        fold.row(.approval(approval), now: .now)
        _ = fold.answer(.option("a"), sessionID: "s", now: .now, op: { "op" })
        fold.unsent(op: "op", macLabel: "Morgan's MacBook Pro")
        fold.waitingForOpen(macLabel: "Morgan's MacBook Pro")
        return fold.card
    }

    static let approval: PermissionRequest = try! JSONValue.object([
        "request_id": .string("r"), "call_id": .string("c"), "tool": .string(command), "kind": .string("execute"),
        "input": .object(["command": .string(command)]),
        "options": .array([
            .object(["id": .string("a"), "kind": .string("allow_once"), "name": .string("Allow once")]),
            .object(["id": .string("l"), "kind": .string("allow_always"), "name": .string("Always")]),
            .object(["id": .string("d"), "kind": .string("reject_once"), "name": .string("Deny")]),
        ]),
    ]).decode()

    static let form: Elicitation = try! JSONValue.parse(#"""
    {"request_id":"e","call_id":null,"message":"Which branch should the fix go on?","mode":{"type":"form","title":null,"fields":[
     {"name":"branch","title":"Branch","description":null,"required":true,"secret":false,"kind":{"type":"choice","options":[{"value":"main","title":"main","description":null},{"value":"fix-recap","title":"A new branch, fix-recap","description":null}],"default":null}},
     {"name":"retries","title":"Retries","description":null,"required":false,"secret":false,"kind":{"type":"number","integer":true,"min":0,"max":5,"default":2}},
     {"name":"push","title":"Push when done","description":null,"required":false,"secret":false,"kind":{"type":"boolean","default":false}},
     {"name":"note","title":"Anything else","description":null,"required":false,"secret":false,"kind":{"type":"text","format":null,"min_len":null,"max_len":null,"pattern":null,"default":null}}]}}
    """#).decode()

    /// As AskUserQuestion can ask: four questions of four options each.
    static let tall: Elicitation = try! JSONValue.object([
        "request_id": .string("t"), "call_id": .null,
        "message": .string("A few choices before I start on the recap timing fix."),
        "mode": .object(["type": .string("form"), "title": .null, "fields": .array([
            ("branch", "Which branch?", ["main", "A new branch, fix-recap", "The release branch", "Ask me later"]),
            ("delay", "How long should the recap wait?", ["One minute", "Three minutes", "Five minutes", "Until the turn ends"]),
            ("tests", "Which tests should run?", ["The recap tests only", "The core's whole suite", "Everything", "None for now"]),
            ("pr", "When it is done?", ["Open a pull request", "Push the branch", "Leave it local", "Ask me"]),
        ].map { name, title, options in
            JSONValue.object([
                "name": .string(name), "title": .string(title), "description": .null, "required": .bool(true), "secret": .bool(false),
                "kind": .object(["type": .string("choice"), "default": .null, "options": .array(options.map {
                    .object(["value": .string($0), "title": .string($0), "description": .null])
                })]),
            ])
        })]),
    ]).decode()

    static let link: Elicitation = try! JSONValue.parse(#"""
    {"request_id":"u","call_id":null,"message":"Sign in to Linear to read the issue.","mode":{"type":"url","url":"https://issues.example.com/login","elicitation_id":"x","sign_in":"Linear"}}
    """#).decode()

    static let asleep = [
        "cedar is asleep. This session will open when that Mac wakes.",
        "Showing what the server last heard.",
        "Status: idle · last turn yesterday",
    ]

    static let noDoor = [
        "driftwood can't be reached for paper-kites. Ask an administrator to check access.",
        "Showing what the server last heard.",
        "Status: idle · last turn 3 h",
    ]

    static let linkUnavailable = [
        "Desk recently heard from Morgan's MacBook Pro in the upstairs study, but this phone can't reach it. Retrying.",
        "Showing what the server last heard.",
        "Status: idle · last turn yesterday",
    ]
}
#endif

/// The new-thread sheet over sample choices.
private struct NewThreadShowcase: View {
    let refused: Bool
    @State private var text: String
    @State private var newBranch = true
    @State private var model: String? = nil
    @State private var effort: String? = nil
    @FocusState private var typing: Bool

    init(refused: Bool, text: String = "Find why the recap fires twice and fix it") {
        self.refused = refused
        _text = State(initialValue: text)
    }

    var body: some View {
        let macs = [MacChoice(organizationID: "o", machineID: "m", label: "Morgan's MacBook Pro", address: "", port: 0),
                    MacChoice(organizationID: "o", machineID: "n", label: "cedar", address: "", port: 0)]
        let projects = [DoorProject(path: "/Users/someone/sample-app", name: "sample-app")]
        NavigationStack {
            NewThreadForm(
                text: $text, newBranch: $newBranch, model: $model, effort: $effort,
                macs: macs, mac: macs[0], projects: projects, project: projects[0],
                harnesses: ["claude", "codex"], harness: "claude", models: [], efforts: [],
                loading: false, line: refused ? "The link to Morgan's MacBook Pro dropped. Try again." : nil, typing: $typing,
                onMac: { _ in }, onProject: { _ in }, onHarness: { _ in }
            )
            .navigationTitle("New thread")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") {}.foregroundStyle(Theme.inkSoft) }
                ToolbarItem(placement: .confirmationAction) { StartButton(ready: !text.isEmpty) {} }
            }
        }
    }
}
