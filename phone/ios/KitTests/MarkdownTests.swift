import XCTest
@testable import DeskPhoneKit

final class MarkdownTests: XCTestCase {
    func testParagraphsSplitOnBlankLinesAndKeepTheirBreaks() {
        XCTAssertEqual(Markdown.blocks("one\ntwo\n\nthree"), [.paragraph("one\ntwo"), .paragraph("three")])
    }

    func testHeadingsDeeperThanThreeDrawAsThree() {
        XCTAssertEqual(Markdown.blocks("# Plan\n## Steps\n#### Detail ##"), [
            .heading(level: 1, text: "Plan"), .heading(level: 2, text: "Steps"), .heading(level: 3, text: "Detail"),
        ])
        XCTAssertEqual(Markdown.blocks("#hashtag"), [.paragraph("#hashtag")])
    }

    func testBulletAndNumberedLists() {
        XCTAssertEqual(Markdown.blocks("- one\n* two\n  - nested\n      - deeper\n\n1. first\n2) second"), [
            .list([
                MarkdownListItem(marker: "•", level: 0, text: "one"),
                MarkdownListItem(marker: "•", level: 0, text: "two"),
                MarkdownListItem(marker: "•", level: 1, text: "nested"),
                MarkdownListItem(marker: "•", level: 1, text: "deeper"),
            ]),
            .list([
                MarkdownListItem(marker: "1.", level: 0, text: "first"),
                MarkdownListItem(marker: "2.", level: 0, text: "second"),
            ]),
        ])
    }

    func testTaskItemsAreBoxes() {
        XCTAssertEqual(Markdown.blocks("- [ ] todo\n- [x] done"), [.list([
            MarkdownListItem(marker: "•", level: 0, text: "☐ todo"),
            MarkdownListItem(marker: "•", level: 0, text: "☑ done"),
        ])])
    }

    func testAQuote() {
        XCTAssertEqual(Markdown.blocks("> careful\n> here\n\nafter"), [.quote("careful\nhere"), .paragraph("after")])
    }

    func testAFencedCodeBlockKeepsItsLanguage() {
        XCTAssertEqual(Markdown.blocks("look:\n```swift\nlet a = 1\n\nlet b = 2\n```\ndone"), [
            .paragraph("look:"),
            .code(language: "swift", text: "let a = 1\n\nlet b = 2", closed: true),
            .paragraph("done"),
        ])
    }

    func testAnUnclosedFenceIsCodeToTheEnd() {
        XCTAssertEqual(Markdown.blocks("Here:\n```\nfn main() {\n  # not a heading"), [
            .paragraph("Here:"),
            .code(language: nil, text: "fn main() {\n  # not a heading", closed: false),
        ])
    }

    func testARule() {
        XCTAssertEqual(Markdown.blocks("above\n\n---\n\n* * *"), [.paragraph("above"), .rule, .rule])
    }

    func testAnImageLineWithAndWithoutATitle() {
        XCTAssertEqual(Markdown.blocks(#"![The login screen](/wt/shots/login.png "After the fix")"#), [
            .image(alt: "The login screen", src: "/wt/shots/login.png", title: "After the fix"),
        ])
        XCTAssertEqual(Markdown.blocks("![diagram](docs/flow.png)"), [.image(alt: "diagram", src: "docs/flow.png", title: nil)])
        XCTAssertEqual(Markdown.blocks("![Image output](</tmp/a b.png>)"), [.image(alt: "Image output", src: "/tmp/a b.png", title: nil)])
    }

    func testAnImageInsideWordsStaysInTheParagraph() {
        XCTAssertEqual(Markdown.blocks("see ![x](a.png) here"), [.paragraph("see ![x](a.png) here")])
    }

    func testATableFallsToCode() {
        XCTAssertEqual(Markdown.blocks("| a | b |\n|---|:-:|\n| 1 | 2 |\n\nafter"), [
            .table("| a | b |\n|---|:-:|\n| 1 | 2 |"), .paragraph("after"),
        ])
    }

    func testHTMLIsText() {
        XCTAssertEqual(Markdown.blocks("<div>hi</div>"), [.paragraph("<div>hi</div>")])
    }

    func testALinkInsideBold() {
        let text = Markdown.inline("**read [the spec](https://example.com/spec)** now")
        XCTAssertEqual(String(text.characters), "read the spec now")
        let link = text.runs.first { $0.link != nil }
        XCTAssertEqual(link?.link, URL(string: "https://example.com/spec"))
        XCTAssertEqual(String(text[link!.range].characters), "the spec")
        XCTAssertEqual(link?.inlinePresentationIntent?.contains(.stronglyEmphasized), true)
    }

    func testInlineCodeAndStrikethrough() {
        let text = Markdown.inline("run `make` ~~not this~~")
        XCTAssertEqual(String(text.characters), "run make not this")
        XCTAssertTrue(text.runs.contains { $0.inlinePresentationIntent?.contains(.code) == true })
        XCTAssertTrue(text.runs.contains { $0.inlinePresentationIntent?.contains(.strikethrough) == true })
    }

    func testInlineKeepsLineBreaks() {
        XCTAssertEqual(String(Markdown.inline("one\ntwo").characters), "one\ntwo")
    }
}
