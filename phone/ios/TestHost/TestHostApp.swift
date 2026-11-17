import SwiftUI

/// Hosts DeskPhoneKitTests: the keychain refuses a test bundle that runs in
/// no app. It draws nothing and links no Go.
@main
struct TestHostApp: App {
    var body: some Scene {
        WindowGroup { Text("DeskPhoneKit tests") }
    }
}
