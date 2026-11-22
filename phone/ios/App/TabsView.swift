import DeskPhoneKit
import SwiftUI

/// The app's parts, each with a stack of its own: opening a run in Loopers
/// leaves a thread open in Threads where it was.
struct TabsView: View {
    enum Tab: Hashable { case threads, loopers, search }

    @ObservedObject var app: AppModel
    @ObservedObject var home: HomeModel
    @State private var tab: Tab = .threads
    @State private var loopersPath: [LooperRoute] = []

    var body: some View {
        TabView(selection: $tab) {
            HomeView(app: app, home: home)
                .tabItem { Label("Threads", systemImage: "bubble.left.and.text.bubble.right") }
                .tag(Tab.threads)
            LoopersTab(app: app, path: $loopersPath)
                .tabItem { Label("Loopers", systemImage: "arrow.triangle.2.circlepath") }
                .tag(Tab.loopers)
            SearchTab(app: app, home: home, onThread: open, onLooper: open)
                .tabItem { Label("Search", systemImage: "magnifyingglass") }
                .tag(Tab.search)
        }
        .tint(Theme.accent)
        // A notification's tap sets Home's path: show the tab it belongs to.
        .onChange(of: app.path) { _, path in
            if !path.isEmpty { tab = .threads }
        }
    }

    private func open(_ row: HomeRow) {
        tab = .threads
        app.path = [row]
    }

    private func open(_ looper: Looper) {
        tab = .loopers
        loopersPath = [.looper(looper)]
    }
}

/// Search, across threads and loopers.
struct SearchTab: View {
    @ObservedObject var app: AppModel
    @ObservedObject var home: HomeModel
    let onThread: (HomeRow) -> Void
    let onLooper: (Looper) -> Void
    @State private var query = ""
    @State private var settings = false

    var body: some View {
        NavigationStack {
            SearchResults(rows: home.rows, loopers: app.loopers?.loopers ?? [], query: query, onThread: onThread, onLooper: onLooper)
                .navigationTitle("Search")
                .toolbar {
                    ToolbarItem(placement: .topBarLeading) { SettingsGear { settings = true } }
                }
                .navigationDestination(isPresented: $settings) { SettingsView(app: app).toolbar(.hidden, for: .tabBar) }
                .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .always), prompt: "Threads and loopers")
                .task { await app.loopers?.refresh() }
        }
    }
}

/// The results, from plain values.
struct SearchResults: View {
    let rows: [HomeRow]
    let loopers: [Looper]
    let query: String
    var onThread: (HomeRow) -> Void = { _ in }
    var onLooper: (Looper) -> Void = { _ in }

    private var needle: String { query.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        let now = Date()
        // Every kind is searched, whatever the Threads filter hides.
        let threads = needle.isEmpty ? [] : Shelves.arrange(rows, filter: HomeFilter(query: needle, project: nil, hidden: []), now: now).flatMap(\.rows)
        let found = needle.isEmpty ? [] : loopers.filter { $0.name.localizedCaseInsensitiveContains(needle) }
        let showsMac = RowFace.showsMacName(rows)
        List {
            if needle.isEmpty {
                Vacant(glyph: "magnifyingglass", title: "Search", line: "Find a thread by its title, branch or project, or a looper by its name.")
                    .listRowBackground(Color.clear).listRowSeparator(.hidden)
            } else if threads.isEmpty, found.isEmpty {
                Vacant(glyph: "magnifyingglass", title: "No results", line: "Nothing matches “\(needle)”.")
                    .listRowBackground(Color.clear).listRowSeparator(.hidden)
            }
            if !found.isEmpty {
                Section {
                    ForEach(found) { looper in
                        Button { onLooper(looper) } label: { LooperRowView(looper: looper, now: now) }
                            .buttonStyle(.plain)
                            .listRowBackground(Theme.ground).listRowSeparatorTint(Theme.hairline)
                    }
                } header: {
                    ShelfHeader(title: "Loopers", count: nil)
                }
            }
            if !threads.isEmpty {
                Section {
                    ForEach(threads) { row in
                        Button { onThread(row) } label: {
                            HomeRowView(row: row, face: RowFace(row, now: now, showsMacName: showsMac))
                        }
                        .buttonStyle(.plain)
                        .listRowBackground(Theme.ground).listRowSeparatorTint(Theme.hairline)
                    }
                } header: {
                    ShelfHeader(title: "Threads", count: nil)
                }
            }
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.ground)
    }
}
