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
// Staging mode (only READ_MEDIA_*): like the iOS bridge, photos are copied
// byte-for-byte into an app-specific folder and new files from the other
// device are inserted into MediaStore with their original bytes.

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

class MediaStoreBridge(private val context: Context, private val root: File) {

    companion object {
        fun hasAllFilesAccess(): Boolean =
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.R && Environment.isExternalStorageManager()
    }

    private val direct get() = hasAllFilesAccess()
    private val mapping = MappingStore(File(context.filesDir, "media-mapping.properties"))

    /** Staging mode only: export new MediaStore items into the staging
     *  folder and drop staging copies of items the user deleted. */
    fun reconcile(rescan: (String) -> Unit) {
        if (direct) return // the camera folder *is* the synced folder
        root.mkdirs()

        val seen = HashSet<Long>()
        val collection = MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL)
        val projection = arrayOf(
            MediaStore.MediaColumns._ID,
            MediaStore.MediaColumns.DISPLAY_NAME,
            MediaStore.MediaColumns.DATE_MODIFIED,
            MediaStore.MediaColumns.RELATIVE_PATH,
        )
        context.contentResolver.query(collection, projection, null, null, null)?.use { c ->
            while (c.moveToNext()) {
                val id = c.getLong(0)
                seen += id
                if (mapping.relPath(id) != null) continue
                val name = c.getString(1)
                val modifiedMs = c.getLong(2) * 1000
                val rel = uniqueRelPath(c.getString(3)?.trimEnd('/') ?: "Pictures", name)
                val dst = File(root, rel).apply { parentFile?.mkdirs() }
                // Original bytes; requireOriginal keeps GPS EXIF (needs ACCESS_MEDIA_LOCATION).
                val uri = MediaStore.setRequireOriginal(ContentUris.withAppendedId(collection, id))
                context.contentResolver.openInputStream(uri)?.use { input ->
                    dst.outputStream().use { input.copyTo(it) }
                }
                dst.setLastModified(modifiedMs)
                mapping.set(id, rel)
                rescan(rel)
            }
        }
        for ((id, rel) in mapping.all()) {
            if (id !in seen) {
                File(root, rel).delete()
                mapping.remove(id)
                rescan(rel)
            }
        }
        mapping.save()
    }

    fun onRemoteChange(relPath: String, action: String) {
        val file = File(root, relPath)
        if (direct) {
            // Tell MediaStore about the new/removed file so the Gallery updates.
            MediaScannerConnection.scanFile(context, arrayOf(file.absolutePath), null, null)
            return
        }
        when (action) {
            "update" -> if (mapping.idFor(relPath) == null) insertIntoMediaStore(file, relPath)
            "delete" -> mapping.idFor(relPath)?.let { id ->
                // Only after "Delete everywhere" on the other device. Android 11+
                // requires user consent for deleting other apps' media, which the
                // UI requests via MediaStore.createDeleteRequest.
                PendingDeletes.add(context, id)
            }
        }
    }

    private fun insertIntoMediaStore(file: File, relPath: String) {
        val isVideo = file.extension.lowercase() in setOf("mp4", "mov", "3gp", "m4v")
        val collection = if (isVideo) MediaStore.Video.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        else MediaStore.Images.Media.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)
        val values = ContentValues().apply {
            put(MediaStore.MediaColumns.DISPLAY_NAME, file.name)
            put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DCIM + "/Keepsake")
            put(MediaStore.MediaColumns.DATE_MODIFIED, file.lastModified() / 1000)
            put(MediaStore.MediaColumns.IS_PENDING, 1)
        }
        val resolver = context.contentResolver
        val uri: Uri = resolver.insert(collection, values) ?: return
        resolver.openOutputStream(uri)?.use { out -> file.inputStream().use { it.copyTo(out) } }
        values.clear()
        values.put(MediaStore.MediaColumns.IS_PENDING, 0)
        resolver.update(uri, values, null, null)
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

/** MediaStore _ID <-> relative path, persisted as a properties file. */
class MappingStore(private val file: File) {
    private val byId = HashMap<Long, String>()
    private val byPath = HashMap<String, Long>()

    init {
        if (file.exists()) {
            java.util.Properties().apply { file.inputStream().use { load(it) } }
                .forEach { (k, v) -> set(k.toString().toLong(), v.toString()) }
        }
    }

    fun relPath(id: Long) = byId[id]
    fun idFor(rel: String) = byPath[rel]
    fun all(): List<Pair<Long, String>> = byId.map { it.key to it.value }
    fun set(id: Long, rel: String) { byId[id] = rel; byPath[rel] = id }
    fun remove(id: Long) { byId.remove(id)?.let { byPath.remove(it) } }

    fun save() {
        val p = java.util.Properties()
        byId.forEach { (k, v) -> p[k.toString()] = v }
        file.outputStream().use { p.store(it, null) }
    }
}

/** Deletions that need the user's consent, shown by the dashboard activity. */
object PendingDeletes {
    private const val PREFS = "pending-deletes"
    fun add(context: Context, id: Long) {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        prefs.edit().putStringSet("ids", (prefs.getStringSet("ids", emptySet())!! + id.toString())).apply()
    }
    fun take(context: Context): List<Long> {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val ids = prefs.getStringSet("ids", emptySet())!!.map { it.toLong() }
        prefs.edit().remove("ids").apply()
        return ids
    }
}
