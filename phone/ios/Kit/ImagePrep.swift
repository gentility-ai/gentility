import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

/// A photo made ready to put: the bytes that go, and a small thumbnail for
/// the tray.
public struct PreparedImage: Equatable, Sendable {
    public let name: String
    public let mime: String
    public let data: Data
    /// A JPEG at most `ImagePrep.thumbnailEdge` on its long edge.
    public let thumbnail: Data
    public let width: Int
    public let height: Int

    public init(name: String, mime: String, data: Data, thumbnail: Data, width: Int, height: Int) {
        self.name = name
        self.mime = mime
        self.data = data
        self.thumbnail = thumbnail
        self.width = width
        self.height = height
    }
}

public enum ImagePrepError: Error, Equatable, Sendable {
    /// Not an image ImageIO reads.
    case unreadable
    /// Over the core's 20 MiB put, even after downscaling.
    case tooLarge
}

/// What a photo becomes before it is put: at most 2048 px on its long edge,
/// upright, JPEG at 0.8, with no metadata copied (so no location). A PNG
/// with alpha that already fits stays PNG (screenshots and diagrams keep
/// crisp text); a GIF under 8 MiB goes as it is (animation). HEIC always
/// becomes JPEG: agents read png, jpeg, gif and webp. Runs off the main
/// actor: call it from a detached task.
public enum ImagePrep {
    public static let longestEdge = 2048
    public static let quality = 0.8
    public static let thumbnailEdge = 256
    /// The core's `attachments::MAX_PUT`.
    public static let largestPut = 20 * 1024 * 1024
    public static let largestGIF = 8 * 1024 * 1024

    public static func prepare(_ data: Data, name original: String? = nil, now: Date = Date()) throws -> PreparedImage {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil), CGImageSourceGetCount(source) > 0 else {
            throw ImagePrepError.unreadable
        }
        let type = CGImageSourceGetType(source).map { $0 as String }
        let (width, height) = pixelSize(source)
        guard width > 0, height > 0 else { throw ImagePrepError.unreadable }
        let base = baseName(original, now: now)
        let thumbnail = try thumbnailJPEG(source)

        if type == UTType.gif.identifier, data.count <= largestGIF {
            return PreparedImage(name: base + ".gif", mime: "image/gif", data: data, thumbnail: thumbnail, width: width, height: height)
        }
        guard let image = downscaled(source, edge: longestEdge) else { throw ImagePrepError.unreadable }
        let out: PreparedImage
        if type == UTType.png.identifier, hasAlpha(image), max(width, height) <= longestEdge {
            guard let png = encode(image, as: .png, quality: nil) else { throw ImagePrepError.unreadable }
            out = PreparedImage(name: base + ".png", mime: "image/png", data: png, thumbnail: thumbnail, width: image.width, height: image.height)
        } else {
            guard let jpeg = encode(opaque(image), as: .jpeg, quality: quality) else { throw ImagePrepError.unreadable }
            out = PreparedImage(name: base + ".jpg", mime: "image/jpeg", data: jpeg, thumbnail: thumbnail, width: image.width, height: image.height)
        }
        guard out.data.count <= largestPut else { throw ImagePrepError.tooLarge }
        return out
    }

    /// A thumbnail of any image ImageIO reads, upright, as JPEG.
    public static func thumbnail(_ data: Data, edge: Int = thumbnailEdge) -> Data? {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil) else { return nil }
        return try? thumbnailJPEG(source, edge: edge)
    }

    /// The image upright, at most `edge` on its long edge; never upscaled.
    public static func downscaled(_ data: Data, edge: Int) -> CGImage? {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil) else { return nil }
        return downscaled(source, edge: edge)
    }

    static func baseName(_ original: String?, now: Date) -> String {
        if let original {
            let stem = (original as NSString).lastPathComponent
            let base = (stem as NSString).deletingPathExtension
            if !base.isEmpty { return base }
        }
        let format = DateFormatter()
        format.locale = Locale(identifier: "en_US_POSIX")
        format.timeZone = .current
        format.dateFormat = "yyyyMMdd-HHmmss"
        return "IMG_" + format.string(from: now)
    }

    private static func thumbnailJPEG(_ source: CGImageSource, edge: Int = thumbnailEdge) throws -> Data {
        guard let small = downscaled(source, edge: edge), let jpeg = encode(opaque(small), as: .jpeg, quality: 0.7) else {
            throw ImagePrepError.unreadable
        }
        return jpeg
    }

    private static func pixelSize(_ source: CGImageSource) -> (Int, Int) {
        let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any] ?? [:]
        let width = (properties[kCGImagePropertyPixelWidth] as? NSNumber)?.intValue ?? 0
        let height = (properties[kCGImagePropertyPixelHeight] as? NSNumber)?.intValue ?? 0
        return (width, height)
    }

    private static func downscaled(_ source: CGImageSource, edge: Int) -> CGImage? {
        let (width, height) = pixelSize(source)
        let options: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: min(edge, max(width, height, 1)),
        ]
        return CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary)
    }

    static func hasAlpha(_ image: CGImage) -> Bool {
        switch image.alphaInfo {
        case .none, .noneSkipFirst, .noneSkipLast: return false
        default: return true
        }
    }

    /// The image over white, so a transparent pixel is not black in a JPEG.
    private static func opaque(_ image: CGImage) -> CGImage {
        guard hasAlpha(image),
              let context = CGContext(data: nil, width: image.width, height: image.height, bitsPerComponent: 8, bytesPerRow: 0,
                                      space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
        else { return image }
        let rect = CGRect(x: 0, y: 0, width: image.width, height: image.height)
        context.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
        context.fill(rect)
        context.draw(image, in: rect)
        return context.makeImage() ?? image
    }

    /// Only the pixels: no properties of the source are copied.
    private static func encode(_ image: CGImage, as type: UTType, quality: Double?) -> Data? {
        let out = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(out, type.identifier as CFString, 1, nil) else { return nil }
        var properties: [CFString: Any] = [:]
        if let quality { properties[kCGImageDestinationLossyCompressionQuality] = quality }
        CGImageDestinationAddImage(destination, image, properties as CFDictionary)
        guard CGImageDestinationFinalize(destination) else { return nil }
        return out as Data
    }
}
