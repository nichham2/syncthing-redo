// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

// Status is the snapshot handed to the native app (as JSON) for
// notifications, widgets and deciding when a background window can end.
type Status struct {
	DeviceID string         `json:"deviceID"`
	Folders  []FolderStatus `json:"folders"`
	Peers    []PeerStatus   `json:"peers"`
	At       time.Time      `json:"at"`
}

type FolderStatus struct {
	ID               string    `json:"id"`
	Label            string    `json:"label"`
	State            string    `json:"state"` // idle, scanning, syncing, error, ...
	NeedItems        int       `json:"needItems"`
	NeedBytes        int64     `json:"needBytes"`
	LocalFiles       int       `json:"localFiles"`
	IgnoredDeletions int       `json:"ignoredDeletions"`
	LastScan         time.Time `json:"lastScan"`
	LastSynced       time.Time `json:"lastSynced"`
	Error            string    `json:"error,omitempty"`
}

type PeerStatus struct {
	DeviceID   string             `json:"deviceID"`
	Name       string             `json:"name"`
	Connected  bool               `json:"connected"`
	Paused     bool               `json:"paused"`
	Address    string             `json:"address,omitempty"`
	Completion map[string]float64 `json:"completion"` // folder ID -> percent the peer has
}

// Settled is true when there is at least one connected peer, every folder
// is idle with nothing left to pull, and every connected peer is complete
// for every folder. Background windows can end then.
func (s *Status) Settled() bool {
	connected := 0
	for _, p := range s.Peers {
		if !p.Connected {
			continue
		}
		connected++
		for _, c := range p.Completion {
			if c < 100 {
				return false
			}
		}
	}
	if connected == 0 {
		return false
	}
	for _, f := range s.Folders {
		if f.State != "idle" || f.NeedItems > 0 {
			return false
		}
	}
	return true
}

func (n *Node) status() (*Status, error) {
	api, err := n.client()
	if err != nil {
		return nil, err
	}
	cfg, err := n.config()
	if err != nil {
		return nil, err
	}
	raw := cfg.RawCopy()

	st := &Status{DeviceID: n.DeviceID(), At: time.Now()}

	var conns struct {
		Connections map[string]struct {
			Connected bool   `json:"connected"`
			Paused    bool   `json:"paused"`
			Address   string `json:"address"`
		} `json:"connections"`
	}
	if err := api.get("/rest/system/connections", nil, &conns); err != nil {
		return nil, err
	}

	var folderStats map[string]struct {
		LastScan time.Time `json:"lastScan"`
		LastFile struct {
			At time.Time `json:"at"`
		} `json:"lastFile"`
	}
	_ = api.get("/rest/stats/folder", nil, &folderStats)

	for _, fcfg := range raw.Folders {
		fs := FolderStatus{ID: fcfg.ID, Label: fcfg.Label}
		var dbs struct {
			State      string `json:"state"`
			Error      string `json:"error"`
			NeedTotal  int    `json:"needTotalItems"`
			NeedBytes  int64  `json:"needBytes"`
			LocalFiles int    `json:"localFiles"`
		}
		if fcfg.Paused {
			fs.State = "paused"
		} else if err := api.get("/rest/db/status", map[string]string{"folder": fcfg.ID}, &dbs); err == nil {
			fs.State, fs.Error = dbs.State, dbs.Error
			fs.NeedItems, fs.NeedBytes, fs.LocalFiles = dbs.NeedTotal, dbs.NeedBytes, dbs.LocalFiles
		} else {
			fs.State, fs.Error = "error", err.Error()
		}
		if fcfg.NonDestructiveDeletes && !fcfg.Paused {
			var ign struct {
				Items []struct{} `json:"items"`
			}
			if err := api.get("/rest/folder/ignoreddeletions", map[string]string{"folder": fcfg.ID}, &ign); err == nil {
				fs.IgnoredDeletions = len(ign.Items)
			}
		}
		if s, ok := folderStats[fcfg.ID]; ok {
			fs.LastScan = s.LastScan
			fs.LastSynced = s.LastFile.At
		}
		st.Folders = append(st.Folders, fs)
	}

	myID := cfg.MyID()
	for _, dcfg := range raw.Devices {
		if dcfg.DeviceID == myID {
			continue
		}
		ps := PeerStatus{
			DeviceID:   dcfg.DeviceID.String(),
			Name:       dcfg.Name,
			Paused:     dcfg.Paused,
			Completion: map[string]float64{},
		}
		if c, ok := conns.Connections[ps.DeviceID]; ok {
			ps.Connected, ps.Address = c.Connected, c.Address
		}
		if ps.Connected {
			for _, fcfg := range raw.Folders {
				if !sharedWith(fcfg, dcfg) {
					continue
				}
				var comp struct {
					Completion float64 `json:"completion"`
				}
				if err := api.get("/rest/db/completion", map[string]string{"folder": fcfg.ID, "device": ps.DeviceID}, &comp); err == nil {
					ps.Completion[fcfg.ID] = comp.Completion
				}
			}
		}
		st.Peers = append(st.Peers, ps)
	}
	return st, nil
}

func sharedWith(f config.FolderConfiguration, d config.DeviceConfiguration) bool {
	return f.SharedWith(d.DeviceID)
}
