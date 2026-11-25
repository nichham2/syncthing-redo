// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Mirrors Apple Photos into the Syncthing staging folder and back.
//
// Apple Photos is a database, not a directory, so the bridge keeps a
// mapping between PHAsset.localIdentifier and the file name in the
// staging folder:
//
//   * New asset in Photos  -> its current version written byte-for-byte to
//     the staging folder (the original, or the edited render if the photo
//     was edited), file mtime set to the capture date. A Live Photo's
//     video goes next to it under the same base name (IMG_0001.HEIC +
//     IMG_0001.MOV).
//   * Asset edited in Photos -> the new render replaces the staging file
//     under the same name, so other devices get the edit as an update.
//   * Asset deleted in Photos -> staging files removed. The folder has
//     nonDestructiveDeletes, so the other device keeps them.
//   * File arrives from the other device -> imported with
//     PHAssetCreationRequest from the original bytes, creation date taken
//     from the file. A photo and a .MOV with the same base name are
//     imported together as one Live Photo.
//   * A file we already have changes on the other device (an edit there)
//     -> applied to the asset as an edit (PHContentEditingOutput), so the
//     original stays revertible in Photos.
//
// Every reconcile also imports staging files that are in neither
// direction of the mapping and applies changed ones, so a missed
// notification is caught up. The mapping is saved after every change, so
// an interrupted pass never exports the same photo twice. Rescans are
// batched: one request per pass (or per 100 files), not one per photo.

import CryptoKit
import Foundation
import ImageIO
import Photos
import UniformTypeIdentifiers

actor PhotoLibraryBridge {
    private let stagingDir: URL
    private unowned let service: SyncthingService
    private var mapping: AssetMapping
    /// Staging paths being imported right now. `reconcile` can run while an
    /// import waits for Photos (actor re-entrancy at `await`), and must not
    /// export the brand-new asset back as a new file.
    private var importing = Set<String>()
    /// Original file names of the imports above, for matching the new
    /// asset before its identifier is known.
    private var importingNames = [String: Int]()
    /// Assets getting an edit from another device right now; their change
    /// in Photos is ours and must not be exported back.
    private var applyingRemote = Set<String>()
    /// Paths written or removed since the last rescan request.
    private var pendingRescans: [String] = []

    private static let rescanBatch = 100
    /// How long a received photo waits for its Live Photo video.
    private static let pairWait: TimeInterval = 10 * 60
    private static let adjustmentFormat = "app.keepsake.photos.sync"

    init(stagingDir: URL, service: SyncthingService) {
        self.stagingDir = stagingDir
        self.service = service
        self.mapping = AssetMapping.load(from: stagingDir.deletingLastPathComponent()
            .appendingPathComponent("asset-mapping.json"))
    }

    static func requestAccess() async -> Bool {
        let status = await PHPhotoLibrary.requestAuthorization(for: .readWrite)
        // .limited works too, but only the selected photos are synced.
        return status == .authorized || status == .limited
    }

    // MARK: Rescans

    private func queueRescan(_ rel: String) {
        pendingRescans.append(rel)
        if pendingRescans.count >= Self.rescanBatch { flushRescans() }
    }

    /// One scan request for everything queued. Failures are not fatal: the
    /// file system watcher and the hourly full scan pick changes up.
    private func flushRescans() {
        guard !pendingRescans.isEmpty else { return }
        let paths = pendingRescans
        pendingRescans = []
        service.rescan(paths)
    }

    // MARK: Photos -> staging folder

    /// Brings the staging folder in line with the photo library, then
    /// imports anything that arrived from other devices but never made it
    /// into Photos. Cheap when nothing changed; call on launch, on
    /// foreground and at the start of every background window.
    func reconcile() async {
        defer { flushRescans() }
        let status = PHPhotoLibrary.authorizationStatus(for: .readWrite)
        guard status == .authorized || status == .limited else { return }

        let opts = PHFetchOptions()
        // Not .typeCloudShared: shared albums hold other people's photos.
        opts.includeAssetSourceTypes = [.typeUserLibrary, .typeiTunesSynced]
        opts.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: true)]
        let assets = PHAsset.fetchAssets(with: opts)

        var present = Set<String>()
        var toExport: [PHAsset] = []
        var toRefresh: [PHAsset] = []
        assets.enumerateObjects { asset, _, _ in
            guard asset.mediaType == .image || asset.mediaType == .video else { return }
            let id = asset.localIdentifier
            present.insert(id)
            if self.mapping.relPath(for: id) == nil {
                toExport.append(asset)
            } else if self.mapping.stamp(for: id) != Self.stamp(of: asset) {
                // Edited, or something else about it changed (a favorite,
                // an album). refresh() finds out which.
                toRefresh.append(asset)
            }
        }

        for asset in toExport {
            if isBeingImported(asset) { continue }
            await export(asset)
        }
        for asset in toRefresh where !applyingRemote.contains(asset.localIdentifier) {
            await refresh(asset)
        }

        // Assets that are gone from Photos were deleted by the user. Only
        // with full access: under limited access the fetch shows just the
        // selected photos, and everything else is merely hidden from us.
        if status == .authorized {
            var removed = false
            for (id, rel) in mapping.all where !present.contains(id) {
                for path in [rel, mapping.pair(for: id)].compactMap({ $0 }) {
                    try? FileManager.default.removeItem(at: stagingDir.appendingPathComponent(path))
                    queueRescan(path)
                }
                mapping.remove(id)
                removed = true
            }
            if removed { mapping.save() }
        }

        await importMissing()
    }

    private func isBeingImported(_ asset: PHAsset) -> Bool {
        guard !importingNames.isEmpty else { return false }
        let names = PHAssetResource.assetResources(for: asset).map(\.originalFilename)
        return names.contains { importingNames[$0, default: 0] > 0 }
    }

    /// What changes when the asset is edited. Also changes for favorites
    /// and the like, so it is only a hint to look closer.
    private static func stamp(of asset: PHAsset) -> Double? {
        asset.modificationDate?.timeIntervalSinceReferenceDate
    }

    /// The resources that make up what the user sees: the edited render
    /// if there is one, else the original, plus a Live Photo's video.
    private struct Current {
        let original: PHAssetResource   // names the file
        let main: PHAssetResource       // what goes into it
        let pairedVideo: PHAssetResource?
        var isEdited: Bool { main !== original }
    }

    private static func current(_ asset: PHAsset) -> Current? {
        let resources = PHAssetResource.assetResources(for: asset)
        func first(_ type: PHAssetResourceType) -> PHAssetResource? { resources.first { $0.type == type } }
        guard let original = first(.photo) ?? first(.video) ?? resources.first else { return nil }
        let edited = original.type == .video ? first(.fullSizeVideo) : first(.fullSizePhoto)
        let paired = first(.fullSizePairedVideo) ?? first(.pairedVideo)
        return Current(original: original, main: edited ?? original, pairedVideo: paired)
    }

    /// Exports a new asset (and its Live Photo video) and records it.
    private func export(_ asset: PHAsset) async {
        guard let cur = Self.current(asset) else { return }
        let date = asset.creationDate ?? Date()
        let comps = Calendar(identifier: .gregorian).dateComponents([.year, .month], from: date)
        let dir = String(format: "%04d/%02d", comps.year ?? 1970, comps.month ?? 1)
        // An edited photo keeps the original's name, with the render's
        // extension if that differs (an edited HEIC can render as JPEG).
        var name = cur.original.originalFilename
        if cur.isEdited {
            let ext = (cur.main.originalFilename as NSString).pathExtension
            if !ext.isEmpty {
                name = ((name as NSString).deletingPathExtension as NSString).appendingPathExtension(
                    Self.matchCase(ext, like: (name as NSString).pathExtension)) ?? name
            }
        }
        // Edits get the edit time, so the file differs from an earlier
        // unedited copy even when the sizes happen to match.
        let mtime = cur.isEdited ? (asset.modificationDate ?? date) : date
        guard let tmp = await write(cur.main, dir: dir, mtime: mtime) else { return }

        let rel: String
        let plain = "\(dir)/\(name)"
        let plainURL = stagingDir.appendingPathComponent(plain)
        do {
            // A file left by an earlier export whose mapping was never
            // saved: adopt it rather than exporting a "(1)" duplicate.
            if !mapping.hasRelPath(plain),
               FileManager.default.contentsEqual(atPath: plainURL.path, andPath: tmp.path) {
                try? FileManager.default.removeItem(at: tmp)
                rel = plain
            } else {
                rel = uniqueRelPath(dir: dir, name: name)
                try FileManager.default.moveItem(at: tmp, to: stagingDir.appendingPathComponent(rel))
            }
        } catch {
            try? FileManager.default.removeItem(at: tmp)
            return
        }
        // On disk before Syncthing sees the file: a crash after this point
        // can no longer turn into a duplicate export.
        let id = asset.localIdentifier
        mapping.set(id, relPath: rel)
        mapping.setSig(rel, sig(of: rel))
        mapping.setStamp(id, Self.stamp(of: asset))
        if cur.isEdited { mapping.setRenderHash(id, Self.sha256(stagingDir.appendingPathComponent(rel))) }
        mapping.save()
        queueRescan(rel)

        if let video = cur.pairedVideo {
            await exportPairedVideo(video, of: id, primary: rel, mtime: date)
        }
    }

    /// Writes a Live Photo's video next to its photo, as IMG_0001.MOV for
    /// IMG_0001.HEIC, which is how cameras and Photos pair them.
    private func exportPairedVideo(_ video: PHAssetResource, of id: String, primary: String, mtime: Date) async {
        let dir = (primary as NSString).deletingLastPathComponent
        let ext = (video.originalFilename as NSString).pathExtension
        let base = ((primary as NSString).lastPathComponent as NSString).deletingPathExtension
        let rel = "\(dir)/\(base).\(ext.isEmpty ? "MOV" : ext)"
        guard let tmp = await write(video, dir: dir, mtime: mtime) else { return }
        let url = stagingDir.appendingPathComponent(rel)
        if FileManager.default.fileExists(atPath: url.path) {
            let same = FileManager.default.contentsEqual(atPath: url.path, andPath: tmp.path)
            if same && mapping.pair(for: id) == rel {
                try? FileManager.default.removeItem(at: tmp) // unchanged
                return
            }
            if !same && mapping.pair(for: id) != rel {
                // Someone else's file under that name; leave it alone.
                try? FileManager.default.removeItem(at: tmp)
                return
            }
        }
        guard (try? Self.replace(url, with: tmp)) != nil else {
            try? FileManager.default.removeItem(at: tmp)
            return
        }
        mapping.setPair(id, relPath: rel)
        mapping.setSig(rel, sig(of: rel))
        mapping.save()
        queueRescan(rel)
    }

    /// Looks at a mapped asset whose modification date changed, and writes
    /// its current version over the staging file if that changed.
    private func refresh(_ asset: PHAsset) async {
        let id = asset.localIdentifier
        guard let rel = mapping.relPath(for: id), let cur = Self.current(asset) else { return }

        // A mapping written before edits were tracked: an unedited photo
        // without a Live Photo video is what was exported, so just note
        // its date instead of reading every photo again.
        if mapping.stamp(for: id) == nil && !cur.isEdited && cur.pairedVideo == nil {
            mapping.setStamp(id, Self.stamp(of: asset))
            mapping.save()
            return
        }

        // Another device changed the file and that isn't applied yet: its
        // version wins, and importMissing applies it to the asset.
        if let known = mapping.sig(for: rel), known != sig(of: rel) { return }

        let dir = (rel as NSString).deletingLastPathComponent
        let mtime = cur.isEdited ? (asset.modificationDate ?? Date()) : (asset.creationDate ?? Date())
        if var tmp = await write(cur.main, dir: dir, mtime: mtime) {
            let url = stagingDir.appendingPathComponent(rel)
            let hash = Self.sha256(tmp)
            let changed: Bool
            if let known = mapping.renderHash(for: id) {
                changed = known != hash
            } else {
                changed = !FileManager.default.contentsEqual(atPath: url.path, andPath: tmp.path)
            }
            if changed {
                // Keep the name other devices know; convert if the render
                // came out in another format.
                if Self.ext(of: cur.main.originalFilename) != Self.ext(of: rel), asset.mediaType == .image,
                   let type = UTType(filenameExtension: (rel as NSString).pathExtension) {
                    let converted = tmp.deletingLastPathComponent()
                        .appendingPathComponent(".keepsake-\(UUID().uuidString).tmp")
                    if Self.transcode(tmp, to: converted, type: type) {
                        try? FileManager.default.removeItem(at: tmp)
                        tmp = converted
                        try? FileManager.default.setAttributes([.modificationDate: mtime], ofItemAtPath: tmp.path)
                    }
                }
                if (try? Self.replace(url, with: tmp)) != nil {
                    mapping.setSig(rel, sig(of: rel))
                    queueRescan(rel)
                }
            }
            try? FileManager.default.removeItem(at: tmp)
            mapping.setRenderHash(id, hash)
        }

        if let video = cur.pairedVideo {
            await exportPairedVideo(video, of: id, primary: rel, mtime: asset.creationDate ?? Date())
        }
        mapping.setStamp(id, Self.stamp(of: asset))
        mapping.save()
    }

    /// Writes a resource to a temporary file in the staging folder (ignored
    /// by Syncthing, see lib/mobile photoIgnores) with the given mtime.
    private func write(_ resource: PHAssetResource, dir: String, mtime: Date) async -> URL? {
        let dirURL = stagingDir.appendingPathComponent(dir)
        try? FileManager.default.createDirectory(at: dirURL, withIntermediateDirectories: true)
        let tmp = dirURL.appendingPathComponent(".keepsake-\(UUID().uuidString).tmp")
        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // fetch iCloud originals
        do {
            try await PHAssetResourceManager.default().writeData(for: resource, toFile: tmp, options: opts)
            try FileManager.default.setAttributes([.modificationDate: mtime, .creationDate: mtime],
                                                  ofItemAtPath: tmp.path)
            return tmp
        } catch {
            try? FileManager.default.removeItem(at: tmp)
            return nil
        }
    }

    private func uniqueRelPath(dir: String, name: String) -> String {
        var candidate = "\(dir)/\(name)"
        let base = (name as NSString).deletingPathExtension
        let ext = (name as NSString).pathExtension
        var n = 1
        while FileManager.default.fileExists(atPath: stagingDir.appendingPathComponent(candidate).path)
                || mapping.hasRelPath(candidate) {
            candidate = "\(dir)/\(base) (\(n)).\(ext)"
            n += 1
        }
        return candidate
    }

    // MARK: Other device -> Photos

    func handleRemoteChange(relPath: String, action: String) async {
        defer { flushRescans() }
        switch action {
        case "update":
            if let id = mapping.localIdentifier(for: relPath) {
                await applyIfChanged(id: id, relPath: relPath)
            } else if mapping.pairOwner(for: relPath) != nil {
                // A Live Photo's video changed elsewhere. Photos can't take a
                // new video for an existing Live Photo without the user, so
                // keep the still as it is.
                mapping.setSig(relPath, sig(of: relPath))
                mapping.save()
            } else {
                await importIntoPhotos(relPath: relPath)
            }
        case "delete":
            // Only happens when the other device chose "Delete everywhere".
            // iOS asks the user to confirm the deletion.
            if mapping.pairOwner(for: relPath) != nil { return } // goes with its photo
            guard let id = mapping.localIdentifier(for: relPath) else { return }
            let assets = PHAsset.fetchAssets(withLocalIdentifiers: [id], options: nil)
            try? await PHPhotoLibrary.shared().performChanges {
                PHAssetChangeRequest.deleteAssets(assets)
            }
            mapping.remove(id)
            mapping.save()
        default:
            break
        }
    }

    /// Imports staging files that are neither our exports nor already
    /// imported (dropped events, a crash between download and import), and
    /// applies files that changed since we last saw them (an edit on
    /// another device while we weren't told).
    private func importMissing() async {
        let root = stagingDir.standardizedFileURL.path + "/"
        guard let walker = FileManager.default.enumerator(
            at: stagingDir, includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]) else { return }
        var pending: [String] = []
        var changed: [(String, String)] = []
        while let url = walker.nextObject() as? URL {
            guard (try? url.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true,
                  Self.isMedia(url) else { continue }
            let rel = String(url.standardizedFileURL.path.dropFirst(root.count))
            if let id = mapping.localIdentifier(for: rel) {
                if let known = mapping.sig(for: rel) {
                    if known != sig(of: rel) { changed.append((id, rel)) }
                } else {
                    mapping.setSig(rel, sig(of: rel)) // first pass with this version
                }
            } else if mapping.pairOwner(for: rel) == nil && !importing.contains(rel) {
                pending.append(rel)
            }
        }
        mapping.save()
        for (id, rel) in changed where !applyingRemote.contains(id) {
            await applyIfChanged(id: id, relPath: rel)
        }
        for rel in pending where mapping.localIdentifier(for: rel) == nil && mapping.pairOwner(for: rel) == nil {
            await importIntoPhotos(relPath: rel)
        }
    }

    private func importIntoPhotos(relPath: String) async {
        let url = stagingDir.appendingPathComponent(relPath)
        guard Self.isMedia(url), !importing.contains(relPath),
              FileManager.default.fileExists(atPath: url.path) else { return }

        // Live Photos travel as IMG_0001.HEIC + IMG_0001.MOV.
        var still = url
        var video: URL?
        if Self.isMovie(url) {
            if let photo = siblingStill(of: url) {
                let photoRel = rel(of: photo)
                if let owner = mapping.localIdentifier(for: photoRel) {
                    // The photo was imported on its own already. Photos
                    // can't add a video to it, so keep the file with it
                    // rather than adding a separate video to the library.
                    mapping.setPair(owner, relPath: relPath)
                    mapping.setSig(relPath, sig(of: relPath))
                    mapping.save()
                    return
                }
                if importing.contains(photoRel) { return }
                still = photo
                video = url
            }
        } else if let mov = siblingMovie(of: url) {
            if mapping.pairOwner(for: rel(of: mov)) == nil && mapping.localIdentifier(for: rel(of: mov)) == nil {
                video = mov
            }
        } else if isAwaitingPairedVideo(url) {
            return // imported together when the video arrives
        }

        let created = captureDate(of: still)
        let paths = [still, video].compactMap { $0 }
        let rels = paths.map(rel(of:))
        let names = paths.map(\.lastPathComponent)
        rels.forEach { importing.insert($0) }
        names.forEach { importingNames[$0, default: 0] += 1 }
        defer {
            rels.forEach { importing.remove($0) }
            for name in names {
                importingNames[name, default: 1] -= 1
                if importingNames[name] == 0 { importingNames[name] = nil }
            }
        }

        var id = await create(still: still, video: video, created: created)
        if id == nil, let video {
            // Not a valid pair after all (a still and a clip that merely
            // share a name): bring them in as two items.
            id = await create(still: still, video: nil, created: created)
            if let vid = await create(still: video, video: nil, created: created) {
                record(vid, rel: rel(of: video))
            }
        } else if let id, let video {
            mapping.setPair(id, relPath: rel(of: video))
            mapping.setSig(rel(of: video), sig(of: rel(of: video)))
        }
        if let id {
            record(id, rel: rel(of: still))
        }
    }

    private func record(_ id: String, rel: String) {
        mapping.set(id, relPath: rel)
        mapping.setSig(rel, sig(of: rel))
        mapping.save()
    }

    /// Adds one asset from the original bytes and returns its identifier.
    private func create(still: URL, video: URL?, created: Date?) async -> String? {
        let placeholder = IdentifierBox()
        do {
            try await PHPhotoLibrary.shared().performChanges {
                let req = PHAssetCreationRequest.forAsset()
                let opts = PHAssetResourceCreationOptions()
                opts.originalFilename = still.lastPathComponent
                req.addResource(with: Self.isMovie(still) ? .video : .photo, fileURL: still, options: opts)
                if let video {
                    let vopts = PHAssetResourceCreationOptions()
                    vopts.originalFilename = video.lastPathComponent
                    req.addResource(with: .pairedVideo, fileURL: video, options: vopts)
                }
                req.creationDate = created
                placeholder.value = req.placeholderForCreatedAsset?.localIdentifier
            }
        } catch {
            return nil
        }
        return placeholder.value
    }

    /// The photo half of a Live Photo pair can arrive before its video.
    /// Wait for the video while another device has it and this one doesn't
    /// yet, but not forever: a video that never comes (ignored, failed)
    /// must not keep the photo out of the library.
    private func isAwaitingPairedVideo(_ url: URL) -> Bool {
        guard Self.isLivePhotoStill(url) else { return false }
        let received = (try? url.resourceValues(forKeys: [.creationDateKey]).creationDate) ?? .distantPast
        guard Date().timeIntervalSince(received) < Self.pairWait else { return false }
        let base = rel(of: url.deletingPathExtension())
        return ["MOV", "mov"].contains { service.globalFileExists("\(base).\($0)") }
    }

    private func siblingMovie(of still: URL) -> URL? {
        guard Self.isLivePhotoStill(still) else { return nil }
        let base = still.deletingPathExtension()
        return ["MOV", "mov"].map { base.appendingPathExtension($0) }
            .first { FileManager.default.fileExists(atPath: $0.path) }
    }

    private func siblingStill(of movie: URL) -> URL? {
        guard ["mov"].contains(movie.pathExtension.lowercased()) else { return nil }
        let base = movie.deletingPathExtension()
        return Self.livePhotoStillExtensions.flatMap { [$0, $0.uppercased()] }
            .map { base.appendingPathExtension($0) }
            .first { FileManager.default.fileExists(atPath: $0.path) }
    }

    // MARK: Edits from other devices

    private func applyIfChanged(id: String, relPath: String) async {
        let now = sig(of: relPath)
        guard mapping.sig(for: relPath) != now else { return } // our own write, or nothing new
        // Recorded even if Photos refuses the edit, so a file it can't
        // take is not converted again on every pass.
        _ = await applyRemoteEdit(id: id, relPath: relPath)
        mapping.setSig(relPath, now)
        mapping.save()
    }

    /// Applies a changed file to its asset as an edit. The original stays
    /// in Photos, so the user can revert it there.
    private func applyRemoteEdit(id: String, relPath: String) async -> Bool {
        guard let asset = PHAsset.fetchAssets(withLocalIdentifiers: [id], options: nil).firstObject else {
            return false
        }
        applyingRemote.insert(id)
        defer { applyingRemote.remove(id) }

        guard let input = await Self.editingInput(for: asset) else { return false }
        let src = stagingDir.appendingPathComponent(relPath)
        let output = PHContentEditingOutput(contentEditingInput: input)
        output.adjustmentData = PHAdjustmentData(formatIdentifier: Self.adjustmentFormat,
                                                 formatVersion: "1",
                                                 data: Data(relPath.utf8))
        let dst = output.renderedContentURL
        do {
            if asset.mediaType == .video || Self.ext(of: relPath) == "jpg" || Self.ext(of: relPath) == "jpeg" {
                try FileManager.default.copyItem(at: src, to: dst)
            } else if !Self.transcode(src, to: dst, type: .jpeg) {
                // Photos takes edited photos as JPEG.
                return false
            }
        } catch {
            return false
        }
        // What the asset renders as from now on; refresh() compares against
        // it, so this edit is not exported back as a change of our own.
        let hash = Self.sha256(dst)
        do {
            try await PHPhotoLibrary.shared().performChanges {
                PHAssetChangeRequest(for: asset).contentEditingOutput = output
            }
        } catch {
            return false
        }
        let updated = PHAsset.fetchAssets(withLocalIdentifiers: [id], options: nil).firstObject
        mapping.setRenderHash(id, hash)
        mapping.setStamp(id, updated.flatMap(Self.stamp(of:)))
        return true
    }

    private static func editingInput(for asset: PHAsset) async -> PHContentEditingInput? {
        await withCheckedContinuation { cont in
            let opts = PHContentEditingInputRequestOptions()
            opts.isNetworkAccessAllowed = true
            opts.canHandleAdjustmentData = { $0.formatIdentifier == Self.adjustmentFormat }
            _ = asset.requestContentEditingInput(with: opts) { input, _ in
                cont.resume(returning: input)
            }
        }
    }

    // MARK: Files

    private func rel(of url: URL) -> String {
        let root = stagingDir.standardizedFileURL.path + "/"
        return String(url.standardizedFileURL.path.dropFirst(root.count))
    }

    /// Size and modification time: what Syncthing changes when it writes a
    /// new version of the file.
    private func sig(of rel: String) -> String? {
        let attrs = try? FileManager.default.attributesOfItem(atPath: stagingDir.appendingPathComponent(rel).path)
        guard let size = attrs?[.size] as? NSNumber, let mtime = attrs?[.modificationDate] as? Date else { return nil }
        return "\(size.int64Value):\(Int64(mtime.timeIntervalSince1970))"
    }

    private static func replace(_ url: URL, with tmp: URL) throws {
        if FileManager.default.fileExists(atPath: url.path) {
            _ = try FileManager.default.replaceItemAt(url, withItemAt: tmp)
        } else {
            try FileManager.default.moveItem(at: tmp, to: url)
        }
    }

    /// Re-encodes an image into another format, keeping its metadata.
    private static func transcode(_ src: URL, to dst: URL, type: UTType) -> Bool {
        guard let source = CGImageSourceCreateWithURL(src as CFURL, nil),
              let dest = CGImageDestinationCreateWithURL(dst as CFURL, type.identifier as CFString, 1, nil)
        else { return false }
        CGImageDestinationAddImageFromSource(dest, source, 0, nil)
        return CGImageDestinationFinalize(dest)
    }

    private static func sha256(_ url: URL) -> String? {
        guard let handle = try? FileHandle(forReadingFrom: url) else { return nil }
        defer { try? handle.close() }
        var hasher = SHA256()
        while let chunk = try? handle.read(upToCount: 1 << 20), !chunk.isEmpty {
            hasher.update(data: chunk)
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }

    private static func ext(of name: String) -> String { (name as NSString).pathExtension.lowercased() }

    private static func matchCase(_ ext: String, like other: String) -> String {
        other == other.uppercased() ? ext.uppercased() : ext.lowercased()
    }

    /// EXIF DateTimeOriginal if present, else the file's modification time
    /// (which Syncthing preserves across devices).
    private func captureDate(of url: URL) -> Date? {
        if let src = CGImageSourceCreateWithURL(url as CFURL, nil),
           let props = CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [CFString: Any],
           let exif = props[kCGImagePropertyExifDictionary] as? [CFString: Any],
           let s = exif[kCGImagePropertyExifDateTimeOriginal] as? String {
            let f = DateFormatter()
            f.locale = Locale(identifier: "en_US_POSIX")
            f.dateFormat = "yyyy:MM:dd HH:mm:ss"
            if let d = f.date(from: s) { return d }
        }
        return (try? FileManager.default.attributesOfItem(atPath: url.path))?[.modificationDate] as? Date
    }

    private static let movieExtensions: Set<String> = ["mov", "mp4", "m4v", "3gp"]
    private static let imageExtensions: Set<String> = ["jpg", "jpeg", "heic", "heif", "png", "gif", "webp", "dng", "tif", "tiff", "avif"]
    private static let livePhotoStillExtensions = ["heic", "jpg", "jpeg"]

    static func isMovie(_ url: URL) -> Bool { movieExtensions.contains(url.pathExtension.lowercased()) }

    static func isLivePhotoStill(_ url: URL) -> Bool {
        livePhotoStillExtensions.contains(url.pathExtension.lowercased())
    }

    /// Only photos and videos go into the library: never Android trash,
    /// thumbnails, sidecars or temporary files.
    static func isMedia(_ url: URL) -> Bool {
        let name = url.lastPathComponent
        if name.hasPrefix(".") { return false }
        let ext = url.pathExtension.lowercased()
        return imageExtensions.contains(ext) || movieExtensions.contains(ext)
    }
}

/// Carries the new asset's identifier out of the change block.
private final class IdentifierBox: @unchecked Sendable {
    var value: String?
}

/// Persistent mapping between assets and staging files:
/// localIdentifier <-> relative path of the photo or video, plus a Live
/// Photo's video path, the asset's last seen modification date, a hash of
/// its last synced render, and each file's size and mtime as last written
/// or imported. Older files with only `byID` load fine.
struct AssetMapping: Codable {
    private var byID: [String: String] = [:]
    private var pairs: [String: String] = [:]
    private var stamps: [String: Double] = [:]
    private var renders: [String: String] = [:]
    private var sigs: [String: String] = [:]
    private var byPath: [String: String] = [:]
    private var byPairPath: [String: String] = [:]
    private var fileURL: URL?

    enum CodingKeys: String, CodingKey { case byID, pairs, stamps, renders, sigs }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        byID = try c.decodeIfPresent([String: String].self, forKey: .byID) ?? [:]
        pairs = try c.decodeIfPresent([String: String].self, forKey: .pairs) ?? [:]
        stamps = try c.decodeIfPresent([String: Double].self, forKey: .stamps) ?? [:]
        renders = try c.decodeIfPresent([String: String].self, forKey: .renders) ?? [:]
        sigs = try c.decodeIfPresent([String: String].self, forKey: .sigs) ?? [:]
    }

    static func load(from url: URL) -> AssetMapping {
        var m = (try? Data(contentsOf: url)).flatMap { try? JSONDecoder().decode(AssetMapping.self, from: $0) }
            ?? AssetMapping()
        m.fileURL = url
        m.byPath = Dictionary(m.byID.map { ($1, $0) }, uniquingKeysWith: { a, _ in a })
        m.byPairPath = Dictionary(m.pairs.map { ($1, $0) }, uniquingKeysWith: { a, _ in a })
        return m
    }

    func save() {
        guard let fileURL, let data = try? JSONEncoder().encode(self) else { return }
        try? data.write(to: fileURL, options: .atomic)
    }

    var all: [(String, String)] { byID.map { ($0.key, $0.value) } }
    func relPath(for id: String) -> String? { byID[id] }
    func localIdentifier(for rel: String) -> String? { byPath[rel] }
    func hasRelPath(_ rel: String) -> Bool { byPath[rel] != nil || byPairPath[rel] != nil }
    func pair(for id: String) -> String? { pairs[id] }
    func pairOwner(for rel: String) -> String? { byPairPath[rel] }
    func stamp(for id: String) -> Double? { stamps[id] }
    func renderHash(for id: String) -> String? { renders[id] }
    func sig(for rel: String) -> String? { sigs[rel] }

    mutating func set(_ id: String, relPath: String) {
        if let old = byID[id], old != relPath { byPath.removeValue(forKey: old) }
        byID[id] = relPath
        byPath[relPath] = id
    }

    mutating func setPair(_ id: String, relPath: String) {
        if let old = pairs[id], old != relPath { byPairPath.removeValue(forKey: old) }
        pairs[id] = relPath
        byPairPath[relPath] = id
    }

    mutating func setStamp(_ id: String, _ stamp: Double?) { stamps[id] = stamp }
    mutating func setRenderHash(_ id: String, _ hash: String?) { renders[id] = hash }
    mutating func setSig(_ rel: String, _ sig: String?) { sigs[rel] = sig }

    mutating func remove(_ id: String) {
        if let rel = byID.removeValue(forKey: id) {
            byPath.removeValue(forKey: rel)
            sigs.removeValue(forKey: rel)
        }
        if let rel = pairs.removeValue(forKey: id) {
            byPairPath.removeValue(forKey: rel)
            sigs.removeValue(forKey: rel)
        }
        stamps.removeValue(forKey: id)
        renders.removeValue(forKey: id)
    }
}
