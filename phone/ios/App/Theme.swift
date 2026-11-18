import SwiftUI
import UIKit

/// The app's colours, spacing, corners and motion, each named for its job.
/// A view names a token, never a literal, so the look changes in one place.
enum Theme {
    // MARK: grounds, back to front

    /// The page.
    static let ground = dynamic(light: 0xFFFFFF, dark: 0x0E0F11)
    /// A surface resting on the page: the person's words, a card, a code block.
    static let raised = dynamic(light: 0xF4F4F6, dark: 0x191A1E)
    /// A control cut into the page: the message field, a quiet button.
    static let field = dynamic(light: 0xF0F0F3, dark: 0x1F2025)
    /// The line between two things, and around a surface.
    static let hairline = dynamic(light: 0x000000, dark: 0xFFFFFF, lightAlpha: 0.09, darkAlpha: 0.09)

    // MARK: ink

    static let ink = dynamic(light: 0x16171A, dark: 0xF2F3F5)
    /// What supports the words: a caption, a tool call.
    static let inkSoft = dynamic(light: 0x5C5F66, dark: 0xA4A7AE)
    /// What can be found but does not ask to be read: a count, a chevron.
    static let inkFaint = dynamic(light: 0x8A8D94, dark: 0x70737B)

    // MARK: meaning

    /// What the person can do: the send button, a link, a picked option.
    static let accent = dynamic(light: 0x4F5BD5, dark: 0x7C87F2)
    /// Something waits on the person.
    static let attention = dynamic(light: 0xC2710C, dark: 0xF0A24B)
    static let good = dynamic(light: 0x1F8A4C, dark: 0x4CC38A)
    static let bad = dynamic(light: 0xD1343F, dark: 0xF2686F)

    // MARK: measure

    enum Space {
        static let xs: CGFloat = 4
        static let s: CGFloat = 8
        static let m: CGFloat = 12
        static let l: CGFloat = 16
        static let xl: CGFloat = 24
    }

    enum Corner {
        /// A chip, a tool row.
        static let small: CGFloat = 8
        /// A code block, a banner.
        static let medium: CGFloat = 12
        /// A card, a bubble, the message field.
        static let large: CGFloat = 18
    }

    /// The least a finger needs.
    static let touch: CGFloat = 44

    /// How things here open, close and arrive.
    static let motion = Animation.snappy(duration: 0.22)

    private static func dynamic(light: Int, dark: Int, lightAlpha: CGFloat = 1, darkAlpha: CGFloat = 1) -> Color {
        Color(UIColor { traits in
            let dim = traits.userInterfaceStyle == .dark
            return UIColor(hex: dim ? dark : light, alpha: dim ? darkAlpha : lightAlpha)
        })
    }
}

private extension UIColor {
    convenience init(hex: Int, alpha: CGFloat) {
        self.init(
            red: CGFloat((hex >> 16) & 0xff) / 255, green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: alpha
        )
    }
}

extension View {
    /// A surface resting on the page, with its edge.
    func surface(_ corner: CGFloat = Theme.Corner.large, fill: Color = Theme.raised) -> some View {
        background(fill, in: RoundedRectangle(cornerRadius: corner, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: corner, style: .continuous).strokeBorder(Theme.hairline))
    }
}
