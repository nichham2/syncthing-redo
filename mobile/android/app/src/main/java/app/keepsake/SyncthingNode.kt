// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Process-wide holder for the embedded node (keepsake-core.aar, built with
// `gomobile bind -target=android ./lib/mobile`). gomobile maps package
// "mobile" to the Java package "mobile": mobile.Mobile.newNode(),
// mobile.Node, mobile.Platform.
//
// The dashboard, WorkManager runs and the optional foreground service all
// run in this one process and share the node. Each of them takes a hold
// (Node.hold / Node.unhold) for as long as it needs the node, so one of
// them finishing never stops it under another.

package app.keepsake

import android.content.Context
import android.os.Environment
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.io.File
import java.util.concurrent.Executors

object SyncthingNode : mobile.Platform {
    const val PHOTO_FOLDER_ID = "photos"
    private const val PREFS = "keepsake"
    private const val KEY_MODE = "photoMode"

    private lateinit var appContext: Context
    private var node: mobile.Node? = null
    private lateinit var media: MediaStoreBridge

    /** All MediaStore and staging-folder work runs here, one task at a
     *  time, so the Go callback thread is never blocked and the bridge's
     *  mapping is never touched by two threads at once. */
    val bridgeExecutor = Executors.newSingleThreadExecutor { r -> Thread(r, "keepsake-bridge") }

    private val _status = MutableStateFlow("{}")
    val status: StateFlow<String> = _status

    @Synchronized
    fun init(context: Context) {
        if (node != null) return
        appContext = context.applicationContext
        media = MediaStoreBridge(appContext, photoDir(appContext), direct = isDirectMode(appContext))
        node = mobile.Mobile.newNode(File(appContext.filesDir, "syncthing").absolutePath).also {
            it.setPlatform(this)
        }
    }

    /** Direct mode syncs the camera folder in place; staging mode syncs a
     *  private copy that MediaStoreBridge mirrors into the gallery.
     *
     *  The mode is decided once, when the folder is first set up, and then
     *  kept: moving an existing folder between the two places would make
     *  the engine stop on a missing folder marker, or upload the whole
     *  camera roll again under new names. */
    fun isModeDecided(context: Context): Boolean =
        context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).contains(KEY_MODE)

    fun isDirectMode(context: Context): Boolean {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        if (!prefs.contains(KEY_MODE)) {
            val direct = MediaStoreBridge.hasAllFilesAccess()
            prefs.edit().putString(KEY_MODE, if (direct) "direct" else "staging").apply()
            return direct
        }
        return prefs.getString(KEY_MODE, "staging") == "direct"
    }

    fun photoDir(context: Context): File =
        if (isDirectMode(context)) {
            File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DCIM), "Camera")
        } else {
            File(context.getExternalFilesDir(Environment.DIRECTORY_PICTURES), "Keepsake")
        }

    /** Takes one hold on the node, starting it and setting up the photo
     *  folder if needed. Pair every call with [unhold]. */
    @Synchronized
    fun hold() {
        val n = node ?: error("init() first")
        n.hold()
        try {
            n.ensurePhotoFolder(PHOTO_FOLDER_ID, "Camera", photoDir(appContext).absolutePath)
        } catch (e: Exception) {
            n.unhold()
            throw e
        }
    }

    fun unhold() {
        node?.unhold()
    }

    /** Mirrors the photo library into the folder (staging mode) and imports
     *  anything received but not yet in the gallery. Rescans go to the
     *  engine in batches. Rescan failures are not fatal: the file watcher and the next full scan pick changes up. */
    fun reconcile() {
        val n = node ?: return
        media.reconcile { rels -> runCatching { n.rescanPaths(PHOTO_FOLDER_ID, rels.joinToString("\n")) } }
    }

    /** Writes edits from other devices that the user just allowed. */
    fun applyPendingEdits(ids: Collection<Long>) {
        if (node == null) return
        media.applyPendingEdits(ids)
    }

    val dashboardUrl: String? get() = node?.dashboardURL()?.takeIf { it.isNotEmpty() }
    val deviceId: String get() = node?.deviceID() ?: ""

    fun addPeer(deviceId: String, name: String) = node?.addPeer(deviceId, name)

    fun setPaused(paused: Boolean) = node?.setPaused(paused)

    /** One bounded background window; see SyncWorker. The node is held and
     *  running before the bridge asks it to rescan anything. */
    fun runBackgroundSync(maxSeconds: Long): String {
        val n = node ?: error("init() first")
        hold()
        try {
            bridgeExecutor.submit { reconcile() }.get()
            return n.runBackgroundSync(maxSeconds)
        } finally {
            unhold()
        }
    }

    // mobile.Platform (called on a Go thread; never block it)

    override fun onRemoteChange(folderID: String, relPath: String, action: String) {
        if (folderID != PHOTO_FOLDER_ID) return
        bridgeExecutor.execute { runCatching { media.onRemoteChange(relPath, action) } }
    }

    override fun onStateChanged(statusJSON: String) {
        _status.value = statusJSON
    }
}
