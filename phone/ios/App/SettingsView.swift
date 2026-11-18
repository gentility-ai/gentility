import DeskPhoneKit
import SwiftUI
import UIKit

/// Settings, reached from Home's menu: what this phone is paired as, and
/// the few things about that the person can change from here.
struct SettingsView: View {
    @ObservedObject var app: AppModel

    var body: some View {
        if let settings = app.settings {
            SettingsPage(settings: settings, version: Self.version, now: Date(), onPairAgain: { Task { await app.pairAgain() } })
        }
    }

    static var version: String {
        let info = Bundle.main.infoDictionary
        let short = info?["CFBundleShortVersionString"] as? String ?? "?"
        guard let build = info?["CFBundleVersion"] as? String else { return short }
        return "\(short) (\(build))"
    }
}

/// The page, from plain values.
struct SettingsPage: View {
    let settings: PhoneSettings
    let version: String
    let now: Date
    let onPairAgain: () -> Void
    @State private var asking = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Theme.Space.xl) {
                SettingsGroup("This phone") {
                    SettingsRow("Name") { Text(settings.name) }
                    SettingsRow("Network") {
                        HStack(spacing: 6) {
                            Circle().fill(dot).frame(width: 7, height: 7)
                            Text(settings.connection.words)
                        }
                    }
                    SettingsRow("Server") { Text(settings.server) }
                }

                SettingsGroup("Notifications", note: settings.notificationsOff
                    ? "Desk can't tell you when a thread needs you or finishes."
                    : "Desk tells you when a thread needs you or finishes.") {
                    SettingsRow("Alerts") {
                        Text(settings.notificationsOff ? "Off" : "On")
                            .foregroundStyle(settings.notificationsOff ? Theme.attention : Theme.inkSoft)
                    }
                    SettingsButton(settings.notificationsOff ? "Turn on in iOS Settings" : "Open iOS Settings") {
                        if let url = URL(string: UIApplication.openNotificationSettingsURLString) {
                            UIApplication.shared.open(url)
                        }
                    }
                }

                SettingsGroup("Pairing", note: "A pairing lasts 30 days. To remove this phone from your account, use Desk → Settings → Phone on your Mac.") {
                    SettingsRow("Runs out") {
                        VStack(alignment: .trailing, spacing: 2) {
                            Text(settings.expires.formatted(.dateTime.day().month(.abbreviated).year()))
                            Text(settings.left(now: now))
                                .font(.caption)
                                .foregroundStyle(settings.endsSoon(now: now) ? Theme.attention : Theme.inkFaint)
                        }
                    }
                    SettingsButton("Pair again", tone: Theme.bad) { asking = true }
                }

                SettingsGroup("About") {
                    SettingsRow("Version") { Text(version).monospacedDigit() }
                }
            }
            .padding(.horizontal, Theme.Space.l)
            .padding(.vertical, Theme.Space.l)
        }
        .background(Theme.ground)
        .navigationTitle("Settings")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("Pair this phone again?", isPresented: $asking, titleVisibility: .visible) {
            Button("Forget this pairing", role: .destructive, action: onPairAgain)
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("This phone forgets its pairing and opens the scanner. You'll need a new code from Desk on your Mac.")
        }
    }

    private var dot: Color {
        switch settings.connection {
        case .connected: return Theme.good
        case .connecting: return Theme.attention
        case .off: return Theme.inkFaint
        case .failed: return Theme.bad
        }
    }
}

/// A named group of rows on one surface, with a line of help under it.
private struct SettingsGroup<Rows: View>: View {
    let title: String
    let note: String?
    @ViewBuilder let rows: Rows

    init(_ title: String, note: String? = nil, @ViewBuilder rows: () -> Rows) {
        self.title = title
        self.note = note
        self.rows = rows()
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.s) {
            Text(title)
                .font(.footnote.weight(.semibold))
                .foregroundStyle(Theme.inkSoft)
                .padding(.horizontal, Theme.Space.xs)
                .accessibilityAddTraits(.isHeader)
            // Each row draws its own line above itself but the first.
            _VariadicView.Tree(Divided()) { rows }
                .surface(Theme.Corner.medium)
            if let note {
                Text(note)
                    .font(.footnote)
                    .foregroundStyle(Theme.inkFaint)
                    .padding(.horizontal, Theme.Space.xs)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

private struct Divided: _VariadicView_MultiViewRoot {
    func body(children: _VariadicView.Children) -> some View {
        VStack(spacing: 0) {
            ForEach(children) { child in
                if child.id != children.first?.id {
                    Rectangle().fill(Theme.hairline).frame(height: 1).padding(.leading, Theme.Space.l)
                }
                child
            }
        }
    }
}

private struct SettingsRow<Value: View>: View {
    let label: String
    @ViewBuilder let value: Value

    init(_ label: String, @ViewBuilder value: () -> Value) {
        self.label = label
        self.value = value()
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Theme.Space.m) {
            Text(label).foregroundStyle(Theme.ink)
            Spacer(minLength: Theme.Space.m)
            value
                .foregroundStyle(Theme.inkSoft)
                .multilineTextAlignment(.trailing)
        }
        .font(.subheadline)
        .padding(.horizontal, Theme.Space.l)
        .padding(.vertical, Theme.Space.m)
        .frame(minHeight: Theme.touch)
        .accessibilityElement(children: .combine)
    }
}

private struct SettingsButton: View {
    let title: String
    let tone: Color
    let action: () -> Void

    init(_ title: String, tone: Color = Theme.accent, action: @escaping () -> Void) {
        self.title = title
        self.tone = tone
        self.action = action
    }

    var body: some View {
        Button(action: action) {
            Text(title)
                .font(.subheadline)
                .foregroundStyle(tone)
                .frame(maxWidth: .infinity, minHeight: Theme.touch, alignment: .leading)
                .padding(.horizontal, Theme.Space.l)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}
