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
            VStack(spacing: 12) {
                ProgressView()
                Text("Asking \(host)…").foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
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
        VStack(alignment: .leading, spacing: 16) {
            Text("GenDesk").font(.title2.weight(.semibold))
            ZStack {
                Color(white: 0.16)
                switch state {
                case .denied:
                    VStack(spacing: 12) {
                        Image(systemName: "camera.fill").font(.largeTitle).foregroundStyle(.white.opacity(0.7))
                        Text("GenDesk needs the camera to read the code on your Mac.")
                            .multilineTextAlignment(.center)
                            .foregroundStyle(.white)
                        Button("Open Settings") {
                            if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                        }
                        .buttonStyle(.borderedProminent)
                    }
                    .padding()
                case .asking:
                    ProgressView().tint(.white)
                case .scanning, .notADeskCode:
                    if camera { CameraView(onCode: onCode) }
                    RoundedRectangle(cornerRadius: 16)
                        .strokeBorder(.white, lineWidth: 2)
                        .frame(width: 200, height: 200)
                    if state == .notADeskCode {
                        Text("Not a Desk code")
                            .padding(.horizontal, 12).padding(.vertical, 6)
                            .background(.thinMaterial, in: Capsule())
                            .frame(maxHeight: .infinity, alignment: .bottom)
                            .padding(.bottom, 16)
                    }
                }
            }
            .frame(maxWidth: .infinity)
            .aspectRatio(0.8, contentMode: .fit)
            .clipShape(RoundedRectangle(cornerRadius: 20))
            Text("Point at the code in Desk → Settings → Phone")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: .infinity)
            HStack {
                Text("This phone:").foregroundStyle(.secondary)
                TextField("Name", text: $name)
                    .textInputAutocapitalization(.words)
                    .autocorrectionDisabled()
                    .submitLabel(.done)
                    .onSubmit { PhoneName.remember(name) }
                Image(systemName: "pencil").foregroundStyle(.secondary)
            }
            .font(.callout)
            .padding(12)
            .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
            Spacer(minLength: 0)
        }
        .padding()
    }
}

/// Match the pairing number on the Mac.
struct NumberScreen: View {
    let machineLabel: String
    let match: String
    let host: String

    var body: some View {
        VStack(spacing: 20) {
            Text("Check on your Mac")
                .font(.title2.weight(.semibold))
                .frame(maxWidth: .infinity, alignment: .leading)
            Spacer()
            Text("Desk on \(Text(machineLabel).bold()) will ask you to confirm. Enter this number there:")
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
            Text(match)
                .font(.system(size: 80, weight: .heavy, design: .rounded))
                .tracking(8)
                .monospacedDigit()
                .foregroundStyle(.purple)
                .accessibilityLabel("The number is \(match.map(String.init).joined(separator: " "))")
            ProgressView()
            Text("Waiting for your Mac…").foregroundStyle(.secondary)
            Spacer()
            Text("Pairing with \(host)").font(.footnote).foregroundStyle(.secondary)
        }
        .padding()
    }
}

/// Join the phone to the network.
struct JoiningScreen: View {
    let machineLabel: String

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Joining your network").font(.title2.weight(.semibold))
            Banner(text: "✓ Allowed on \(machineLabel)", tone: .ok)
            ProgressView().frame(maxWidth: .infinity)
            Text("Setting up a private link to your Macs…")
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity)
            Spacer()
        }
        .padding()
    }
}
