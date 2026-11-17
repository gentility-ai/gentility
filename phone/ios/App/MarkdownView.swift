import DeskPhoneKit
import SwiftUI

/// An agent's words as Markdown blocks (Kit's `Markdown`), with the writing
/// cursor after the last of them while the turn runs.
struct MarkdownView: View {
    let text: String
    var cursor = false

    var body: some View {
        let blocks = Markdown.blocks(text)
        VStack(alignment: .leading, spacing: 10) {
            ForEach(Array(blocks.enumerated()), id: \.offset) { at, block in
                BlockView(block: block, cursor: cursor && at == blocks.count - 1)
            }
            if cursor, !(blocks.last.map(Self.takesCursor) ?? false) {
                Text("▍").foregroundStyle(.tint)
            }
        }
    }

    /// The blocks the cursor sits at the end of, rather than under.
    static func takesCursor(_ block: MarkdownBlock) -> Bool {
        switch block {
        case .paragraph, .heading, .quote: true
        default: false
        }
    }
}

private struct BlockView: View {
    let block: MarkdownBlock
    let cursor: Bool

    var body: some View {
        switch block {
        case .paragraph(let text):
            inline(text).font(.body).textSelection(.enabled)
        case .heading(let level, let text):
            inline(text)
                .font(level == 1 ? .title3.bold() : level == 2 ? .headline : .subheadline.bold())
                .accessibilityAddTraits(.isHeader)
        case .list(let items):
            VStack(alignment: .leading, spacing: 4) {
                ForEach(Array(items.enumerated()), id: \.offset) { _, item in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Text(item.marker).foregroundStyle(.secondary).monospacedDigit()
                        Text(Markdown.inline(item.text)).textSelection(.enabled)
                    }
                    .padding(.leading, CGFloat(item.level) * 16)
                }
            }
        case .quote(let text):
            HStack(alignment: .top, spacing: 8) {
                RoundedRectangle(cornerRadius: 1.5).fill(.secondary).frame(width: 3)
                inline(text).foregroundStyle(.secondary).textSelection(.enabled)
            }
            .fixedSize(horizontal: false, vertical: true)
        case .code(let language, let text, _):
            CodeBlock(language: language, code: text)
        case .table(let source):
            CodeBlock(language: "table", code: source)
        case .rule:
            Divider().padding(.vertical, 2)
        case .image(let alt, let src, let title):
            TranscriptImage(src: src, alt: alt, title: title)
        }
    }

    private func inline(_ text: String) -> Text {
        let words = Text(Markdown.inline(text))
        return cursor ? Text("\(words)\(Text("▍").foregroundStyle(.tint))") : words
    }
}

/// Monospaced, scrolling sideways, with its language and a copy button.
private struct CodeBlock: View {
    let language: String?
    let code: String
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                if let language { Text(language).font(.caption2).foregroundStyle(.secondary) }
                Spacer()
                Button {
                    UIPasteboard.general.string = code
                    copied = true
                    Task {
                        try? await Task.sleep(for: .seconds(1.5))
                        copied = false
                    }
                } label: {
                    if copied {
                        Text("Copied").font(.caption2.weight(.semibold))
                    } else {
                        Image(systemName: "doc.on.doc").font(.caption)
                    }
                }
                .foregroundStyle(.secondary)
                .accessibilityLabel(copied ? "Copied" : "Copy code")
            }
            ScrollView(.horizontal, showsIndicators: false) {
                Text(code)
                    .font(.system(.footnote, design: .monospaced))
                    .textSelection(.enabled)
                    .fixedSize(horizontal: true, vertical: false)
            }
        }
        .padding(10)
        .background(Color(.tertiarySystemBackground), in: RoundedRectangle(cornerRadius: 8))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(language.map { "\($0) code" } ?? "Code"): \(code)")
    }
}
