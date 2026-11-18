import DeskPhoneKit
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers

/// The + left of the field: Photo Library, Take Photo (where there is a
/// camera), Paste Image (where the pasteboard holds one). A Mac whose Desk
/// takes no photos draws it dimmed, and a tap says why.
struct AttachButton: View {
    let acting: Acting

    @State private var picking = false
    @State private var picked: [PhotosPickerItem] = []
    @State private var camera = false
    @State private var oldMac = false

    static let updateDesk = "Update Desk on your Mac to send photos."

    var body: some View {
        Group {
            if acting.canAttach {
                Menu {
                    Button { picking = true } label: { Label("Photo Library", systemImage: "photo.on.rectangle") }
                    if UIImagePickerController.isSourceTypeAvailable(.camera) {
                        Button { camera = true } label: { Label("Take Photo", systemImage: "camera") }
                    }
                    if UIPasteboard.general.hasImages {
                        Button(action: paste) { Label("Paste Image", systemImage: "doc.on.clipboard") }
                    }
                } label: {
                    PlusLabel()
                }
                .menuStyle(.button)
                .buttonStyle(.bordered)
                .buttonBorderShape(.circle)
                .disabled(acting.tray.room == 0)
            } else {
                Button { oldMac = true } label: { PlusLabel() }
                    .buttonStyle(.bordered)
                    .buttonBorderShape(.circle)
                    .foregroundStyle(.tertiary)
                    .accessibilityHint(Self.updateDesk)
                    .alert(Self.updateDesk, isPresented: $oldMac) { Button("OK", role: .cancel) {} }
            }
        }
        .accessibilityLabel("Attach")
        .photosPicker(
            isPresented: $picking, selection: $picked, maxSelectionCount: max(acting.tray.room, 1),
            selectionBehavior: .ordered, matching: .images, preferredItemEncoding: .compatible
        )
        .onChange(of: picked) { _, items in
            guard !items.isEmpty else { return }
            picked = []
            Task {
                var photos: [Data] = []
                for item in items {
                    if let data = try? await item.loadTransferable(type: Data.self) { photos.append(data) }
                }
                if !photos.isEmpty { acting.onAttach(photos, []) }
            }
        }
        .fullScreenCover(isPresented: $camera) {
            CameraPicker { image in
                if let data = image.jpegData(compressionQuality: 0.95) { acting.onAttach([data], [nil]) }
            }
            .ignoresSafeArea()
        }
    }

    private func paste() {
        // PNG keeps a screenshot's text crisp; ImagePrep makes an opaque one JPEG.
        let photos = (UIPasteboard.general.images ?? []).compactMap { $0.pngData() }
        if !photos.isEmpty { acting.onAttach(photos, []) }
    }
}

private struct PlusLabel: View {
    var body: some View {
        Image(systemName: "plus")
            .font(.body.weight(.semibold))
            .frame(width: 22, height: 22)
    }
}

/// The attach menu as it draws open, for the Showcase's picture of it.
struct AttachMenuPicture: View {
    var body: some View {
        VStack(spacing: 0) {
            row("Photo Library", "photo.on.rectangle")
            Divider()
            row("Take Photo", "camera")
            Divider()
            row("Paste Image", "doc.on.clipboard")
        }
        .frame(width: 240)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 14))
        .shadow(color: .black.opacity(0.15), radius: 12, y: 4)
    }

    private func row(_ title: String, _ symbol: String) -> some View {
        HStack {
            Text(title)
            Spacer()
            Image(systemName: symbol)
        }
        .padding(.horizontal, 16).padding(.vertical, 12)
    }
}

/// The photos the next message carries, above the field.
struct TrayView: View {
    let tray: AttachmentTray
    var failureShown: UUID?
    let onRemove: (UUID) -> Void
    let onRetry: (UUID) -> Void

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(tray.items) { item in
                    TrayThumb(item: item, label: tray.accessibilityLabel(item.id), failureShown: failureShown == item.id,
                              onRemove: { onRemove(item.id) }, onRetry: { onRetry(item.id) })
                }
            }
            .padding(.top, 8).padding(.trailing, 8)
        }
    }
}

/// One photo in the tray: dimmed with a spinner while it goes, a red mark
/// when it failed (a tap offers Try Again and Remove), and an ✕ to remove it.
private struct TrayThumb: View {
    let item: TrayItem
    let label: String
    let onRemove: () -> Void
    let onRetry: () -> Void

    @State private var failure: Bool
    @State private var viewing = false

    init(item: TrayItem, label: String, failureShown: Bool, onRemove: @escaping () -> Void, onRetry: @escaping () -> Void) {
        self.item = item
        self.label = label
        self.onRemove = onRemove
        self.onRetry = onRetry
        _failure = State(initialValue: failureShown)
    }

    private var failed: TrayError? {
        if case .failed(let error) = item.state { return error }
        return nil
    }

    var body: some View {
        thumbnail
            .frame(width: 56, height: 56)
            .clipShape(RoundedRectangle(cornerRadius: 10))
            .overlay {
                if item.busy {
                    ProgressView()
                } else if failed != nil {
                    Image(systemName: "exclamationmark.circle.fill")
                        .font(.title3)
                        .symbolRenderingMode(.palette)
                        .foregroundStyle(.white, .red)
                }
            }
            .contentShape(Rectangle())
            .onTapGesture {
                if failed != nil { failure = true } else if case .ready = item.state { viewing = true }
            }
            .overlay(alignment: .topTrailing) {
                Button(action: onRemove) {
                    Image(systemName: "xmark.circle.fill")
                        .font(.system(size: 20))
                        .symbolRenderingMode(.palette)
                        .foregroundStyle(.white, .black.opacity(0.6))
                        .frame(width: 44, height: 44)
                        .contentShape(Rectangle())
                }
                .offset(x: 14, y: -14)
                .accessibilityLabel("Remove photo")
            }
            .padding(.top, 6).padding(.trailing, 6)
            .accessibilityElement(children: .contain)
            .accessibilityLabel(label)
            .confirmationDialog(failed?.sentence ?? "", isPresented: $failure, titleVisibility: .visible) {
                if item.image != nil { Button("Try Again", action: onRetry) }
                Button("Remove", role: .destructive, action: onRemove)
            }
            .fullScreenCover(isPresented: $viewing) {
                if let data = item.image?.data {
                    ImageViewer(pages: [ViewerPage(request: .inline(data), label: "Photo", caption: nil)], start: .inline(data))
                        .environment(\.images, ImageContext(store: ImageStore { _ in data }))
                }
            }
    }

    @ViewBuilder private var thumbnail: some View {
        if let data = item.thumbnail, let image = UIImage(data: data) {
            Image(uiImage: image)
                .resizable()
                .aspectRatio(contentMode: .fill)
                .opacity(item.busy || failed != nil ? 0.5 : 1)
        } else {
            Color(.secondarySystemFill)
        }
    }
}

/// The system camera, for one photo.
struct CameraPicker: UIViewControllerRepresentable {
    let onPhoto: (UIImage) -> Void
    @Environment(\.dismiss) private var dismiss

    func makeUIViewController(context: Context) -> UIImagePickerController {
        let picker = UIImagePickerController()
        picker.sourceType = .camera
        picker.delegate = context.coordinator
        return picker
    }

    func updateUIViewController(_ picker: UIImagePickerController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    final class Coordinator: NSObject, UIImagePickerControllerDelegate, UINavigationControllerDelegate {
        let parent: CameraPicker

        init(_ parent: CameraPicker) { self.parent = parent }

        func imagePickerController(_ picker: UIImagePickerController, didFinishPickingMediaWithInfo info: [UIImagePickerController.InfoKey: Any]) {
            if let image = info[.originalImage] as? UIImage { parent.onPhoto(image) }
            parent.dismiss()
        }

        func imagePickerControllerDidCancel(_ picker: UIImagePickerController) {
            parent.dismiss()
        }
    }
}
