import Foundation

/// The name a claim carries and the node is labelled with. iOS answers
/// `UIDevice.current.name` with the model ("iPhone") to an app without the
/// user-assigned-device-name entitlement, so the person may change it on the
/// scanner, and the choice is remembered.
public enum PhoneName {
    private static let key = "ai.gentility.desk.phone.name"

    public static func current(defaults: UserDefaults = .standard, device: String) -> String {
        let chosen = defaults.string(forKey: key)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return chosen.isEmpty ? device : chosen
    }

    public static func remember(_ name: String, defaults: UserDefaults = .standard) {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            defaults.removeObject(forKey: key)
        } else {
            defaults.set(String(trimmed.prefix(63)), forKey: key)
        }
    }

    /// The node's own hostname: a DNS label made from the name.
    public static func hostname(_ name: String) -> String {
        var label = ""
        for character in name.lowercased() {
            if character.isASCII, character.isLetter || character.isNumber {
                label.append(character)
            } else if !label.isEmpty, label.last != "-" {
                label.append("-")
            }
        }
        while label.hasSuffix("-") { label.removeLast() }
        var cut = String(label.prefix(63))
        while cut.hasSuffix("-") { cut.removeLast() }
        return cut.isEmpty ? "iphone" : cut
    }
}
