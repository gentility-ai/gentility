import Foundation

/// Written by Xcode from the source revision and build time. This identifies
/// simulator candidates separately from the build eventually delivered.
enum PhoneBuild {
    private final class BundleMarker {}

    static var stamp: String {
        guard let url = Bundle(for: BundleMarker.self).url(forResource: "PhoneBuild", withExtension: "txt"),
              let value = try? String(contentsOf: url, encoding: .utf8),
              !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            return "ios-unstamped"
        }
        return value.trimmingCharacters(in: .whitespacesAndNewlines)
    }
}
