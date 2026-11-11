// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNodeLifecycle starts a real embedded node, configures a photo folder
// and reads the status back through the bridge.
func TestNodeLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a full Syncthing instance")
	}
	// Not t.TempDir(): background services may still be flushing files
	// for a moment after Stop returns, so clean up on a best-effort basis.
	home, err := os.MkdirTemp("", "stmobile")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 50 {
			if os.RemoveAll(home) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	n, err := NewNode(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer n.Stop()

	if !n.IsRunning() || len(n.DeviceID()) < 50 {
		t.Fatal("node not running or no device ID", n.DeviceID())
	}
	if u := n.DashboardURL(); !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, "/mobile/#apikey=") {
		t.Error("unexpected dashboard URL", u)
	}

	photos := filepath.Join(home, "Photos")
	if err := n.EnsurePhotoFolder("photos", "Camera Roll", photos); err != nil {
		t.Fatal(err)
	}
	// Idempotent
	if err := n.EnsurePhotoFolder("photos", "Camera Roll", photos); err != nil {
		t.Fatal(err)
	}
	const peer = "MFZWI3D-BONSGYC-YLTMRWG-C43ENR5-QXGZDMM-FZWI3DP-BONSGYY-LTMRWAD"
	if err := n.AddPeer(peer, "Pixel"); err != nil {
		t.Fatal(err)
	}

	cfg, _ := n.config()
	fcfg, ok := cfg.Folder("photos")
	if !ok || !fcfg.NonDestructiveDeletes || !fcfg.IgnorePerms || len(fcfg.Devices) != 2 {
		t.Fatalf("unexpected folder config %+v", fcfg)
	}

	js, err := n.StatusJSON()
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Folders) != 1 || st.Folders[0].ID != "photos" {
		t.Error("unexpected folders in status", js)
	}
	if len(st.Peers) != 1 || st.Peers[0].Name != "Pixel" || st.Peers[0].Connected {
		t.Error("unexpected peers in status", js)
	}
	if err := n.Rescan("photos", ""); err != nil {
		t.Error(err)
	}

	n.Stop()
	if n.IsRunning() {
		t.Error("still running after Stop")
	}
}
