// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Hosts the shared mobile dashboard (gui/default/mobile) in a WebView, so
// Android and iOS present the same UI.

package app.keepsake

import android.os.Bundle
import android.webkit.WebView
import androidx.activity.ComponentActivity
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class DashboardActivity : ComponentActivity() {
    private lateinit var web: WebView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        web = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
        }
        setContentView(web)

        lifecycleScope.launch {
            withContext(Dispatchers.IO) {
                SyncthingNode.init(applicationContext)
                SyncthingNode.start()
            }
            SyncthingNode.dashboardUrl?.let { web.loadUrl(it) }
            SyncWorker.schedule(applicationContext, wifiOnly = true, chargingOnly = false)
        }
    }
}
