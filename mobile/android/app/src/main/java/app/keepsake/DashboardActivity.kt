// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Hosts the shared mobile dashboard (gui/default/mobile) in a WebView, so
// Android and iOS present the same UI. Also asks for the permissions the
// app needs and for consent to deletions requested from other devices.

package app.keepsake

import android.Manifest
import android.app.AlertDialog
import android.content.ContentUris
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.MediaStore
import android.provider.Settings
import android.util.Log
import android.webkit.JavascriptInterface
import android.webkit.WebView
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.activity.result.IntentSenderRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class DashboardActivity : ComponentActivity() {
    private lateinit var web: WebView
    private var held = false
    private var askedPermissions = false
    private var askedAllFiles = false
    private var pendingDeleteIds: List<Long> = emptyList()

    private val askPermissions = registerForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) {
        startNode()
    }

    private val askDelete = registerForActivityResult(ActivityResultContracts.StartIntentSenderForResult()) {
        // Answered either way: don't ask again for these. If the user said
        // no, the photos stay here (and stay deleted on the other devices).
        PendingDeletes.remove(this, pendingDeleteIds)
        pendingDeleteIds = emptyList()
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        web = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            addJavascriptInterface(NativeBridge(), "KeepsakeNative")
        }
        setContentView(web)
    }

    override fun onStart() {
        super.onStart()
        if (!askedPermissions) {
            askedPermissions = true
            askPermissions.launch(runtimePermissions())
        } else {
            startNode()
        }
    }

    override fun onResume() {
        super.onResume()
        askDeleteConsent()
    }

    override fun onStop() {
        // Let go of the node; a background run or "Keep syncing" may
        // still hold it.
        if (held) {
            held = false
            lifecycleScope.launch(Dispatchers.IO) { SyncthingNode.unhold() }
        }
        super.onStop()
    }

    private fun runtimePermissions(): Array<String> {
        val perms = mutableListOf<String>()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            perms += Manifest.permission.READ_MEDIA_IMAGES
            perms += Manifest.permission.READ_MEDIA_VIDEO
            perms += Manifest.permission.POST_NOTIFICATIONS
        } else {
            perms += Manifest.permission.READ_EXTERNAL_STORAGE
        }
        perms += Manifest.permission.ACCESS_MEDIA_LOCATION
        return perms.toTypedArray()
    }

    private fun startNode() {
        if (held) return
        // Direct mode (sync DCIM/Camera in place) needs "All files access",
        // and the mode is fixed when the photo folder is first set up, so
        // offer it before that happens.
        if (!SyncthingNode.isModeDecided(this) && Build.VERSION.SDK_INT >= Build.VERSION_CODES.R &&
            !MediaStoreBridge.hasAllFilesAccess() && !askedAllFiles
        ) {
            askedAllFiles = true
            AlertDialog.Builder(this)
                .setTitle("Sync your camera folder directly?")
                .setMessage("With “All files access”, Keepsake syncs DCIM/Camera in place with no extra copy. " +
                    "Without it, Keepsake keeps its own copy of your camera photos. You can’t change this later without reinstalling.")
                .setPositiveButton("Allow") { _, _ ->
                    startActivity(Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION, Uri.parse("package:$packageName")))
                }
                .setNegativeButton("Use a copy") { _, _ -> startNode() }
                .setCancelable(false)
                .show()
            return
        }
        held = true
        lifecycleScope.launch {
            val result = runCatching {
                withContext(Dispatchers.IO) {
                    SyncthingNode.init(applicationContext)
                    SyncthingNode.hold()
                }
            }
            result.onSuccess {
                SyncthingNode.dashboardUrl?.let { url -> if (web.url == null) web.loadUrl(url) }
                SyncWorker.schedule(applicationContext, wifiOnly = true, chargingOnly = false)
                // After the dashboard is up: the first pass may copy a lot.
                SyncthingNode.bridgeExecutor.execute { SyncthingNode.reconcile() }
                if (SyncthingService.isEnabled(applicationContext)) SyncthingService.setEnabled(applicationContext, true)
            }.onFailure { e ->
                held = false
                Log.e("Keepsake", "Failed to start the sync engine", e)
                showError(e)
            }
        }
    }

    private fun showError(e: Throwable) {
        AlertDialog.Builder(this)
            .setTitle("Keepsake couldn’t start")
            .setView(TextView(this).apply { text = e.message ?: e.toString(); setPadding(48, 24, 48, 0) })
            .setPositiveButton("Try again") { _, _ -> startNode() }
            .setNegativeButton("Close") { _, _ -> finish() }
            .show()
    }

    /** "Delete everywhere" on another device: Android 11+ only lets us
     *  delete photos other apps own with the user's consent. */
    private fun askDeleteConsent() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R || pendingDeleteIds.isNotEmpty()) return
        val ids = PendingDeletes.peek(this)
        if (ids.isEmpty()) return
        val uris = ids.map { ContentUris.withAppendedId(MediaStore.Files.getContentUri(MediaStore.VOLUME_EXTERNAL), it) }
        pendingDeleteIds = ids
        runCatching {
            askDelete.launch(IntentSenderRequest.Builder(MediaStore.createDeleteRequest(contentResolver, uris)).build())
        }.onFailure {
            // An item is already gone: nothing left to ask about.
            PendingDeletes.remove(this, ids)
            pendingDeleteIds = emptyList()
        }
    }

    /** Native settings the web dashboard shows when it runs in this app. */
    private inner class NativeBridge {
        @JavascriptInterface
        fun isKeepSyncing(): Boolean = SyncthingService.isEnabled(applicationContext)

        @JavascriptInterface
        fun setKeepSyncing(on: Boolean) = runOnUiThread { SyncthingService.setEnabled(applicationContext, on) }
    }
}
