import DeskPhoneKit
import SwiftUI

/// The pinned approval: a short heading, what the call does, one button an
/// option. The heading is the request's kind (server ruling FR1); the call's
/// title, for Bash the whole command, is the detail, cut to a few lines with
/// the rest a tap away.
struct ApprovalCard: View {
    let request: PermissionRequest
    let detail: String?
    let card: Card
    let enabled: Bool
    let onAnswer: (Answer) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(request.summary, systemImage: "hand.raised.fill")
                .font(.subheadline.weight(.semibold)).foregroundStyle(Theme.attention)
            if let detail { CommandDetail(heading: request.summary, detail: detail) }
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 8) { buttons }
                VStack(spacing: 8) { buttons }
            }
            .controlSize(.large)
            .buttonBorderShape(.capsule)
            .disabled(!enabled || card.answer != nil || card.checking)
            CardFoot(card: card, chosen: chosen)
        }
        .cardStyle()
    }

    /// The option the person picked, while its answer is out or owed.
    private var chosen: String? {
        guard case .option(let id)? = card.answer, let option = request.options.first(where: { $0.id == id }) else { return nil }
        return "You chose \(option.label)"
    }

    @ViewBuilder private var buttons: some View {
        ForEach(request.options) { option in
            if option.kind == "allow_once" {
                Button { onAnswer(.option(option.id)) } label: { Text(option.label).lineLimit(1).fixedSize().frame(maxWidth: .infinity) }
                    .buttonStyle(.borderedProminent)
            } else {
                Button { onAnswer(.option(option.id)) } label: { Text(option.label).lineLimit(1).frame(maxWidth: .infinity) }
                    .buttonStyle(QuietButtonStyle(tone: option.rejects ? Theme.bad : Theme.inkSoft))
            }
        }
    }

}

/// What the call does, cut to four lines. Show all appears whenever the cut
/// hides anything: a copy laid out whole, unseen, is measured against the
/// lines drawn, so the tail of a command being approved is never out of reach.
private struct CommandDetail: View {
    let heading: String
    let detail: String
    @State private var drawn: CGFloat = 0
    @State private var whole: CGFloat = 0
    @State private var showingAll = false

    var body: some View {
        Text(detail)
            .font(.footnote.monospaced())
            .foregroundStyle(Theme.ink)
            .lineLimit(4)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(Theme.Space.m)
            .background(Theme.ground, in: RoundedRectangle(cornerRadius: Theme.Corner.small, style: .continuous))
            .background(alignment: .topLeading) {
                Text(detail)
                    .font(.footnote.monospaced())
                    .fixedSize(horizontal: false, vertical: true)
                    .hidden()
                    .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { whole = $0 }
            }
            .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { drawn = $0 }
        if whole > drawn + 1 {
            Button("Show all") { showingAll = true }
                .font(.footnote)
                .sheet(isPresented: $showingAll) { WholeDetail(heading: heading, detail: detail) }
        }
    }
}

/// The whole of a call's detail, selectable, in a sheet.
struct WholeDetail: View {
    let heading: String
    let detail: String
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                Text(detail)
                    .font(.footnote.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
            }
            .navigationTitle(heading)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
            }
        }
        .presentationDetents([.medium, .large])
    }
}

/// An agent's question or a server prompt's: a form, or a link. It takes the
/// composer's place.
struct QuestionCard: View {
    let card: Card
    let enabled: Bool
    /// The tallest the question's words and fields grow before they scroll;
    /// its buttons stay under them, always in sight.
    let cap: CGFloat
    let onAnswer: (Answer) -> Void
    @State private var draft: FormDraft
    @State private var tried = false

    init(card: Card, enabled: Bool, cap: CGFloat, onAnswer: @escaping (Answer) -> Void) {
        self.card = card
        self.enabled = enabled
        self.cap = cap
        self.onAnswer = onAnswer
        _draft = State(initialValue: FormDraft(fields: Self.fields(of: card.ask)))
    }

    private var request: Elicitation? {
        switch card.ask {
        case .elicitation(let request), .question(_, _, let request): request
        case .approval: nil
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let request {
                CappedScroll(cap: cap) {
                    Text(heading(request)).font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Text(request.message)
                    asked(request.mode)
                }
                actions(request.mode)
            }
            CardFoot(card: card, chosen: chosen)
        }
        .disabled(!enabled || card.answer != nil || card.checking)
        .cardStyle()
    }

    /// What is asked: the link, the form's fields, or why it is not here.
    @ViewBuilder private func asked(_ mode: ElicitationMode) -> some View {
        switch mode {
        case .url(let url, _):
            if let link = URL(string: url) {
                Link(destination: link) {
                    Label("Open the link", systemImage: "arrow.up.forward.square")
                }
            }
        case .form(_, let fields) where draft.answerableHere:
            let problems = tried ? draft.problems() : [:]
            ForEach(fields) { field in
                FieldView(field: field, draft: $draft, problem: problems[field.name])
            }
        default:
            Text("This question needs Desk on your Mac.").font(.footnote).foregroundStyle(.secondary)
        }
    }

    @ViewBuilder private func actions(_ mode: ElicitationMode) -> some View {
        switch mode {
        case .url:
            HStack(spacing: 8) {
                Button("Done") { onAnswer(.accept(nil)) }.buttonStyle(.borderedProminent)
                Button("Decline") { onAnswer(.decline) }.buttonStyle(QuietButtonStyle())
            }
        case .form where draft.answerableHere:
            HStack(spacing: 8) {
                Button("Submit") {
                    tried = true
                    if let content = draft.content() { onAnswer(.accept(content)) }
                }
                .buttonStyle(.borderedProminent)
                Button("Decline") { onAnswer(.decline) }.buttonStyle(QuietButtonStyle())
            }
        default:
            Button("Decline") { onAnswer(.decline) }.buttonStyle(QuietButtonStyle())
        }
    }

    /// What the person answered, while it is out or owed.
    private var chosen: String? {
        switch card.answer {
        case .accept?: "You answered"
        case .decline?: "You declined"
        case .option?, nil: nil
        }
    }

    private func heading(_ request: Elicitation) -> String {
        if case .url(_, let signIn?) = request.mode { return "Sign in to \(signIn)" }
        return "Question"
    }

    static func fields(of ask: Ask) -> [FormField] {
        switch ask {
        case .elicitation(let request), .question(_, _, let request):
            if case .form(_, let fields) = request.mode { return fields }
            return []
        case .approval:
            return []
        }
    }
}

private struct FieldView: View {
    let field: FormField
    @Binding var draft: FormDraft
    let problem: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            switch field.kind {
            case .boolean:
                Toggle(field.label, isOn: Binding(get: { draft.flags[field.name] ?? false }, set: { draft.flags[field.name] = $0 }))
                    .font(.subheadline)
            case .choice(let options, _):
                label
                ForEach(options, id: \.value) { option in
                    let picked = draft.picks[field.name] == option.value
                    Button { draft.picks[field.name] = option.value } label: {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            Image(systemName: picked ? "largecircle.fill.circle" : "circle")
                                .foregroundStyle(picked ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
                            Text(option.title).foregroundStyle(.primary)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .padding(.vertical, 2)
                    .accessibilityAddTraits(picked ? .isSelected : [])
                }
            case .number(let integer, _, _, _):
                label
                TextField(field.description ?? "", text: text)
                    .keyboardType(integer ? .numberPad : .decimalPad)
                    .textFieldStyle(.roundedBorder)
            case .text, .unknown:
                label
                if field.secret {
                    SecureField(field.description ?? "", text: text).textFieldStyle(.roundedBorder)
                } else {
                    TextField(field.description ?? "", text: text, axis: .vertical)
                        .lineLimit(1...4)
                        .textFieldStyle(.roundedBorder)
                }
            case .multiChoice:
                EmptyView()
            }
            if let problem { Text(problem).font(.caption).foregroundStyle(.red) }
        }
    }

    private var label: some View {
        Text(field.required ? "\(field.label) *" : field.label).font(.footnote).foregroundStyle(.secondary)
    }

    private var text: Binding<String> {
        Binding(get: { draft.texts[field.name] ?? "" }, set: { draft.texts[field.name] = $0 })
    }
}

/// Its content at its own height up to `cap`, then scrolling within it.
private struct CappedScroll<Content: View>: View {
    let cap: CGFloat
    @ViewBuilder let content: Content
    @State private var height: CGFloat = 0

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 10) { content }
                .frame(maxWidth: .infinity, alignment: .leading)
                .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height = $0 }
        }
        .frame(height: min(height, cap))
        .scrollBounceBehavior(.basedOnSize)
        .scrollIndicatorsFlash(onAppear: true)
    }
}

/// Under a card: what the person answered, its answer on the way, or a line
/// from the Mac.
private struct CardFoot: View {
    let card: Card
    let chosen: String?

    var body: some View {
        if let chosen {
            Label(chosen, systemImage: "checkmark.circle.fill").font(.footnote.weight(.semibold))
        }
        if card.sending || card.checking { ProgressView().controlSize(.small) }
        if let note = card.note { Text(note).font(.footnote).foregroundStyle(.orange) }
    }
}

extension View {
    func cardStyle() -> some View {
        padding(Theme.Space.l)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Theme.raised, in: RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous).strokeBorder(Theme.attention.opacity(0.55)))
    }
}
