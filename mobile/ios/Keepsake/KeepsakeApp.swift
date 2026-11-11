// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

import SwiftUI
import UIKit

@main
struct KeepsakeApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) var appDelegate
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            DashboardView()
        }
        .onChange(of: scenePhase) { phase in
            switch phase {
            case .active:
                try? SyncthingService.shared.start()
                Task { await SyncthingService.shared.library.reconcile() }
            case .background:
                BackgroundSync.schedule()
                BackgroundSync.finishInBackground()
            default:
                break
            }
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        // Background task handlers must be registered before launch finishes.
        BackgroundSync.register()
        return true
    }
}
