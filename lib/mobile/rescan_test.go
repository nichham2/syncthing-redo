// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package mobile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
)

func TestSplitPaths(t *testing.T) {
	t.Parallel()
	if got := splitPaths(""); !slices.Equal(got, []string{""}) {
		t.Error("empty list should scan the whole folder, got", got)
	}
	got := splitPaths("2026/01/a.jpg\n2026/01/b.jpg\n2026/01/a.jpg")
	if !slices.Equal(got, []string{"2026/01/a.jpg", "2026/01/b.jpg"}) {
		t.Error("unexpected paths", got)
	}
}

func TestRescanPathsBatches(t *testing.T) {
	t.Parallel()
	myID := protocol.DeviceID{1, 2, 3}
	var mut sync.Mutex
	var calls [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Syncthing-Id", myID.String())
		if r.URL.Path != "/rest/db/scan" || r.URL.Query().Get("folder") != "photos" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		mut.Lock()
		calls = append(calls, r.URL.Query()["sub"])
		mut.Unlock()
	}))
	defer srv.Close()
	api := newAPIClient(srv.URL, "key", myID)

	var paths []string
	for i := range 250 {
		paths = append(paths, fmt.Sprintf("2026/01/IMG_%04d.HEIC", i))
	}
	if err := rescanPaths(context.Background(), api, "photos", splitPaths(strings.Join(paths, "\n"))); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || len(calls[0]) != rescanBatch || len(calls[2]) != 50 {
		t.Fatalf("expected 3 batched scans, got %d", len(calls))
	}

	// Very many paths: one scan of the whole folder.
	calls = nil
	for i := range rescanWholeFolder {
		paths = append(paths, fmt.Sprintf("2027/01/IMG_%04d.HEIC", i))
	}
	if err := rescanPaths(context.Background(), api, "photos", paths); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(calls[0]) != 0 {
		t.Errorf("expected one whole-folder scan, got %v", calls)
	}
}
