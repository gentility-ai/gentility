import Foundation
import UIKit

/// Where a transcript image's bytes come from, as the Mac's window decides
/// it (`desk/ui/src/session/images.logic.ts`).
public enum ImageRequest: Hashable, Sendable {
    /// `file_image_read`: a raster image inside the session's worktree, by
    /// its path relative to the root.
    case worktree(sessionID: String, path: String)
    /// `attachment_read`: a file a person attached, by its absolute path.
    case attachment(path: String)
    /// An http or https image: fetched only once the person taps Load image.
    case remote(URL)
    /// A `data:` URL's own bytes.
    case inline(Data)
    /// Nothing to fetch: the alt text draws instead.
    case unsupported
}

public enum ImageSource {
    /// The Markdown image `src`, resolved: a path under the worktree root is
    /// read relative to it; any other absolute path, or any path while the
    /// root is not known, is an attachment; a relative path is the
    /// worktree's.
    public static func request(for src: String, sessionID: String, worktreeRoot: String?) -> ImageRequest {
        var src = src.trimmingCharacters(in: .whitespaces)
        if src.hasPrefix("<"), src.hasSuffix(">") { src = String(src.dropFirst().dropLast()) }
        if src.count > 28 * 1024 * 1024 { return .unsupported }
        let lower = src.lowercased()
        if lower.hasPrefix("data:image/") { return inline(src).map(ImageRequest.inline) ?? .unsupported }
        if lower.hasPrefix("http://") || lower.hasPrefix("https://") {
            return URL(string: src).map(ImageRequest.remote) ?? .unsupported
        }
        for prefix in ["file://localhost/", "file:///"] where lower.hasPrefix(prefix) {
            src = "/" + src.dropFirst(prefix.count)
        }
        if src.isEmpty || src.hasPrefix("//") || src.range(of: #"^[A-Za-z][A-Za-z0-9+.-]*:"#, options: .regularExpression) != nil {
            return .unsupported
        }
        guard let path = src.removingPercentEncoding else { return .unsupported }
        guard let root = worktreeRoot else { return .attachment(path: path) }
        var base = root
        while base.hasSuffix("/") { base.removeLast() }
        let relative = path.hasPrefix(base + "/") ? String(path.dropFirst(base.count + 1)) : path
        if relative.hasPrefix("/") || relative.contains("\\") { return .attachment(path: path) }
        return .worktree(sessionID: sessionID, path: relative)
    }

    /// A base64 `data:image/…` URL's bytes.
    static func inline(_ src: String) -> Data? {
        guard let comma = src.firstIndex(of: ","), src[..<comma].lowercased().hasSuffix(";base64") else { return nil }
        return Data(base64Encoded: String(src[src.index(after: comma)...]))
    }
}

public enum ImageLoadError: Error, Equatable, Sendable {
    /// The Mac no longer holds it (`not_found`).
    case notFound
    /// No door open: the image is on the Mac.
    case asleep
    /// Any other failure: a tap tries again.
    case failed
}

/// The transcript's images, fetched once and kept: decoded and downscaled
/// for the line, coalesced while a read is out, at most two reads out at
/// once on a door, and a failure kept for the session so a scroll does not
/// ask again (Try again forgets it).
public actor ImageStore {
    public typealias Loader = @Sendable (ImageRequest) async throws -> Data

    private let loader: Loader
    private let most: Int
    private var out = 0
    private var queue: [CheckedContinuation<Void, Never>] = []
    private var flying: [ImageRequest: Task<Data, Error>] = [:]
    private var bytes: [ImageRequest: Data] = [:]
    private var failures: [ImageRequest: ImageLoadError] = [:]
    private let images = NSCache<NSString, UIImage>()

    public init(maxConcurrent: Int = 2, loader: @escaping Loader) {
        self.loader = loader
        most = maxConcurrent
    }

    /// The bytes, read once.
    public func data(for request: ImageRequest) async throws -> Data {
        if case .inline(let data) = request { return data }
        if let kept = bytes[request] { return kept }
        if let failed = failures[request] { throw failed }
        if let task = flying[request] { return try await task.value }
        let task = Task { try await self.read(request) }
        flying[request] = task
        defer { flying[request] = nil }
        do {
            let data = try await task.value
            bytes[request] = data
            return data
        } catch {
            let failed = (error as? ImageLoadError) ?? .failed
            // Asleep is not kept: the door opening is what it waits on.
            if failed != .asleep { failures[request] = failed }
            throw failed
        }
    }

    /// The image, upright, at most `edge` points' pixels on its long edge;
    /// the whole image when `edge` is nil.
    public func image(for request: ImageRequest, edge: Int? = 1024) async throws -> UIImage {
        let key = "\(request.hashValue)-\(edge ?? 0)" as NSString
        if let image = images.object(forKey: key) { return image }
        let data = try await data(for: request)
        let image: UIImage? = if let edge, let cg = ImagePrep.downscaled(data, edge: edge) { UIImage(cgImage: cg) } else { UIImage(data: data) }
        guard let image else {
            failures[request] = .failed
            throw ImageLoadError.failed
        }
        images.setObject(image, forKey: key)
        return image
    }

    /// The failure kept for this image, if any.
    public func failure(for request: ImageRequest) -> ImageLoadError? { failures[request] }

    /// Try again: the next ask reads it afresh.
    public func forget(_ request: ImageRequest) {
        failures[request] = nil
    }

    private func read(_ request: ImageRequest) async throws -> Data {
        await slot()
        defer { release() }
        return try await loader(request)
    }

    private func slot() async {
        if out < most {
            out += 1
            return
        }
        await withCheckedContinuation { queue.append($0) }
    }

    private func release() {
        if queue.isEmpty {
            out -= 1
        } else {
            queue.removeFirst().resume()
        }
    }
}

// MARK: reading an image through the door

extension ConversationModel {
    /// One image's bytes: from the door for a worktree image or an
    /// attachment, from the web for a remote one (after a tap).
    func imageBytes(_ request: ImageRequest) async throws -> Data {
        let ask: DoorRequest
        switch request {
        case .inline(let data): return data
        case .unsupported: throw ImageLoadError.failed
        case .remote(let url):
            guard let fetched = try? await URLSession.shared.data(from: url),
                  (fetched.1 as? HTTPURLResponse).map({ (200..<300).contains($0.statusCode) }) ?? false
            else { throw ImageLoadError.failed }
            return fetched.0
        case .worktree(let sessionID, let path): ask = .fileImageRead(sessionID: sessionID, path: path)
        case .attachment(let path): ask = .attachmentRead(path: path)
        }
        guard let door, door.isOpen else { throw ImageLoadError.asleep }
        let response: DoorResponse
        do {
            response = try await door.request(ask)
        } catch {
            Log.conversation.error("an image read did not reach the Mac: \(String(describing: error), privacy: .public)")
            throw ImageLoadError.failed
        }
        switch response {
        case .attachmentBytes(_, let b64):
            guard let data = Data(base64Encoded: b64) else { throw ImageLoadError.failed }
            return data
        case .error(let refusal) where refusal.code == "not_found":
            throw ImageLoadError.notFound
        default:
            Log.conversation.notice("an image read answered \(response.kind, privacy: .public)")
            throw ImageLoadError.failed
        }
    }
}
