import DeskPhoneKit
import SwiftUI

/// Start a thread: the words, and under them where it runs, each choice
/// already made so the common case is typing and Send.
struct NewThreadView: View {
    @StateObject var model: NewThreadModel
    let onStarted: (StartedThread) -> Void
    @Environment(\.dismiss) private var dismiss
    @FocusState private var typing: Bool

    var body: some View {
        NavigationStack {
            NewThreadForm(
                text: $model.text, newBranch: $model.newBranch, model: $model.model, effort: $model.effort,
                macs: model.macs, mac: model.mac, projects: model.projects, project: model.project,
                harnesses: model.harnesses, harness: model.harness, models: model.models, efforts: model.efforts,
                loading: model.loading, line: model.line, typing: $typing,
                onMac: { mac in Task { await model.pick(mac: mac) } },
                onProject: { project in Task { await model.pick(project: project) } },
                onHarness: { harness in Task { await model.pick(harness: harness) } }
            )
            .navigationTitle("New thread")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }.foregroundStyle(Theme.inkSoft)
                }
                ToolbarItem(placement: .confirmationAction) {
                    if model.sending {
                        ProgressView()
                    } else {
                        StartButton(ready: model.canSend) {
                            Task {
                                guard let started = await model.send() else { return }
                                onStarted(started)
                                dismiss()
                            }
                        }
                    }
                }
            }
        }
        .task {
            typing = true
            await model.appear()
        }
        .onDisappear { model.close() }
        .interactiveDismissDisabled(model.sending)
    }
}

/// The sheet's page, from plain values.
struct NewThreadForm: View {
    @Binding var text: String
    @Binding var newBranch: Bool
    @Binding var model: String?
    @Binding var effort: String?
    let macs: [MacChoice]
    let mac: MacChoice?
    let projects: [DoorProject]
    let project: DoorProject?
    let harnesses: [String]
    let harness: String
    let models: [AgentOption.Choice]
    let efforts: [AgentOption.Choice]
    let loading: Bool
    let line: String?
    var typing: FocusState<Bool>.Binding
    let onMac: (MacChoice) -> Void
    let onProject: (DoorProject) -> Void
    let onHarness: (String) -> Void

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: Theme.Space.l) {
                TextField("", text: $text, prompt: Text("What should the agent do?").foregroundStyle(Theme.inkFaint), axis: .vertical)
                    .lineLimit(4...12)
                    .focused(typing)
                    .padding(Theme.Space.m)
                    .surface(Theme.Corner.large, fill: Theme.field)
                if let line {
                    Label { Text(line).foregroundStyle(Theme.ink) } icon: {
                        Image(systemName: "exclamationmark.circle").foregroundStyle(Theme.attention)
                    }
                    .font(.subheadline)
                }
                VStack(spacing: 0) {
                    if macs.count > 1, let mac {
                        ChoiceRow(glyph: "laptopcomputer", label: "Mac", value: mac.label) {
                            ForEach(macs) { choice in
                                Button { onMac(choice) } label: { picked(choice.label, choice == mac) }
                            }
                        }
                        rule
                    }
                    ChoiceRow(glyph: "folder", label: "Project", value: project?.name ?? (loading ? "Reading…" : "None"), busy: loading) {
                        ForEach(projects) { choice in
                            Button { onProject(choice) } label: { picked(choice.name, choice == project) }
                        }
                    }
                    .disabled(projects.isEmpty)
                    rule
                    ChoiceRow(glyph: "arrow.triangle.branch", label: "Starts in", value: newBranch ? "A new branch" : "The checkout") {
                        Button { newBranch = true } label: { picked("A new branch", newBranch) }
                        Button { newBranch = false } label: { picked("The checkout", !newBranch) }
                    }
                    .disabled(project == nil)
                    rule
                    ChoiceRow(glyph: "sparkles", label: "Agent", value: agentLine) {
                        Section("Agent") {
                            ForEach(harnesses, id: \.self) { choice in
                                Button { onHarness(choice) } label: { picked(NewThreadModel.agentName(choice), choice == harness) }
                            }
                        }
                        if !models.isEmpty {
                            Menu("Model") {
                                Button { model = nil } label: { picked("Automatic", model == nil) }
                                ForEach(models) { choice in
                                    Button { model = choice.value } label: { picked(choice.name, model == choice.value) }
                                }
                            }
                        }
                        if !efforts.isEmpty {
                            Menu("Effort") {
                                Button { effort = nil } label: { picked("Automatic", effort == nil) }
                                ForEach(efforts) { choice in
                                    Button { effort = choice.value } label: { picked(choice.name, effort == choice.value) }
                                }
                            }
                        }
                    }
                    .disabled(project == nil)
                }
                .surface(Theme.Corner.large)
                Text(newBranch
                    ? "Your Mac makes a worktree on a new branch and names both from your words."
                    : "The agent works in the project's own checkout, on whatever branch it is on.")
                    .font(.footnote).foregroundStyle(Theme.inkFaint)
            }
            .padding(Theme.Space.l)
        }
        .scrollDismissesKeyboard(.interactively)
        .background(Theme.ground)
    }

    private var rule: some View { Theme.hairline.frame(height: 1).padding(.leading, 44) }

    private var agentLine: String {
        var words = [NewThreadModel.agentName(harness)]
        if let model, let name = models.first(where: { $0.value == model })?.name { words.append(name) }
        if let effort, let name = efforts.first(where: { $0.value == effort })?.name { words.append(name) }
        return words.joined(separator: ", ")
    }

    @ViewBuilder private func picked(_ name: String, _ on: Bool) -> some View {
        if on { Label(name, systemImage: "checkmark") } else { Text(name) }
    }
}

/// One choice: what it is, what stands, and a menu of the others.
private struct ChoiceRow<Choices: View>: View {
    let glyph: String
    let label: String
    let value: String
    var busy = false
    @ViewBuilder let choices: () -> Choices

    var body: some View {
        Menu(content: choices) {
            HStack(spacing: Theme.Space.m) {
                Image(systemName: glyph).font(.subheadline).foregroundStyle(Theme.inkSoft).frame(width: 20)
                Text(label).foregroundStyle(Theme.ink)
                Spacer(minLength: Theme.Space.m)
                if busy { ProgressView().controlSize(.small) }
                Text(value).foregroundStyle(Theme.inkSoft).lineLimit(1).truncationMode(.middle)
                Image(systemName: "chevron.up.chevron.down").font(.caption2.weight(.semibold)).foregroundStyle(Theme.inkFaint)
            }
            .padding(.horizontal, Theme.Space.m)
            .frame(minHeight: 48)
            .contentShape(Rectangle())
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(label): \(value)")
    }
}

/// The form's Start. Before there is anything to send it takes no tap and
/// wears faint ink: not `disabled`, which dims the word below 4.5:1.
struct StartButton: View {
    let ready: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Text("Start").fontWeight(.semibold).foregroundStyle(ready ? Theme.accent : Theme.inkFaint)
        }
        .allowsHitTesting(ready)
        .accessibilityValue(ready ? "" : "Not ready")
    }
}
