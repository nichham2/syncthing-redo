// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
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

func startTestNode(t *testing.T) *Node {
	t.Helper()
	if testing.Short() {
		t.Skip("starts a full Syncthing instance")
	}
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
	if err := n.Hold(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

// Other apps on the phone share the loopback interface. Loading the GUI
// and replaying its CSRF cookie must not give them the REST API; only the
// API key does.
func TestNodeRESTNeedsAPIKey(t *testing.T) {
	n := startTestNode(t)
	cfg, _ := n.config()
	base := strings.TrimSuffix(cfg.GUI().URL(), "/")
	short := cfg.MyID().Short().String()

	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := cl.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(base)
	var token string
	for _, c := range jar.Cookies(u) {
		if c.Name == "CSRF-Token-"+short {
			token = c.Value
		}
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/rest/config", nil)
	if token != "" {
		req.Header.Set("X-CSRF-Token-"+short, token)
	}
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatal("REST API reachable without the API key:", resp.Status)
	}

	// The dashboard's static files load without auth; data needs the key.
	resp, err = cl.Get(base + "/mobile/mobile.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("dashboard static files need auth:", resp.Status)
	}
	if _, err := n.StatusJSON(); err != nil {
		t.Error("bridge can't use the API with its key:", err)
	}
}

func TestNodeHoldUnhold(t *testing.T) {
	n := startTestNode(t) // holds once
	if err := n.Hold(); err != nil {
		t.Fatal(err)
	}
	n.Unhold()
	if !n.IsRunning() {
		t.Fatal("stopped while another holder remains")
	}
	if _, err := n.RunBackgroundSync(1); err != nil {
		t.Fatal(err)
	}
	if !n.IsRunning() {
		t.Fatal("background sync stopped a node the app holds")
	}
	n.Unhold()
	if n.IsRunning() {
		t.Fatal("still running after the last Unhold")
	}
	if _, err := n.config(); err == nil {
		t.Error("config available after Stop")
	}
}

func TestEnsurePhotoFolderKeepsUserChoicesAndIgnores(t *testing.T) {
	n := startTestNode(t)
	cfg, _ := n.config()
	photos := filepath.Join(n.homeDir, "PhotosIgn")
	if err := n.EnsurePhotoFolder("photos", "Camera", photos); err != nil {
		t.Fatal(err)
	}
	// The user turns non-destructive deletes off; a restart keeps it off.
	w, err := cfg.Modify(func(c *config.Configuration) {
		f, i, _ := c.Folder("photos")
		f.NonDestructiveDeletes = false
		c.Folders[i] = f
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Wait()
	if err := n.EnsurePhotoFolder("photos", "Camera", photos); err != nil {
		t.Fatal(err)
	}
	if f, _ := cfg.Folder("photos"); f.NonDestructiveDeletes {
		t.Error("EnsurePhotoFolder overrode the user's choice")
	}

	bs, err := os.ReadFile(filepath.Join(photos, ".stignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range photoIgnores {
		if !strings.Contains(string(bs), p) {
			t.Errorf("ignore pattern %q missing from %q", p, bs)
		}
	}
}
