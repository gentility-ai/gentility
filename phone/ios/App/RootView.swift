import DeskPhoneKit
import SwiftUI

struct RootView: View {
    @ObservedObject var app: AppModel

    var body: some View {
        Group {
            switch app.route {
            case .launching:
                ProgressView()
            case .pairing:
                PairingView(app: app, pairing: app.pairing, scanner: app.scanner)
            case .home:
                if let home = app.home {
                    TabsView(app: app, home: home)
                }
            case .failure(let failure):
                FailureView(failure: failure) { app.again() }
            }
        }
        .task { await app.launch() }
    }
}
