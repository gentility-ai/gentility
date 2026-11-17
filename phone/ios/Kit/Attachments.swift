import Foundation

/// Why a photo in the tray did not go.
public enum TrayError: Equatable, Sendable {
    /// Over the core's 20 MiB put.
    case tooLarge
    /// The agent cannot read images: the core's own sentence, where it said one.
    case cannotAttach(String?)
    /// The door closed, or the put timed out.
    case unreachable
    /// The Mac refused the put (`io` or any other code).
    case notSaved
    /// The photo itself could not be read here.
    case unreadable

    public var sentence: String {
        switch self {
        case .tooLarge: "This photo is too big to send (20 MB max)."
        case .cannotAttach(let said): said.flatMap { $0.isEmpty ? nil : $0 } ?? "This agent can't read images. Remove the photo to send."
        case .unreachable: "Couldn't reach your Mac. Try again when it's awake."
        case .notSaved: "Your Mac couldn't save this photo. Try again."
        case .unreadable: "Couldn't read this photo."
        }
    }

    /// A refusal's code, in the tray's words.
    static func refused(_ refusal: DoorRefusal) -> TrayError {
        switch refusal.code {
        case "too_large": .tooLarge
        case "cannot_attach": .cannotAttach(refusal.message)
        default: .notSaved
        }
    }
}

/// One photo in the composer's tray.
public struct TrayItem: Equatable, Sendable, Identifiable {
    public enum State: Equatable, Sendable {
        /// Being read and downscaled on the phone.
        case preparing
        /// Waiting to be put, or being put.
        case uploading
        case ready(AttachmentRef)
        case failed(TrayError)
    }

    public let id: UUID
    public var thumbnail: Data?
    public var state: State
    /// What goes, kept so Try Again puts it again.
    public var image: PreparedImage?

    public init(id: UUID, thumbnail: Data? = nil, state: State, image: PreparedImage? = nil) {
        self.id = id
        self.thumbnail = thumbnail
        self.state = state
        self.image = image
    }

    public var busy: Bool {
        switch state {
        case .preparing, .uploading: true
        case .ready, .failed: false
        }
    }
}

/// The photos the next prompt carries. Puts go one at a time, in the tray's
/// order (one socket: parallel puts gain nothing). Send waits for every put,
/// and a failed photo blocks it until it is tried again or removed.
public struct AttachmentTray: Equatable, Sendable {
    /// The core's `attachments::MAX_COUNT`.
    public static let maxCount = 20
    public static let full = "Up to 20 photos per message."

    public private(set) var items: [TrayItem] = []

    public init(items: [TrayItem] = []) {
        self.items = items
    }

    public var isEmpty: Bool { items.isEmpty }
    public var count: Int { items.count }
    public var room: Int { Self.maxCount - items.count }

    /// A photo the person chose, still being read. False when the tray is full.
    @discardableResult
    public mutating func add(_ id: UUID, thumbnail: Data? = nil) -> Bool {
        guard items.count < Self.maxCount else { return false }
        items.append(TrayItem(id: id, thumbnail: thumbnail, state: .preparing))
        return true
    }

    /// Read and downscaled: it waits its turn to be put.
    public mutating func prepared(_ id: UUID, _ image: PreparedImage) {
        update(id) {
            $0.image = image
            $0.thumbnail = image.thumbnail
            $0.state = .uploading
        }
    }

    public mutating func failed(_ id: UUID, _ error: TrayError) {
        update(id) { $0.state = .failed(error) }
    }

    /// The Mac kept it. A photo removed while its put was out is not brought back.
    /// A ref of a kind this app does not know is not sent on with a prompt:
    /// the put reads as not saved, which Try Again puts again.
    public mutating func uploaded(_ id: UUID, _ ref: AttachmentRef) {
        update(id) { $0.state = ref.kind == .unknown ? .failed(.notSaved) : .ready(ref) }
    }

    /// Try Again: a failed photo with its bytes goes back in line.
    public mutating func retry(_ id: UUID) {
        update(id) {
            guard case .failed = $0.state, $0.image != nil else { return }
            $0.state = .uploading
        }
    }

    public mutating func remove(_ id: UUID) {
        items.removeAll { $0.id == id }
    }

    public mutating func clear() {
        items.removeAll()
    }

    /// The next photo to put: the first waiting, in the tray's order.
    public var nextUpload: TrayItem? {
        items.first { $0.state == .uploading && $0.image != nil }
    }

    /// The refs a prompt carries, in the tray's order.
    public var refs: [AttachmentRef] {
        items.compactMap {
            if case .ready(let ref) = $0.state { return ref }
            return nil
        }
    }

    /// Photos still being read or put.
    public var busyCount: Int { items.filter(\.busy).count }

    public var hasFailed: Bool {
        items.contains {
            if case .failed = $0.state { return true }
            return false
        }
    }

    /// Words or a ready photo, and nothing still going or failed.
    public func canSend(_ text: String) -> Bool {
        let words = !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        return (words || !refs.isEmpty) && busyCount == 0 && !hasFailed
    }

    /// "Uploading 2 photos…", while any is being read or put.
    public var uploadingLine: String? {
        switch busyCount {
        case 0: nil
        case 1: "Uploading 1 photo…"
        case let n: "Uploading \(n) photos…"
        }
    }

    /// What VoiceOver says of a photo: "Photo 1 of 3, uploading".
    public func accessibilityLabel(_ id: UUID) -> String {
        guard let at = items.firstIndex(where: { $0.id == id }) else { return "Photo" }
        let state: String = switch items[at].state {
        case .preparing, .uploading: "uploading"
        case .ready: "ready"
        case .failed: "failed, double tap for options"
        }
        return "Photo \(at + 1) of \(items.count), \(state)"
    }

    private mutating func update(_ id: UUID, _ change: (inout TrayItem) -> Void) {
        guard let at = items.firstIndex(where: { $0.id == id }) else { return }
        change(&items[at])
    }
}

// MARK: the tray's verbs

extension ConversationModel {
    /// Photos picked, taken or pasted: each joins the tray, is read and
    /// downscaled off the main actor, and then put. Answers how many fit;
    /// past twenty, the note says so.
    @discardableResult
    public func attach(_ photos: [Data], names: [String?] = []) -> Int {
        var added = 0
        for (at, data) in photos.enumerated() {
            let id = UUID()
            guard tray.add(id) else {
                composerNote = AttachmentTray.full
                break
            }
            added += 1
            let name = at < names.count ? names[at] : nil
            Task {
                let made = await Task.detached(priority: .userInitiated) { () -> Result<PreparedImage, ImagePrepError> in
                    do { return .success(try ImagePrep.prepare(data, name: name)) } catch let error as ImagePrepError {
                        return .failure(error)
                    } catch {
                        return .failure(.unreadable)
                    }
                }.value
                switch made {
                case .success(let image): attach(prepared: image, id: id)
                case .failure(let error):
                    Log.conversation.error("a photo did not prepare: \(String(describing: error), privacy: .public)")
                    tray.failed(id, error == .tooLarge ? .tooLarge : .unreadable)
                }
            }
        }
        return added
    }

    /// A photo already prepared: into the tray (or, under `id`, in its place
    /// there), and put in its turn.
    public func attach(prepared image: PreparedImage, id: UUID = UUID()) {
        if !tray.items.contains(where: { $0.id == id }) {
            guard tray.add(id) else {
                composerNote = AttachmentTray.full
                return
            }
        }
        tray.prepared(id, image)
        startUploads()
    }

    public func retryAttachment(_ id: UUID) {
        tray.retry(id)
        startUploads()
    }

    /// Off the tray. A put already out finishes, and its ref is dropped.
    public func removeAttachment(_ id: UUID) {
        tray.remove(id)
        if tray.isEmpty, composerNote == AttachmentTray.full { composerNote = nil }
    }

    /// The tray's puts, one at a time, until none waits. Returns once they are done.
    @discardableResult
    func startUploads() -> Task<Void, Never> {
        if let uploads { return uploads }
        let task = Task { [weak self] in
            while let self, let next = self.tray.nextUpload, let image = next.image {
                await self.put(next.id, image)
            }
            self?.uploads = nil
        }
        uploads = task
        return task
    }

    /// Waits for every put out or waiting: for tests.
    func uploadsSettled() async {
        while let uploads { await uploads.value }
    }

    private func put(_ id: UUID, _ image: PreparedImage) async {
        guard let door, door.isOpen else {
            Log.conversation.notice("a photo put with no door open")
            tray.failed(id, .unreachable)
            return
        }
        Log.conversation.info("put of a photo (\(image.data.count) bytes, \(image.mime, privacy: .public))")
        do {
            let response = try await door.request(.attachmentPut(name: image.name, mime: image.mime, bytesB64: image.data.base64EncodedString()))
            Log.conversation.info("photo put answered \(response.kind, privacy: .public)")
            switch response {
            case .attachment(let ref): tray.uploaded(id, ref)
            case .error(let refusal): tray.failed(id, .refused(refusal))
            default: tray.failed(id, .notSaved)
            }
        } catch {
            Log.conversation.error("a photo put did not reach the Mac: \(String(describing: error), privacy: .public)")
            tray.failed(id, .unreachable)
        }
    }
}
