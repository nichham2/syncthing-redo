// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Owns the embedded sync engine (Mobile.xcframework, built from ./lib/mobile
// by mobile/build-bridge.sh) and connects it to the photo
// library. Type and method names follow gomobile's generated Objective-C
// headers (package "mobile" -> prefix "Mobile").

import Foundation
import Mobile

final class SyncthingService: NSObject, MobilePlatformProtocol {
    static let shared = SyncthingService()

    static let photoFolderID = "photos"

    let homeDir: URL
    /// The folder Syncthing syncs. Photos are exported here from Apple
    /// Photos and imported into Apple Photos from here. Excluded from
    /// iCloud backup, since the photo library itself is already backed up.
    let photoDir: URL

    private(set) var node: MobileNode?
    private(set) lazy var library = PhotoLibraryBridge(stagingDir: photoDir, service: self)

    enum State: Equatable {
        case starting
        case running(URL)
        case needsPhotoAccess
        case failed(String)
    }

    /// What the dashboard screen shows.
    @Published private(set) var state: State = .starting

    /// Latest status snapshot, for the native UI and widgets.
    @Published private(set) var lastStatusJSON: String = "{}"

    /// Whether the visible app currently holds the node (see Node.Hold).
    private var foregroundHeld = false

    private override init() {
        let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        homeDir = support.appendingPathComponent("Syncthing", isDirectory: true)
        photoDir = support.appendingPathComponent("Keepsake", isDirectory: true)
        super.init()
        try? FileManager.default.createDirectory(at: photoDir, withIntermediateDirectories: true)
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var dir = photoDir
        try? dir.setResourceValues(values)
    }

    // MARK: Lifecycle

    private func makeNode() throws -> MobileNode {
        if let node { return node }
        var err: NSError?
        guard let n = MobileNewNode(homeDir.path, &err) else {
            throw err ?? NSError(domain: "Syncthing", code: 1)
        }
        n.setPlatform(self)
        node = n
        return n
    }

    /// Takes one hold on the engine (starting it if needed) and makes sure
    /// the photo folder exists. Pair every call with `unhold()`.
    func hold() throws {
        let n = try makeNode()
        try n.hold()
        do {
            try n.ensurePhotoFolder(Self.photoFolderID, label: "Camera Roll", path: photoDir.path)
        } catch {
            n.unhold()
            throw error
        }
    }

    func unhold() {
        node?.unhold()
    }

    /// The app became visible: keep the engine running while it is.
    @MainActor
    func enterForeground() async {
        guard await PhotoLibraryBridge.requestAccess() else {
            state = .needsPhotoAccess
            return
        }
        if !foregroundHeld {
            do {
                try hold()
                foregroundHeld = true
            } catch {
                state = .failed(error.localizedDescription)
                return
            }
        }
        if let url = dashboardURL { state = .running(url) }
        await library.reconcile()
    }

    /// The app left the screen: give up the foreground hold once the
    /// short grace period is over (BackgroundSync.finishInBackground).
    @MainActor
    func leaveForeground() {
        guard foregroundHeld else { return }
        foregroundHeld = false
        unhold()
    }

    /// Stops the engine whoever holds it, so the database is closed
    /// cleanly before the OS suspends the app.
    func stop() {
        node?.stop()
        DispatchQueue.main.async {
            self.foregroundHeld = false
            if case .running = self.state { self.state = .starting }
        }
    }

    var dashboardURL: URL? {
        guard let s = node?.dashboardURL(), !s.isEmpty else { return nil }
        return URL(string: s)
    }

    var deviceID: String { node?.deviceID() ?? "" }

    func addPeer(deviceID: String, name: String) throws {
        try node?.addPeer(deviceID, name: name)
    }

    /// Runs one background window: export new photos, apply deletions
    /// made in Apple Photos, then let Syncthing sync until settled or the
    /// budget runs out. Holds the engine for the whole window, so it
    /// neither stops an engine the visible app uses nor leaves one running
    /// that nobody needs.
    func runBackgroundSync(maxSeconds: Int64) async -> String {
        do { try hold() } catch { return "{}" }
        defer { unhold() }
        await library.reconcile()
        return await withCheckedContinuation { cont in
            DispatchQueue.global(qos: .utility).async {
                var err: NSError?
                let json = self.node?.runBackgroundSync(maxSeconds, error: &err) ?? "{}"
                cont.resume(returning: json)
            }
        }
    }

    func rescan(_ relPath: String) {
        try? node?.rescan(Self.photoFolderID, relPath: relPath)
    }

    // MARK: MobilePlatformProtocol (called on a Go goroutine)

    func onRemoteChange(_ folderID: String?, relPath: String?, action: String?) {
        guard folderID == Self.photoFolderID, let relPath, let action else { return }
        Task { await library.handleRemoteChange(relPath: relPath, action: action) }
    }

    func onStateChanged(_ statusJSON: String?) {
        guard let statusJSON else { return }
        DispatchQueue.main.async { self.lastStatusJSON = statusJSON }
    }
}

extension SyncthingService: ObservableObject {}
