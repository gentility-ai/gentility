import DeskPhoneKit
import SwiftUI

/// Where the transcript's images are read: the conversation's store, the
/// session they belong to, its worktree root, and whether the door is open.
struct ImageContext {
    var store: ImageStore?
    var sessionID = ""
    var worktreeRoot: String?
    var open = false

    func request(_ src: String) -> ImageRequest {
        ImageSource.request(for: src, sessionID: sessionID, worktreeRoot: worktreeRoot)
    }
}

private struct ImageContextKey: EnvironmentKey {
    static let defaultValue = ImageContext()
}

extension EnvironmentValues {
    var images: ImageContext {
        get { self[ImageContextKey.self] }
        set { self[ImageContextKey.self] = newValue }
    }
}

/// One image the full-screen viewer pages through.
struct ViewerPage: Identifiable, Hashable {
    let request: ImageRequest
    let label: String
    let caption: String?
    var id: ImageRequest { request }
}

/// Reads one image through the context's store, and says how it went.
@MainActor
private final class ImageLoad: ObservableObject {
    enum State: Equatable {
        case loading
        case loaded(UIImage)
        case failed(ImageLoadError)
        /// A web image: nothing is fetched until Load image is tapped.
        case waiting
    }

    @Published var state: State = .loading

    func load(_ request: ImageRequest, _ context: ImageContext, edge: Int?, tapped: Bool = false) async {
        if case .remote = request, !tapped {
            state = .waiting
            return
        }
        guard let store = context.store else {
            state = .failed(.asleep)
            return
        }
        if case .loaded = state {} else { state = .loading }
        do {
            state = .loaded(try await store.image(for: request, edge: edge))
        } catch {
            state = .failed((error as? ImageLoadError) ?? .failed)
        }
    }

    func retry(_ request: ImageRequest, _ context: ImageContext, edge: Int?) async {
        await context.store?.forget(request)
        await load(request, context, edge: edge, tapped: true)
    }
}

/// An image in an agent's words: full width, aspect fit, at most 320 pt
/// tall, its title as a caption under it. Tap opens it full screen.
struct TranscriptImage: View {
    let src: String
    let alt: String
    let title: String?

    @Environment(\.images) private var context
    @StateObject private var load = ImageLoad()
    @State private var viewing = false

    private var request: ImageRequest { context.request(src) }

    var body: some View {
        if request == .unsupported {
            Text(alt.isEmpty ? src : alt).foregroundStyle(.secondary)
        } else {
            VStack(alignment: .leading, spacing: 4) {
                content
                if let title, !title.isEmpty {
                    Text(title).font(.caption).foregroundStyle(.secondary)
                }
            }
            .task(id: "\(request.hashValue)-\(context.open)") { await load.load(request, context, edge: 1024) }
            .fullScreenCover(isPresented: $viewing) {
                ImageViewer(pages: [ViewerPage(request: request, label: alt, caption: title)], start: request).environment(\.images, context)
            }
        }
    }

    @ViewBuilder private var content: some View {
        switch load.state {
        case .loaded(let image):
            Image(uiImage: image)
                .resizable()
                .aspectRatio(contentMode: .fit)
                .frame(maxWidth: .infinity, maxHeight: 320, alignment: .leading)
                .clipShape(RoundedRectangle(cornerRadius: 10))
                .contentShape(Rectangle())
                .onTapGesture { viewing = true }
                .accessibilityLabel(alt.isEmpty ? "Image" : alt)
                .accessibilityAddTraits([.isImage, .isButton])
        case .loading:
            Placeholder(height: 160) { ProgressView() }
                .accessibilityLabel("Loading image")
        case .waiting:
            RemoteCard(alt: alt, host: remoteHost) {
                Task { await load.load(request, context, edge: 1024, tapped: true) }
            }
        case .failed(let error):
            ImageFailure(error: error) {
                Task { await load.retry(request, context, edge: 1024) }
            }
        }
    }

    private var remoteHost: String? {
        if case .remote(let url) = request { return url.host() }
        return nil
    }
}

/// A grey rounded box with something in its middle.
private struct Placeholder<Inner: View>: View {
    var height: CGFloat = 120
    var width: CGFloat? = nil
    @ViewBuilder let inner: () -> Inner

    var body: some View {
        RoundedRectangle(cornerRadius: 10)
            .fill(Color(.secondarySystemFill))
            .frame(maxWidth: width ?? .infinity)
            .frame(width: width, height: height)
            .overlay { inner() }
    }
}

/// Why an image is not there, and a tap to try again where one helps.
private struct ImageFailure: View {
    let error: ImageLoadError
    var width: CGFloat? = nil
    var height: CGFloat = 120
    let onRetry: () -> Void

    var body: some View {
        Placeholder(height: height, width: width) {
            VStack(spacing: 6) {
                Image(systemName: error == .asleep ? "desktopcomputer" : "photo.badge.exclamationmark")
                    .font(.title2)
                Text(words).font(.footnote).multilineTextAlignment(.center)
            }
            .foregroundStyle(.secondary)
            .padding(8)
        }
        .contentShape(Rectangle())
        .onTapGesture { if error == .failed { onRetry() } }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(error == .failed ? .isButton : [])
        .accessibilityHint(error == .failed ? "Double tap to try again" : "")
    }

    private var words: String {
        switch error {
        case .notFound: "Image no longer on your Mac."
        case .asleep: "Image is on your Mac"
        case .failed: "Couldn't load image"
        }
    }
}

/// A web image, not fetched until asked: its description and Load image.
private struct RemoteCard: View {
    let alt: String
    let host: String?
    let onLoad: () -> Void

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: "globe").font(.title2).foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 2) {
                Text(alt.isEmpty ? "Web image" : alt).lineLimit(2)
                if let host { Text(host).font(.caption).foregroundStyle(.secondary) }
            }
            Spacer(minLength: 8)
            Button("Load image", action: onLoad)
                .buttonStyle(.bordered)
                .font(.footnote.weight(.semibold))
        }
        .padding(12)
        .background(Color(.secondarySystemFill), in: RoundedRectangle(cornerRadius: 10))
    }
}

/// What the person attached to a message: photos as 120 pt thumbnails (a
/// grid of two past two), any other file as a chip.
struct AttachmentsRow: View {
    let attachments: [AttachmentRef]

    @Environment(\.images) private var context
    @State private var viewing: ImageRequest?

    private var images: [AttachmentRef] { attachments.filter { $0.kind == .image } }
    private var files: [AttachmentRef] { attachments.filter { $0.kind != .image } }
    private var pages: [ViewerPage] {
        images.map { ViewerPage(request: .attachment(path: $0.path), label: "Photo you sent", caption: nil) }
    }

    var body: some View {
        VStack(alignment: .trailing, spacing: 8) {
            if images.count > 2 {
                LazyVGrid(columns: [GridItem(.fixed(120), spacing: 8), GridItem(.fixed(120), spacing: 8)], alignment: .trailing, spacing: 8) {
                    thumbnails
                }
                .frame(width: 248, alignment: .trailing)
            } else if !images.isEmpty {
                HStack(spacing: 8) { thumbnails }
            }
            ForEach(files, id: \.self) { FileChip(file: $0) }
        }
        .fullScreenCover(item: $viewing) { start in
            ImageViewer(pages: pages, start: start).environment(\.images, context)
        }
    }

    private var thumbnails: some View {
        ForEach(images, id: \.self) { ref in
            AttachmentThumbnail(request: .attachment(path: ref.path)) { viewing = .attachment(path: ref.path) }
        }
    }
}

extension ImageRequest: Identifiable {
    public var id: Self { self }
}

/// One photo the person sent, 120 pt square.
private struct AttachmentThumbnail: View {
    let request: ImageRequest
    let onOpen: () -> Void

    @Environment(\.images) private var context
    @StateObject private var load = ImageLoad()

    var body: some View {
        Group {
            switch load.state {
            case .loaded(let image):
                Image(uiImage: image)
                    .resizable()
                    .aspectRatio(contentMode: .fill)
                    .frame(width: 120, height: 120)
                    .clipShape(RoundedRectangle(cornerRadius: 10))
                    .contentShape(Rectangle())
                    .onTapGesture(perform: onOpen)
                    .accessibilityLabel("Photo you sent")
                    .accessibilityAddTraits([.isImage, .isButton])
            case .loading, .waiting:
                Placeholder(height: 120, width: 120) { ProgressView() }
            case .failed(let error):
                ImageFailure(error: error, width: 120, height: 120) {
                    Task { await load.retry(request, context, edge: 480) }
                }
            }
        }
        .task(id: "\(request.hashValue)-\(context.open)") { await load.load(request, context, edge: 480) }
    }
}

/// A file attached that is not a photo: its name and size.
private struct FileChip: View {
    let file: AttachmentRef

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: Self.symbol(file.kind))
            Text(file.name).lineLimit(1).truncationMode(.middle)
            if let size = file.size {
                Text(ByteCountFormatter.string(fromByteCount: Int64(size), countStyle: .file)).foregroundStyle(.secondary)
            }
        }
        .font(.footnote)
        .padding(.horizontal, 10).padding(.vertical, 6)
        .background(Color(.secondarySystemFill), in: Capsule())
        .accessibilityElement(children: .combine)
    }

    /// A kind this app does not know is drawn as any attachment.
    private static func symbol(_ kind: AttachmentRef.Kind) -> String {
        switch kind {
        case .folder: "folder"
        case .file, .image: "doc"
        case .unknown: "paperclip"
        }
    }
}

// MARK: full screen

/// An image, or a message's images paged, on black: pinch or double-tap to
/// zoom, swipe down to close, share the whole image.
struct ImageViewer: View {
    let pages: [ViewerPage]
    @State var start: ImageRequest

    @Environment(\.dismiss) private var dismiss
    @State private var drag: CGFloat = 0
    @State private var shown: [ImageRequest: UIImage] = [:]

    init(pages: [ViewerPage], start: ImageRequest) {
        self.pages = pages
        _start = State(initialValue: start)
    }

    private var current: ViewerPage? { pages.first { $0.request == start } ?? pages.first }

    var body: some View {
        ZStack {
            Color.black.opacity(1 - min(abs(drag) / 400, 0.6)).ignoresSafeArea()
            TabView(selection: $start) {
                ForEach(pages) { page in
                    ZoomableImage(page: page) { shown[page.request] = $0 }
                        .tag(page.request)
                }
            }
            .tabViewStyle(.page(indexDisplayMode: pages.count > 1 ? .automatic : .never))
            .offset(y: drag)
            .simultaneousGesture(
                DragGesture(minimumDistance: 20)
                    .onChanged { value in
                        if abs(value.translation.height) > abs(value.translation.width) { drag = value.translation.height }
                    }
                    .onEnded { value in
                        if abs(value.translation.height) > 120 { dismiss() } else { withAnimation(.spring) { drag = 0 } }
                    }
            )
            VStack {
                HStack {
                    Button { dismiss() } label: {
                        Image(systemName: "xmark").font(.body.weight(.semibold)).frame(width: 44, height: 44)
                    }
                    .accessibilityLabel("Close")
                    Spacer()
                    if let page = current, let image = shown[page.request] {
                        ShareLink(item: Image(uiImage: image), preview: SharePreview(page.label, image: Image(uiImage: image))) {
                            Image(systemName: "square.and.arrow.up").font(.body.weight(.semibold)).frame(width: 44, height: 44)
                        }
                        .accessibilityLabel("Share")
                    }
                }
                .foregroundStyle(.white)
                .padding(.horizontal, 8)
                Spacer()
                if let caption = current?.caption, !caption.isEmpty {
                    Text(caption)
                        .font(.callout)
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, 24).padding(.bottom, pages.count > 1 ? 36 : 16)
                }
            }
            .opacity(drag == 0 ? 1 : 0.4)
        }
        .preferredColorScheme(.dark)
        .statusBarHidden()
    }
}

/// One image at full resolution, zoomed by pinch or a double tap.
private struct ZoomableImage: View {
    let page: ViewerPage
    let onLoaded: (UIImage) -> Void

    @Environment(\.images) private var context
    @StateObject private var load = ImageLoad()
    @State private var scale: CGFloat = 1
    @State private var settled: CGFloat = 1

    var body: some View {
        GeometryReader { space in
            Group {
                switch load.state {
                case .loaded(let image):
                    ScrollView([.horizontal, .vertical], showsIndicators: false) {
                        Image(uiImage: image)
                            .resizable()
                            .aspectRatio(contentMode: .fit)
                            .frame(width: space.size.width * scale, height: space.size.height * scale)
                    }
                    .scrollDisabled(scale <= 1)
                    .gesture(
                        MagnifyGesture()
                            .onChanged { scale = min(max(settled * $0.magnification, 1), 5) }
                            .onEnded { _ in settled = scale }
                    )
                    .onTapGesture(count: 2) {
                        withAnimation(.easeInOut(duration: 0.2)) { scale = scale > 1 ? 1 : 2 }
                        settled = scale
                    }
                    .accessibilityLabel(page.label)
                    .accessibilityAddTraits(.isImage)
                    .onAppear { onLoaded(image) }
                case .loading, .waiting:
                    ProgressView().tint(.white).frame(maxWidth: .infinity, maxHeight: .infinity)
                case .failed:
                    Text("Couldn't load image").foregroundStyle(.white.opacity(0.7)).frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .task { await load.load(page.request, context, edge: nil, tapped: true) }
    }
}
