// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/model"
	modelmocks "github.com/syncthing/syncthing/lib/model/mocks"
	"github.com/syncthing/syncthing/lib/protocol"
)

func TestIgnoredDeletionsEndpoints(t *testing.T) {
	t.Parallel()

	m := new(modelmocks.Model)
	m.IgnoredDeletionsReturns([]model.IgnoredDeletion{
		{Name: "DCIM/IMG_0001.HEIC", Type: "file", Size: 1234, AvailableOn: []protocol.DeviceID{{1}}},
	}, nil)
	svc := &service{model: m}

	// List
	rec := httptest.NewRecorder()
	svc.getFolderIgnoredDeletions(rec, httptest.NewRequest(http.MethodGet, "/rest/folder/ignoreddeletions?folder=photos", nil))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var res struct {
		Folder string `json:"folder"`
		Items  []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Folder != "photos" || len(res.Items) != 1 || res.Items[0].Name != "DCIM/IMG_0001.HEIC" || res.Items[0].Size != 1234 {
		t.Error("unexpected response", rec.Body.String())
	}
	if got := m.IgnoredDeletionsArgsForCall(0); got != "photos" {
		t.Error("wrong folder", got)
	}

	// Restore
	rec = httptest.NewRecorder()
	body := strings.NewReader(`{"names": ["DCIM/IMG_0001.HEIC"]}`)
	svc.postFolderIgnoredDeletionsRestore(rec, httptest.NewRequest(http.MethodPost, "/rest/folder/ignoreddeletions/restore?folder=photos", body))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	folder, names := m.RestoreIgnoredDeletionsArgsForCall(0)
	if folder != "photos" || !slices.Equal(names, []string{"DCIM/IMG_0001.HEIC"}) {
		t.Error("restore called with", folder, names)
	}

	// Propagate, with an unknown folder
	m.PropagateIgnoredDeletionsReturns(model.ErrFolderMissing)
	rec = httptest.NewRecorder()
	body = strings.NewReader(`{"names": ["x"]}`)
	svc.postFolderIgnoredDeletionsPropagate(rec, httptest.NewRequest(http.MethodPost, "/rest/folder/ignoreddeletions/propagate?folder=nope", body))
	if rec.Code != http.StatusNotFound {
		t.Error("expected 404, got", rec.Code)
	}

	// Empty request
	rec = httptest.NewRecorder()
	svc.postFolderIgnoredDeletionsRestore(rec, httptest.NewRequest(http.MethodPost, "/rest/folder/ignoreddeletions/restore?folder=photos", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Error("expected 400, got", rec.Code)
	}
}
