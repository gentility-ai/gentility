import DeskPhoneKit
import SwiftUI

/// F1, F2, F3 and F5, in the storyboard's shape: what happened in a banner,
/// what to do under it, and one way forward.
struct FailureView: View {
    let failure: Failure
    let onAction: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(failure.title).font(.title2.weight(.semibold))
            if let first = failure.lines.first {
                Banner(text: first, tone: tone)
            }
            ForEach(failure.lines.dropFirst(), id: \.self) { line in
                Text(line)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: .infinity)
            }
            Button(action: onAction) {
                Text(failure.action).foregroundStyle(Theme.onAccent).frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
            Spacer()
        }
        .padding()
    }

    private var tone: Banner.Tone {
        switch failure {
        case .codeSpent, .unreachable, .lapsed: return .warn
        case .denied, .cannotJoin, .removed, .notJoined: return .bad
        }
    }
}

/// A line on a tinted ground: done, a warning, or a refusal.
struct Banner: View {
    enum Tone {
        case ok, warn, bad

        var color: Color {
            switch self {
            case .ok: return .green
            case .warn: return .orange
            case .bad: return .red
            }
        }
    }

    let text: String
    let tone: Tone
    var foreground: Color? = nil

    var body: some View {
        Text(text)
            .foregroundStyle(foreground ?? tone.color)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(12)
            .background(tone.color.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
    }
}
