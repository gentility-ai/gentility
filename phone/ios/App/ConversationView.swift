import DeskPhoneKit
import SwiftUI

struct ConversationView: View {
    let app: AppModel
    @StateObject var conversation: ConversationModel

    init(app: AppModel, conversation: ConversationModel) {
        self.app = app
        _conversation = StateObject(wrappedValue: conversation)
    }

    var body: some View {
        TranscriptScreen(turns: conversation.fold.turns, phase: conversation.phase, asleepLines: conversation.asleepLines, acting: acting,
                         requested: conversation.waitingPrompt, compacting: conversation.fold.compacting, strip: strip) {
            Task { await conversation.retry() }
        }
        .environment(\.images, ImageContext(
            store: conversation.images, sessionID: conversation.row.id,
            worktreeRoot: conversation.worktreeRoot, open: conversation.phase == .open
        ))
        .navigationTitle(conversation.row.title)
        .navigationBarTitleDisplayMode(.inline)
        .onAppear { app.showing(conversation) }
        .task { await conversation.open() }
        .onDisappear {
            conversation.leave()
            app.hid(conversation)
        }
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
    let onRetry: () -> Void

    /// The screen's height above the keyboard, measured.
    @State private var height: CGFloat = 0
    /// The end of the transcript is on screen.
    @State private var atEnd: Bool

    /// `atEnd` false draws the jump arrow from the start (Showcase: it opens
    /// at the top of a long thread, where the app would have scrolled down).
    init(turns: [Turn], phase: ConversationModel.Phase, asleepLines: [String], acting: Acting,
         requested: RequestedPrompt? = nil, compacting: Bool = false, strip: StatusStrip? = nil, onRetry: @escaping () -> Void, atEnd: Bool = true) {
        self.turns = turns
        self.phase = phase
        self.asleepLines = asleepLines
        self.acting = acting
        self.requested = requested
        self.compacting = compacting
        self.strip = strip
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
                    LazyVStack(alignment: .leading, spacing: 10) {
                        if phase.offline {
                            Asleep(lines: asleepLines)
                        }
                        ForEach(turns) { turn in
                            ForEach(turn.lines) { line in
                                LineView(line: line, cursor: caret == .inLine(line.id))
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
                .contentMargins(.bottom, strip == nil ? 0 : 52, for: .scrollContent)
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
                    StripView(strip: strip, atEnd: atEnd) {
                        withAnimation { reader.scrollTo("end", anchor: .bottom) }
                    }
                    .padding(12)
                }
                .animation(.easeOut(duration: 0.15), value: atEnd)
                .animation(.easeOut(duration: 0.15), value: strip?.doing)
            }
            // Its height first, a question's capped at most of the screen
            // so its buttons stay in sight; the transcript scrolls in the rest.
            Foot(acting: acting, cap: height * 0.55).layoutPriority(1)
        }
        .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height = $0 }
    }

    /// A line under the title, and Retry beside it.
    private func retryBar(_ line: String) -> some View {
        HStack {
            Text(line).font(.footnote)
            Spacer()
            Button(retry, action: onRetry).font(.footnote.weight(.semibold))
        }
        .padding(.horizontal).padding(.vertical, 8)
        .background(Color.orange.opacity(0.15))
    }

    private var caret: Caret { Caret(turns: turns, working: working, compacting: compacting) }
}

/// The strip over the foot of the transcript: what the session is doing and
/// how many prompts wait, with the jump to the latest line at its end once
/// the person has scrolled up. Only the jump, where the strip has nothing to
/// say; nothing at all at the end of a quiet session.
private struct StripView: View {
    let strip: StatusStrip?
    let atEnd: Bool
    let onJump: () -> Void

    var body: some View {
        if strip != nil || !atEnd {
            HStack(spacing: 10) {
                if let strip {
                    if strip.doing != nil { ProgressView().controlSize(.small) }
                    TimelineView(.periodic(from: .now, by: 1)) { context in
                        Text(strip.words(now: context.date)).monospacedDigit()
                    }
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                }
                if !atEnd {
                    if strip != nil { Divider().frame(height: 18) }
                    Button(action: onJump) {
                        Image(systemName: "arrow.down").font(.body.weight(.semibold))
                    }
                    .accessibilityLabel("Jump to latest")
                }
            }
            .padding(.horizontal, strip == nil ? 0 : 14)
            .frame(minWidth: 40, minHeight: 40)
            .background(.regularMaterial, in: Capsule())
            .shadow(radius: 2, y: 1)
            .transition(.opacity)
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
                Banner(text: first, tone: .warn, foreground: .primary)
            }
            if lines.count > 1 {
                Text(lines[1]).foregroundStyle(.primary.opacity(0.7)).frame(maxWidth: .infinity)
            }
            ForEach(lines.dropFirst(2), id: \.self) { line in
                Text(line)
                    .padding(10)
                    .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
                    .opacity(0.7)
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

/// One line of a turn: the person's words on the right, the agent's on the
/// left, a tool call as one monospaced line with its mark first.
private struct LineView: View {
    let line: Line
    /// The agent is still writing this line.
    let cursor: Bool

    var body: some View {
        switch line {
        case .you(_, let text, let attachments):
            VStack(alignment: .trailing, spacing: 6) {
                if !attachments.isEmpty { AttachmentsRow(attachments: attachments) }
                if !text.isEmpty {
                    Text(text)
                        .padding(10)
                        .background(Color.accentColor.opacity(0.15), in: RoundedRectangle(cornerRadius: 12))
                }
            }
            .frame(maxWidth: .infinity, alignment: .trailing)
            .padding(.leading, 40)
        case .agent(_, let text):
            MarkdownView(text: text, cursor: cursor)
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
                .padding(.trailing, 24)
        case .compaction(_, let text):
            HStack(spacing: 8) {
                Rectangle().fill(Color.secondary.opacity(0.35)).frame(height: 0.5)
                Text(text).font(.footnote).foregroundStyle(.secondary).lineLimit(1).layoutPriority(1)
                Rectangle().fill(Color.secondary.opacity(0.35)).frame(height: 0.5)
            }
            .padding(.vertical, 4)
        case .tool(_, let text):
            Text(text)
                .font(.footnote.monospaced())
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .padding(.horizontal, 10).padding(.vertical, 6)
                .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.secondary.opacity(0.35)))
        }
    }
}

/// A prompt asked for whose turn has not started: the person's own bubble,
/// dimmed, and what it waits on under it. The row stops carrying it once its
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
            Text(words)
                .padding(10)
                .background(Color.accentColor.opacity(0.15), in: RoundedRectangle(cornerRadius: 12))
                .opacity(0.6)
            HStack(spacing: 4) {
                Image(systemName: "clock")
                Text(caption)
            }
            .font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .trailing)
        .padding(.leading, 40)
        .accessibilityElement(children: .combine)
    }
}
