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
import WebKit

struct DashboardView: View {
    @ObservedObject var service = SyncthingService.shared

    var body: some View {
        Group {
            if let url = service.dashboardURL {
                WebView(url: url).ignoresSafeArea(edges: .bottom)
            } else {
                ProgressView("Starting sync…")
            }
        }
        .task {
            if await PhotoLibraryBridge.requestAccess() {
                try? service.start()
                await service.library.reconcile()
            }
        }
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

    func updateUIView(_ view: WKWebView, context: Context) {}
}
