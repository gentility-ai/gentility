import Foundation

public enum CameraAccess: Equatable, Sendable {
    case undetermined, granted, denied, restricted
}

/// The camera's permission, behind a seam so the scanner's states are tested
/// without one. The app's is `AVCaptureDevice`'s (App/CameraView.swift).
public protocol CameraPermission: Sendable {
    func status() -> CameraAccess
    func request() async -> Bool
}

/// The scanner, before and around the camera.
@MainActor
public final class ScannerModel: ObservableObject {
    public enum State: Equatable {
        case asking
        case scanning
        /// Denied or restricted: say how to fix it, offer Settings.
        case denied
        /// The last code seen was not a Desk QR. Still scanning.
        case notADeskCode
    }

    @Published public private(set) var state: State = .asking
    public var onLink: ((ClaimLink) -> Void)?

    private let permission: CameraPermission
    private var taken = false

    public init(permission: CameraPermission) {
        self.permission = permission
    }

    public func appear() async {
        switch permission.status() {
        case .granted:
            state = .scanning
        case .denied, .restricted:
            state = .denied
        case .undetermined:
            state = await permission.request() ? .scanning : .denied
        }
        Log.pairing.info("scanner: camera \(String(describing: self.state), privacy: .public)")
    }

    /// One string the camera read. The camera reads the same code many times
    /// a second; only the first Desk link is taken until `reset()`. A code
    /// that is not Desk's is never logged: it may be anybody's.
    public func read(_ text: String) {
        guard !taken, state == .scanning || state == .notADeskCode else { return }
        guard let link = ClaimLink.parse(text) else {
            if state != .notADeskCode { Log.pairing.info("scanner: not a Desk code") }
            state = .notADeskCode
            return
        }
        taken = true
        state = .scanning
        Log.pairing.info("scanner: a Desk code for \(link.host, privacy: .public)")
        onLink?(link)
    }

    public func reset() {
        taken = false
        if state == .notADeskCode { state = .scanning }
    }
}
