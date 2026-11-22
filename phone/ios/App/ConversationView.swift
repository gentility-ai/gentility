import DeskPhoneKit
import SwiftUI

struct ConversationView: View {
    let app: AppModel
    @StateObject var conversation: ConversationModel
    @Environment(\.dismiss) private var dismiss

    init(app: AppModel, conversation: ConversationModel) {
        self.app = app
        _conversation = StateObject(wrappedValue: conversation)
    }

    var body: some View {
        TranscriptScreen(turns: conversation.fold.turns, phase: conversation.phase, asleepLines: conversation.asleepLines, acting: acting,
                         requested: conversation.waitingPrompt, compacting: conversation.fold.compacting, strip: strip,
                         onStop: stop) {
            Task { await conversation.retry() }
        }
        .environment(\.images, ImageContext(
            store: conversation.images, sessionID: conversation.row.id,
            worktreeRoot: conversation.worktreeRoot, open: conversation.phase == .open
        ))
        .threadChrome(
            title: conversation.row.title, branch: conversation.row.branch, machine: conversation.row.macLabel,
            menu: HomeModel.menu(for: conversation.row),
            onStop: stop
        ) { action in
            let row = conversation.row
            Task { await app.home?.act(action, on: row) }
            dismiss()
        }
        .onAppear { app.showing(conversation) }
        .task { await conversation.open() }
        .onDisappear {
            conversation.leave()
            app.hid(conversation)
        }
    }

    /// Set while a turn runs and the Mac can hear a stop.
    private var stop: (() -> Void)? {
        conversation.working && conversation.phase == .open ? { Task { await conversation.stop() } } : nil
    }

    private var strip: StatusStrip? {
        StatusStrip(phase: conversation.phase, held: !conversation.fold.turns.isEmpty, working: conversation.working,
                    compacting: conversation.fold.compacting, since: conversation.workingSince, queued: conversation.pending.lines.count)
    }

    private var acting: Acting {
        let detail: String? = if case .approval(let request)? = conversation.card?.ask { conversation.detail(for: request) } else { nil }
        return Acting(
            open: conversation.phase == .open, working: conversation.working,
            card: conversation.card, detail: detail, cleared: conversation.cleared,
            pending: conversation.pending.lines, note: conversation.composerNote, sending: conversation.sending,
            onAnswer: { answer in Task { await conversation.answer(answer) } },
            onSend: { await conversation.send($0) },
            onSteer: { await conversation.steer($0) },
            onTakeBack: { await conversation.takeBack($0) },
            tray: conversation.tray, canAttach: conversation.canAttach,
            onAttach: { conversation.attach($0, names: $1) },
            onRemoveAttachment: { conversation.removeAttachment($0) },
            onRetryAttachment: { conversation.retryAttachment($0) }
        )
    }
}

/// The conversation from plain values, with its composer and approval card.
struct TranscriptScreen: View {
    let turns: [Turn]
    let phase: ConversationModel.Phase
    let asleepLines: [String]
    let acting: Acting
    /// A prompt asked for whose turn has not started, drawn after the turns.
    let requested: RequestedPrompt?
    /// A compaction is running in the last turn: its line, not the caret,
    /// is the live sign.
    let compacting: Bool
    let strip: StatusStrip?
    /// Stops the running turn; nil where there is none to stop.
    let onStop: (() -> Void)?
    let onRetry: () -> Void

    /// The screen's height above the keyboard, measured.
    @State private var height: CGFloat = 0
    /// The end of the transcript is on screen.
    @State private var atEnd: Bool

    /// `atEnd` false draws the jump arrow from the start (Showcase: it opens
    /// at the top of a long thread, where the app would have scrolled down).
    init(turns: [Turn], phase: ConversationModel.Phase, asleepLines: [String], acting: Acting,
         requested: RequestedPrompt? = nil, compacting: Bool = false, strip: StatusStrip? = nil,
         onStop: (() -> Void)? = nil, onRetry: @escaping () -> Void, atEnd: Bool = true) {
        self.turns = turns
        self.phase = phase
        self.asleepLines = asleepLines
        self.acting = acting
        self.requested = requested
        self.compacting = compacting
        self.strip = strip
        self.onStop = onStop
        self.onRetry = onRetry
        _atEnd = State(initialValue: atEnd)
    }

    private var working: Bool { acting.working }

    /// An owed answer redials by itself after a backoff that grows; Retry
    /// then means sooner than that.
    private var retry: String {
        if case .lost = phase, acting.card?.owed == true { return "Retry now" }
        return "Retry"
    }

    var body: some View {
        VStack(spacing: 0) {
            switch phase {
            case .refused(let line), .lost(let line):
                retryBar(line)
            case .noDoor(let organization):
                // The banner below says why; the bar offers Retry.
                retryBar("Can't reach this machine for \(organization).")
            case .connecting, .open, .asleep, .linkUnavailable, .notOpen:
                // Connecting is the strip's to say.
                EmptyView()
            }
            ScrollViewReader { reader in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: Theme.Space.l) {
                        if phase.offline {
                            Asleep(lines: asleepLines)
                        }
                        ForEach(turns) { turn in
                            ForEach(TranscriptRow.rows(turn.lines)) { row in
                                switch row {
                                case .line(let line): LineView(line: line, cursor: caret == .inLine(line.id), live: phase == .open)
                                case .tools(let run): ToolRunView(run: run, live: phase == .open)
                                }
                            }
                            if let end = turn.drawnEnd {
                                EndView(end: end)
                            }
                        }
                        if caret == .alone {
                            Text("▍").foregroundStyle(.tint)
                        }
                        if let requested {
                            WaitingPromptView(prompt: requested)
                        }
                        Color.clear.frame(height: 1).id("end")
                            .onAppear { atEnd = true }
                            .onDisappear { atEnd = false }
                    }
                    .padding()
                }
                // Room under the last line for the strip floating over it.
                .contentMargins(.bottom, strip == nil ? 0 : 60, for: .scrollContent)
                // New lines follow only someone already at the end; scrolled
                // up to read, they stay put and the arrow takes them down.
                .onChange(of: turns) { _, _ in
                    if atEnd { withAnimation { reader.scrollTo("end", anchor: .bottom) } }
                }
                .onChange(of: requested) { _, _ in
                    if atEnd { withAnimation { reader.scrollTo("end", anchor: .bottom) } }
                }
                // At the trailing edge, where the jump has always been: the
                // strip grows leftward from it.
                .overlay(alignment: .bottomTrailing) {
                    StripView(strip: strip, atEnd: atEnd, onStop: onStop) {
                        withAnimation { reader.scrollTo("end", anchor: .bottom) }
                    }
                    .padding(Theme.Space.m)
                }
                .animation(.easeOut(duration: 0.15), value: atEnd)
                .animation(.easeOut(duration: 0.15), value: strip?.doing)
            }
            // Its height first, a question's capped at most of the screen
            // so its buttons stay in sight; the transcript scrolls in the rest.
            Foot(acting: acting, cap: height * 0.55).layoutPriority(1)
        }
        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height = $0 }
        .background(Theme.ground)
    }

    /// A line under the title, and Retry beside it.
    private func retryBar(_ line: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
            Image(systemName: "wifi.exclamationmark").font(.footnote).foregroundStyle(Theme.attention)
            Text(line).font(.footnote).foregroundStyle(Theme.inkSoft)
            Spacer(minLength: Theme.Space.s)
            Button(retry, action: onRetry)
                .font(.footnote.weight(.semibold))
                .foregroundStyle(Theme.accent)
                .frame(minHeight: Theme.touch)
                .contentShape(Rectangle())
        }
        .padding(.horizontal, Theme.Space.l)
        .background(Theme.raised)
        .overlay(alignment: .bottom) { Theme.hairline.frame(height: 1) }
    }

    /// Nothing in the transcript moves unless the conversation is open:
    /// connecting, the strip's spinner is the one live sign.
    private var caret: Caret { Caret(turns: turns, working: working && phase == .open, compacting: compacting) }
}

/// The strip over the foot of the transcript: what the session is doing and
/// how many prompts wait, with the jump to the latest line at its end once
/// the person has scrolled up. Only the jump, where the strip has nothing to
/// say; nothing at all at the end of a quiet session.
private struct StripView: View {
    let strip: StatusStrip?
    let atEnd: Bool
    /// Stops the running turn. Its button stands where the spinner would:
    /// the clock beside it is the sign of life.
    let onStop: (() -> Void)?
    let onJump: () -> Void

    private var stops: Bool { onStop != nil && strip?.doing != nil }

    var body: some View {
        if strip != nil || !atEnd {
            HStack(spacing: 0) {
                if stops, let onStop {
                    Button(action: onStop) {
                        Image(systemName: "stop.fill")
                            .font(.caption2.weight(.bold))
                            .foregroundStyle(Theme.ground)
                            .frame(width: 26, height: 26)
                            .background(Theme.ink, in: Circle())
                            .frame(width: Theme.touch, height: Theme.touch)
                            .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Stop the turn")
                }
                // The rest of the capsule takes the jump, not the arrow alone.
                Button(action: onJump) {
                    HStack(spacing: 10) {
                        if let strip {
                            if strip.spins(atEnd: atEnd), !stops { ProgressView().controlSize(.small) }
                            TimelineView(.periodic(from: .now, by: 1)) { context in
                                Text(strip.words(now: context.date)).monospacedDigit()
                            }
                            .font(.footnote)
                            .foregroundStyle(Theme.inkSoft)
                        }
                        if !atEnd {
                            if strip != nil { Divider().frame(height: 18) }
                            Image(systemName: "arrow.down").font(.body.weight(.semibold)).foregroundStyle(Theme.ink)
                        }
                    }
                    .padding(.leading, strip == nil ? 0 : stops ? Theme.Space.xs : Theme.Space.l)
                    .padding(.trailing, strip == nil ? 0 : Theme.Space.l)
                    .frame(minWidth: Theme.touch, minHeight: Theme.touch)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                // At the end there is nowhere to jump. Not `disabled`, which dims the words.
                .allowsHitTesting(!atEnd)
                .accessibilityRemoveTraits(atEnd ? .isButton : [])
                .accessibilityLabel(atEnd ? "" : "Jump to latest")
            }
            .background(.regularMaterial, in: Capsule())
            .overlay(Capsule().strokeBorder(Theme.hairline))
            .shadow(color: .black.opacity(0.18), radius: 10, y: 3)
            .transition(.opacity.combined(with: .scale(scale: 0.9)))
        }
    }
}

/// F4: the Mac is asleep, or B6: it has no door here; with what the server
/// last heard.
private struct Asleep: View {
    let lines: [String]

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let first = lines.first {
                Label { Text(first).foregroundStyle(Theme.ink) } icon: {
                    Image(systemName: "exclamationmark.circle").foregroundStyle(Theme.attention)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(Theme.Space.m)
                .surface(Theme.Corner.medium)
            }
            if lines.count > 1 {
                Text(lines[1]).font(.footnote).foregroundStyle(Theme.inkFaint)
            }
            ForEach(lines.dropFirst(2), id: \.self) { line in
                Text(line).font(.subheadline).foregroundStyle(Theme.inkSoft)
            }
        }
    }
}

/// How a turn that did not end as turns do ended, under its last line: a
/// quiet "Stopped", an amber limit, or a red failure with its error.
private struct EndView: View {
    let end: TurnEnd

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label(end.words, systemImage: symbol)
                .font(.footnote.weight(end.tone == .stopped ? .regular : .semibold))
                .foregroundStyle(color)
            if let more = end.more {
                Text(more)
                    .font(.footnote)
                    .foregroundStyle(.primary.opacity(0.8))
                    .textSelection(.enabled)
                    .lineLimit(12)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.horizontal, 10).padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(end.tone == .stopped ? 0 : 0.1), in: RoundedRectangle(cornerRadius: 10))
        .accessibilityElement(children: .combine)
    }

    private var symbol: String {
        switch end.tone {
        case .stopped: return "stop.circle"
        case .limited: return "exclamationmark.circle"
        case .failed: return "xmark.octagon"
        }
    }

    private var color: Color {
        switch end.tone {
        case .stopped: return .secondary
        case .limited: return .orange
        case .failed: return .red
        }
    }
}

/// One line of a turn: the person's words on a surface at the right, the
/// agent's across the page as they are written. Tool calls draw as runs.
private struct LineView: View {
    let line: Line
    /// The agent is still writing this line.
    let cursor: Bool
    /// The conversation is open, so a running call is known to be running.
    let live: Bool

    var body: some View {
        switch line {
        case .you(_, let text, let attachments):
            VStack(alignment: .trailing, spacing: 6) {
                if !attachments.isEmpty { AttachmentsRow(attachments: attachments) }
                if !text.isEmpty {
                    Text(text).foregroundStyle(Theme.ink).youBubble()
                }
            }
            .frame(maxWidth: .infinity, alignment: .trailing)
            .padding(.leading, 48)
        case .agent(_, let text):
            MarkdownView(text: text, cursor: cursor)
                .frame(maxWidth: .infinity, alignment: .leading)
        case .compaction(_, let text):
            HStack(spacing: 8) {
                Rectangle().fill(Theme.hairline).frame(height: 1)
                Text(text).font(.footnote).foregroundStyle(Theme.inkFaint).lineLimit(1).layoutPriority(1)
                Rectangle().fill(Theme.hairline).frame(height: 1)
            }
            .padding(.vertical, 4)
        case .tool:
            // Tool calls reach the screen as runs; one alone is a run of one.
            if case .tools(let run)? = TranscriptRow.rows([line]).first { ToolRunView(run: run, live: live) }
        }
    }
}

extension View {
    /// The person's own words.
    func youBubble() -> some View {
        padding(.horizontal, 14).padding(.vertical, 10)
            .background(Theme.raised, in: RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous))
    }
}

/// A run of tool calls as one quiet row: the call going now, or how many
/// there were. A run of several opens to each call.
private struct ToolRunView: View {
    let run: ToolRun
    /// The conversation is open. Otherwise a running call holds still.
    let live: Bool
    @State private var open = false

    private var several: Bool { run.steps.count > 1 }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button {
                withAnimation(Theme.motion) { open.toggle() }
            } label: {
                HStack(spacing: Theme.Space.s) {
                    glyph
                    Text(run.headline)
                        .font(several && run.running == nil ? .footnote : .footnote.monospaced())
                        .foregroundStyle(Theme.inkSoft)
                        .lineLimit(1).truncationMode(.middle)
                    if several {
                        if run.running != nil {
                            Text("\(run.steps.count)").font(.caption.monospacedDigit()).foregroundStyle(Theme.inkFaint)
                        }
                        Image(systemName: "chevron.right")
                            .font(.caption2.weight(.semibold)).foregroundStyle(Theme.inkFaint)
                            .rotationEffect(.degrees(open ? 90 : 0))
                    }
                    Spacer(minLength: 0)
                }
                .frame(minHeight: several ? 36 : 24)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            // One call has nothing to open. Not `disabled`, which dims the words.
            .allowsHitTesting(several)
            .accessibilityRemoveTraits(several ? [] : .isButton)
            .accessibilityLabel(several ? "\(run.headline), \(open ? "open" : "closed")" : run.headline)
            if open, several {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(run.steps) { step in
                        HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                            Text(step.mark).font(.caption).foregroundStyle(Self.color(step.state)).frame(width: 14)
                            Text(step.title).font(.caption.monospaced()).foregroundStyle(Theme.inkSoft)
                                .lineLimit(2).truncationMode(.middle)
                        }
                    }
                }
                .padding(.leading, Theme.Space.m).padding(.vertical, 6)
                .overlay(alignment: .leading) { Rectangle().fill(Theme.hairline).frame(width: 1).padding(.leading, 7) }
                .transition(.opacity)
            }
        }
    }

    @ViewBuilder private var glyph: some View {
        Group {
            if run.spins(live: live) {
                ProgressView().controlSize(.mini)
            } else if run.running != nil {
                Image(systemName: "ellipsis.circle").foregroundStyle(Theme.inkFaint)
            } else if run.failed > 0 {
                Image(systemName: "xmark.circle.fill").foregroundStyle(Theme.bad)
            } else if run.steps.allSatisfy({ $0.state == .done }) {
                Image(systemName: "checkmark.circle").foregroundStyle(Theme.inkFaint)
            } else {
                Image(systemName: "minus.circle").foregroundStyle(Theme.inkFaint)
            }
        }
        .font(.footnote)
        .frame(width: 16)
        .accessibilityHidden(true)
    }

    static func color(_ state: ToolStep.State) -> Color {
        switch state {
        case .failed: Theme.bad
        case .running: Theme.accent
        case .done, .other: Theme.inkFaint
        }
    }
}

/// The title with where the thread runs under it, and its actions at the
/// trailing edge: what Home offers on a row, without going back for it.
struct ThreadChrome: ViewModifier {
    let title: String
    let branch: String?
    let machine: String?
    let menu: [HomeMenuItem]
    /// Set while a turn runs and the Mac can hear a stop.
    var onStop: (() -> Void)? = nil
    let onAct: (HomeAction) -> Void

    private var place: String? { branch ?? machine }

    func body(content: Content) -> some View {
        content
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .principal) {
                    VStack(spacing: 1) {
                        Text(title).font(.subheadline.weight(.semibold)).foregroundStyle(Theme.ink).lineLimit(1)
                        if let place {
                            HStack(spacing: 3) {
                                Image(systemName: branch == nil ? "laptopcomputer" : "arrow.triangle.branch")
                                Text(place).lineLimit(1).truncationMode(.middle)
                            }
                            .font(.caption2).foregroundStyle(Theme.inkFaint)
                        }
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityAddTraits(.isHeader)
                }
                if !menu.isEmpty || onStop != nil {
                    ToolbarItem(placement: .topBarTrailing) {
                        Menu {
                            if let onStop {
                                Section {
                                    Button(role: .destructive, action: onStop) { Label("Stop the turn", systemImage: "stop.circle") }
                                }
                            }
                            ForEach(Array(menu.enumerated()), id: \.offset) { _, item in item_(item) }
                        } label: {
                            Label("Thread actions", systemImage: "ellipsis")
                        }
                    }
                }
            }
    }

    @ViewBuilder private func item_(_ item: HomeMenuItem) -> some View {
        switch item {
        case .copy(let copies):
            CopyItems(copies: copies)
        case .settle:
            Button { onAct(.settle) } label: { Label("Settle", systemImage: "checkmark.circle") }
        case .unsettle:
            Button { onAct(.unsettle) } label: { Label("Unsettle", systemImage: "arrow.uturn.backward.circle") }
        case .snooze(let presets):
            Menu {
                ForEach(presets, id: \.label) { preset in
                    Button("\(preset.label) · \(preset.when)") { onAct(.snooze(preset.until)) }
                }
            } label: {
                Label("Snooze", systemImage: "moon.zzz")
            }
        case .wake:
            Button { onAct(.wake) } label: { Label("Wake", systemImage: "sun.max") }
        case .archive:
            Divider()
            Button(role: .destructive) { onAct(.archive) } label: { Label("Archive", systemImage: "archivebox") }
        case .managedOnMac:
            Button {} label: { Label("Looper runs are managed on your Mac", systemImage: "laptopcomputer") }
                .disabled(true)
        }
    }
}

extension View {
    func threadChrome(title: String, branch: String?, machine: String?, menu: [HomeMenuItem], onStop: (() -> Void)? = nil, onAct: @escaping (HomeAction) -> Void) -> some View {
        modifier(ThreadChrome(title: title, branch: branch, machine: machine, menu: menu, onStop: onStop, onAct: onAct))
    }
}

/// A prompt asked for whose turn has not started: the person's own bubble,
/// in soft ink, and what it waits on under it. The row stops carrying it once its
/// turn starts, and the transcript has the words.
private struct WaitingPromptView: View {
    let prompt: RequestedPrompt

    private var words: String {
        prompt.text.isEmpty ? ConversationModel.photosOnly(prompt.attachments) : prompt.text
    }

    private var caption: String {
        guard !prompt.text.isEmpty, prompt.attachments > 0 else { return prompt.caption }
        return prompt.caption + " · " + ConversationModel.photosOnly(prompt.attachments)
    }

    var body: some View {
        VStack(alignment: .trailing, spacing: 4) {
            // Soft ink says it waits. Not `opacity`, which dims the words past reading.
            Text(words).foregroundStyle(Theme.inkSoft).youBubble()
            HStack(spacing: 4) {
                Image(systemName: "clock")
                Text(caption)
            }
            .font(.caption).foregroundStyle(Theme.inkFaint)
        }
        .frame(maxWidth: .infinity, alignment: .trailing)
        .padding(.leading, 48)
        .accessibilityElement(children: .combine)
    }
}
