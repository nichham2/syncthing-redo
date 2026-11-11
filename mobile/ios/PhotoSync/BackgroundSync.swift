// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// iOS gives apps no persistent background execution. Syncing happens:
//   * while the app is in the foreground,
//   * for ~30 s after it is backgrounded (beginBackgroundTask),
//   * in BGAppRefreshTask windows (~30 s, opportunistic), and
//   * in BGProcessingTask windows (minutes, typically overnight while
//     charging), which is where most catch-up happens.
//
// Both identifiers must be listed under BGTaskSchedulerPermittedIdentifiers
// in Info.plist, with UIBackgroundModes "fetch" and "processing".

import BackgroundTasks
import UIKit

enum BackgroundSync {
    static let refreshID = "net.syncthing.photosync.refresh"
    static let processingID = "net.syncthing.photosync.processing"

    /// Call from application(_:didFinishLaunchingWithOptions:).
    static func register() {
        BGTaskScheduler.shared.register(forTaskWithIdentifier: refreshID, using: nil) { task in
            run(task, budgetSeconds: 25)
        }
        BGTaskScheduler.shared.register(forTaskWithIdentifier: processingID, using: nil) { task in
            run(task, budgetSeconds: 8 * 60)
        }
    }

    /// Call when the app enters the background.
    static func schedule() {
        let refresh = BGAppRefreshTaskRequest(identifier: refreshID)
        refresh.earliestBeginDate = Date(timeIntervalSinceNow: 15 * 60)
        try? BGTaskScheduler.shared.submit(refresh)

        let processing = BGProcessingTaskRequest(identifier: processingID)
        processing.requiresNetworkConnectivity = true
        processing.requiresExternalPower = UserDefaults.standard.bool(forKey: "syncOnlyWhenCharging")
        processing.earliestBeginDate = Date(timeIntervalSinceNow: 60 * 60)
        try? BGTaskScheduler.shared.submit(processing)
    }

    /// Keeps syncing for the short grace period after backgrounding.
    static func finishInBackground() {
        var id: UIBackgroundTaskIdentifier = .invalid
        id = UIApplication.shared.beginBackgroundTask(withName: "syncthing-finish") {
            SyncthingService.shared.stop()
            UIApplication.shared.endBackgroundTask(id)
        }
        Task {
            _ = await SyncthingService.shared.runBackgroundSync(maxSeconds: 20)
            UIApplication.shared.endBackgroundTask(id)
        }
    }

    private static func run(_ task: BGTask, budgetSeconds: Int64) {
        schedule() // always line up the next window
        let work = Task {
            do { try SyncthingService.shared.start() } catch {
                task.setTaskCompleted(success: false)
                return
            }
            _ = await SyncthingService.shared.runBackgroundSync(maxSeconds: budgetSeconds)
            task.setTaskCompleted(success: true)
        }
        task.expirationHandler = {
            work.cancel()
            SyncthingService.shared.stop() // close the database before suspension
            task.setTaskCompleted(success: false)
        }
    }
}
