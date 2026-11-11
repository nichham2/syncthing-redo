// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Optional "keep syncing" foreground service, for users who want
// near-real-time sync (e.g. while both phones are on the same Wi-Fi).
// Android 14+ requires the dataSync type; Android 15 caps dataSync
// services at 6 hours per day and calls onTimeout when the cap is hit.

package net.syncthing.photosync

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat

class SyncthingService : Service() {

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        ServiceCompat.startForeground(
            this, 1, notification(),
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC else 0,
        )
        Thread {
            SyncthingNode.init(this)
            runCatching { SyncthingNode.start() }.onFailure { stopSelf() }
        }.start()
        return START_STICKY
    }

    override fun onTimeout(startId: Int, fgsType: Int) {
        // Daily dataSync budget exhausted; WorkManager takes over.
        stopSelf()
    }

    override fun onDestroy() {
        Thread { SyncthingNode.stop() }.start()
        super.onDestroy()
    }

    private fun notification(): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel("sync", "Photo sync", NotificationManager.IMPORTANCE_LOW))
        return NotificationCompat.Builder(this, "sync")
            .setContentTitle("Syncing photos")
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setOngoing(true)
            .build()
    }
}
