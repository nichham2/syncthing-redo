// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import "testing"

func TestStatusSettled(t *testing.T) {
	t.Parallel()

	idle := []FolderStatus{{ID: "photos", State: "idle"}}
	done := map[string]float64{"photos": 100}

	cases := []struct {
		name string
		st   Status
		want bool
	}{
		{"no peers", Status{Folders: idle}, false},
		{"peer offline", Status{Folders: idle, Peers: []PeerStatus{{Connected: false}}}, false},
		{"in sync", Status{Folders: idle, Peers: []PeerStatus{{Connected: true, Completion: done}}}, true},
		{"peer behind", Status{Folders: idle, Peers: []PeerStatus{{Connected: true, Completion: map[string]float64{"photos": 80}}}}, false},
		{"we are pulling", Status{Folders: []FolderStatus{{State: "syncing", NeedItems: 3}}, Peers: []PeerStatus{{Connected: true, Completion: done}}}, false},
		{"offline peer ignored", Status{Folders: idle, Peers: []PeerStatus{{Connected: true, Completion: done}, {Connected: false}}}, true},
	}
	for _, tc := range cases {
		if got := tc.st.Settled(); got != tc.want {
			t.Errorf("%s: Settled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
