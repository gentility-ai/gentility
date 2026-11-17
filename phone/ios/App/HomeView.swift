import DeskPhoneKit
import SwiftUI
import TipKit
import UIKit

struct HomeView: View {
    @ObservedObject var app: AppModel
    @ObservedObject var home: HomeModel

    var body: some View {
        NavigationStack(path: $app.path) {
            HomeList(
                rows: home.rows, line: home.line, empty: home.isEmpty, joining: app.nodeState == .joining,
                notificationsOff: app.notificationsOff,
                undo: home.undo,
                onAct: { action, row in Task { await home.act(action, on: row) } },
                onUndo: { Task { await home.undoLast() } }
            )
                .navigationTitle("Home")
                .navigationDestination(for: HomeRow.self) { row in
                    if let conversation = app.conversation(for: row) {
                        ConversationView(app: app, conversation: conversation)
                    }
                }
                .refreshable { await home.refresh() }
                .task { await app.homeAppeared() }
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

    init(rows: [HomeRow], line: String?, empty: Bool, joining: Bool, notificationsOff: Bool, undo: Undo? = nil, onAct: @escaping (HomeAction, HomeRow) -> Void = { _, _ in }, onUndo: @escaping () -> Void = {}, query: String = "", open: Set<Shelf> = [], snoozing: HomeRow? = nil, preset: HomeListPreset? = nil) {
        self.rows = rows
        self.line = line
        self.empty = empty
        self.joining = joining
        self.notificationsOff = notificationsOff
        self.undo = undo
        self.onAct = onAct
        self.onUndo = onUndo
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
                            Label("Notifications are off — turn them on in Settings", systemImage: "bell.slash")
                                .font(.footnote)
                        }
                    }
                }
            }
            if empty {
                Section {
                    ContentUnavailableView {
                        Label("No threads yet", systemImage: "laptopcomputer")
                    } description: {
                        Text("No sessions yet. Start one in Desk on your Mac.")
                    }
                }
                .listRowBackground(Color.clear)
            } else if shelves.isEmpty {
                Section {
                    if query.trimmingCharacters(in: .whitespaces).isEmpty {
                        ContentUnavailableView("Nothing here", systemImage: "line.3.horizontal.decrease.circle", description: Text("No thread matches the filter."))
                    } else {
                        ContentUnavailableView.search(text: query)
                    }
                }
                .listRowBackground(Color.clear)
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
                    Text("Threads").font(.headline).foregroundStyle(.primary).textCase(nil)
                        .accessibilityAddTraits(.isHeader)
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
                                    .foregroundStyle(.primary)
                                Spacer()
                                Text("\(group.rows.count)").foregroundStyle(.secondary).monospacedDigit()
                            }
                        }
                    }
                } header: {
                    Text("Later").font(.subheadline).foregroundStyle(.secondary).textCase(nil)
                        .accessibilityAddTraits(.isHeader)
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
            }
        }
        .listStyle(.insetGrouped)
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
                .padding(.horizontal, 16).padding(.vertical, 12)
                .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
                .padding(.horizontal, 12).padding(.bottom, 4)
                .transition(.move(edge: .bottom).combined(with: .opacity))
            }
        }
        .animation(.default, value: undo)
        .searchable(text: $query, prompt: "Title, branch or project")
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
            NavigationLink(value: row) {
                HomeRowView(row: row, face: face)
            }
            .accessibilityIdentifier("home.row.\(row.title)")
            .listRowBackground(face.needsYou ? AnyView(NeedsYouGround()) : AnyView(Color(.secondarySystemGroupedBackground)))
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

/// "Needs you", orange, with its count.
struct NeedsYouHeader: View {
    let count: Int

    var body: some View {
        HStack(spacing: 8) {
            Label("Needs you", systemImage: "exclamationmark.bubble.fill")
                .font(.headline)
                .foregroundStyle(.orange)
            Text("\(count)")
                .font(.caption.bold())
                .foregroundStyle(.white)
                .padding(.horizontal, 6).padding(.vertical, 1)
                .background(Capsule().fill(Color.orange))
        }
        .textCase(nil)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Needs you, \(count) \(count == 1 ? "thread" : "threads")")
        .accessibilityAddTraits(.isHeader)
    }
}

/// A Needs-you row's ground: an orange wash and a leading bar.
struct NeedsYouGround: View {
    var body: some View {
        Color("NeedsYouRow")
            .background(Color(.secondarySystemGroupedBackground))
            .overlay(alignment: .leading) {
                Rectangle().fill(.orange).frame(width: 3)
            }
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
        VStack(alignment: .leading, spacing: 3) {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 6) {
                    place
                    Spacer(minLength: 4)
                    status
                }
                VStack(alignment: .leading, spacing: 3) {
                    place
                    status
                }
            }
            .font(.footnote)

            Text(row.title)
                .font(.body)
                .fontWeight(face.needsYou ? .semibold : .regular)
                .foregroundStyle(face.recedes ? .secondary : .primary)
                .lineLimit(2)

            Group {
                if dynamicTypeSize.isAccessibilitySize {
                    VStack(alignment: .leading, spacing: 2) {
                        branchMark
                        macMark
                    }
                } else {
                    HStack(spacing: 4) {
                        branchMark
                        Spacer(minLength: 8)
                        macMark
                    }
                }
            }
            .font(.caption)
            .foregroundStyle(.tertiary)

            if let line = face.attentionLine {
                Text(line)
                    .font(.subheadline)
                    .foregroundStyle(.primary)
                    .lineLimit(2)
            }
        }
        .padding(.vertical, 2)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(face.accessibilityLabel)
    }

    private var place: some View {
        HStack(spacing: 6) {
            if let chip = face.chip {
                ChipView(chip: chip)
            }
            if let place = face.place {
                Text(place).foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
            }
        }
    }

    private var status: some View {
        HStack(spacing: 6) {
            if let word = face.word {
                Label {
                    Text(word.text)
                } icon: {
                    symbol(word)
                }
                .labelStyle(.titleAndIcon)
                .font(.footnote.weight(.semibold))
                .foregroundStyle(word.tone.color)
                .lineLimit(1)
                .fixedSize()
            }
            Text(face.age).monospacedDigit().foregroundStyle(.tertiary).fixedSize()
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
            Text(chip.letter).font(.system(size: 10, weight: .bold)).foregroundStyle(.white)
        }
        .frame(width: 18, height: 18)
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
        case .approval: .orange
        case .input: .indigo
        case .working: .cyan
        case .snoozed: .indigo
        case .settled: .green
        }
    }
}
