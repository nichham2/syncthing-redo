// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package mobile is the bridge between the Syncthing core and the native
// iOS and Android apps. It is built with gomobile:
//
//	gomobile bind -target=ios     -o Syncthing.xcframework ./lib/mobile
//	gomobile bind -target=android -o syncthing.aar         ./lib/mobile
//
// The exported API only uses types gomobile can bridge (strings, bools,
// int64s, errors and callback interfaces). Everything richer is passed as
// JSON. The native app owns the photo library (Apple Photos / Android
// MediaStore) and the background scheduling (BGTaskScheduler /
// WorkManager); this package owns syncing a plain folder of photo files
// between devices, with non-destructive deletes enabled.
package mobile

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// deleteRetention matches the desktop default: deleted-file records are
// kept for about 15 months so that offline devices still learn about
// deletions.
const deleteRetention = 10920 * time.Hour

// Platform is implemented by the native app. Calls arrive on a Go
// goroutine; implementations must hop to the main thread themselves
// before touching UI.
type Platform interface {
	// OnRemoteChange is called after an item was written to (action
	// "update") or removed from (action "delete") the synced folder
	// because of a change on another device. The native side imports new
	// photos into the photo library. relPath uses forward slashes.
	OnRemoteChange(folderID, relPath, action string)

	// OnStateChanged is called with a JSON status snapshot (see Status)
	// whenever a folder or connection changes state.
	OnStateChanged(statusJSON string)
}

// Node is one embedded Syncthing instance.
type Node struct {
	homeDir string

	mut      sync.Mutex
	platform Platform
	cancel   context.CancelFunc
	app      *syncthing.App
	cfg      config.Wrapper
	evLogger events.Logger
	cert     tls.Certificate
	api      *apiClient
}

// NewNode prepares a node that keeps its configuration, keys and database
// in homeDir (an app-private directory: the app container's Library
// directory on iOS, Context.getFilesDir() on Android).
func NewNode(homeDir string) (*Node, error) {
	if homeDir == "" {
		return nil, errors.New("homeDir must be set")
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return nil, err
	}
	return &Node{homeDir: homeDir}, nil
}

// SetPlatform registers the native callbacks. May be called at any time.
func (n *Node) SetPlatform(p Platform) {
	n.mut.Lock()
	n.platform = p
	n.mut.Unlock()
}

func (n *Node) getPlatform() Platform {
	n.mut.Lock()
	defer n.mut.Unlock()
	return n.platform
}

// IsRunning returns true between a successful Start and Stop.
func (n *Node) IsRunning() bool {
	n.mut.Lock()
	defer n.mut.Unlock()
	return n.app != nil
}

// Start starts Syncthing. It returns once the GUI/REST API is listening.
func (n *Node) Start() error {
	n.mut.Lock()
	defer n.mut.Unlock()
	if n.app != nil {
		return nil
	}

	if err := locations.SetBaseDir(locations.ConfigBaseDir, n.homeDir); err != nil {
		return err
	}
	if err := locations.SetBaseDir(locations.DataBaseDir, n.homeDir); err != nil {
		return err
	}

	cert, err := syncthing.LoadOrGenerateCertificate(locations.Get(locations.CertFile), locations.Get(locations.KeyFile))
	if err != nil {
		return fmt.Errorf("certificate: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	early := suture.New("mobile-early", svcutil.SpecWithDebugLogger())
	early.ServeBackground(ctx)

	evLogger := events.NewLogger()
	early.Add(evLogger)

	cfg, err := syncthing.LoadConfigAtStartup(locations.Get(locations.ConfigFile), cert, evLogger, false, false)
	if err != nil {
		cancel()
		return fmt.Errorf("config: %w", err)
	}
	early.Add(cfg)

	if err := applyMobileDefaults(cfg); err != nil {
		cancel()
		return fmt.Errorf("config: %w", err)
	}

	if err := syncthing.TryMigrateDatabase(ctx, deleteRetention); err != nil {
		cancel()
		return fmt.Errorf("database migration: %w", err)
	}
	sdb, err := syncthing.OpenDatabase(locations.Get(locations.Database), deleteRetention)
	if err != nil {
		cancel()
		return fmt.Errorf("database: %w", err)
	}

	app, err := syncthing.New(cfg, sdb, evLogger, cert, syncthing.Options{
		NoUpgrade:             true, // app stores own upgrades
		DBMaintenanceInterval: 8 * time.Hour,
	})
	if err != nil {
		cancel()
		_ = sdb.Close()
		return err
	}
	if err := app.Start(); err != nil {
		cancel()
		return err
	}

	n.app = app
	n.cfg = cfg
	n.cancel = cancel
	n.evLogger = evLogger
	n.cert = cert
	n.api = newAPIClient(cfg.GUI().URL(), cfg.GUI().APIKey)

	go n.forwardEvents(ctx, evLogger)
	return nil
}

// Stop stops Syncthing and waits for it to shut down. Call it when the OS
// is about to suspend the app, so the database is closed cleanly.
func (n *Node) Stop() {
	n.mut.Lock()
	app, cancel := n.app, n.cancel
	n.app, n.cancel, n.api = nil, nil, nil
	n.mut.Unlock()
	if app == nil {
		return
	}
	app.Stop(svcutil.ExitSuccess)
	app.Wait()
	cancel()
}

// DeviceID returns this device's ID, for pairing (shown as a QR code).
func (n *Node) DeviceID() string {
	n.mut.Lock()
	defer n.mut.Unlock()
	if n.app == nil {
		return ""
	}
	return protocol.NewDeviceID(n.cert.Certificate[0]).String()
}

// DashboardURL is the URL the native app loads in its WebView to show the
// mobile dashboard. The API key is passed in the fragment, which never
// leaves the device.
func (n *Node) DashboardURL() string {
	n.mut.Lock()
	defer n.mut.Unlock()
	if n.cfg == nil {
		return ""
	}
	gui := n.cfg.GUI()
	return strings.TrimSuffix(gui.URL(), "/") + "/mobile/#apikey=" + gui.APIKey
}

// EnsurePhotoFolder creates or updates the photo folder: send & receive,
// non-destructive deletes, permissions ignored (mobile file systems do not
// carry meaningful POSIX permissions) and a short rescan interval as a
// backstop for the file system watcher.
func (n *Node) EnsurePhotoFolder(folderID, label, path string) error {
	cfg, err := n.config()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	myID := cfg.MyID()
	waiter, err := cfg.Modify(func(c *config.Configuration) {
		fcfg, idx, ok := c.Folder(folderID)
		if !ok {
			fcfg = c.Defaults.Folder.Copy()
			fcfg.ID = folderID
			fcfg.Devices = []config.FolderDeviceConfiguration{{DeviceID: myID}}
		}
		fcfg.Label = label
		fcfg.Path = path
		fcfg.Type = config.FolderTypeSendReceive
		fcfg.NonDestructiveDeletes = true
		fcfg.IgnorePerms = true
		fcfg.FSWatcherEnabled = true
		fcfg.RescanIntervalS = 3600
		fcfg.MarkerName = config.DefaultMarkerName
		if ok {
			c.Folders[idx] = fcfg
		} else {
			c.Folders = append(c.Folders, fcfg)
		}
	})
	if err != nil {
		return err
	}
	waiter.Wait()
	return cfg.Save()
}

// AddPeer adds (or renames) a device and shares every photo folder with it.
func (n *Node) AddPeer(deviceID, name string) error {
	id, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}
	cfg, err := n.config()
	if err != nil {
		return err
	}
	waiter, err := cfg.Modify(func(c *config.Configuration) {
		dev, idx, ok := c.Device(id)
		if !ok {
			dev = c.Defaults.Device.Copy()
			dev.DeviceID = id
		}
		dev.Name = name
		if ok {
			c.Devices[idx] = dev
		} else {
			c.Devices = append(c.Devices, dev)
		}
		for i := range c.Folders {
			if !c.Folders[i].NonDestructiveDeletes || c.Folders[i].SharedWith(id) {
				continue
			}
			c.Folders[i].Devices = append(c.Folders[i].Devices, config.FolderDeviceConfiguration{DeviceID: id})
		}
	})
	if err != nil {
		return err
	}
	waiter.Wait()
	return cfg.Save()
}

// Rescan asks Syncthing to look at a path (relative to the folder root,
// "" for everything) now. The native side calls it after exporting new
// photos from, or removing deleted photos out of, the photo library.
func (n *Node) Rescan(folderID, relPath string) error {
	api, err := n.client()
	if err != nil {
		return err
	}
	q := map[string]string{"folder": folderID}
	if relPath != "" {
		q["sub"] = relPath
	}
	return api.post(context.Background(), "/rest/db/scan", q, nil, nil)
}

// SetPaused pauses or resumes all device connections, e.g. when the user
// restricts syncing to Wi-Fi and the phone is on cellular.
func (n *Node) SetPaused(paused bool) error {
	api, err := n.client()
	if err != nil {
		return err
	}
	if paused {
		return api.post(context.Background(), "/rest/system/pause", nil, nil, nil)
	}
	return api.post(context.Background(), "/rest/system/resume", nil, nil, nil)
}

// StatusJSON returns the current Status as JSON.
func (n *Node) StatusJSON() (string, error) {
	return n.statusJSON(context.Background())
}

func (n *Node) statusJSON(ctx context.Context) (string, error) {
	st, err := n.status(ctx)
	if err != nil {
		return "", err
	}
	bs, err := json.Marshal(st)
	return string(bs), err
}

// RunBackgroundSync is the entry point for OS background windows
// (iOS BGProcessingTask / BGAppRefreshTask, Android WorkManager). It
// starts the node if needed, waits until every shared folder is in sync
// with every connected peer (or the time budget runs out), then stops the
// node again if it was not running before. It returns the final Status as
// JSON. maxSeconds should leave headroom before the OS deadline.
func (n *Node) RunBackgroundSync(maxSeconds int64) (string, error) {
	wasRunning := n.IsRunning()
	if !wasRunning {
		if err := n.Start(); err != nil {
			return "", err
		}
		defer n.Stop()
	}

	deadline := time.Now().Add(time.Duration(maxSeconds) * time.Second)
	// Give discovery and connections a moment before judging "settled".
	const minWait = 10 * time.Second
	started := time.Now()
	settledSince := time.Time{}

	var last *Status
	for time.Now().Before(deadline) {
		st, err := n.status(context.Background())
		if err == nil {
			last = st
			if st.Settled() && time.Since(started) > minWait {
				if settledSince.IsZero() {
					settledSince = time.Now()
				} else if time.Since(settledSince) > 3*time.Second {
					break
				}
			} else {
				settledSince = time.Time{}
			}
		}
		time.Sleep(time.Second)
	}

	if last == nil {
		return "{}", nil
	}
	bs, err := json.Marshal(last)
	return string(bs), err
}

func (n *Node) config() (config.Wrapper, error) {
	n.mut.Lock()
	defer n.mut.Unlock()
	if n.cfg == nil {
		return nil, errors.New("node is not running")
	}
	return n.cfg, nil
}

func (n *Node) client() (*apiClient, error) {
	n.mut.Lock()
	defer n.mut.Unlock()
	if n.api == nil {
		return nil, errors.New("node is not running")
	}
	return n.api, nil
}

// applyMobileDefaults adjusts a configuration for running inside a phone
// app: loopback-only GUI with an API key, no self-upgrades, no usage
// reporting prompt, no browser to open.
func applyMobileDefaults(cfg config.Wrapper) error {
	waiter, err := cfg.Modify(func(c *config.Configuration) {
		host, port, err := net.SplitHostPort(c.GUI.RawAddress)
		if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			if port == "" {
				port = "8384"
			}
			c.GUI.RawAddress = net.JoinHostPort("127.0.0.1", port)
		}
		c.GUI.Enabled = true
		c.GUI.RawUseTLS = false // loopback only
		c.Options.StartBrowser = false
		c.Options.AutoUpgradeIntervalH = 0
		if c.Options.URAccepted == 0 {
			c.Options.URAccepted = -1
		}
		// Phones change networks constantly; keep relays and global
		// discovery on so peers find each other off the home Wi-Fi.
		c.Options.RelaysEnabled = true
		c.Options.GlobalAnnEnabled = true
		c.Options.LocalAnnEnabled = true
	})
	if err != nil {
		return err
	}
	waiter.Wait()
	return cfg.Save()
}

// forwardEvents relays core events to the native side.
func (n *Node) forwardEvents(ctx context.Context, evLogger events.Logger) {
	sub := evLogger.Subscribe(events.ItemFinished | events.StateChanged | events.DeviceConnected | events.DeviceDisconnected)
	defer sub.Unsubscribe()

	var lastStatePush time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			p := n.getPlatform()
			if p == nil {
				continue
			}
			switch ev.Type {
			case events.ItemFinished:
				data, _ := ev.Data.(map[string]any)
				if data == nil || data["error"] != nil || data["type"] != "file" {
					continue
				}
				folder, _ := data["folder"].(string)
				item, _ := data["item"].(string)
				action, _ := data["action"].(string)
				p.OnRemoteChange(folder, filepath.ToSlash(item), action)
			default:
				// Coalesce bursts of state changes.
				if time.Since(lastStatePush) < 500*time.Millisecond {
					continue
				}
				lastStatePush = time.Now()
				if js, err := n.statusJSON(ctx); err == nil {
					p.OnStateChanged(js)
				} else {
					slog.Debug("Mobile status", "error", err)
				}
			}
		}
	}
}
