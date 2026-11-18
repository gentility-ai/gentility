import DeskPhoneKit
import SwiftUI
import TipKit
import UIKit

struct HomeView: View {
    @ObservedObject var app: AppModel
    @ObservedObject var home: HomeModel
    @State private var composing = false
    @State private var settings = false

    var body: some View {
        NavigationStack(path: $app.path) {
            HomeList(
                rows: home.rows, line: home.line, empty: home.isEmpty, joining: app.nodeState == .joining,
                notificationsOff: app.notificationsOff,
                undo: home.undo,
                onAct: { action, row in Task { await home.act(action, on: row) } },
                onUndo: { Task { await home.undoLast() } },
                onSettings: { settings = true }
            )
                .navigationTitle("Home")
                .navigationDestination(for: HomeRow.self) { row in
                    if let conversation = app.conversation(for: row) {
                        ConversationView(app: app, conversation: conversation)
                    }
                }
                .navigationDestination(isPresented: $settings) { SettingsView(app: app) }
                .refreshable { await home.refresh() }
                .task { await app.homeAppeared() }
                .overlay(alignment: .bottomTrailing) {
                    if home.loaded { ComposeButton { composing = true } }
                }
        }
        .sheet(isPresented: $composing) {
            NewThreadView(model: app.newThread()) { started in
                Task { await app.started(started) }
            }
        }
        // AppModel pauses this cadence for conversations except notOpen.
        .onAppear { home.appear() }
        .onDisappear { home.disappear() }
    }
}

/// From plain values: Needs you first, then Threads, then Later
/// (Snoozed and Settled), narrowed by a search and the filter menu.
struct HomeList: View {
    let rows: [HomeRow]
    let line: String?
    let empty: Bool
    let joining: Bool
    /// Section 3 step 1: denied in Settings.
    let notificationsOff: Bool
    /// The last action, while it can be taken back.
    let undo: Undo?
    let onAct: (HomeAction, HomeRow) -> Void
    let onUndo: () -> Void
    /// Nil where there is no Settings page to open.
    let onSettings: (() -> Void)?
    /// Set by Showcase: the filter to draw with, in place of the remembered one.
    private let preset: HomeListPreset?
    private let tip = HomeActionsTip()

    /// `owner/repo`; empty is every project.
    @AppStorage("home.project") private var project = ""
    @AppStorage("home.showLoopers") private var showLoopers = false
    /// Only the threads waiting on you.
    @AppStorage("home.waitingOnly") private var waitingOnly = false
    @State private var query: String
    /// The folds the person opened.
    @State private var open: Set<Shelf>
    /// The row whose snooze times are being offered.
    @State private var snoozing: HomeRow?

    init(rows: [HomeRow], line: String?, empty: Bool, joining: Bool, notificationsOff: Bool, undo: Undo? = nil, onAct: @escaping (HomeAction, HomeRow) -> Void = { _, _ in }, onUndo: @escaping () -> Void = {}, onSettings: (() -> Void)? = nil, query: String = "", open: Set<Shelf> = [], snoozing: HomeRow? = nil, preset: HomeListPreset? = nil) {
        self.rows = rows
        self.line = line
        self.empty = empty
        self.joining = joining
        self.notificationsOff = notificationsOff
        self.undo = undo
        self.onAct = onAct
        self.onUndo = onUndo
        self.onSettings = onSettings
        self.preset = preset
        _snoozing = State(initialValue: snoozing)
        _query = State(initialValue: query)
        _open = State(initialValue: open)
    }

    private var projects: [String] { Shelves.projects(rows) }

    private var filter: HomeFilter {
        HomeFilter(query: query, project: project.isEmpty ? nil : project, showLoopers: showLoopers, waitingOnly: waitingOnly).effective(projects: projects)
    }

    var body: some View {
        let filter = filter
        let now = Date()
        let shelves = Shelves.arrange(rows, filter: filter, now: now)
        let showsMac = RowFace.showsMacName(rows)
        let needs = shelves.first { $0.shelf == .needsYou }
        let live = shelves.first { $0.shelf == .live }
        let later = shelves.filter(\.shelf.folds)
        List {
            if joining || line != nil || notificationsOff {
                Section {
                    if joining {
                        Label("Joining your network…", systemImage: "network")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if let line {
                        Label(line, systemImage: "exclamationmark.icloud")
                            .font(.footnote).foregroundStyle(.secondary)
                    }
                    if notificationsOff {
                        Button {
                            if let settings = URL(string: UIApplication.openNotificationSettingsURLString) {
                                UIApplication.shared.open(settings)
                            }
                        } label: {
                            HStack(spacing: Theme.Space.s) {
                                Image(systemName: "bell.slash").foregroundStyle(Theme.attention)
                                Text("Notifications are off").foregroundStyle(Theme.ink)
                                Spacer()
                                Text("Turn on").fontWeight(.semibold).foregroundStyle(Theme.accent)
                            }
                            .font(.footnote)
                            .padding(.horizontal, Theme.Space.m)
                            .frame(minHeight: Theme.touch)
                            .surface(Theme.Corner.medium)
                        }
                        .buttonStyle(.plain)
                    }
                }
                .listRowBackground(Theme.ground)
                .listRowSeparator(.hidden)
            }
            if empty {
                Section {
                    Vacant(glyph: "laptopcomputer", title: "No threads yet", line: "Start one in Desk on your Mac and it shows here.")
                }
                .listRowBackground(Color.clear)
                .listRowSeparator(.hidden)
            } else if shelves.isEmpty {
                Section {
                    if query.trimmingCharacters(in: .whitespaces).isEmpty {
                        Vacant(glyph: "line.3.horizontal.decrease", title: "Nothing here", line: "No thread matches the filter.")
                    } else {
                        Vacant(glyph: "magnifyingglass", title: "No results", line: "No thread's title, branch or project has “\(query)”.")
                    }
                }
                .listRowBackground(Color.clear)
                .listRowSeparator(.hidden)
            }
            if let needs {
                Section {
                    rows(needs.rows, now: now, showsMac: showsMac)
                } header: {
                    NeedsYouHeader(count: needs.rows.count)
                }
            }
            if let live {
                Section {
                    rows(live.rows, now: now, showsMac: showsMac, tipOnFirst: true)
                } header: {
                    ShelfHeader(title: "Threads", count: nil)
                }
            }
            if !later.isEmpty {
                Section {
                    ForEach(later) { group in
                        DisclosureGroup(isExpanded: Binding(
                            get: { filter.narrowed || open.contains(group.shelf) },
                            set: { if $0 { open.insert(group.shelf) } else { open.remove(group.shelf) } }
                        )) {
                            rows(group.rows, now: now, showsMac: showsMac)
                        } label: {
                            HStack {
                                Label(group.title, systemImage: group.shelf == .snoozed ? "moon.zzz" : "checkmark.circle")
                                    .foregroundStyle(Theme.ink)
                                Spacer()
                                Text("\(group.rows.count)").font(.footnote).foregroundStyle(Theme.inkFaint).monospacedDigit()
                            }
                            .frame(minHeight: 32)
                        }
                        .listRowBackground(Theme.ground)
                        .listRowSeparatorTint(Theme.hairline)
                    }
                } header: {
                    ShelfHeader(title: "Later", count: nil)
                }
            }
            if let shown = filter.project {
                Section {
                    HStack(spacing: 4) {
                        Text("Showing \(shown) only ·").foregroundStyle(.secondary)
                        Button("Show all") { project = "" }
                            .buttonStyle(.borderless)
                    }
                    .font(.footnote)
                    .frame(maxWidth: .infinity)
                }
                .listRowBackground(Color.clear)
                .listRowSeparator(.hidden)
            }
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.ground)
        .task {
            if let preset {
                project = preset.project
                showLoopers = preset.showLoopers
                waitingOnly = preset.waitingOnly
            }
        }
        .confirmationDialog("Snooze until", isPresented: Binding(get: { snoozing != nil }, set: { if !$0 { snoozing = nil } }), titleVisibility: .visible, presenting: snoozing) { row in
            ForEach(SnoozePresets.at(Date()), id: \.label) { preset in
                Button("\(preset.label) · \(preset.when)") { act(.snooze(preset.until), row) }
            }
        }
        .safeAreaInset(edge: .bottom) {
            if let undo {
                HStack {
                    Text(undo.words)
                    Spacer()
                    Button("Undo", action: onUndo).fontWeight(.semibold)
                }
                .padding(.horizontal, Theme.Space.l).padding(.vertical, Theme.Space.m)
                .background(.regularMaterial, in: RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous))
                .overlay(RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous).strokeBorder(Theme.hairline))
                .shadow(color: .black.opacity(0.18), radius: 10, y: 3)
                .padding(.horizontal, Theme.Space.m).padding(.bottom, 4)
                .transition(.move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(.default, value: undo)
        .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .automatic), prompt: "Title, branch or project")
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Picker("Project", selection: $project) {
                        Text("All projects").tag("")
                        ForEach(projects, id: \.self) { Text($0).tag($0) }
                    }
                    Toggle("Show looper runs", isOn: $showLoopers)
                    Toggle(isOn: $waitingOnly) {
                        Label("Waiting on you", systemImage: "hand.raised")
                    }
                    if let onSettings {
                        Divider()
                        Button(action: onSettings) { Label("Settings", systemImage: "gearshape") }
                    }
                } label: {
                    Label(
                        "Filter",
                        systemImage: filter.project != nil || showLoopers || waitingOnly
                            ? "line.3.horizontal.decrease.circle.fill" : "line.3.horizontal.decrease.circle"
                    )
                }
            }
        }
    }

    /// Every action, by any path, goes through here: the first one retires
    /// the tip.
    private func act(_ action: HomeAction, _ row: HomeRow) {
        HomeActionsTip.used()
        onAct(action, row)
    }

    private func rows(_ rows: [HomeRow], now: Date, showsMac: Bool, tipOnFirst: Bool = false) -> some View {
        ForEach(rows) { row in
            let actions = HomeModel.actions(for: row, now: now)
            let menu = HomeModel.menu(for: row, now: now)
            let face = RowFace(row, now: now, showsMacName: showsMac)
            // The link under the row, unseen: the row draws no chevron.
            ZStack(alignment: .leading) {
                NavigationLink(value: row) { EmptyView() }.opacity(0)
                HomeRowView(row: row, face: face)
            }
            .accessibilityIdentifier("home.row.\(row.title)")
            .listRowBackground(Theme.ground)
            .listRowSeparatorTint(Theme.hairline)
            .listRowInsets(EdgeInsets(top: 10, leading: Theme.Space.l, bottom: 10, trailing: Theme.Space.l))
            .alignmentGuide(.listRowSeparatorLeading) { _ in 32 }
            .modifier(TipAnchor(tip: tipOnFirst && row.id == rows.first?.id ? tip : nil))
            .contextMenu {
                menuItems(menu, row)
            }
            .accessibilityActions {
                ForEach(Array(menu.enumerated()), id: \.offset) { _, item in
                    accessibilityItems(item, row)
                }
            }
            .swipeActions(edge: .leading) {
                ForEach(actions.leading, id: \.self) { button($0, row) }
            }
            .swipeActions(edge: .trailing) {
                ForEach(actions.trailing.reversed(), id: \.self) { button($0, row) }
            }
        }
    }
}

/// The filter Showcase draws a screen with.
struct HomeListPreset {
    var project: String
    var showLoopers: Bool
    var waitingOnly = false
}

extension HomeList {
    /// One swipe button. Snooze asks when first; the rest act at once.
    @ViewBuilder
    private func button(_ action: HomeAction, _ row: HomeRow) -> some View {
        switch action {
        case .settle:
            Button { act(.settle, row) } label: { Label("Settle", systemImage: "checkmark.circle") }.tint(.green)
        case .unsettle:
            Button { act(.unsettle, row) } label: { Label("Unsettle", systemImage: "arrow.uturn.backward.circle") }.tint(.green)
        case .snooze:
            Button { snoozing = row } label: { Label("Snooze", systemImage: "moon.zzz") }.tint(.indigo)
        case .wake:
            Button { act(.wake, row) } label: { Label("Wake", systemImage: "sun.max") }.tint(.orange)
        case .archive:
            Button(role: .destructive) { act(.archive, row) } label: { Label("Archive", systemImage: "archivebox") }
        case .unarchive:
            EmptyView()
        }
    }

    /// The long-press menu, as `HomeModel.menu(for:)` orders it.
    @ViewBuilder
    func menuItems(_ menu: [HomeMenuItem], _ row: HomeRow) -> some View {
        ForEach(Array(menu.enumerated()), id: \.offset) { _, item in
            switch item {
            case .settle:
                Button { act(.settle, row) } label: { Label("Settle", systemImage: "checkmark.circle") }
            case .unsettle:
                Button { act(.unsettle, row) } label: { Label("Unsettle", systemImage: "arrow.uturn.backward.circle") }
            case .snooze(let presets):
                Menu {
                    ForEach(presets, id: \.label) { preset in
                        Button("\(preset.label) · \(preset.when)") { act(.snooze(preset.until), row) }
                    }
                } label: {
                    Label("Snooze", systemImage: "moon.zzz")
                }
            case .wake:
                Button { act(.wake, row) } label: { Label("Wake", systemImage: "sun.max") }
            case .archive:
                Divider()
                Button(role: .destructive) { act(.archive, row) } label: { Label("Archive", systemImage: "archivebox") }
            case .managedOnMac:
                Button {} label: { Label("Looper runs are managed on your Mac", systemImage: "laptopcomputer") }
                    .disabled(true)
            }
        }
    }

    /// The menu again, for VoiceOver.
    @ViewBuilder
    private func accessibilityItems(_ item: HomeMenuItem, _ row: HomeRow) -> some View {
        switch item {
        case .settle: Button("Settle") { act(.settle, row) }
        case .unsettle: Button("Unsettle") { act(.unsettle, row) }
        case .snooze(let presets):
            ForEach(presets, id: \.label) { preset in
                Button("Snooze until \(preset.when)") { act(.snooze(preset.until), row) }
            }
        case .wake: Button("Wake") { act(.wake, row) }
        case .archive: Button("Archive") { act(.archive, row) }
        case .managedOnMac: EmptyView()
        }
    }
}

/// "Needs you", with its count, in the colour of something waiting.
struct NeedsYouHeader: View {
    let count: Int

    var body: some View {
        ShelfHeader(title: "Needs you", count: count, tone: Theme.attention)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Needs you, \(count) \(count == 1 ? "thread" : "threads")")
            .accessibilityAddTraits(.isHeader)
    }
}

/// A shelf's name over its rows, small and quiet, with a count where it has one.
struct ShelfHeader: View {
    let title: String
    let count: Int?
    var tone: Color = Theme.inkSoft

    var body: some View {
        HStack(spacing: 6) {
            Text(title).font(.footnote.weight(.semibold)).foregroundStyle(tone)
            if let count {
                Text("\(count)").font(.footnote.monospacedDigit()).foregroundStyle(Theme.inkFaint)
            }
        }
        .textCase(nil)
        .padding(.top, Theme.Space.s)
        .listRowInsets(EdgeInsets(top: 0, leading: Theme.Space.l, bottom: 6, trailing: Theme.Space.l))
        .accessibilityAddTraits(.isHeader)
    }
}

/// The tip, as a popover on the row it names. The iOS 17 popover cannot be
/// named while the newer SDK's overload is in scope, so before iOS 26 the
/// tip draws inline above the row instead.
struct TipAnchor: ViewModifier {
    let tip: HomeActionsTip?

    func body(content: Content) -> some View {
        if #available(iOS 26.0, *) {
            content.popoverTip(tip, arrowEdge: .top)
        } else if let tip {
            VStack(alignment: .leading, spacing: 8) {
                TipView(tip, arrowEdge: .bottom)
                content
            }
        } else {
            content
        }
    }
}

/// Settle, Snooze and Archive, found by holding a row.
struct HomeActionsTip: Tip {
    static let action = Tips.Event(id: "home-action")

    var title: Text { Text("Settle a thread when you're done with it") }
    var message: Text? { Text("Touch and hold a thread for Settle, Snooze and Archive, or swipe right to settle.") }
    var image: Image? { Image(systemName: "hand.tap") }
    var rules: [Rule] {
        #Rule(Self.action) { $0.donations.count == 0 }
    }

    /// Any Home action, by any path, retires the tip.
    static func used() {
        Task { await action.donate() }
    }

    /// At launch. Showcase shows the tip on its one screen and hides it on
    /// every other.
    static func configure() {
        #if DEBUG
        if let screen = ProcessInfo.processInfo.environment["DESK_PHONE_SHOWCASE"] {
            if screen == "home-tip" {
                try? Tips.resetDatastore()
                Tips.showAllTipsForTesting()
            } else {
                Tips.hideAllTipsForTesting()
            }
        }
        #endif
        try? Tips.configure([.displayFrequency(.immediate)])
    }
}

struct HomeRowView: View {
    let row: HomeRow
    let face: RowFace
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.dynamicTypeSize) private var dynamicTypeSize

    @ViewBuilder
    private var branchMark: some View {
        if let branch = face.branch {
            HStack(spacing: 4) {
                Image(systemName: "arrow.triangle.branch")
                Text(branch).lineLimit(1).truncationMode(.middle)
            }
        }
    }

    private var macMark: some View {
        HStack(spacing: 4) {
            Image(systemName: face.mac.asleep ? "moon.fill" : "laptopcomputer")
            if let name = face.mac.name {
                Text(face.mac.asleep ? "\(name), asleep" : name).lineLimit(1)
            } else if face.mac.asleep {
                Text("asleep")
            }
        }
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: Theme.Space.m) {
            glyph.frame(width: 20)
            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                    Text(row.title)
                        .font(.body.weight(face.needsYou ? .semibold : .regular))
                        .foregroundStyle(face.recedes ? Theme.inkSoft : Theme.ink)
                        .lineLimit(2)
                    Spacer(minLength: 4)
                    Text(face.age).font(.footnote).monospacedDigit().foregroundStyle(Theme.inkFaint).fixedSize()
                }
                if let line = face.attentionLine {
                    Text(line).font(.subheadline).foregroundStyle(Theme.inkSoft).lineLimit(2)
                }
                Group {
                    if dynamicTypeSize.isAccessibilitySize {
                        VStack(alignment: .leading, spacing: 2) {
                            status
                            place
                            branchMark
                            macMark
                        }
                    } else {
                        // What gives way as the row narrows: the Mac, then the branch.
                        ViewThatFits(in: .horizontal) {
                            HStack(spacing: Theme.Space.s) { status; place; branchMark; macMark }
                            // The last fits always: a long branch is cut in its middle.
                            HStack(spacing: Theme.Space.s) { status; place.fixedSize(); branchMark }
                        }
                    }
                }
                .font(.footnote)
                .foregroundStyle(Theme.inkFaint)
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(face.accessibilityLabel)
    }

    /// The row's state at its leading edge: what it waits on, that it works,
    /// or a quiet ring for a thread at rest.
    @ViewBuilder private var glyph: some View {
        if let word = face.word {
            symbol(word).font(.subheadline.weight(.semibold)).foregroundStyle(word.tone.color)
        } else {
            Image(systemName: "circle").font(.subheadline).foregroundStyle(Theme.inkFaint.opacity(0.6))
        }
    }

    private var place: some View {
        HStack(spacing: 5) {
            if let chip = face.chip {
                ChipView(chip: chip)
            }
            if let place = face.place {
                Text(place).foregroundStyle(Theme.inkSoft).lineLimit(1).truncationMode(.tail)
            }
        }
    }

    @ViewBuilder private var status: some View {
        if let word = face.word {
            Text(word.text).fontWeight(.medium).foregroundStyle(word.tone.color).lineLimit(1).fixedSize()
        }
    }

    @ViewBuilder
    private func symbol(_ word: StatusWord) -> some View {
        if word.tone == .working, !reduceMotion, #available(iOS 18, *) {
            Image(systemName: word.symbol).symbolEffect(.rotate, options: .repeat(.continuous))
        } else {
            Image(systemName: word.symbol)
        }
    }
}

/// A project's letter tile, in the Mac's hue for it.
struct ChipView: View {
    let chip: Chip

    var body: some View {
        let fill = Self.hues[chip.colorIndex % Self.hues.count]
        ZStack {
            if chip.round {
                Circle().fill(fill)
            } else {
                RoundedRectangle(cornerRadius: 4).fill(fill)
            }
            Text(chip.letter).font(.system(size: 9, weight: .bold)).foregroundStyle(.white)
        }
        .frame(width: 15, height: 15)
        .accessibilityHidden(true)
    }

    /// The Mac's eighteen hues (`chip.logic.ts`), at their 600 shade so a
    /// white letter reads on each.
    static let hues: [Color] = [
        0x4b5563, 0xdc2626, 0xea580c, 0xd97706, 0xca8a04, 0x65a30d, 0x16a34a, 0x059669, 0x0d9488,
        0x0891b2, 0x0284c7, 0x2563eb, 0x4f46e5, 0x7c3aed, 0x9333ea, 0xc026d3, 0xdb2777, 0xe11d48,
    ].map { (hex: Int) in
        Color(red: Double((hex >> 16) & 0xff) / 255, green: Double((hex >> 8) & 0xff) / 255, blue: Double(hex & 0xff) / 255)
    }
}

extension StatusTone {
    var color: Color {
        switch self {
        case .approval: Theme.attention
        case .input: Theme.accent
        case .working: Theme.accent
        case .snoozed: Theme.inkSoft
        case .settled: Theme.good
        }
    }
}

/// An empty list: what is missing and how it fills.
private struct Vacant: View {
    let glyph: String
    let title: String
    let line: String

    var body: some View {
        VStack(spacing: Theme.Space.s) {
            Image(systemName: glyph).font(.title2).foregroundStyle(Theme.inkFaint).padding(.bottom, Theme.Space.xs)
            Text(title).font(.headline).foregroundStyle(Theme.ink)
            Text(line).font(.subheadline).foregroundStyle(Theme.inkSoft).multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.top, 72).padding(.horizontal, Theme.Space.xl)
        .accessibilityElement(children: .combine)
    }
}

/// Start a thread: where a thumb rests.
struct ComposeButton: View {
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Image(systemName: "square.and.pencil")
                .font(.title3.weight(.semibold))
                .foregroundStyle(.white)
                .frame(width: 56, height: 56)
                .background(Theme.accentFill, in: Circle())
                .shadow(color: .black.opacity(0.25), radius: 12, y: 4)
        }
        .padding(.trailing, Theme.Space.l).padding(.bottom, Theme.Space.l)
        .accessibilityLabel("New thread")
    }
}
