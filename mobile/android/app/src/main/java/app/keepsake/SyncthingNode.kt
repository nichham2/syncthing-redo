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

package app.keepsake

import android.content.Context
import android.os.Environment
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import java.io.File

object SyncthingNode : mobile.Platform {
    const val PHOTO_FOLDER_ID = "photos"

    private lateinit var appContext: Context
    private var node: mobile.Node? = null
    private lateinit var media: MediaStoreBridge

    private val _status = MutableStateFlow("{}")
    val status: StateFlow<String> = _status

    @Synchronized
    fun init(context: Context) {
        if (node != null) return
        appContext = context.applicationContext
        media = MediaStoreBridge(appContext, photoDir(appContext))
        node = mobile.Mobile.newNode(File(appContext.filesDir, "syncthing").absolutePath).also {
            it.setPlatform(this)
        }
    }

    /** Direct mode syncs the camera folder in place; staging mode syncs a
     *  private copy that MediaStoreBridge mirrors into the gallery. */
    fun photoDir(context: Context): File =
        if (MediaStoreBridge.hasAllFilesAccess()) {
            File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DCIM), "Camera")
        } else {
            File(context.getExternalFilesDir(Environment.DIRECTORY_PICTURES), "Keepsake")
        }

    @Synchronized
    fun start() {
        val n = node ?: error("init() first")
        n.start()
        n.ensurePhotoFolder(PHOTO_FOLDER_ID, "Camera", photoDir(appContext).absolutePath)
        media.reconcile { rel -> n.rescan(PHOTO_FOLDER_ID, rel) }
    }

    @Synchronized
    fun stop() {
        node?.stop()
    }

    val dashboardUrl: String? get() = node?.dashboardURL()?.takeIf { it.isNotEmpty() }
    val deviceId: String get() = node?.deviceID() ?: ""

    fun addPeer(deviceId: String, name: String) = node?.addPeer(deviceId, name)

    fun setPaused(paused: Boolean) = node?.setPaused(paused)

    /** One bounded background window; see SyncWorker. */
    fun runBackgroundSync(maxSeconds: Long): String {
        media.reconcile { rel -> node?.rescan(PHOTO_FOLDER_ID, rel) }
        return node?.runBackgroundSync(maxSeconds) ?: "{}"
    }

    // mobile.Platform (called on a Go thread)

    override fun onRemoteChange(folderID: String, relPath: String, action: String) {
        if (folderID != PHOTO_FOLDER_ID) return
        media.onRemoteChange(relPath, action)
    }

    override fun onStateChanged(statusJSON: String) {
        _status.value = statusJSON
    }
}
