import DeskPhoneKit
import SwiftUI
import UIKit

/// Where the Loopers tab's stack can stand.
enum LooperRoute: Hashable {
    case looper(Looper)
    case run(Looper, LooperRunRow)
}

/// The Loopers tab: the loopers a person owns, and each one's page.
struct LoopersTab: View {
    @ObservedObject var app: AppModel
    @Binding var path: [LooperRoute]
    @State private var settings = false

    var body: some View {
        NavigationStack(path: $path) {
            Group {
                if let loopers = app.loopers {
                    LoopersScreen(model: loopers)
                } else {
                    Vacant(glyph: "arrow.triangle.2.circlepath", title: "No loopers yet", line: "Make one on the web and it shows here.")
                }
            }
            .navigationTitle("Loopers")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { SettingsGear { settings = true } }
            }
            .navigationDestination(isPresented: $settings) { SettingsView(app: app).toolbar(.hidden, for: .tabBar) }
            .navigationDestination(for: LooperRoute.self) { route in
                if let loopers = app.loopers {
                    switch route {
                    case .looper(let looper):
                        LooperPageScreen(page: loopers.page(for: looper), onRun: { path.append(.run(looper, $0)) })
                            .toolbar(.hidden, for: .tabBar)
                    case .run(let looper, let run):
                        LooperRunScreen(model: loopers.page(for: looper).runModel(for: run))
                            .toolbar(.hidden, for: .tabBar)
                    }
                }
            }
        }
    }
}

private struct LoopersScreen: View {
    @ObservedObject var model: LoopersModel

    var body: some View {
        LoopersList(loopers: model.loopers, loaded: model.loaded, line: model.line)
            .refreshable { await model.refresh() }
            .task { await model.refresh() }
    }
}

/// The list, from plain values.
struct LoopersList: View {
    let loopers: [Looper]
    let loaded: Bool
    let line: String?

    var body: some View {
        let now = Date()
        List {
            if let line {
                Label(line, systemImage: "exclamationmark.icloud")
                    .font(.footnote).foregroundStyle(Theme.inkSoft)
                    .listRowBackground(Theme.ground).listRowSeparator(.hidden)
            }
            if loaded, loopers.isEmpty, line == nil {
                Vacant(glyph: "arrow.triangle.2.circlepath", title: "No loopers yet", line: "Make one on the web and it shows here.")
                    .listRowBackground(Color.clear).listRowSeparator(.hidden)
            }
            ForEach(loopers) { looper in
                ZStack(alignment: .leading) {
                    NavigationLink(value: LooperRoute.looper(looper)) { EmptyView() }.opacity(0)
                    LooperRowView(looper: looper, now: now)
                }
                .accessibilityIdentifier("loopers.row.\(looper.name)")
                .listRowBackground(Theme.ground)
                .listRowSeparatorTint(Theme.hairline)
                .listRowInsets(EdgeInsets(top: 10, leading: Theme.Space.l, bottom: 10, trailing: Theme.Space.l))
            }
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.ground)
        .overlay { if !loaded { ProgressView() } }
    }
}

extension LooperTone {
    var color: Color {
        switch self {
        case .asking: Theme.attention
        case .working: Theme.accent
        case .bad: Theme.bad
        case .faint: Theme.inkFaint
        }
    }
}

/// One looper: its name, its schedule in words, and its one state.
struct LooperRowView: View {
    let looper: Looper
    let now: Date

    var body: some View {
        let state = looper.state(now: now)
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
                Text(looper.name)
                    .font(.body.weight(looper.asking ? .semibold : .regular))
                    .foregroundStyle(Theme.ink).lineLimit(2)
                if looper.muted {
                    Image(systemName: "bell.slash").font(.caption2).foregroundStyle(Theme.inkFaint)
                        .accessibilityLabel("Muted")
                }
                Spacer(minLength: 4)
                Text(state.text).font(.footnote.weight(state.tone == .faint ? .regular : .semibold))
                    .foregroundStyle(state.tone.color)
            }
            Text(looper.scheduleLine).font(.subheadline).foregroundStyle(Theme.inkSoft).lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }
}

/// A looper run tapped in Threads: its looper's page, when it is one of the
/// person's.
struct LooperRunLanding: View {
    @ObservedObject var app: AppModel
    let row: HomeRow

    var body: some View {
        Group {
            if let loopers = app.loopers {
                Landing(loopers: loopers, looperID: row.looperID, title: row.title)
            } else {
                Vacant(glyph: "arrow.triangle.2.circlepath", title: row.title, line: LooperCopy.opensOnWeb)
            }
        }
        .navigationBarTitleDisplayMode(.inline)
    }

    private struct Landing: View {
        @ObservedObject var loopers: LoopersModel
        let looperID: String?
        let title: String
        @State private var opened: LooperRunRow?

        var body: some View {
            Group {
                if let looper = looperID.flatMap(loopers.looper(id:)) {
                    LooperPageScreen(page: loopers.page(for: looper), onRun: { opened = $0 })
                        .navigationDestination(item: $opened) { run in
                            LooperRunScreen(model: loopers.page(for: looper).runModel(for: run))
                        }
                } else if let line = loopers.line {
                    // A list that did not load cannot say whose the looper is.
                    Vacant(glyph: "exclamationmark.icloud", title: title, line: line)
                } else if loopers.loaded {
                    Vacant(glyph: "arrow.triangle.2.circlepath", title: title, line: LooperCopy.notYours)
                } else {
                    ProgressView()
                }
            }
            .task { if !loopers.loaded { await loopers.refresh() } }
        }
    }
}

/// A looper's page, with its model kept for as long as it is on screen.
struct LooperPageScreen: View {
    @StateObject private var page: LooperPageModel
    let onRun: (LooperRunRow) -> Void
    @State private var editing = false
    @State private var message = ""

    init(page: @autoclosure @escaping () -> LooperPageModel, onRun: @escaping (LooperRunRow) -> Void) {
        _page = StateObject(wrappedValue: page())
        self.onRun = onRun
    }

    var body: some View {
        LooperPage(
            looper: page.looper, runs: page.runs, loaded: page.loaded, line: page.line, open: page.open.map { WaitingRequest(run: $0.run, request: $0.request) },
            runBlocked: page.runBlocked, busy: page.busy, notice: page.notice, failure: page.failure, message: $message,
            onRunNow: { Task { await page.run() } },
            onSend: {
                let words = message
                Task {
                    if await page.run(message: words) { message = "" }
                }
            },
            onRun: onRun
        )
        .navigationTitle(page.looper.name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Button { editing = true } label: { Label("Edit", systemImage: "slider.horizontal.3") }
                    Button { Task { await page.setMuted(!page.looper.muted) } } label: {
                        Label(page.looper.muted ? "Unmute notifications" : "Mute notifications", systemImage: page.looper.muted ? "bell" : "bell.slash")
                    }
                    Section {
                        Button { copy(page.looper.name) } label: { Label("Copy looper name", systemImage: "textformat") }
                        Button { copy(page.looper.id) } label: { Label("Copy looper id", systemImage: "number") }
                    }
                } label: {
                    Label("Looper actions", systemImage: "ellipsis")
                }
            }
        }
        .sheet(isPresented: $editing) {
            LooperEditSheet(page: page)
        }
        .refreshable { await page.refresh() }
        .task { await page.refresh() }
        .onChange(of: page.started) { _, run in
            if let run {
                page.started = nil
                onRun(run)
            }
        }
        .task(id: page.notice) {
            guard page.notice != nil else { return }
            try? await Task.sleep(for: .seconds(3))
            page.notice = nil
        }
    }
}

func copy(_ text: String) {
    UIPasteboard.general.string = text
    UIImpactFeedbackGenerator(style: .light).impactOccurred()
}

/// A request waiting on a person, and the run that asked.
struct WaitingRequest: Identifiable, Equatable {
    let run: LooperRunRow
    let request: LooperRequest
    var id: String { request.id }
}

/// The page, from plain values.
struct LooperPage: View {
    let looper: Looper
    let runs: [LooperRunRow]
    let loaded: Bool
    /// Why its runs did not load.
    var line: String? = nil
    let open: [WaitingRequest]
    let runBlocked: String?
    let busy: Bool
    let notice: String?
    let failure: String?
    @Binding var message: String
    var onRunNow: () -> Void = {}
    var onSend: () -> Void = {}
    var onRun: (LooperRunRow) -> Void = { _ in }

    var body: some View {
        List {
            Section {
                VStack(alignment: .leading, spacing: Theme.Space.m) {
                    VStack(alignment: .leading, spacing: 4) {
                        HStack(spacing: Theme.Space.s) {
                            Text(looper.scheduleLine).font(.subheadline).foregroundStyle(Theme.ink)
                            if looper.muted {
                                Image(systemName: "bell.slash").font(.caption).foregroundStyle(Theme.inkFaint).accessibilityLabel("Muted")
                            }
                        }
                        let state = looper.state(now: Date())
                        Text(state.text).font(.footnote).foregroundStyle(state.tone.color)
                    }
                    Button(action: onRunNow) {
                        HStack(spacing: Theme.Space.s) {
                            if busy { ProgressView().controlSize(.small).tint(Theme.onAccent) } else { Image(systemName: "play.fill") }
                            Text("Run now").fontWeight(.semibold)
                        }
                        .frame(maxWidth: .infinity, minHeight: Theme.touch)
                        .foregroundStyle(canRun ? Theme.onAccent : Theme.inkFaint)
                        .background(canRun ? Theme.accent : Theme.field, in: RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
                    }
                    .buttonStyle(.plain)
                    .allowsHitTesting(canRun && !busy)
                    .accessibilityValue(canRun && !busy ? "" : "Not ready")
                    if let said = failure ?? runBlocked {
                        Text(said).font(.footnote).foregroundStyle(failure == nil ? Theme.inkSoft : Theme.bad)
                    }
                }
                .padding(.vertical, Theme.Space.s)
            }
            .listRowBackground(Theme.ground).listRowSeparator(.hidden)

            if !open.isEmpty {
                Section {
                    ForEach(open) { waiting in
                        Button { onRun(waiting.run) } label: {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(waiting.request.rendered ?? waiting.request.body ?? waiting.request.kindLabel ?? "A request")
                                    .font(.subheadline).foregroundStyle(Theme.ink).lineLimit(3)
                                Text("\(RunFace(waiting.run).title) · \(waiting.request.kindLabel ?? "Request")")
                                    .font(.footnote).foregroundStyle(Theme.inkSoft)
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                    }
                } header: {
                    HStack {
                        ShelfHeader(title: "Requests", count: nil, tone: Theme.attention)
                        Spacer()
                        Text("\(open.count) waiting").font(.footnote).foregroundStyle(Theme.attention).textCase(nil)
                    }
                }
                .listRowBackground(Theme.ground).listRowSeparatorTint(Theme.hairline)
            }

            Section {
                if let line {
                    Label(line, systemImage: "exclamationmark.icloud").font(.footnote).foregroundStyle(Theme.inkSoft)
                } else if loaded, runs.isEmpty {
                    Text(LooperCopy.noRuns).font(.subheadline).foregroundStyle(Theme.inkSoft)
                }
                ForEach(runs) { run in
                    Button { onRun(run) } label: { RunRowView(run: run, now: Date()) }
                        .buttonStyle(.plain)
                        .accessibilityIdentifier("looper.run.\(run.number ?? 0)")
                }
            } header: {
                ShelfHeader(title: "Recent runs", count: nil)
            }
            .listRowBackground(Theme.ground).listRowSeparatorTint(Theme.hairline)
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .background(Theme.ground)
        .overlay { if !loaded { ProgressView() } }
        .safeAreaInset(edge: .bottom) {
            VStack(spacing: Theme.Space.s) {
                if let notice {
                    Text(notice).font(.footnote.weight(.semibold)).foregroundStyle(Theme.ink)
                        .padding(.horizontal, Theme.Space.m).padding(.vertical, Theme.Space.s)
                        .background(.regularMaterial, in: Capsule())
                        .transition(.opacity)
                }
                HStack(spacing: Theme.Space.s) {
                    TextField("Start a run with a message", text: $message, prompt: Text("Start a run with a message").foregroundStyle(Theme.inkFaint), axis: .vertical)
                        .foregroundStyle(Theme.ink)
                        .lineLimit(1...4)
                        .padding(.horizontal, Theme.Space.m).padding(.vertical, 10)
                        .background(Theme.field, in: RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous))
                    Button(action: onSend) {
                        Image(systemName: "arrow.up").font(.body.weight(.semibold)).foregroundStyle(canSend ? Theme.onAccent : Theme.inkFaint)
                            .frame(width: 36, height: 36)
                            .background(canSend ? Theme.accent : Theme.field, in: Circle())
                    }
                    .allowsHitTesting(canSend)
                    .accessibilityValue(canSend ? "" : "Not ready")
                    .accessibilityLabel("Start a run with this message")
                }
                .padding(.horizontal, Theme.Space.m).padding(.vertical, Theme.Space.s)
                .background(Theme.ground)
            }
            .animation(.default, value: notice)
        }
    }

    /// Run now waits for the runs: until they are read, the page cannot
    /// know whether one is going, and its own spinner is the sign.
    private var canRun: Bool { loaded && runBlocked == nil }

    private var canSend: Bool {
        !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && canRun && !busy
    }
}

/// One run in Recent runs.
struct RunRowView: View {
    let run: LooperRunRow
    let now: Date

    var body: some View {
        let face = RunFace(run)
        HStack(alignment: .firstTextBaseline, spacing: Theme.Space.s) {
            VStack(alignment: .leading, spacing: 2) {
                Text(face.title).font(.body).foregroundStyle(Theme.ink)
                Text(face.woke).font(.footnote).foregroundStyle(Theme.inkSoft).lineLimit(1)
            }
            Spacer(minLength: 4)
            VStack(alignment: .trailing, spacing: 2) {
                if let word = face.word {
                    Text(word).font(.footnote.weight(.semibold)).foregroundStyle(face.tone.color)
                }
                if let started = run.startedAt.flatMap(WireDate.parse) {
                    Text(started, format: .relative(presentation: .named)).font(.footnote).foregroundStyle(Theme.inkFaint)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }
}

/// A run, with its model kept for as long as it is on screen.
struct LooperRunScreen: View {
    @StateObject private var model: LooperRunModel

    init(model: @autoclosure @escaping () -> LooperRunModel) {
        _model = StateObject(wrappedValue: model())
    }

    var body: some View {
        LooperRunPage(run: model.run, items: model.items, open: model.open, loaded: model.loaded, line: model.line, failure: model.failure, notice: model.notice, answering: model.answering) { request, words in
            Task { await model.answer(request, with: words) }
        }
        .navigationTitle(RunFace(model.run).title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Button { copy(model.run.id) } label: { Label("Copy run id", systemImage: "number") }
                } label: {
                    Label("Run actions", systemImage: "ellipsis")
                }
            }
        }
        .refreshable { await model.refresh() }
        .task {
            // A run that is going is read again as it goes.
            while !Task.isCancelled {
                await model.refresh()
                guard RunFace.going(model.run.status) else { return }
                try? await Task.sleep(for: .seconds(5))
            }
        }
    }
}

/// The run, from plain values: what was said and done, and any request
/// with its answer form. It has no composer.
struct LooperRunPage: View {
    let run: LooperRunRow
    let items: [RunItem]
    let open: [LooperRequest]
    let loaded: Bool
    /// Why the run did not load.
    var line: String? = nil
    let failure: String?
    let notice: String?
    var answering = false
    var onAnswer: (LooperRequest, String) -> Void = { _, _ in }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Theme.Space.l) {
                let face = RunFace(run)
                HStack(spacing: Theme.Space.s) {
                    Text(face.woke)
                    if let word = face.word {
                        Text(word).fontWeight(.semibold).foregroundStyle(face.tone.color)
                    }
                }
                .font(.footnote).foregroundStyle(Theme.inkSoft)

                ForEach(items) { item in
                    switch item {
                    case .said(_, let text):
                        Text(text).foregroundStyle(Theme.ink)
                            .padding(.horizontal, Theme.Space.m).padding(.vertical, 10)
                            .surface(Theme.Corner.large)
                            .frame(maxWidth: .infinity, alignment: .trailing)
                    case .words(_, let text):
                        MarkdownView(text: text)
                    case .tools(_, let names, let failed):
                        ToolsLine(names: names, failed: failed)
                    case .withheld:
                        Label("Private to its owner", systemImage: "lock")
                            .font(.footnote).foregroundStyle(Theme.inkFaint)
                    }
                }

                if let error = run.error, !error.isEmpty {
                    Label(LooperCopy.runFailed(error), systemImage: "xmark.circle.fill").font(.footnote).foregroundStyle(Theme.bad)
                }
                if let line {
                    Label(line, systemImage: "exclamationmark.icloud").font(.footnote).foregroundStyle(Theme.inkSoft)
                } else if loaded, items.isEmpty, open.isEmpty, run.error == nil {
                    Text(LooperCopy.nothingSaid(going: RunFace.going(run.status)))
                        .font(.subheadline).foregroundStyle(Theme.inkSoft)
                }

                ForEach(open) { request in
                    RequestForm(request: request, answering: answering) { onAnswer(request, $0) }
                }
                if let said = failure ?? notice {
                    Text(said).font(.footnote).foregroundStyle(failure == nil ? Theme.inkSoft : Theme.bad)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, Theme.Space.l).padding(.vertical, Theme.Space.m)
        }
        .background(Theme.ground)
        .overlay { if !loaded { ProgressView() } }
    }
}

/// Consecutive tool calls, folded to one line that opens to their names.
private struct ToolsLine: View {
    let names: [String]
    let failed: Int
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button { withAnimation { open.toggle() } } label: {
                HStack(spacing: Theme.Space.s) {
                    Image(systemName: failed > 0 ? "xmark.circle.fill" : "checkmark.circle")
                        .foregroundStyle(failed > 0 ? Theme.bad : Theme.inkFaint)
                    Text(headline).foregroundStyle(Theme.inkSoft)
                    if names.count > 1 {
                        Image(systemName: "chevron.right").font(.caption2.weight(.semibold)).foregroundStyle(Theme.inkFaint)
                            .rotationEffect(.degrees(open ? 90 : 0))
                    }
                }
                .font(names.count > 1 ? .footnote : .footnote.monospaced())
                .frame(minHeight: 28)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            // Not `disabled`, which dims the words.
            .allowsHitTesting(names.count > 1)
            if open {
                ForEach(Array(names.enumerated()), id: \.offset) { _, name in
                    Text(name).font(.caption.monospaced()).foregroundStyle(Theme.inkSoft).padding(.leading, Theme.Space.xl)
                }
            }
        }
    }

    private var headline: String { LooperCopy.tools(names: names, failed: failed) }
}

/// A request with its answer form. A question is answered here; any other
/// kind says where it is answered.
private struct RequestForm: View {
    let request: LooperRequest
    var answering = false
    let onAnswer: (String) -> Void
    @State private var answer = ""

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Space.m) {
            Label(request.kindLabel ?? "Request", systemImage: "hand.raised")
                .font(.footnote.weight(.semibold)).foregroundStyle(Theme.attention)
            Text(request.rendered ?? request.body ?? "").foregroundStyle(Theme.ink)
            if request.kind == "question", request.canResolve != false {
                TextField("Your answer", text: $answer, prompt: Text("Your answer").foregroundStyle(Theme.inkFaint), axis: .vertical)
                    .foregroundStyle(Theme.ink)
                    .lineLimit(1...5)
                    .padding(.horizontal, Theme.Space.m).padding(.vertical, 10)
                    .background(Theme.field, in: RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
                Button { onAnswer(answer) } label: {
                    Text("Answer").fontWeight(.semibold).foregroundStyle(blank ? Theme.inkFaint : Theme.onAccent)
                        .frame(maxWidth: .infinity, minHeight: Theme.touch)
                        .background(blank ? Theme.field : Theme.accent, in: RoundedRectangle(cornerRadius: Theme.Corner.medium, style: .continuous))
                }
                .buttonStyle(.plain)
                .allowsHitTesting(!blank && !answering)
                .accessibilityValue(blank ? "Not ready" : answering ? "Sending" : "")
            } else {
                Text(LooperCopy.answerOnWeb).font(.footnote).foregroundStyle(Theme.inkSoft)
            }
        }
        .padding(Theme.Space.m)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: Theme.Corner.large, style: .continuous).strokeBorder(Theme.attention.opacity(0.5)))
    }

    private var blank: Bool { answer.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
}

/// Edit, kept open by a refusal so nothing typed is lost.
private struct LooperEditSheet: View {
    @ObservedObject var page: LooperPageModel
    @Environment(\.dismiss) private var dismiss
    @State private var draft: LooperDraft
    @State private var saving = false

    init(page: LooperPageModel) {
        self.page = page
        _draft = State(initialValue: LooperDraft(page.looper.row))
    }

    var body: some View {
        NavigationStack {
            LooperEditForm(draft: $draft, nextRun: page.looper.nextRun(now: Date()), failure: page.editFailure)
                .navigationTitle("Edit")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        SaveButton(ready: !saving && !draft.changes(from: page.looper.row).isEmpty) {
                            saving = true
                            Task {
                                if await page.save(draft) { dismiss() }
                                saving = false
                            }
                        }
                    }
                }
        }
        .onAppear { page.editing() }
    }
}

/// The form's Save. With nothing changed it takes no tap and wears faint
/// ink: not `disabled`, which dims the word below 4.5:1.
struct SaveButton: View {
    let ready: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Text("Save").fontWeight(.semibold).foregroundStyle(ready ? Theme.accent : Theme.inkFaint)
        }
        .allowsHitTesting(ready)
        .accessibilityValue(ready ? "" : "Not ready")
    }
}

/// The form, in Desk's groups and labels, drawn with the theme's tokens.
struct LooperEditForm: View {
    @Binding var draft: LooperDraft
    /// When it next runs; nil where no run is scheduled.
    let nextRun: String?
    let failure: String?

    var body: some View {
        Form {
            if let failure {
                Section { Label(failure, systemImage: "exclamationmark.triangle").foregroundStyle(Theme.bad).font(.footnote) }
                    .listRowBackground(Theme.raised)
            }
            Section {
                row("Name") { field("Name", $draft.name) }
                TextField("Instructions", text: $draft.instructions, prompt: faint("Instructions"), axis: .vertical)
                    .foregroundStyle(Theme.ink).lineLimit(4...12)
            } header: {
                heading("Instructions")
            }
            .listRowBackground(Theme.raised)
            Section {
                row("Runs") {
                    field("No schedule", $draft.cron).font(.body.monospaced())
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                }
                row("Timezone") {
                    field("UTC", $draft.timezone).textInputAutocapitalization(.never).autocorrectionDisabled()
                }
                Toggle(isOn: $draft.scheduleOn) { Text("Schedule").foregroundStyle(Theme.ink) }.tint(Theme.accent)
                if let nextRun {
                    row("Next run") { Text(nextRun).foregroundStyle(Theme.inkSoft) }
                }
            } header: {
                heading("Schedule")
            } footer: {
                note(draft.cronLine)
            }
            .listRowBackground(Theme.raised)
            Section {
                row("Notify") {
                    field("Nobody", $draft.notify).textInputAutocapitalization(.never).autocorrectionDisabled()
                }
                Picker(selection: $draft.isPublic) {
                    Text("Private").tag(false)
                    Text("Public").tag(true)
                } label: {
                    Text("Visibility").foregroundStyle(Theme.ink)
                }
                .tint(Theme.inkSoft)
            } header: {
                heading("Details")
            } footer: {
                note(draft.visibilityLine)
            }
            .listRowBackground(Theme.raised)
            Section {
                Text("Model, grants, triggers and files").foregroundStyle(Theme.inkSoft)
            } header: {
                heading("Set on the web")
            } footer: {
                note("Open this looper on the web to change them.")
            }
            .listRowBackground(Theme.raised)
        }
        .scrollContentBackground(.hidden)
        .background(Theme.ground)
    }

    private func faint(_ words: String) -> Text { Text(words).foregroundStyle(Theme.inkFaint) }

    private func heading(_ words: String) -> some View {
        Text(words).font(.footnote.weight(.semibold)).foregroundStyle(Theme.inkSoft).textCase(nil)
    }

    private func note(_ words: String) -> some View {
        Text(words).font(.footnote).foregroundStyle(Theme.inkSoft)
    }

    private func row<Value: View>(_ label: String, @ViewBuilder _ value: () -> Value) -> some View {
        LabeledContent { value() } label: { Text(label).foregroundStyle(Theme.ink) }
    }

    /// A trailing field whose prompt states the fact an empty value means.
    private func field(_ prompt: String, _ text: Binding<String>) -> some View {
        TextField(prompt, text: text, prompt: faint(prompt)).foregroundStyle(Theme.ink).multilineTextAlignment(.trailing)
    }
}
