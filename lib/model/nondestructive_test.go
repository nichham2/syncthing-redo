// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

// waitFor polls cond until it returns true or the timeout expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for", what)
}

// sentIndex records the latest index entry we announced to the fake
// remote, per file name.
type sentIndex struct {
	mut   sync.Mutex
	files map[string]protocol.FileInfo
}

func (s *sentIndex) fn(_ context.Context, _ string, fs []protocol.FileInfo) error {
	s.mut.Lock()
	defer s.mut.Unlock()
	for _, f := range fs {
		s.files[f.Name] = f
	}
	return nil
}

func (s *sentIndex) get(name string) (protocol.FileInfo, bool) {
	s.mut.Lock()
	defer s.mut.Unlock()
	f, ok := s.files[name]
	return f, ok
}

func setupNonDestructive(t *testing.T) (*testModel, *fakeConnection, fs.Filesystem, *sentIndex) {
	t.Helper()
	w, fcfg := newDefaultCfgWrapper(t)
	fcfg.NonDestructiveDeletes = true
	setFolder(t, w, fcfg)
	m, fc := setupModelWithConnectionFromWrapper(t, w)
	t.Cleanup(func() { cleanupModel(m) })

	sent := &sentIndex{files: make(map[string]protocol.FileInfo)}
	fc.setIndexFn(sent.fn)
	return m, fc, fcfg.Filesystem(), sent
}

func syncFromRemote(t *testing.T, m *testModel, fc *fakeConnection, tfs fs.Filesystem, name string, data []byte) {
	t.Helper()
	fc.addFile(name, 0o644, protocol.FileInfoTypeFile, data)
	fc.sendIndexUpdate()
	waitFor(t, name+" to be pulled", func() bool {
		return equalContents(tfs, name, data) == nil
	})
	waitFor(t, name+" to be in the local db", func() bool {
		f, ok := m.testCurrentFolderFile("default", name)
		return ok && !f.IsDeleted() && !f.IsInvalid()
	})
}

func TestNonDestructiveDeleteKeepsRemoteCopy(t *testing.T) {
	m, fc, tfs, sent := setupNonDestructive(t)

	const name = "IMG_0001.jpg"
	photo := []byte("pretend this is a jpeg with exif data")
	syncFromRemote(t, m, fc, tfs, name, photo)
	before, _ := m.testCurrentFolderFile("default", name)

	// The user deletes the photo on this device.
	must(t, tfs.Remove(name))
	must(t, m.ScanFolder("default"))

	// The local entry is delete-ignored, with the version unchanged, so
	// it does not dominate (and delete) the remote copy.
	cur, ok := m.testCurrentFolderFile("default", name)
	if !ok {
		t.Fatal("local entry vanished")
	}
	if !cur.IsDeleteIgnored() || cur.IsDeleted() {
		t.Fatal("expected a delete-ignored, non-deleted entry, got", cur)
	}
	if !cur.Version.Equal(before.Version) {
		t.Error("version changed on non-destructive delete", before.Version, cur.Version)
	}

	// What we told the remote: invalid (we don't have it), not deleted.
	waitFor(t, "invalid index entry to be announced", func() bool {
		f, ok := sent.get(name)
		return ok && f.IsInvalid()
	})
	if f, _ := sent.get(name); f.IsDeleted() {
		t.Error("deletion was announced to the remote")
	}

	// The global version is still the remote's and we don't need it.
	gf, ok, err := m.CurrentGlobalFile("default", name)
	must(t, err)
	if !ok || gf.IsDeleted() || gf.IsInvalid() {
		t.Error("global file should be the remote's copy", gf)
	}
	need, err := m.NeedSize("default", protocol.LocalDeviceID)
	must(t, err)
	if need.TotalItems() != 0 {
		t.Error("should not need anything, needs", need)
	}

	// The ledger lists it, with the remote as the device that has it.
	items, err := m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 1 || items[0].Name != name || items[0].Type != "file" || items[0].Size != int64(len(photo)) {
		t.Fatal("unexpected ledger", items)
	}
	if len(items[0].AvailableOn) != 1 || items[0].AvailableOn[0] != device1 {
		t.Error("expected the item to be available on device1", items[0].AvailableOn)
	}

	// Rescanning does not change anything.
	must(t, m.ScanFolder("default"))
	again, _ := m.testCurrentFolderFile("default", name)
	if again.Sequence != cur.Sequence {
		t.Error("rescan touched the delete-ignored entry")
	}
}

func TestNonDestructiveDeleteNotPulledBack(t *testing.T) {
	m, fc, tfs, sent := setupNonDestructive(t)

	const name = "IMG_0002.jpg"
	photo := []byte("original photo bytes")
	syncFromRemote(t, m, fc, tfs, name, photo)
	must(t, tfs.Remove(name))
	must(t, m.ScanFolder("default"))

	// The remote edits the photo, and also adds a copy of the original
	// under another name. Neither must come back here.
	fc.updateFile(name, 0o644, protocol.FileInfoTypeFile, []byte("edited photo bytes"))
	fc.addFile("copy-of-2.jpg", 0o644, protocol.FileInfoTypeFile, photo)
	// An unrelated new photo still syncs.
	fc.addFile("IMG_0003.jpg", 0o644, protocol.FileInfoTypeFile, []byte("a new photo"))
	fc.sendIndexUpdate()

	waitFor(t, "unrelated photo to sync", func() bool {
		return equalContents(tfs, "IMG_0003.jpg", []byte("a new photo")) == nil
	})
	waitFor(t, "edited and copied photo to be recorded as delete-ignored", func() bool {
		a, aok := m.testCurrentFolderFile("default", name)
		b, bok := m.testCurrentFolderFile("default", "copy-of-2.jpg")
		return aok && bok && a.IsDeleteIgnored() && b.IsDeleteIgnored()
	})

	for _, n := range []string{name, "copy-of-2.jpg"} {
		if _, err := tfs.Lstat(n); !fs.IsNotExist(err) {
			t.Errorf("%s was pulled back to the device (err=%v)", n, err)
		}
		if f, ok := sent.get(n); ok && f.IsDeleted() {
			t.Errorf("%s: a deletion was announced", n)
		}
	}
	need, err := m.NeedSize("default", protocol.LocalDeviceID)
	must(t, err)
	if need.TotalItems() != 0 {
		t.Error("should not need anything, needs", need)
	}
}

func TestNonDestructiveDeleteRestore(t *testing.T) {
	m, fc, tfs, _ := setupNonDestructive(t)

	must(t, tfs.MkdirAll("Album", 0o755))
	fc.addFile("Album", 0o755, protocol.FileInfoTypeDirectory, nil)
	syncFromRemote(t, m, fc, tfs, "Album/a.jpg", []byte("photo a"))
	syncFromRemote(t, m, fc, tfs, "Album/b.jpg", []byte("photo b"))

	// Delete the whole album.
	must(t, tfs.RemoveAll("Album"))
	must(t, m.ScanFolder("default"))

	items, err := m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 3 {
		t.Fatal("expected album and both photos in the ledger, got", items)
	}

	// Restoring one photo brings back the photo and its album, but not
	// the other photo.
	must(t, m.RestoreIgnoredDeletions("default", []string{"Album/a.jpg"}))
	waitFor(t, "restored photo to be pulled", func() bool {
		return equalContents(tfs, "Album/a.jpg", []byte("photo a")) == nil
	})
	if _, err := tfs.Lstat("Album/b.jpg"); !fs.IsNotExist(err) {
		t.Error("b.jpg should still be deleted here")
	}
	items, err = m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 1 || items[0].Name != "Album/b.jpg" {
		t.Error("expected only b.jpg to remain in the ledger, got", items)
	}
}

func TestNonDestructiveDeletePropagate(t *testing.T) {
	m, fc, tfs, sent := setupNonDestructive(t)

	const name = "IMG_0004.jpg"
	syncFromRemote(t, m, fc, tfs, name, []byte("delete me everywhere"))
	must(t, tfs.Remove(name))
	must(t, m.ScanFolder("default"))

	must(t, m.PropagateIgnoredDeletions("default", []string{name}))

	waitFor(t, "deletion to be announced", func() bool {
		f, ok := sent.get(name)
		return ok && f.IsDeleted() && !f.IsInvalid()
	})
	items, err := m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 0 {
		t.Error("ledger should be empty, got", items)
	}
}

func TestNonDestructiveDeleteLocalOnlyFileDeletedNormally(t *testing.T) {
	m, _, tfs, _ := setupNonDestructive(t)

	// A file that never left this device is deleted the regular way.
	writeFile(t, tfs, "scratch.txt", []byte("local only"))
	must(t, m.ScanFolder("default"))
	must(t, tfs.Remove("scratch.txt"))
	must(t, m.ScanFolder("default"))

	f, ok := m.testCurrentFolderFile("default", "scratch.txt")
	if !ok || !f.IsDeleted() || f.IsDeleteIgnored() {
		t.Error("expected a regular deletion record, got", f)
	}
	items, err := m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 0 {
		t.Error("ledger should be empty, got", items)
	}
}

func TestNonDestructiveDeleteDisabledByDefault(t *testing.T) {
	m, fc, fcfg := setupModelWithConnection(t)
	t.Cleanup(func() { cleanupModel(m) })
	tfs := fcfg.Filesystem()

	syncFromRemote(t, m, fc, tfs, "IMG_0005.jpg", []byte("regular folder"))
	must(t, tfs.Remove("IMG_0005.jpg"))
	must(t, m.ScanFolder("default"))

	f, _ := m.testCurrentFolderFile("default", "IMG_0005.jpg")
	if !f.IsDeleted() || f.IsDeleteIgnored() {
		t.Error("expected a regular deletion without the option, got", f)
	}
}

func TestNonDestructiveDeleteReappearedFileLeavesLedger(t *testing.T) {
	m, fc, tfs, _ := setupNonDestructive(t)

	const name = "IMG_0006.jpg"
	syncFromRemote(t, m, fc, tfs, name, []byte("back again"))
	must(t, tfs.Remove(name))
	must(t, m.ScanFolder("default"))

	// The user puts the photo back.
	writeFile(t, tfs, name, []byte("back again"))
	must(t, m.ScanFolder("default"))

	f, _ := m.testCurrentFolderFile("default", name)
	if f.IsInvalid() || f.IsDeleted() {
		t.Error("expected a regular entry again, got", f)
	}
	items, err := m.IgnoredDeletions("default")
	must(t, err)
	if len(items) != 0 {
		t.Error("ledger should be empty, got", items)
	}
}
