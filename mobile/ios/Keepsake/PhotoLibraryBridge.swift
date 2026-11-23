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
//   * New asset in Photos  -> original resource written byte-for-byte to
//     the staging folder (EXIF, HEIC/ProRAW/Live Photo pairing preserved),
//     file mtime set to the capture date, then Syncthing rescans.
//   * Asset deleted in Photos -> staging file removed, Syncthing rescans.
//     The folder has nonDestructiveDeletes, so the other device keeps it.
//   * File arrives from the other device -> imported with
//     PHAssetCreationRequest from the original bytes, creation date taken
//     from the file, and recorded in the mapping so it is not re-exported.
//     Every reconcile also imports staging files that are in neither
//     direction of the mapping, so a missed notification is caught up.
//
// The mapping is saved after every change, so an interrupted pass never
// exports the same photo twice.

import Foundation
import Photos
import ImageIO

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

    // MARK: Photos -> staging folder

    /// Brings the staging folder in line with the photo library, then
    /// imports anything that arrived from other devices but never made it
    /// into Photos. Cheap when nothing changed; call on launch, on
    /// foreground and at the start of every background window.
    func reconcile() async {
        let status = PHPhotoLibrary.authorizationStatus(for: .readWrite)
        guard status == .authorized || status == .limited else { return }

        let opts = PHFetchOptions()
        // Not .typeCloudShared: shared albums hold other people's photos.
        opts.includeAssetSourceTypes = [.typeUserLibrary, .typeiTunesSynced]
        opts.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: true)]
        let assets = PHAsset.fetchAssets(with: opts)

        var present = Set<String>()
        var toExport: [PHAsset] = []
        assets.enumerateObjects { asset, _, _ in
            guard asset.mediaType == .image || asset.mediaType == .video else { return }
            present.insert(asset.localIdentifier)
            if self.mapping.relPath(for: asset.localIdentifier) == nil {
                toExport.append(asset)
            }
        }

        for asset in toExport {
            if isBeingImported(asset) { continue }
            if let rel = await export(asset) {
                // On disk before Syncthing sees the file: a crash after this
                // point can no longer turn into a duplicate export.
                mapping.set(asset.localIdentifier, relPath: rel)
                mapping.save()
                service.rescan(rel)
            }
        }

        // Assets that are gone from Photos were deleted by the user. Only
        // with full access: under limited access the fetch shows just the
        // selected photos, and everything else is merely hidden from us.
        if status == .authorized {
            var removed = false
            for (id, rel) in mapping.all where !present.contains(id) {
                let url = stagingDir.appendingPathComponent(rel)
                try? FileManager.default.removeItem(at: url)
                mapping.remove(id)
                removed = true
                service.rescan(rel)
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

    private func export(_ asset: PHAsset) async -> String? {
        let resources = PHAssetResource.assetResources(for: asset)
        // The unedited original keeps full quality and all metadata. (A Live
        // Photo's paired video and edits are not synced yet.)
        guard let original = resources.first(where: { $0.type == .photo || $0.type == .video })
            ?? resources.first else { return nil }

        let date = asset.creationDate ?? Date()
        let comps = Calendar(identifier: .gregorian).dateComponents([.year, .month], from: date)
        let dir = String(format: "%04d/%02d", comps.year ?? 1970, comps.month ?? 1)
        let dirURL = stagingDir.appendingPathComponent(dir)
        try? FileManager.default.createDirectory(at: dirURL, withIntermediateDirectories: true)

        // Written under a temporary name (ignored by Syncthing, see
        // lib/mobile photoIgnores) and moved into place when complete.
        let tmp = dirURL.appendingPathComponent(".keepsake-\(UUID().uuidString).tmp")

        let opts = PHAssetResourceRequestOptions()
        opts.isNetworkAccessAllowed = true // fetch iCloud originals
        do {
            try await PHAssetResourceManager.default().writeData(for: original, toFile: tmp, options: opts)
            try FileManager.default.setAttributes([.modificationDate: date, .creationDate: date],
                                                  ofItemAtPath: tmp.path)
            // A file left by an earlier export whose mapping was never
            // saved: adopt it rather than exporting a "(1)" duplicate.
            let plain = "\(dir)/\(original.originalFilename)"
            let plainURL = stagingDir.appendingPathComponent(plain)
            if !mapping.hasRelPath(plain),
               FileManager.default.contentsEqual(atPath: plainURL.path, andPath: tmp.path) {
                try? FileManager.default.removeItem(at: tmp)
                return plain
            }
            let rel = uniqueRelPath(dir: dir, name: original.originalFilename)
            try FileManager.default.moveItem(at: tmp, to: stagingDir.appendingPathComponent(rel))
            return rel
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
        switch action {
        case "update":
            guard mapping.localIdentifier(for: relPath) == nil else { return } // our own export
            await importIntoPhotos(relPath: relPath)
        case "delete":
            // Only happens when the other device chose "Delete everywhere".
            // iOS asks the user to confirm the deletion.
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
    /// imported: files received while the app was not told about them
    /// (dropped events, a crash between download and import).
    private func importMissing() async {
        let root = stagingDir.standardizedFileURL.path + "/"
        guard let walker = FileManager.default.enumerator(
            at: stagingDir, includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]) else { return }
        var pending: [String] = []
        while let url = walker.nextObject() as? URL {
            guard (try? url.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true,
                  Self.isMedia(url) else { continue }
            let rel = String(url.standardizedFileURL.path.dropFirst(root.count))
            if mapping.localIdentifier(for: rel) == nil && !importing.contains(rel) {
                pending.append(rel)
            }
        }
        for rel in pending {
            await importIntoPhotos(relPath: rel)
        }
    }

    private func importIntoPhotos(relPath: String) async {
        let url = stagingDir.appendingPathComponent(relPath)
        guard Self.isMedia(url), !importing.contains(relPath) else { return }
        let type: PHAssetResourceType = Self.isMovie(url) ? .video : .photo
        let created = captureDate(of: url)

        let name = url.lastPathComponent
        importing.insert(relPath)
        importingNames[name, default: 0] += 1
        defer {
            importing.remove(relPath)
            importingNames[name, default: 1] -= 1
            if importingNames[name] == 0 { importingNames[name] = nil }
        }

        let placeholder = IdentifierBox()
        do {
            try await PHPhotoLibrary.shared().performChanges {
                let req = PHAssetCreationRequest.forAsset()
                let opts = PHAssetResourceCreationOptions()
                opts.originalFilename = name
                req.addResource(with: type, fileURL: url, options: opts) // original bytes
                req.creationDate = created
                placeholder.value = req.placeholderForCreatedAsset?.localIdentifier
            }
        } catch {
            return
        }
        if let id = placeholder.value {
            mapping.set(id, relPath: relPath)
            mapping.save()
        }
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

    static func isMovie(_ url: URL) -> Bool { movieExtensions.contains(url.pathExtension.lowercased()) }

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

/// Persistent localIdentifier <-> staging relative path mapping.
struct AssetMapping: Codable {
    private var byID: [String: String] = [:]
    private var byPath: [String: String] = [:]
    private var fileURL: URL?

    enum CodingKeys: String, CodingKey { case byID }

    static func load(from url: URL) -> AssetMapping {
        var m = (try? Data(contentsOf: url)).flatMap { try? JSONDecoder().decode(AssetMapping.self, from: $0) }
            ?? AssetMapping()
        m.fileURL = url
        m.byPath = Dictionary(uniqueKeysWithValues: m.byID.map { ($1, $0) })
        return m
    }

    func save() {
        guard let fileURL, let data = try? JSONEncoder().encode(self) else { return }
        try? data.write(to: fileURL, options: .atomic)
    }

    var all: [(String, String)] { byID.map { ($0.key, $0.value) } }
    func relPath(for id: String) -> String? { byID[id] }
    func localIdentifier(for rel: String) -> String? { byPath[rel] }
    func hasRelPath(_ rel: String) -> Bool { byPath[rel] != nil }

    mutating func set(_ id: String, relPath: String) {
        byID[id] = relPath
        byPath[relPath] = id
    }

    mutating func remove(_ id: String) {
        if let rel = byID.removeValue(forKey: id) { byPath.removeValue(forKey: rel) }
    }
}
