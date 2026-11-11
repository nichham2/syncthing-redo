// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/syncthing/syncthing/lib/model"
)

// Endpoints for the non-destructive deletion ledger ("ignored files").
//
//	GET  /rest/folder/ignoreddeletions?folder=ID
//	POST /rest/folder/ignoreddeletions/restore?folder=ID    {"names": [...]}
//	POST /rest/folder/ignoreddeletions/propagate?folder=ID  {"names": [...]}

type ignoredDeletionsRequest struct {
	Names []string `json:"names"`
}

func (s *service) getFolderIgnoredDeletions(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	items, err := s.model.IgnoredDeletions(folder)
	if err != nil {
		http.Error(w, err.Error(), ignoredDeletionsErrorStatus(err))
		return
	}
	if items == nil {
		items = []model.IgnoredDeletion{}
	}
	sendJSON(w, map[string]any{
		"folder": folder,
		"items":  items,
	})
}

func (s *service) postFolderIgnoredDeletionsRestore(w http.ResponseWriter, r *http.Request) {
	s.handleIgnoredDeletionsAction(w, r, s.model.RestoreIgnoredDeletions)
}

func (s *service) postFolderIgnoredDeletionsPropagate(w http.ResponseWriter, r *http.Request) {
	s.handleIgnoredDeletionsAction(w, r, s.model.PropagateIgnoredDeletions)
}

func (*service) handleIgnoredDeletionsAction(w http.ResponseWriter, r *http.Request, action func(folder string, names []string) error) {
	folder := r.URL.Query().Get("folder")

	bs, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var req ignoredDeletionsRequest
	if err := json.Unmarshal(bs, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Names) == 0 {
		http.Error(w, "no names given", http.StatusBadRequest)
		return
	}

	if err := action(folder, req.Names); err != nil {
		http.Error(w, err.Error(), ignoredDeletionsErrorStatus(err))
		return
	}
	sendJSON(w, map[string]any{"folder": folder, "count": len(req.Names)})
}

func ignoredDeletionsErrorStatus(err error) int {
	switch {
	case errors.Is(err, model.ErrFolderMissing):
		return http.StatusNotFound
	case errors.Is(err, model.ErrFolderPaused), errors.Is(err, model.ErrFolderNotRunning):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
