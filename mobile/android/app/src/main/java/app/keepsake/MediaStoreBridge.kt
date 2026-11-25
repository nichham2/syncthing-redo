// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Keeps Android's MediaStore (what the Gallery shows) in step with the
// synced folder.
//
// Direct mode (All files access granted): Syncthing syncs DCIM/Camera in
// place. Files keep their original bytes, so EXIF and quality are intact,
// and Syncthing preserves modification times. New files from the other
// device only need to be announced to MediaStore with scanFile; deletions
// in the Gallery remove the file, Syncthing notices on its next scan and
// (with nonDestructiveDeletes) records it as ignored instead of deleting
// it on the other device.
//
// Edits made in the Gallery change the file in place, so Syncthing sends
// them as updates; edits arriving from another device overwrite the file
// and the scan below makes the Gallery show them.
//
// Staging mode (only READ_MEDIA_*): like the iOS bridge, camera photos and
// videos (DCIM) are copied byte-for-byte into an app-specific folder and
// new files from the other device are inserted into MediaStore with their
// original bytes. An item edited in the Gallery is copied again over its
// staging file; a staging file changed by another device is written back
// into its gallery item (asking the user first when another app owns it).
// A Live Photo's video (IMG_0001.MOV next to IMG_0001.HEIC) stays in the
// staging folder with its photo instead of showing up as a separate clip.
// The mapping is saved after every change, and every reconcile also
// inserts received files that never made it into the gallery and applies
// changed ones, so an interrupted pass neither duplicates nor loses
// anything. Rescans are batched: one request per pass, or per 100 files.
//
// Not thread-safe on its own: SyncthingNode runs all calls on its single
// bridge executor.

package app.keepsake

import android.content.ContentUris
import android.content.ContentValues
import android.content.Context
import android.media.MediaScannerConnection
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import java.io.File

class MediaStoreBridge(
    private val context: Context,
    private val root: File,
    /** Fixed when the folder is set up; see SyncthingNode.isDirectMode. */
    private val direct: Boolean,
) {

    companion object {
        fun hasAllFilesAccess(): Boolean =
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.R && Environment.isExternalStorageManager()

        private val videoExtensions = setOf("mp4", "mov", "3gp", "m4v", "mkv", "webm")
        private val imageExtensions = setOf("jpg", "jpeg", "heic", "heif", "png", "gif", "webp", "dng", "avif")

        fun isMedia(file: File): Boolean {
            if (file.name.startsWith(".")) return false
            val ext = file.extension.lowercase()
            return ext in imageExtensions || ext in videoExtensions
        }

        fun isVideo(file: File) = file.extension.lowercase() in videoExtensions

        private val livePhotoStillExtensions = listOf("heic", "jpg", "jpeg")

        /** IMG_0001.HEIC for IMG_0001.MOV, if it is there. */
        fun livePhotoStill(movie: File): File? {
            if (movie.extension.lowercase() != "mov") return null
            val base = movie.nameWithoutExtension
            return livePhotoStillExtensions.flatMap { listOf(it, it.uppercase()) }
                .map { File(movie.parentFile, "$base.$it") }
                .firstOrNull { it.isFile }
        }

        private const val RESCAN_BATCH = 100
    }

    /** Paths to rescan, sent in batches rather than one request each. */
    private class Rescans(private val send: (List<String>) -> Unit) {
        private val paths = ArrayList<String>()
        fun add(rel: String) {
            paths += rel
            if (paths.size >= RESCAN_BATCH) flush()
        }
        fun flush() {
            if (paths.isEmpty()) return
            val batch = paths.toList()
            paths.clear()
            send(batch)
        }
    }

    private val mapping = MappingStore(File(context.filesDir, "media-mapping.properties"))

    /** Staging mode only: export new camera items into the staging folder,
     *  drop staging copies of items the user deleted, and insert received
     *  files the gallery doesn't have yet. */
    fun reconcile(rescan: (List<String>) -> Unit) {
        if (direct) return // the camera folder *is* the synced folder
        root.mkdirs()
        val rescans = Rescans(rescan)
        try {
            reconcileStaging(rescans)
        } finally {
            rescans.flush()
        }
    }

    private fun reconcileStaging(rescans: Rescans) {
        val seen = HashSet<Long>()
        val collection = MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL)
        val projection = arrayOf(
            MediaStore.MediaColumns._ID,
            MediaStore.MediaColumns.DISPLAY_NAME,
            MediaStore.MediaColumns.DATE_MODIFIED,
            MediaStore.MediaColumns.RELATIVE_PATH,
            MediaStore.Files.FileColumns.MEDIA_TYPE,
            MediaStore.MediaColumns.SIZE,
        )
        // Photos and videos under DCIM only: not screenshots, downloads or
        // other apps' pictures. Our own inserts live in DCIM/Keepsake.
        val selection = "(${MediaStore.Files.FileColumns.MEDIA_TYPE} IN (?, ?)) AND " +
            "${MediaStore.MediaColumns.RELATIVE_PATH} LIKE ?"
        val args = arrayOf(
            MediaStore.Files.FileColumns.MEDIA_TYPE_IMAGE.toString(),
            MediaStore.Files.FileColumns.MEDIA_TYPE_VIDEO.toString(),
            Environment.DIRECTORY_DCIM + "/%",
        )
        val cursor = context.contentResolver.query(collection, projection, selection, args, null)
            ?: return // no access: don't mistake that for "everything was deleted"
        cursor.use { c ->
            while (c.moveToNext()) {
                val id = c.getLong(0)
                seen += id
                val modifiedMs = c.getLong(2) * 1000
                val stamp = "${c.getLong(2)}:${c.getLong(5)}"
                val itemUri = ContentUris.withAppendedId(collection, id)
                val mapped = mapping.relPath(id)
                if (mapped != null) {
                    when (mapping.stamp(id)) {
                        stamp -> {}
                        null -> { mapping.setStamp(id, stamp); mapping.save() } // first pass with this version
                        else -> refresh(itemUri, id, mapped, modifiedMs, stamp, rescans)
                    }
                    continue
                }
                val name = c.getString(1) ?: continue
                val dir = c.getString(3)?.trimEnd('/') ?: Environment.DIRECTORY_DCIM
                val rel = export(itemUri, id, dir, name, modifiedMs) ?: continue
                mapping.set(id, rel)
                mapping.setStamp(id, stamp)
                mapping.setSig(rel, sig(File(root, rel)))
                mapping.save() // before Syncthing sees the file
                rescans.add(rel)
            }
        }
        var removed = false
        for ((id, rel) in mapping.all()) {
            if (id !in seen) {
                for (path in listOf(rel) + mapping.pairsOf(id)) {
                    File(root, path).delete()
                    rescans.add(path)
                }
                mapping.remove(id)
                removed = true
            }
        }
        if (removed) mapping.save()

        insertMissing()
    }

    /** A mapped item changed in the gallery (an edit saved in place, or
     *  just new metadata): copy it over its staging file if the bytes
     *  differ, under the same name so other devices get an update. */
    private fun refresh(itemUri: Uri, id: Long, rel: String, modifiedMs: Long, stamp: String, rescans: Rescans) {
        val file = File(root, rel)
        // Another device changed the file and that isn't applied yet: its
        // version wins, and insertMissing writes it into the gallery.
        if (file.exists() && mapping.sig(rel) != null && mapping.sig(rel) != sig(file)) return
        file.parentFile?.mkdirs()
        val tmp = File(file.parentFile, ".keepsake-$id.tmp")
        if (!copyOriginal(itemUri, tmp)) {
            tmp.delete()
            return
        }
        if (file.exists() && sameBytes(file, tmp)) {
            tmp.delete()
        } else {
            tmp.setLastModified(modifiedMs)
            if (!tmp.renameTo(file)) {
                tmp.delete()
                return
            }
            mapping.setSig(rel, sig(file))
            rescans.add(rel)
        }
        mapping.setStamp(id, stamp)
        mapping.save()
    }

    private fun copyOriginal(itemUri: Uri, to: File): Boolean {
        // Original bytes; requireOriginal keeps GPS EXIF (needs ACCESS_MEDIA_LOCATION).
        val uri = runCatching { MediaStore.setRequireOriginal(itemUri) }.getOrDefault(itemUri)
        return runCatching {
            context.contentResolver.openInputStream(uri)?.use { input ->
                to.outputStream().use { input.copyTo(it) }
            } != null
        }.getOrDefault(false)
    }

    /** Size and modification time: what changes when Syncthing writes a
     *  new version of the file. */
    private fun sig(file: File): String? = if (file.isFile) "${file.length()}:${file.lastModified() / 1000}" else null

    /** Copies one item into the staging folder through a temporary file
     *  (ignored by Syncthing) and returns its relative path. A file left by
     *  an interrupted earlier pass with the same bytes is adopted instead
     *  of being exported again as "name (1).ext". */
    private fun export(itemUri: Uri, id: Long, dir: String, name: String, modifiedMs: Long): String? {
        val plain = "$dir/$name"
        val plainFile = File(root, plain)
        plainFile.parentFile?.mkdirs()
        val tmp = File(plainFile.parentFile, ".keepsake-$id.tmp")
        if (!copyOriginal(itemUri, tmp)) {
            tmp.delete()
            return null
        }
        tmp.setLastModified(modifiedMs)
        if (mapping.idFor(plain) == null && plainFile.exists() && sameBytes(plainFile, tmp)) {
            tmp.delete()
            return plain
        }
        val rel = uniqueRelPath(dir, name)
        if (!tmp.renameTo(File(root, rel))) {
            tmp.delete()
            return null
        }
        return rel
    }

    private fun sameBytes(a: File, b: File): Boolean {
        if (a.length() != b.length()) return false
        a.inputStream().buffered().use { ia ->
            b.inputStream().buffered().use { ib ->
                while (true) {
                    val x = ia.read()
                    if (x != ib.read()) return false
                    if (x == -1) return true
                }
            }
        }
    }

    /** Inserts staging files that are neither our exports nor already in
     *  the gallery (a missed notification, or the app died mid-way), and
     *  writes back files another device changed while we weren't told.
     *  Photos go before videos, so a Live Photo's video finds its photo. */
    private fun insertMissing() {
        val files = root.walkTopDown()
            .onEnter { !it.name.startsWith(".") || it == root }
            .filter { it.isFile && isMedia(it) }
            .sortedBy { isVideo(it) }
            .toList()
        for (f in files) {
            val rel = f.relativeTo(root).invariantSeparatorsPath
            val id = mapping.idFor(rel)
            when {
                id != null -> when (mapping.sig(rel)) {
                    sig(f) -> {}
                    null -> { mapping.setSig(rel, sig(f)); mapping.save() } // first pass with this version
                    else -> applyRemoteEdit(id, f, rel)
                }
                mapping.pairOwner(rel) != null -> {}
                else -> received(f, rel)
            }
        }
    }

    fun onRemoteChange(relPath: String, action: String) {
        val file = File(root, relPath)
        if (direct) {
            // Tell MediaStore about the new, changed or removed file so the
            // Gallery updates (an edit on another device included).
            MediaScannerConnection.scanFile(context, arrayOf(file.absolutePath), null, null)
            return
        }
        when (action) {
            "update" -> {
                if (!isMedia(file)) return
                val id = mapping.idFor(relPath)
                when {
                    id != null -> if (mapping.sig(relPath) != sig(file)) applyRemoteEdit(id, file, relPath)
                    mapping.pairOwner(relPath) != null -> { mapping.setSig(relPath, sig(file)); mapping.save() }
                    else -> received(file, relPath)
                }
            }
            "delete" -> {
                if (mapping.pairOwner(relPath) != null) return // goes with its photo
                mapping.idFor(relPath)?.let { id ->
                    // Only after "Delete everywhere" on another device. Android 11+
                    // requires user consent for deleting other apps' media; the
                    // dashboard asks with MediaStore.createDeleteRequest.
                    PendingDeletes.add(context, id)
                }
            }
        }
    }

    /** A new file from another device. A Live Photo's video stays with
     *  its photo in the staging folder; everything else goes into the
     *  gallery. */
    private fun received(file: File, rel: String) {
        val still = livePhotoStill(file)
        if (still != null) {
            val owner = mapping.idFor(still.relativeTo(root).invariantSeparatorsPath)
                ?: return // the photo goes in first; the next pass pairs them
            mapping.addPair(owner, rel)
            mapping.setSig(rel, sig(file))
            mapping.save()
            return
        }
        insertIntoMediaStore(file, rel)
    }

    /** Writes a staging file that another device changed (an edit there)
     *  into its gallery item. The gallery only lets us write items another
     *  app owns with the user's consent; those wait for the dashboard to
     *  ask (MediaStore.createWriteRequest) and are retried after that. */
    fun applyRemoteEdit(id: Long, file: File, rel: String) {
        val uri = itemUri(id, file)
        val written = try {
            context.contentResolver.openOutputStream(uri, "wt")?.use { out ->
                file.inputStream().use { it.copyTo(out) }
            } != null
        } catch (e: SecurityException) {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                PendingEdits.add(context, id)
                return // retried once the user allows it
            }
            false // Android 10 has no way to ask for several items
        } catch (e: Exception) {
            false
        }
        // Recorded either way, so a file the gallery can't take is not
        // copied again on every pass.
        mapping.setSig(rel, sig(file))
        if (written) {
            // MediaStore rescans the item when the stream closes. Note its
            // new date and size: our own write is not an edit to send back.
            currentStamp(uri)?.let { mapping.setStamp(id, it) }
        }
        mapping.save()
    }

    /** Retries edits the user has now allowed us to write. */
    fun applyPendingEdits(ids: Collection<Long>) {
        for (id in ids) {
            val rel = mapping.relPath(id) ?: continue
            val file = File(root, rel)
            if (file.isFile) applyRemoteEdit(id, file, rel)
        }
    }

    private fun itemUri(id: Long, file: File): Uri {
        val collection = if (isVideo(file)) MediaStore.Video.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        else MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        return ContentUris.withAppendedId(collection, id)
    }

    private fun currentStamp(uri: Uri): String? =
        context.contentResolver.query(
            uri, arrayOf(MediaStore.MediaColumns.DATE_MODIFIED, MediaStore.MediaColumns.SIZE), null, null, null,
        )?.use { c -> if (c.moveToFirst()) "${c.getLong(0)}:${c.getLong(1)}" else null }

    private fun insertIntoMediaStore(file: File, relPath: String) {
        val collection = if (isVideo(file)) MediaStore.Video.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        else MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        val values = ContentValues().apply {
            put(MediaStore.MediaColumns.DISPLAY_NAME, file.name)
            put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DCIM + "/Keepsake")
            put(MediaStore.MediaColumns.DATE_MODIFIED, file.lastModified() / 1000)
            put(MediaStore.MediaColumns.IS_PENDING, 1)
        }
        val resolver = context.contentResolver
        val uri: Uri = resolver.insert(collection, values) ?: return
        val copied = runCatching {
            resolver.openOutputStream(uri)?.use { out -> file.inputStream().use { it.copyTo(out) } } != null
        }.getOrDefault(false)
        if (!copied) {
            resolver.delete(uri, null, null)
            return
        }
        values.clear()
        values.put(MediaStore.MediaColumns.IS_PENDING, 0)
        resolver.update(uri, values, null, null)
        // MediaStore.Files and the Images/Video collections share IDs, so
        // reconcile recognizes this item as ours on the next pass.
        val id = ContentUris.parseId(uri)
        mapping.set(id, relPath)
        mapping.setSig(relPath, sig(file))
        currentStamp(uri)?.let { mapping.setStamp(id, it) }
        mapping.save()
    }

    private fun uniqueRelPath(dir: String, name: String): String {
        var candidate = "$dir/$name"
        var n = 1
        while (File(root, candidate).exists() || mapping.idFor(candidate) != null) {
            candidate = "$dir/${name.substringBeforeLast('.')} ($n).${name.substringAfterLast('.')}"
            n++
        }
        return candidate
    }
}

/** MediaStore _ID <-> relative path, persisted as a properties file,
 *  plus a Live Photo's video paths per item, each item's last seen
 *  "date_modified:size", and each staging file's "size:mtime" as last
 *  written or inserted. Saved through a temporary file so a crash never
 *  leaves it half written. Older files with only id=path lines load fine. */
class MappingStore(private val file: File) {
    private val byId = HashMap<Long, String>()
    private val byPath = HashMap<String, Long>()
    private val pairOwners = HashMap<String, Long>()
    private val stamps = HashMap<Long, String>()
    private val sigs = HashMap<String, String>()

    init {
        if (file.exists()) {
            runCatching {
                java.util.Properties().apply { file.inputStream().use { load(it) } }
                    .forEach { (k, v) ->
                        val key = k.toString()
                        val value = v.toString()
                        runCatching {
                            when {
                                key.startsWith(PAIR) -> pairOwners[key.removePrefix(PAIR)] = value.toLong()
                                key.startsWith(STAMP) -> stamps[key.removePrefix(STAMP).toLong()] = value
                                key.startsWith(SIG) -> sigs[key.removePrefix(SIG)] = value
                                else -> set(key.toLong(), value)
                            }
                        }
                    }
            }
        }
    }

    @Synchronized fun relPath(id: Long) = byId[id]
    @Synchronized fun idFor(rel: String) = byPath[rel]
    @Synchronized fun all(): List<Pair<Long, String>> = byId.map { it.key to it.value }
    @Synchronized fun set(id: Long, rel: String) {
        byId[id]?.let { if (it != rel) byPath.remove(it) }
        byId[id] = rel
        byPath[rel] = id
    }
    @Synchronized fun pairOwner(rel: String) = pairOwners[rel]
    @Synchronized fun pairsOf(id: Long): List<String> = pairOwners.filterValues { it == id }.keys.toList()
    @Synchronized fun addPair(id: Long, rel: String) { pairOwners[rel] = id }
    @Synchronized fun stamp(id: Long) = stamps[id]
    @Synchronized fun setStamp(id: Long, stamp: String) { stamps[id] = stamp }
    @Synchronized fun sig(rel: String) = sigs[rel]
    @Synchronized fun setSig(rel: String, sig: String?) { if (sig == null) sigs.remove(rel) else sigs[rel] = sig }
    @Synchronized fun remove(id: Long) {
        byId.remove(id)?.let { byPath.remove(it); sigs.remove(it) }
        pairsOf(id).forEach { pairOwners.remove(it); sigs.remove(it) }
        stamps.remove(id)
    }

    @Synchronized fun save() {
        val p = java.util.Properties()
        byId.forEach { (k, v) -> p[k.toString()] = v }
        pairOwners.forEach { (k, v) -> p[PAIR + k] = v.toString() }
        stamps.forEach { (k, v) -> p[STAMP + k] = v }
        sigs.forEach { (k, v) -> p[SIG + k] = v }
        val tmp = File(file.parentFile, file.name + ".tmp")
        tmp.outputStream().use { p.store(it, null) }
        if (!tmp.renameTo(file)) {
            file.delete()
            tmp.renameTo(file)
        }
    }

    private companion object {
        const val PAIR = "pair:"
        const val STAMP = "stamp:"
        const val SIG = "sig:"
    }
}

/** Deletions that need the user's consent, shown by the dashboard activity. */
object PendingDeletes {
    private const val PREFS = "pending-deletes"

    @Synchronized
    fun add(context: Context, id: Long) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        prefs.edit().putStringSet("ids", (prefs.getStringSet("ids", emptySet())!! + id.toString())).apply()
    }

    @Synchronized
    fun peek(context: Context): List<Long> =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getStringSet("ids", emptySet())!!.mapNotNull { it.toLongOrNull() }

    /** Removes the ids once the delete request was answered. */
    @Synchronized
    fun remove(context: Context, ids: Collection<Long>) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val left = prefs.getStringSet("ids", emptySet())!! - ids.map { it.toString() }.toSet()
        prefs.edit().putStringSet("ids", left).apply()
    }
}

/** Edits from other devices to gallery items another app owns: Android 11+
 *  needs the user's consent to write them, asked by the dashboard. */
object PendingEdits {
    private const val PREFS = "pending-edits"

    @Synchronized
    fun add(context: Context, id: Long) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        prefs.edit().putStringSet("ids", (prefs.getStringSet("ids", emptySet())!! + id.toString())).apply()
    }

    @Synchronized
    fun peek(context: Context): List<Long> =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            .getStringSet("ids", emptySet())!!.mapNotNull { it.toLongOrNull() }

    @Synchronized
    fun remove(context: Context, ids: Collection<Long>) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val left = prefs.getStringSet("ids", emptySet())!! - ids.map { it.toString() }.toSet()
        prefs.edit().putStringSet("ids", left).apply()
    }
}
