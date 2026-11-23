import DeskPhoneKit
import SwiftUI

struct PairingView: View {
    @ObservedObject var app: AppModel
    @ObservedObject var pairing: PairingFlow
    @ObservedObject var scanner: ScannerModel
    @State private var name = PhoneName.current(device: UIDevice.current.name)

    var body: some View {
        switch pairing.state {
        case .scanning:
            ScannerScreen(state: scanner.state, name: $name, onCode: scanner.read)
                .task {
                    scanner.onLink = { link in
                        PhoneName.remember(name)
                        let phoneName = PhoneName.current(device: UIDevice.current.name)
                        Task { await app.scanned(link, phoneName: phoneName) }
                    }
                    await scanner.appear()
                    #if DEBUG
                    DebugClaim.take { scanner.onLink?($0) }
                    #endif
                }
        case .claiming(let host):
            Waiting(line: "Asking \(host)…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Theme.ground)
        case .number(let machineLabel, let match, let host):
            // No Cancel: calling the poll off here would leave the Mac asking
            // with nobody to hear its Allow.
            NumberScreen(machineLabel: machineLabel, match: match, host: host)
        case .joining(let machineLabel):
            JoiningScreen(machineLabel: machineLabel)
        case .paired:
            ProgressView()
        case .failed(let failure):
            FailureView(failure: failure) { app.again() }
        }
    }
}

#if DEBUG
/// A worktree run in the simulator, which has no camera: the link in
/// `DESK_PHONE_CLAIM_URL` is taken as though scanned, once per process.
/// Never logged: the link carries the code.
@MainActor
enum DebugClaim {
    private static var taken = false

    static func take(_ onLink: (ClaimLink) -> Void) {
        guard !taken, let text = ProcessInfo.processInfo.environment["DESK_PHONE_CLAIM_URL"] else { return }
        taken = true
        guard let link = ClaimLink.parse(text) else {
            Log.pairing.error("DESK_PHONE_CLAIM_URL is not a Desk link")
            return
        }
        Log.pairing.info("taking the Desk link from the environment, for \(link.host, privacy: .public)")
        onLink(link)
    }
}
#endif

/// Scan a pairing code.
struct ScannerScreen: View {
    let state: ScannerModel.State
    @Binding var name: String
    /// False in the showcase, which draws the camera's place and opens none.
    var camera = true
    let onCode: (String) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.xl) {
            PageHead(title: "Pair with your Mac", line: "In Desk on your Mac, open Settings, then Phone, and point this phone at the code.")
            ZStack {
                Color.black
                switch state {
                case .denied:
                    VStack(spacing: Theme.Space.m) {
                        Image(systemName: "camera").font(.title).foregroundStyle(.white.opacity(0.6))
                        Text("GenDesk needs the camera to read the code on your Mac.")
                            .multilineTextAlignment(.center)
                            .foregroundStyle(.white)
                        Button {
                            if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                        } label: {
                            Text("Open Settings").foregroundStyle(Theme.onAccent)
                        }
                        .buttonStyle(PageButtonStyle(wide: false))
                    }
                    .padding(Theme.Space.xl)
                case .asking:
                    ProgressView().tint(.white)
                case .scanning, .notADeskCode:
                    if camera { CameraView(onCode: onCode) }
                    Reticle().stroke(.white, style: StrokeStyle(lineWidth: 3, lineCap: .round, lineJoin: .round))
                        .frame(width: 208, height: 208)
                    if state == .notADeskCode {
                        Text("Not a Desk code")
                            .font(.subheadline.weight(.medium))
                            .foregroundStyle(.white)
                            .padding(.horizontal, Theme.Space.m).padding(.vertical, 6)
                            .background(.black.opacity(0.6), in: Capsule())
                            .frame(maxHeight: .infinity, alignment: .bottom)
                            .padding(.bottom, Theme.Space.l)
                    }
                }
            }
            .frame(maxWidth: .infinity)
            .aspectRatio(1, contentMode: .fit)
            .clipShape(RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous).strokeBorder(Theme.hairline))
            VStack(alignment: .leading, spacing: Theme.Space.s) {
                Text("This phone's name").font(.footnote.weight(.medium)).foregroundStyle(Theme.inkSoft)
                TextField("Name", text: $name)
                    .textInputAutocapitalization(.words)
                    .autocorrectionDisabled()
                    .submitLabel(.done)
                    .onSubmit { PhoneName.remember(name) }
                    .padding(.horizontal, Theme.Space.m)
                    .frame(minHeight: Theme.touch)
                    .surface(Theme.Corner.medium, fill: Theme.field)
                Text("Your Mac shows this name when it asks you to allow the phone.")
                    .font(.footnote).foregroundStyle(Theme.inkFaint)
            }
            Spacer(minLength: 0)
        }
        .page()
    }
}

/// The four corners of the square a code is held in.
private struct Reticle: Shape {
    func path(in rect: CGRect) -> Path {
        let arm = rect.width * 0.2, r: CGFloat = 14
        var path = Path()
        for (x, y, dx, dy) in [(rect.minX, rect.minY, 1.0, 1.0), (rect.maxX, rect.minY, -1.0, 1.0),
                               (rect.maxX, rect.maxY, -1.0, -1.0), (rect.minX, rect.maxY, 1.0, -1.0)] {
            path.move(to: CGPoint(x: x, y: y + dy * arm))
            path.addLine(to: CGPoint(x: x, y: y + dy * r))
            path.addQuadCurve(to: CGPoint(x: x + dx * r, y: y), control: CGPoint(x: x, y: y))
            path.addLine(to: CGPoint(x: x + dx * arm, y: y))
        }
        return path
    }
}

/// Match the pairing number on the Mac.
struct NumberScreen: View {
    let machineLabel: String
    let match: String
    let host: String

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.xl) {
            PageHead(title: "Enter this number on your Mac", line: "Desk on \(Text(machineLabel).foregroundStyle(Theme.ink).fontWeight(.medium)) is asking for it.")
            HStack(spacing: Theme.Space.m) {
                ForEach(Array(match.enumerated()), id: \.offset) { _, digit in
                    Text(String(digit))
                        .font(.system(size: 64, weight: .semibold, design: .rounded))
                        .monospacedDigit()
                        .foregroundStyle(Theme.ink)
                        .frame(width: 84, height: 108)
                        .surface()
                }
            }
            .frame(maxWidth: .infinity)
            .padding(.vertical, Theme.Space.l)
            .accessibilityElement()
            .accessibilityLabel("The number is \(match.map(String.init).joined(separator: " "))")
            Waiting(line: "Waiting for your Mac…")
            Spacer(minLength: 0)
            Text("Pairing with \(host)").font(.footnote).foregroundStyle(Theme.inkFaint).frame(maxWidth: .infinity)
        }
        .page()
    }
}

/// Join the phone to the network.
struct JoiningScreen: View {
    let machineLabel: String

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.xl) {
            PageHead(title: "Joining your network", line: "Setting up a private link to your Macs.")
            Label("Allowed on \(machineLabel)", systemImage: "checkmark.circle.fill")
                .font(.subheadline.weight(.medium))
                .foregroundStyle(Theme.good)
            Waiting(line: "This takes a few seconds…")
            Spacer(minLength: 0)
        }
        .page()
    }
}

/// A page's title and the one line under it.
struct PageHead: View {
    let title: String
    let line: Text

    init(title: String, line: Text) { self.title = title; self.line = line }
    init(title: String, line: LocalizedStringKey) { self.init(title: title, line: Text(line)) }

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.s) {
            Text(title).font(.title2.weight(.semibold)).foregroundStyle(Theme.ink)
            line.font(.body).foregroundStyle(Theme.inkSoft)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .combine)
    }
}

/// Something is under way, and what.
struct Waiting: View {
    let line: String

    var body: some View {
        HStack(spacing: Theme.Space.s) {
            ProgressView().controlSize(.small)
            Text(line).font(.subheadline).foregroundStyle(Theme.inkSoft)
        }
    }
}

/// The one thing to do on a page.
struct PageButtonStyle: ButtonStyle {
    var wide = true

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.semibold))
            .foregroundStyle(Theme.onAccent)
            .padding(.horizontal, Theme.Space.xl)
            .frame(maxWidth: wide ? .infinity : nil, minHeight: 50)
            .background(Theme.accent, in: RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
            .opacity(configuration.isPressed ? 0.8 : 1)
    }
}

extension View {
    /// A whole-screen page outside Home: its margins and its ground.
    func page() -> some View {
        padding(.horizontal, Theme.Space.xl).padding(.top, Theme.Space.xl).padding(.bottom, Theme.Space.l)
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .background(Theme.ground)
    }
}
