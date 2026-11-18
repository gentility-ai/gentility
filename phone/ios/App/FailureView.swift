import DeskPhoneKit
import SwiftUI

/// F1, F2, F3 and F5: what happened, what to do about it, and one way
/// forward at the foot of the page where a thumb rests.
struct FailureView: View {
    let failure: Failure
    let onAction: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.l) {
            Image(systemName: glyph)
                .font(.title2.weight(.medium))
                .foregroundStyle(tone)
                .frame(width: 52, height: 52)
                .background(tone.opacity(0.14), in: RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
                .accessibilityHidden(true)
            Text(failure.title).font(.title2.weight(.semibold)).foregroundStyle(Theme.ink)
            VStack(alignment: .leading, spacing: Theme.Space.s) {
                ForEach(Array(failure.lines.enumerated()), id: \.offset) { at, line in
                    Text(line).foregroundStyle(at == 0 ? Theme.ink : Theme.inkSoft)
                }
            }
            Spacer(minLength: 0)
            Button(failure.action, action: onAction).buttonStyle(PageButtonStyle())
        }
        .page()
    }

    /// Amber for what waiting or scanning again mends, red for a refusal.
    private var tone: Color {
        switch failure {
        case .codeSpent, .unreachable, .lapsed: return Theme.attention
        case .denied, .cannotJoin, .removed, .notJoined: return Theme.bad
        }
    }

    private var glyph: String {
        switch failure {
        case .codeSpent, .lapsed: return "clock.badge.exclamationmark"
        case .unreachable: return "wifi.exclamationmark"
        case .denied, .removed: return "hand.raised"
        case .cannotJoin, .notJoined: return "exclamationmark.triangle"
        }
    }
}
