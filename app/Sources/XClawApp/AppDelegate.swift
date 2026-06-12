import SwiftUI
import AppKit

/// Hybrid Dock presence: the app rests as a menu-bar agent (LSUIElement →
/// `.accessory`, no Dock icon) and promotes to a regular Dock app (`.regular` —
/// shows the octopus + ⌘-Tab) whenever a real window is open, demoting back when
/// the last one closes. The MenuBarExtra popover is excluded (it's an `NSPanel`
/// that can't become main, so it never flips the Dock icon).
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        let nc = NotificationCenter.default
        for name: NSNotification.Name in [
            NSWindow.didBecomeMainNotification,
            NSWindow.didBecomeKeyNotification,
            NSWindow.willCloseNotification,
        ] {
            nc.addObserver(self, selector: #selector(windowsChanged), name: name, object: nil)
        }
        syncActivationPolicy()
    }

    @objc private func windowsChanged() {
        // Defer one runloop tick so a closing window is already out of NSApp.windows.
        Task { @MainActor in self.syncActivationPolicy() }
    }

    /// A "real" app window (not the menu-bar popover, which is a non-main panel).
    private func hasMainWindow() -> Bool {
        NSApp.windows.contains { $0.isVisible && $0.canBecomeMain && !($0 is NSPanel) }
    }

    private func syncActivationPolicy() {
        let want: NSApplication.ActivationPolicy = hasMainWindow() ? .regular : .accessory
        guard NSApp.activationPolicy() != want else { return }
        NSApp.setActivationPolicy(want)
        if want == .regular { NSApp.activate(ignoringOtherApps: true) }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows: Bool) -> Bool {
        if !hasVisibleWindows {
            NSApp.windows.first { $0.canBecomeMain && !($0 is NSPanel) }?.makeKeyAndOrderFront(nil)
        }
        return true
    }
}
