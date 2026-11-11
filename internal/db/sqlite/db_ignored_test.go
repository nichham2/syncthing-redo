// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sqlite

import (
	"bytes"
	"testing"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/itererr"
	"github.com/syncthing/syncthing/lib/protocol"
)

func TestIgnoredDeletions(t *testing.T) {
	t.Parallel()

	sdb, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sdb.Close(); err != nil {
			t.Fatal(err)
		}
	})

	// Listing an unknown folder is empty, not an error
	items, err := itererr.Collect(sdb.AllIgnoredDeletions("unknown"))
	if err != nil || len(items) != 0 {
		t.Fatal("expected empty result", items, err)
	}

	now := time.Now().UnixNano()
	in := []db.IgnoredDeletion{
		{Name: "DCIM/b.jpg", Type: protocol.FileInfoTypeFile, Size: 42, ModNanos: 1, BlocklistHash: []byte{1, 2, 3}, IgnoredNanos: now},
		{Name: "DCIM/a.heic", Type: protocol.FileInfoTypeFile, Size: 7, ModNanos: 2, BlocklistHash: []byte{4, 5, 6}, IgnoredNanos: now},
		{Name: "Album", Type: protocol.FileInfoTypeDirectory, IgnoredNanos: now},
	}
	if err := sdb.PutIgnoredDeletions("photos", in); err != nil {
		t.Fatal(err)
	}

	items, err = itererr.Collect(sdb.AllIgnoredDeletions("photos"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatal("expected three items, got", len(items))
	}
	// Sorted by name
	if items[0].Name != "Album" || items[1].Name != "DCIM/a.heic" || items[2].Name != "DCIM/b.jpg" {
		t.Fatal("unexpected order", items)
	}
	if items[0].BlocklistHash != nil || items[0].Type != protocol.FileInfoTypeDirectory {
		t.Error("directory entry mangled", items[0])
	}
	if !bytes.Equal(items[2].BlocklistHash, []byte{1, 2, 3}) || items[2].Size != 42 || items[2].IgnoredNanos != now {
		t.Error("file entry mangled", items[2])
	}

	// Replacing an entry is fine
	in[0].Size = 43
	if err := sdb.PutIgnoredDeletions("photos", in[:1]); err != nil {
		t.Fatal(err)
	}

	// Deleting
	if err := sdb.DeleteIgnoredDeletions("photos", []string{"Album", "DCIM/b.jpg", "does-not-exist"}); err != nil {
		t.Fatal(err)
	}
	items, err = itererr.Collect(sdb.AllIgnoredDeletions("photos"))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "DCIM/a.heic" {
		t.Fatal("unexpected remaining items", items)
	}
}

func TestDeleteIgnoredFlagHiddenFromNeedAndCounts(t *testing.T) {
	t.Parallel()

	sdb, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sdb.Close(); err != nil {
			t.Fatal(err)
		}
	})

	const folder = "photos"
	remote := protocol.DeviceID{42}

	// The remote and we both have the same version of the file
	file := genFile("IMG_0001.jpg", 4, 1)
	if err := sdb.Update(folder, remote, []protocol.FileInfo{file}); err != nil {
		t.Fatal(err)
	}
	local := file
	local.Sequence = 0
	if err := sdb.Update(folder, protocol.LocalDeviceID, []protocol.FileInfo{local}); err != nil {
		t.Fatal(err)
	}

	// Then we delete it non-destructively: same version, flagged
	local.SetDeleteIgnored()
	if err := sdb.Update(folder, protocol.LocalDeviceID, []protocol.FileInfo{local}); err != nil {
		t.Fatal(err)
	}

	// We do not need the file
	need, err := sdb.CountNeed(folder, protocol.LocalDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if need.TotalItems() != 0 {
		t.Error("should not need the delete-ignored file", need)
	}

	// The global file is still the remote's valid, non-deleted copy
	gf, ok, err := sdb.GetGlobalFile(folder, file.Name)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if gf.IsDeleted() || gf.IsInvalid() {
		t.Error("global should be the remote valid file", gf)
	}

	// The local count does not include the hidden file
	lc, err := sdb.CountLocal(folder, protocol.LocalDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Files != 0 {
		t.Error("local count should not include delete-ignored file", lc)
	}

	// A newer remote version is needed at the db level; the puller is
	// responsible for keeping it ignored via the ledger.
	newer := file
	newer.Version = newer.Version.Update(remote.Short())
	newer.Sequence = 2
	if err := sdb.Update(folder, remote, []protocol.FileInfo{newer}); err != nil {
		t.Fatal(err)
	}
	need, err = sdb.CountNeed(folder, protocol.LocalDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if need.TotalItems() != 1 {
		t.Error("should need the newer version", need)
	}
}
