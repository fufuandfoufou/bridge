import AppKit
import SwiftUI

final class AppDelegate: NSObject, NSApplicationDelegate {
    // The menu bar item stays available after the window is closed.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
}

@main
struct BridgeApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var model = AppModel()

    var body: some Scene {
        Window("Bridge", id: "main") {
            RootView()
                .environmentObject(model)
                .frame(minWidth: 860, minHeight: 540)
        }
        .windowToolbarStyle(.unified)

        MenuBarExtra {
            MenuPanel().environmentObject(model)
        } label: {
            MenuBarLabel()
        }
        .menuBarExtraStyle(.window)
    }
}

/// The label exists from launch, even when no window has been restored, so it
/// is where the main window gets opened.
struct MenuBarLabel: View {
    @Environment(\.openWindow) private var openWindow
    @State private var opened = false

    var body: some View {
        Image(nsImage: MenuBarIcon.image)
            .onAppear {
                guard !opened else { return }
                opened = true
                openWindow(id: "main")
                NSApp.activate(ignoringOtherApps: true)
            }
    }
}
