import DeskPhoneKit
import SwiftUI

/// An agent's words as Markdown blocks (Kit's `Markdown`), with the writing
/// cursor after the last of them while the turn runs.
struct MarkdownView: View {
    let text: String
    var cursor = false

    var body: some View {
        let blocks = Markdown.blocks(text)
        VStack(alignment: .leading, spacing: Theme.Space.m) {
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
            inline(text).font(.body).lineSpacing(3).foregroundStyle(Theme.ink).textSelection(.enabled)
        case .heading(let level, let text):
            inline(text)
                .font(level == 1 ? .title3.weight(.semibold) : level == 2 ? .headline : .subheadline.weight(.semibold))
                .foregroundStyle(Theme.ink)
                .padding(.top, level == 1 ? 4 : 2)
                .accessibilityAddTraits(.isHeader)
        case .list(let items):
            VStack(alignment: .leading, spacing: 6) {
                ForEach(Array(items.enumerated()), id: \.offset) { _, item in
                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                        Text(item.marker).foregroundStyle(Theme.inkFaint).monospacedDigit()
                        Text(Markdown.inline(item.text)).lineSpacing(3).foregroundStyle(Theme.ink).textSelection(.enabled)
                    }
                    .padding(.leading, CGFloat(item.level) * 16)
                }
            }
        case .quote(let text):
            HStack(alignment: .top, spacing: 8) {
                RoundedRectangle(cornerRadius: 1).fill(Theme.hairline).frame(width: 2)
                inline(text).lineSpacing(3).foregroundStyle(Theme.inkSoft).textSelection(.enabled)
            }
            .fixedSize(horizontal: false, vertical: true)
        case .code(let language, let text, _):
            CodeBlock(language: language, code: text)
        case .table(let source):
            if let table = MarkdownTable(source: source) {
                TableView(table: table)
            } else {
                CodeBlock(language: nil, code: source)
            }
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
                if let language { Text(language).font(.caption2.weight(.medium)).foregroundStyle(Theme.inkFaint) }
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
                .frame(minWidth: 32, minHeight: 28, alignment: .trailing)
                .contentShape(Rectangle())
                .foregroundStyle(Theme.inkSoft)
                .accessibilityLabel(copied ? "Copied" : "Copy code")
            }
            ScrollView(.horizontal, showsIndicators: false) {
                Text(code)
                    .font(.system(.footnote, design: .monospaced))
                    .lineSpacing(2)
                    .foregroundStyle(Theme.ink)
                    .textSelection(.enabled)
                    .fixedSize(horizontal: true, vertical: false)
            }
        }
        .padding(.horizontal, Theme.Space.m).padding(.top, 6).padding(.bottom, Theme.Space.m)
        .surface(Theme.Corner.medium)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(language.map { "\($0) code" } ?? "Code"): \(code)")
    }
}

/// A table as a grid, its header set apart, scrolling sideways where it is
/// wider than the page.
private struct TableView: View {
    let table: MarkdownTable

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            Grid(alignment: .leading, horizontalSpacing: 0, verticalSpacing: 0) {
                GridRow {
                    ForEach(Array(table.header.enumerated()), id: \.offset) { _, cell in
                        self.cell(cell).font(.footnote.weight(.semibold)).foregroundStyle(Theme.inkSoft)
                    }
                }
                .background(Theme.raised)
                ForEach(Array(table.rows.enumerated()), id: \.offset) { _, row in
                    Rectangle().fill(Theme.hairline).frame(height: 1).gridCellUnsizedAxes(.horizontal)
                    GridRow {
                        ForEach(Array(row.enumerated()), id: \.offset) { _, cell in
                            self.cell(cell).font(.subheadline).foregroundStyle(Theme.ink)
                        }
                    }
                }
            }
            .clipShape(RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous).strokeBorder(Theme.hairline))
        }
        .accessibilityElement(children: .contain)
    }

    private func cell(_ text: String) -> some View {
        Text(Markdown.inline(text))
            .textSelection(.enabled)
            .frame(maxWidth: 220, alignment: .leading)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.horizontal, Theme.Space.m).padding(.vertical, Theme.Space.s)
    }
}
