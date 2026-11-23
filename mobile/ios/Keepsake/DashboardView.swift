// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//
// Hosts the shared mobile dashboard (gui/default/mobile) in a WKWebView,
// so iOS and Android present the same UI. The node's REST API is only
// reachable on loopback; the API key travels in the URL fragment.

import SwiftUI
import UIKit
import WebKit

struct DashboardView: View {
    @ObservedObject var service = SyncthingService.shared

    var body: some View {
        Group {
            switch service.state {
            case .running(let url):
                WebView(url: url).ignoresSafeArea(edges: .bottom)
            case .starting:
                ProgressView("Starting Keepsake…")
            case .needsPhotoAccess:
                VStack(spacing: 16) {
                    Text("Keepsake needs access to your photos to sync them.")
                        .multilineTextAlignment(.center)
                    Button("Open Settings") {
                        if let url = URL(string: UIApplication.openSettingsURLString) {
                            UIApplication.shared.open(url)
                        }
                    }
                }
                .padding()
            case .failed(let message):
                VStack(spacing: 16) {
                    Text("Keepsake couldn’t start").font(.headline)
                    Text(message).font(.footnote).multilineTextAlignment(.center)
                    Button("Try again") { Task { await service.enterForeground() } }
                }
                .padding()
            }
        }
        // scenePhase changes don't fire for the initial launch.
        .task { await service.enterForeground() }
    }
}

private struct WebView: UIViewRepresentable {
    let url: URL

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        let view = WKWebView(frame: .zero, configuration: config)
        view.isOpaque = false
        view.backgroundColor = .systemBackground
        view.scrollView.contentInsetAdjustmentBehavior = .always
        view.load(URLRequest(url: url))
        return view
    }

    func updateUIView(_ view: WKWebView, context: Context) {
        // The engine can come back on another port (it moves off a port
        // that another app holds); follow it.
        if view.url?.port != url.port {
            view.load(URLRequest(url: url))
        }
    }
}
