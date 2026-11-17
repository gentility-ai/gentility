import CoreGraphics
import ImageIO
import UniformTypeIdentifiers
import XCTest
@testable import DeskPhoneKit

final class ImagePrepTests: XCTestCase {
    /// A width×height image: red on its left half, blue on its right, with
    /// `alpha` making the right half see-through.
    static func image(_ width: Int, _ height: Int, alpha: Bool = false) -> CGImage {
        let context = CGContext(
            data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
            space: CGColorSpace(name: CGColorSpace.sRGB)!,
            bitmapInfo: (alpha ? CGImageAlphaInfo.premultipliedLast : CGImageAlphaInfo.noneSkipLast).rawValue
        )!
        context.setFillColor(CGColor(red: 1, green: 0, blue: 0, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width / 2, height: height))
        context.setFillColor(CGColor(red: 0, green: 0, blue: 1, alpha: alpha ? 0.3 : 1))
        context.fill(CGRect(x: width / 2, y: 0, width: width - width / 2, height: height))
        return context.makeImage()!
    }

    static func encode(_ image: CGImage, _ type: UTType, properties: [CFString: Any] = [:]) -> Data? {
        let out = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(out, type.identifier as CFString, 1, nil) else { return nil }
        CGImageDestinationAddImage(destination, image, properties as CFDictionary)
        guard CGImageDestinationFinalize(destination) else { return nil }
        return out as Data
    }

    static let gps: [CFString: Any] = [
        kCGImagePropertyGPSDictionary: [
            kCGImagePropertyGPSLatitude: 51.5, kCGImagePropertyGPSLatitudeRef: "N",
            kCGImagePropertyGPSLongitude: 0.12, kCGImagePropertyGPSLongitudeRef: "W",
        ] as [CFString: Any],
    ]

    private func properties(_ data: Data) -> [CFString: Any] {
        let source = CGImageSourceCreateWithData(data as CFData, nil)!
        return CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any] ?? [:]
    }

    private func type(_ data: Data) -> String? {
        CGImageSourceCreateWithData(data as CFData, nil).flatMap(CGImageSourceGetType).map { $0 as String }
    }

    func testABigCameraPhotoComesOutAt2048AsJPEGWithNoLocation() throws {
        // HEIC where this machine writes it; JPEG otherwise, which takes the same path.
        let big = Self.image(4032, 3024)
        let data = try XCTUnwrap(Self.encode(big, .heic, properties: Self.gps) ?? Self.encode(big, .jpeg, properties: Self.gps))
        XCTAssertNotNil(properties(data)[kCGImagePropertyGPSDictionary], "the fixture carries a location")

        let out = try ImagePrep.prepare(data, name: "IMG_0042.HEIC")
        XCTAssertEqual(out.mime, "image/jpeg")
        XCTAssertEqual(out.name, "IMG_0042.jpg")
        XCTAssertEqual(type(out.data), UTType.jpeg.identifier)
        XCTAssertEqual(max(out.width, out.height), 2048)
        XCTAssertEqual(out.width, 2048)
        XCTAssertEqual(out.height, 1536)
        XCTAssertNil(properties(out.data)[kCGImagePropertyGPSDictionary], "no location leaves the phone")
        XCTAssertLessThanOrEqual(out.data.count, ImagePrep.largestPut)
    }

    func testASmallPNGWithAlphaStaysPNG() throws {
        let data = try XCTUnwrap(Self.encode(Self.image(300, 200, alpha: true), .png))
        let out = try ImagePrep.prepare(data, name: "Screenshot.png")
        XCTAssertEqual(out.mime, "image/png")
        XCTAssertEqual(out.name, "Screenshot.png")
        XCTAssertEqual(type(out.data), UTType.png.identifier)
        XCTAssertEqual(out.width, 300)
        XCTAssertEqual(out.height, 200)
    }

    func testAnOpaquePNGBecomesJPEG() throws {
        let data = try XCTUnwrap(Self.encode(Self.image(300, 200), .png))
        let out = try ImagePrep.prepare(data, name: "photo.png")
        XCTAssertEqual(out.mime, "image/jpeg")
        XCTAssertEqual(out.width, 300, "never upscaled")
    }

    func testARotatedPhotoComesOutUpright() throws {
        // Stored 400×200, EXIF orientation 6: shown rotated a quarter turn, 200×400.
        let data = try XCTUnwrap(Self.encode(Self.image(400, 200), .jpeg, properties: [kCGImagePropertyOrientation: 6]))
        let out = try ImagePrep.prepare(data)
        XCTAssertEqual(out.width, 200)
        XCTAssertEqual(out.height, 400)
        let orientation = (properties(out.data)[kCGImagePropertyOrientation] as? NSNumber)?.intValue ?? 1
        XCTAssertEqual(orientation, 1, "the pixels are upright, not tagged")
    }

    func testAGIFUnderTheCapGoesAsItIs() throws {
        let data = try XCTUnwrap(Self.encode(Self.image(40, 40), .gif))
        let out = try ImagePrep.prepare(data, name: "dance.gif")
        XCTAssertEqual(out.data, data)
        XCTAssertEqual(out.mime, "image/gif")
        XCTAssertEqual(out.name, "dance.gif")
    }

    func testAPhotoWithNoNameIsNamedByItsTime() throws {
        let data = try XCTUnwrap(Self.encode(Self.image(10, 10), .jpeg))
        var parts = DateComponents()
        (parts.year, parts.month, parts.day, parts.hour, parts.minute, parts.second) = (2026, 10, 31, 9, 5, 7)
        let out = try ImagePrep.prepare(data, now: Calendar.current.date(from: parts)!)
        XCTAssertEqual(out.name, "IMG_20261031-090507.jpg")
    }

    func testTheThumbnailIsSmall() throws {
        let data = try XCTUnwrap(Self.encode(Self.image(1200, 900), .jpeg))
        let out = try ImagePrep.prepare(data)
        let thumb = CGImageSourceCreateWithData(out.thumbnail as CFData, nil).flatMap { CGImageSourceCreateImageAtIndex($0, 0, nil) }
        XCTAssertEqual(thumb.map { max($0.width, $0.height) }, ImagePrep.thumbnailEdge)
    }

    func testBytesThatAreNoImageAreRefused() {
        XCTAssertThrowsError(try ImagePrep.prepare(Data("not a photo".utf8))) {
            XCTAssertEqual($0 as? ImagePrepError, .unreadable)
        }
    }
}
