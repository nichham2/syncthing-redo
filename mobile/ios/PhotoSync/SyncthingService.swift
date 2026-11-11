// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Owns the embedded Syncthing node (Syncthing.xcframework, built with
// `gomobile bind -target=ios ./lib/mobile`) and connects it to the photo
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

    /// Latest status snapshot, for the native UI and widgets.
    @Published private(set) var lastStatusJSON: String = "{}"

    private override init() {
        let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        homeDir = support.appendingPathComponent("Syncthing", isDirectory: true)
        photoDir = support.appendingPathComponent("PhotoSync", isDirectory: true)
        super.init()
        try? FileManager.default.createDirectory(at: photoDir, withIntermediateDirectories: true)
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        var dir = photoDir
        try? dir.setResourceValues(values)
    }

    // MARK: Lifecycle

    func start() throws {
        if node == nil {
            var err: NSError?
            guard let n = MobileNewNode(homeDir.path, &err) else {
                throw err ?? NSError(domain: "Syncthing", code: 1)
            }
            n.setPlatform(self)
            node = n
        }
        guard let node else { return }
        try node.start()
        try node.ensurePhotoFolder(Self.photoFolderID, label: "Camera Roll", path: photoDir.path)
    }

    /// Called when the app moves to the background without a background
    /// task, so the database is closed cleanly before suspension.
    func stop() {
        node?.stop()
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
    /// budget runs out.
    func runBackgroundSync(maxSeconds: Int64) async -> String {
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
