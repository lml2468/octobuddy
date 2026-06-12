import SwiftUI
import AppKit

/// Bridges a SwiftUI view to its host `NSWindow` to apply chrome SwiftUI doesn't
/// expose: a transparent, full-size-content title bar so content extends to the
/// top edge (the Things/Linear/Arc look). Drag-anywhere on the background.
///
/// Used as a `.background(WindowAccessor())` on a window's root view. Re-applies
/// in `updateNSView` because the window may not exist on first layout.
struct WindowAccessor: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView {
        let v = NSView(frame: .zero)
        DispatchQueue.main.async { [weak v] in configure(v?.window) }
        return v
    }

    func updateNSView(_ nsView: NSView, context: Context) {
        DispatchQueue.main.async { [weak nsView] in configure(nsView?.window) }
    }

    private func configure(_ window: NSWindow?) {
        guard let window else { return }
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .hidden
        window.styleMask.insert(.fullSizeContentView)
        window.isMovableByWindowBackground = true
        // Keep the standard traffic-light controls visible over the content.
        window.standardWindowButton(.closeButton)?.superview?.superview?.wantsLayer = true
    }
}
