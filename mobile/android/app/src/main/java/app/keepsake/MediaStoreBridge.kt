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
// Staging mode (only READ_MEDIA_*): like the iOS bridge, camera photos and
// videos (DCIM) are copied byte-for-byte into an app-specific folder and
// new files from the other device are inserted into MediaStore with their
// original bytes. The mapping is saved after every change, and every
// reconcile also inserts received files that never made it into the
// gallery, so an interrupted pass neither duplicates nor loses anything.
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
    }

    private val mapping = MappingStore(File(context.filesDir, "media-mapping.properties"))

    /** Staging mode only: export new camera items into the staging folder,
     *  drop staging copies of items the user deleted, and insert received
     *  files the gallery doesn't have yet. */
    fun reconcile(rescan: (String) -> Unit) {
        if (direct) return // the camera folder *is* the synced folder
        root.mkdirs()

        val seen = HashSet<Long>()
        val collection = MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL)
        val projection = arrayOf(
            MediaStore.MediaColumns._ID,
            MediaStore.MediaColumns.DISPLAY_NAME,
            MediaStore.MediaColumns.DATE_MODIFIED,
            MediaStore.MediaColumns.RELATIVE_PATH,
            MediaStore.Files.FileColumns.MEDIA_TYPE,
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
                if (mapping.relPath(id) != null) continue
                val name = c.getString(1) ?: continue
                val modifiedMs = c.getLong(2) * 1000
                val dir = c.getString(3)?.trimEnd('/') ?: Environment.DIRECTORY_DCIM
                val itemUri = ContentUris.withAppendedId(collection, id)
                val rel = export(itemUri, id, dir, name, modifiedMs) ?: continue
                mapping.set(id, rel)
                mapping.save() // before Syncthing sees the file
                rescan(rel)
            }
        }
        var removed = false
        for ((id, rel) in mapping.all()) {
            if (id !in seen) {
                File(root, rel).delete()
                mapping.remove(id)
                removed = true
                rescan(rel)
            }
        }
        if (removed) mapping.save()

        insertMissing()
    }

    /** Copies one item into the staging folder through a temporary file
     *  (ignored by Syncthing) and returns its relative path. A file left by
     *  an interrupted earlier pass with the same bytes is adopted instead
     *  of being exported again as "name (1).ext". */
    private fun export(itemUri: Uri, id: Long, dir: String, name: String, modifiedMs: Long): String? {
        val plain = "$dir/$name"
        val plainFile = File(root, plain)
        plainFile.parentFile?.mkdirs()
        val tmp = File(plainFile.parentFile, ".keepsake-$id.tmp")
        // Original bytes; requireOriginal keeps GPS EXIF (needs ACCESS_MEDIA_LOCATION).
        val uri = runCatching { MediaStore.setRequireOriginal(itemUri) }.getOrDefault(itemUri)
        val ok = runCatching {
            context.contentResolver.openInputStream(uri)?.use { input ->
                tmp.outputStream().use { input.copyTo(it) }
            } != null
        }.getOrDefault(false)
        if (!ok) {
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
     *  the gallery (a missed notification, or the app died mid-way). */
    private fun insertMissing() {
        root.walkTopDown()
            .onEnter { !it.name.startsWith(".") || it == root }
            .filter { it.isFile && isMedia(it) }
            .forEach { f ->
                val rel = f.relativeTo(root).invariantSeparatorsPath
                if (mapping.idFor(rel) == null) insertIntoMediaStore(f, rel)
            }
    }

    fun onRemoteChange(relPath: String, action: String) {
        val file = File(root, relPath)
        if (direct) {
            // Tell MediaStore about the new/removed file so the Gallery updates.
            MediaScannerConnection.scanFile(context, arrayOf(file.absolutePath), null, null)
            return
        }
        when (action) {
            "update" -> if (mapping.idFor(relPath) == null && isMedia(file)) insertIntoMediaStore(file, relPath)
            "delete" -> mapping.idFor(relPath)?.let { id ->
                // Only after "Delete everywhere" on another device. Android 11+
                // requires user consent for deleting other apps' media; the
                // dashboard asks with MediaStore.createDeleteRequest.
                PendingDeletes.add(context, id)
            }
        }
    }

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
        mapping.set(ContentUris.parseId(uri), relPath)
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

/** MediaStore _ID <-> relative path, persisted as a properties file.
 *  Saved through a temporary file so a crash never leaves it half written. */
class MappingStore(private val file: File) {
    private val byId = HashMap<Long, String>()
    private val byPath = HashMap<String, Long>()

    init {
        if (file.exists()) {
            runCatching {
                java.util.Properties().apply { file.inputStream().use { load(it) } }
                    .forEach { (k, v) -> set(k.toString().toLong(), v.toString()) }
            }
        }
    }

    @Synchronized fun relPath(id: Long) = byId[id]
    @Synchronized fun idFor(rel: String) = byPath[rel]
    @Synchronized fun all(): List<Pair<Long, String>> = byId.map { it.key to it.value }
    @Synchronized fun set(id: Long, rel: String) { byId[id] = rel; byPath[rel] = id }
    @Synchronized fun remove(id: Long) { byId.remove(id)?.let { byPath.remove(it) } }

    @Synchronized fun save() {
        val p = java.util.Properties()
        byId.forEach { (k, v) -> p[k.toString()] = v }
        val tmp = File(file.parentFile, file.name + ".tmp")
        tmp.outputStream().use { p.store(it, null) }
        if (!tmp.renameTo(file)) {
            file.delete()
            tmp.renameTo(file)
        }
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
