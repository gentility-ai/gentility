import DeskPhoneKit
import PhotosUI
import SwiftUI

/// What the foot of a conversation draws, from plain values.
struct Acting {
    /// The door is open: the composer draws, and the card's buttons work.
    var open: Bool
    var working: Bool
    var card: Card?
    var detail: String?
    var cleared: String?
    var pending: [PendingLine]
    var note: String?
    var sending: Bool
    var onAnswer: (Answer) -> Void = { _ in }
    var onSend: (String) async -> Bool = { _ in false }
    var onSteer: (String) async -> Bool = { _ in false }
    var onTakeBack: (String) async -> String? = { _ in nil }
    /// The photos the next prompt carries.
    var tray = AttachmentTray()
    /// The Mac's hello offered `attachments`.
    var canAttach = true
    var onAttach: ([Data], [String?]) -> Void = { _, _ in }
    var onRemoveAttachment: (UUID) -> Void = { _ in }
    var onRetryAttachment: (UUID) -> Void = { _ in }
    /// The words already in the field as the screen draws (Showcase).
    var draft = ""
    /// Draws the attach menu open (Showcase: a menu cannot be opened from code).
    var menuShown = false
    /// Opens this photo's failure dialog as the screen draws (Showcase).
    var failureShown: UUID?
}

/// The approval card above the composer, or a question in its
/// place. While the door redials a kept card still draws, its buttons off,
/// and no composer draws until the door is open.
struct Foot: View {
    let acting: Acting
    /// The tallest a question's words and fields grow before they scroll.
    let cap: CGFloat
    @State private var draft: String

    init(acting: Acting, cap: CGFloat) {
        self.acting = acting
        self.cap = cap
        _draft = State(initialValue: acting.draft)
    }

    var body: some View {
        VStack(spacing: 8) {
            if let cleared = acting.cleared {
                Text(cleared).font(.footnote).foregroundStyle(Theme.inkSoft)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            if let card = acting.card {
                switch card.ask {
                case .approval(let request):
                    ApprovalCard(request: request, detail: acting.detail, card: card, enabled: acting.open, onAnswer: acting.onAnswer)
                        .id(request.requestID)
                    composer
                case .elicitation, .question:
                    QuestionCard(card: card, enabled: acting.open, cap: cap, onAnswer: acting.onAnswer)
                        .id(card.ask.id)
                }
            } else {
                composer
            }
        }
        .padding(.horizontal).padding(.vertical, 8)
        .background(Theme.ground)
    }

    @ViewBuilder private var composer: some View {
        if acting.open { Composer(draft: $draft, acting: acting) }
    }
}

/// The photo tray, then + "Message…" and ⏎; during a turn ⏎ steers the
/// words in and "Queue instead" sits under it, then the queued lines with
/// Take back, then why words did not go.
struct Composer: View {
    @Binding var draft: String
    let acting: Acting

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if acting.menuShown { AttachMenuPicture() }
            if !acting.tray.isEmpty {
                TrayView(tray: acting.tray, failureShown: acting.failureShown,
                         onRemove: acting.onRemoveAttachment, onRetry: acting.onRetryAttachment)
            }
            HStack(alignment: .bottom, spacing: 8) {
                AttachButton(acting: acting)
                TextField("", text: $draft, prompt: Text(acting.working ? "Steer the turn" : "Message").foregroundStyle(Theme.inkFaint), axis: .vertical)
                    .lineLimit(1...5)
                    .padding(.horizontal, 14).padding(.vertical, 9)
                    .frame(minHeight: 40)
                    .surface(20, fill: Theme.field)
                Button { send(acting.working ? acting.onSteer : acting.onSend) } label: {
                    Image(systemName: "arrow.up")
                        .font(.body.weight(.bold))
                        .foregroundStyle(sendable ? Theme.onAccent : Theme.inkFaint)
                        .frame(width: 40, height: 40)
                        .background(sendable ? Theme.accent : Theme.field, in: Circle())
                        .contentShape(Rectangle().inset(by: -4))
                }
                .buttonStyle(.plain)
                .accessibilityLabel(acting.working ? "Steer now" : "Send")
                .disabled(!sendable)
                .animation(Theme.motion, value: sendable)
            }
            if let uploading = acting.tray.uploadingLine {
                Text(uploading).font(.footnote).foregroundStyle(Theme.inkSoft).padding(.leading, 44)
            }
            if acting.working {
                // Sending steers the running turn; this waits for it to end.
                Button { send(acting.onSend) } label: { Label("Queue instead", systemImage: "clock") }
                    .font(.footnote.weight(.medium))
                    .buttonStyle(.bordered)
                    .buttonBorderShape(.capsule)
                    .controlSize(.small)
                    .tint(Theme.inkSoft)
                    .disabled(!sendable)
                    .padding(.leading, 48)
            }
            ForEach(acting.pending) { line in
                PendingLineView(line: line) {
                    Task {
                        if let words = await acting.onTakeBack(line.queueID) { takeBack(words) }
                    }
                }
            }
            if let note = acting.note {
                Text(note).font(.footnote).foregroundStyle(Theme.attention)
            }
        }
    }

    private var blank: Bool { draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

    /// Words or a ready photo, nothing still going or failed, and no prompt out.
    private var sendable: Bool { acting.tray.canSend(draft) && !acting.sending }

    /// The words taken back go in front of whatever was typed since, which
    /// they came before: nothing the person wrote is dropped.
    private func takeBack(_ words: String) {
        draft = blank ? words : words + "\n" + draft
    }

    /// The words leave the field only once the Mac took them, and only if
    /// the person has not typed on since.
    private func send(_ how: @escaping (String) async -> Bool) {
        let words = draft
        Task {
            if await how(words), draft == words { draft = "" }
        }
    }
}

/// A prompt queued behind the turn, and Take back.
private struct PendingLineView: View {
    let line: PendingLine
    let onTakeBack: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Image(systemName: "clock").foregroundStyle(Theme.inkSoft).accessibilityLabel("Queued:")
                Text(line.text).lineLimit(1)
                Spacer(minLength: 8)
                Button("Take back", action: onTakeBack)
            }
            if let note = line.note {
                Text(note).font(.caption).foregroundStyle(Theme.inkSoft).padding(.leading, 22)
            }
        }
        .font(.footnote)
        .padding(.horizontal, 4)
    }
}
