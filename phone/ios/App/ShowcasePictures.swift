#if DEBUG
import DeskPhoneKit
import SwiftUI

/// The photo and Markdown screens (`DESK_PHONE_SHOWCASE=<screen>`): images
/// drawn here, read through an `ImageStore` whose loader is these pictures,
/// so nothing dials.
struct PictureShowcase: View {
    let screen: String

    static let root = "/Users/someone/wt/fix-login"
    static let ready = UUID(), uploading = UUID(), failed = UUID()

    var body: some View {
        Group {
            switch screen {
            case "attach-menu":
                conversation(acting(menuShown: true, draft: ""))
            case "attach-tray":
                conversation(acting(tray: Self.tray, draft: "The login button overlaps the field here"))
            case "attach-failed":
                conversation(acting(tray: Self.tray, draft: "The login button overlaps the field here", failureShown: Self.failed))
            case "attach-old-mac":
                conversation(acting(canAttach: false, draft: ""))
            case "transcript-images":
                conversation(acting(), turns: Self.imageTurns)
            case "transcript-markdown":
                conversation(acting(), turns: Self.markdownTurns)
            case "transcript-long":
                // Opens at the top of a long thread mid-turn: the jump arrow
                // and ⏎ that steers.
                conversation(acting(working: true), turns: Self.markdownTurns + Self.imageTurns, atEnd: false)
            case "strip-scrolled":
                // Scrolled up mid-turn: the strip and the jump are one.
                conversation(acting(working: true, pending: [PendingLine(queueID: "q", text: "then run the store tests")]),
                             turns: Self.markdownTurns + Self.imageTurns, atEnd: false,
                             strip: StatusStrip(phase: .open, held: true, working: true, since: Date(timeIntervalSinceNow: -185), queued: 1))
            case "transcript-streaming":
                conversation(acting(working: true), turns: Self.streamingTurns)
            case "transcript-remote-image":
                conversation(acting(), turns: Self.remoteTurns)
            case "image-viewer":
                ImageViewer(pages: [ViewerPage(request: .worktree(sessionID: "s", path: "shots/login.png"), label: "The login screen",
                                               caption: "After the fix: the button sits under the field")],
                            start: .worktree(sessionID: "s", path: "shots/login.png"))
            default:
                Text("No showcase named \(screen)")
            }
        }
        .environment(\.images, ImageContext(store: Self.store, sessionID: "s", worktreeRoot: Self.root, open: true))
    }

    private func acting(
        working: Bool = false, pending: [PendingLine] = [], tray: AttachmentTray = AttachmentTray(), canAttach: Bool = true,
        menuShown: Bool = false, draft: String = "", failureShown: UUID? = nil
    ) -> Acting {
        Acting(open: true, working: working, card: nil, detail: nil, cleared: nil, pending: pending, note: nil, sending: false,
               tray: tray, canAttach: canAttach, draft: draft, menuShown: menuShown, failureShown: failureShown)
    }

    private func conversation(_ acting: Acting, turns: [Turn] = Showcase.turns, atEnd: Bool = true, strip: StatusStrip? = nil) -> some View {
        NavigationStack(path: .constant(NavigationPath(["s"]))) {
            Color.clear
                .navigationTitle("Home")
                .navigationDestination(for: String.self) { _ in
                    TranscriptScreen(turns: turns, phase: .open, asleepLines: [], acting: acting, strip: strip, onRetry: {}, atEnd: atEnd)
                        .threadChrome(title: "Fix the login layout", branch: "fix-login-layout", machine: nil, menu: HomeModel.menu(for: Showcase.rows[2]), onAct: { _ in })
                }
        }
    }

    // MARK: the pictures

    static let store = ImageStore { request in
        switch request {
        case .worktree(_, "shots/slow.png"):
            try await Task.sleep(for: .seconds(3600))
            throw ImageLoadError.failed
        case .worktree(_, "shots/missing.png"):
            throw ImageLoadError.notFound
        case .attachment(let path):
            return picture(path.hasSuffix("2.jpg") ? 1 : 0, size: CGSize(width: 1200, height: 900))
        case .worktree:
            return screenshot()
        default:
            return picture(2, size: CGSize(width: 1200, height: 800))
        }
    }

    static let palettes: [[UIColor]] = [
        [.systemIndigo, .systemTeal],
        [.systemOrange, .systemPink],
        [.systemGreen, .systemBlue],
    ]

    /// A photo-like picture: a gradient with a sun and hills.
    static func picture(_ palette: Int, size: CGSize) -> Data {
        let colors = palettes[palette % palettes.count]
        return UIGraphicsImageRenderer(size: size).jpegData(withCompressionQuality: 0.85) { context in
            let cg = context.cgContext
            let gradient = CGGradient(colorsSpace: nil, colors: colors.map(\.cgColor) as CFArray, locations: [0, 1])!
            cg.drawLinearGradient(gradient, start: .zero, end: CGPoint(x: 0, y: size.height), options: [])
            UIColor.white.withAlphaComponent(0.85).setFill()
            cg.fillEllipse(in: CGRect(x: size.width * 0.62, y: size.height * 0.18, width: size.width * 0.16, height: size.width * 0.16))
            UIColor.black.withAlphaComponent(0.25).setFill()
            let hills = UIBezierPath()
            hills.move(to: CGPoint(x: 0, y: size.height * 0.75))
            hills.addQuadCurve(to: CGPoint(x: size.width * 0.5, y: size.height * 0.7), controlPoint: CGPoint(x: size.width * 0.25, y: size.height * 0.5))
            hills.addQuadCurve(to: CGPoint(x: size.width, y: size.height * 0.68), controlPoint: CGPoint(x: size.width * 0.8, y: size.height * 0.85))
            hills.addLine(to: CGPoint(x: size.width, y: size.height))
            hills.addLine(to: CGPoint(x: 0, y: size.height))
            hills.fill()
        }
    }

    /// A phone screenshot of a login form, as an agent's worktree image.
    static func screenshot() -> Data {
        let size = CGSize(width: 900, height: 600)
        return UIGraphicsImageRenderer(size: size).pngData { _ in
            UIColor(white: 0.97, alpha: 1).setFill()
            UIRectFill(CGRect(origin: .zero, size: size))
            UIColor.systemBlue.setFill()
            UIRectFill(CGRect(x: 0, y: 0, width: size.width, height: 90))
            let title: [NSAttributedString.Key: Any] = [.font: UIFont.boldSystemFont(ofSize: 40), .foregroundColor: UIColor.white]
            ("Sign in" as NSString).draw(at: CGPoint(x: 40, y: 22), withAttributes: title)
            for (at, label) in ["Email", "Password"].enumerated() {
                let y = 140 + CGFloat(at) * 130
                let font: [NSAttributedString.Key: Any] = [.font: UIFont.systemFont(ofSize: 26), .foregroundColor: UIColor.darkGray]
                (label as NSString).draw(at: CGPoint(x: 60, y: y), withAttributes: font)
                UIColor.white.setFill()
                let field = UIBezierPath(roundedRect: CGRect(x: 60, y: y + 38, width: 780, height: 64), cornerRadius: 12)
                field.fill()
                UIColor.lightGray.setStroke()
                field.stroke()
            }
            UIColor.systemBlue.setFill()
            UIBezierPath(roundedRect: CGRect(x: 60, y: 430, width: 780, height: 72), cornerRadius: 14).fill()
            let button: [NSAttributedString.Key: Any] = [.font: UIFont.boldSystemFont(ofSize: 28), .foregroundColor: UIColor.white]
            ("Continue" as NSString).draw(at: CGPoint(x: 390, y: 448), withAttributes: button)
        }
    }

    static func thumbnail(_ palette: Int) -> Data {
        ImagePrep.thumbnail(picture(palette, size: CGSize(width: 400, height: 300))) ?? Data()
    }

    static var tray: AttachmentTray {
        var tray = AttachmentTray()
        let image = { (palette: Int) in
            PreparedImage(name: "IMG_\(palette).jpg", mime: "image/jpeg", data: picture(palette, size: CGSize(width: 800, height: 600)),
                          thumbnail: thumbnail(palette), width: 800, height: 600)
        }
        tray.add(ready)
        tray.prepared(ready, image(0))
        tray.uploaded(ready, AttachmentRef(path: "/att/IMG_0.jpg", name: "IMG_0.jpg", kind: .image, size: 48_213))
        tray.add(uploading)
        tray.prepared(uploading, image(1))
        tray.add(failed)
        tray.prepared(failed, image(2))
        tray.failed(failed, .unreachable)
        return tray
    }

    // MARK: the turns

    static let imageTurns = [Turn(number: 1, lines: [
        .you(id: "u", text: "The login button overlaps the password field on my phone. Here's what I see.", attachments: [
            AttachmentRef(path: "/att/a/IMG_1.jpg", name: "IMG_1.jpg", kind: .image, size: 812_004, mime: "image/jpeg"),
            AttachmentRef(path: "/att/b/IMG_2.jpg", name: "IMG_2.jpg", kind: .image, size: 790_118, mime: "image/jpeg"),
            AttachmentRef(path: "/Users/someone/Desktop/crash.log", name: "crash.log", kind: .file, size: 18_342),
        ]),
        .tool(id: "t", text: "✓ Edit src/screens/Login.tsx"),
        .agent(id: "a", text: """
        Fixed: the button now sits under the field with 16 pt between them.

        ![The login screen](\(root)/shots/login.png "After the fix: the button sits under the field")

        The before shot is still rendering:

        ![Before the fix](shots/slow.png)

        ![An older capture](shots/missing.png)
        """),
    ])]

    static let markdownTurns = [Turn(number: 1, lines: [
        .you(id: "u", text: "What changed?"),
        .agent(id: "a", text: """
        ## What changed

        The login form now lays out with **one stack** instead of two, so the button can't overlap the field. See [the spec](https://example.com/spec).

        - Moved the button under the fields
        - Dropped the fixed `height: 320`
          - it clipped on small phones
        - [x] Ran the snapshot tests

        1. Rebuild the bundle
        2. Reload the simulator

        > Keyboard avoidance is unchanged; the stack scrolls when it opens.

        ```swift
        VStack(spacing: 16) {
            EmailField()
            PasswordField()
            ContinueButton()
        }
        ```

        | Screen | Before | After |
        |---|---|---|
        | iPhone SE | clipped | fits |
        | iPhone 17 | overlaps | fits |
        """),
    ])]

    static let streamingTurns = [Turn(number: 1, lines: [
        .you(id: "u", text: "Show me the new layout code"),
        .agent(id: "a", text: """
        Here is the stack as it stands:

        ```swift
        VStack(spacing: 16) {
            EmailField()
            PasswordField()
        """),
    ])]

    static let remoteTurns = [Turn(number: 1, lines: [
        .you(id: "u", text: "What does the design say?"),
        .agent(id: "a", text: """
        The design file's frame for this screen:

        ![Login frame from the design file](https://images.example.com/login-frame.png)

        It puts the button 16 pt under the last field, which is what the fix does.
        """),
    ])]
}
#endif
