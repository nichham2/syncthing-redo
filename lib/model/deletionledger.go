// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/itererr"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

// deletionLedger is the in-memory view of a folder's non-destructive
// deletion ledger, backed by the ignored_deletions table in the folder
// database. It answers "should this item stay away from this device?"
// quickly (by name, and by content hash so that a renamed copy of a
// deleted photo is not pulled back either), and writes through to the
// database on every change so that the ledger survives restarts.
//
// Names are kept in normalized form (NFC, forward slashes), the same way
// the database stores file names.
type deletionLedger struct {
	folder string
	sdb    db.DB

	mut    sync.RWMutex
	loaded bool
	byName map[string]db.IgnoredDeletion
	byHash map[string]int // string(blocklist hash) -> number of entries with that hash
}

func newDeletionLedger(folder string, sdb db.DB) *deletionLedger {
	return &deletionLedger{
		folder: folder,
		sdb:    sdb,
		byName: make(map[string]db.IgnoredDeletion),
		byHash: make(map[string]int),
	}
}

func ledgerKey(name string) string {
	return osutil.NormalizedFilename(name)
}

// load reads the ledger from the database, once.
func (l *deletionLedger) load() error {
	l.mut.RLock()
	loaded := l.loaded
	l.mut.RUnlock()
	if loaded {
		return nil
	}

	items, err := itererr.Collect(l.sdb.AllIgnoredDeletions(l.folder))
	if err != nil {
		return err
	}

	l.mut.Lock()
	defer l.mut.Unlock()
	if l.loaded {
		return nil
	}
	for _, it := range items {
		l.addLocked(it)
	}
	l.loaded = true
	return nil
}

func (l *deletionLedger) addLocked(it db.IgnoredDeletion) {
	key := ledgerKey(it.Name)
	if old, ok := l.byName[key]; ok {
		l.dropHashLocked(old.BlocklistHash)
	}
	it.Name = key
	l.byName[key] = it
	if len(it.BlocklistHash) > 0 {
		l.byHash[string(it.BlocklistHash)]++
	}
}

func (l *deletionLedger) dropHashLocked(hash []byte) {
	if len(hash) == 0 {
		return
	}
	h := string(hash)
	if l.byHash[h] <= 1 {
		delete(l.byHash, h)
	} else {
		l.byHash[h]--
	}
}

// Len returns the number of entries in the ledger.
func (l *deletionLedger) Len() int {
	if err := l.load(); err != nil {
		return 0
	}
	l.mut.RLock()
	defer l.mut.RUnlock()
	return len(l.byName)
}

// Has returns true if the exact name is in the ledger.
func (l *deletionLedger) Has(name string) bool {
	if err := l.load(); err != nil {
		return false
	}
	l.mut.RLock()
	defer l.mut.RUnlock()
	_, ok := l.byName[ledgerKey(name)]
	return ok
}

// Matches returns true if the given (remote) file should be kept away
// from this device: either its name is in the ledger, or it is a regular
// file with the same content as a file in the ledger. Errors loading the
// ledger fail safe, i.e. the file is considered a match and is not pulled.
func (l *deletionLedger) Matches(file protocol.FileInfo) bool {
	if err := l.load(); err != nil {
		return true
	}
	l.mut.RLock()
	defer l.mut.RUnlock()
	if len(l.byName) == 0 {
		return false
	}
	if _, ok := l.byName[ledgerKey(file.Name)]; ok {
		return true
	}
	if file.IsDirectory() || file.IsSymlink() || len(file.BlocksHash) == 0 {
		return false
	}
	return l.byHash[string(file.BlocksHash)] > 0
}

// Add records the given items in the database and in memory.
func (l *deletionLedger) Add(items ...db.IgnoredDeletion) error {
	if len(items) == 0 {
		return nil
	}
	if err := l.load(); err != nil {
		return err
	}
	for i := range items {
		items[i].Name = ledgerKey(items[i].Name)
	}
	if err := l.sdb.PutIgnoredDeletions(l.folder, items); err != nil {
		return err
	}
	l.mut.Lock()
	defer l.mut.Unlock()
	for _, it := range items {
		l.addLocked(it)
	}
	return nil
}

// Remove deletes the named entries, if present.
func (l *deletionLedger) Remove(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if err := l.load(); err != nil {
		return err
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = ledgerKey(n)
	}
	if err := l.sdb.DeleteIgnoredDeletions(l.folder, keys); err != nil {
		return err
	}
	l.mut.Lock()
	defer l.mut.Unlock()
	for _, k := range keys {
		if old, ok := l.byName[k]; ok {
			l.dropHashLocked(old.BlocklistHash)
			delete(l.byName, k)
		}
	}
	return nil
}

// All returns all entries, sorted by name.
func (l *deletionLedger) All() ([]db.IgnoredDeletion, error) {
	if err := l.load(); err != nil {
		return nil, err
	}
	l.mut.RLock()
	defer l.mut.RUnlock()
	res := make([]db.IgnoredDeletion, 0, len(l.byName))
	for _, it := range l.byName {
		res = append(res, it)
	}
	sort.Slice(res, func(a, b int) bool { return res[a].Name < res[b].Name })
	return res, nil
}

// Expand returns the given names plus every ledger entry below any of
// them (so restoring an album restores its photos) and every ledger entry
// above them (so the parent directories come back too). The result is
// sorted and de-duplicated, and only contains names that are in the
// ledger.
func (l *deletionLedger) Expand(names []string) ([]string, error) {
	if err := l.load(); err != nil {
		return nil, err
	}
	l.mut.RLock()
	defer l.mut.RUnlock()

	want := make(map[string]struct{})
	for _, n := range names {
		key := ledgerKey(n)
		if key == "" {
			continue
		}
		if _, ok := l.byName[key]; ok {
			want[key] = struct{}{}
		}
		// Ledger keys use native separators (see ledgerKey).
		prefix := key + string(os.PathSeparator)
		for name := range l.byName {
			if strings.HasPrefix(name, prefix) {
				want[name] = struct{}{}
			}
		}
		for parent := parentOf(key); parent != ""; parent = parentOf(parent) {
			if _, ok := l.byName[parent]; ok {
				want[parent] = struct{}{}
			}
		}
	}

	res := make([]string, 0, len(want))
	for n := range want {
		res = append(res, n)
	}
	sort.Strings(res)
	return res, nil
}

func parentOf(name string) string {
	idx := strings.LastIndexByte(name, os.PathSeparator)
	if idx <= 0 {
		return ""
	}
	return name[:idx]
}
