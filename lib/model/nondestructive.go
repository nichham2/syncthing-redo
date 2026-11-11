// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

// Non-destructive deletes ("ignore tag" sync logic)
//
// In a folder with nonDestructiveDeletes enabled, deleting a previously
// synced item on this device does not delete it anywhere else:
//
//  1. The scanner notices the item is gone. If some other device still has
//     a valid copy, the item is written to the deletion ledger (name, type,
//     size, mtime, blocklist hash) and the local file entry keeps its
//     version but gets the FlagLocalDeleteIgnored bit. No deletion record
//     is created, so no other device is told to delete anything.
//
//  2. The flag makes the local entry "invalid": other devices see that we
//     do not provide the item and keep their own copy as the global
//     version. Locally the version is equal to the global one, so the
//     item is not needed and is not pulled back.
//
//  3. If another device later changes the item, or adds the same content
//     under a different name, the puller consults the ledger and records
//     the new version as delete-ignored instead of downloading it.
//
//  4. The user can restore items (they are pulled back from the other
//     devices) or choose to delete them everywhere (a regular deletion is
//     then announced), via the REST API and the mobile dashboard.

import (
	"context"
	"slices"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

// IgnoredDeletion is an item that was deleted on this device only, as
// presented to API consumers.
type IgnoredDeletion struct {
	Name        string                `json:"name"`
	Type        string                `json:"type"`
	Size        int64                 `json:"size"`
	ModTime     time.Time             `json:"modTime"`
	IgnoredAt   time.Time             `json:"ignoredAt"`
	AvailableOn []protocol.DeviceID   `json:"availableOn"`
	RawType     protocol.FileInfoType `json:"-"`
}

func (f *folder) nonDestructiveDeletesActive() bool {
	if !f.NonDestructiveDeletes {
		return false
	}
	switch f.Type {
	case config.FolderTypeSendReceive, config.FolderTypeSendOnly:
		return true
	default:
		// Receive-only folders already keep local deletions local, and
		// receive-encrypted folders hold no user data to delete.
		return false
	}
}

// handleNonDestructiveDelete is called by the scanner for a tracked item
// that has disappeared from disk. It returns true when the deletion was
// recorded as local-only, in which case no deletion record must be
// created.
func (f *folder) handleNonDestructiveDelete(ctx context.Context, fi protocol.FileInfo, batch *scanBatch) (bool, error) {
	if !f.nonDestructiveDeletesActive() || fi.ShouldConflict() {
		return false, nil
	}

	// Only items that were actually synced, i.e. that some other device
	// still has, are worth protecting. Something that only ever existed
	// here is deleted the regular way.
	elsewhere, err := f.presentOnOtherDevice(fi.Name)
	if err != nil {
		return false, err
	}
	if !elsewhere {
		return false, nil
	}

	// Write the ledger entry before the flagged file entry, so that a
	// crash in between leads to the deletion being detected (and recorded)
	// again on the next scan rather than to a flagged entry that the
	// ledger does not know about.
	entry := db.IgnoredDeletion{
		Name:          fi.Name,
		Type:          fi.Type,
		Size:          fi.Size,
		ModNanos:      fi.ModTime().UnixNano(),
		BlocklistHash: fi.BlocksHash,
		IgnoredNanos:  time.Now().UnixNano(),
	}
	if err := f.ledger.Add(entry); err != nil {
		return false, err
	}

	nf := fi
	nf.SetDeleteIgnored() // keeps the version, drops blocks
	f.sl.DebugContext(ctx, "Marking locally deleted item as delete-ignored", slogutil.FilePath(nf.Name))
	if _, err := batch.Update(nf); err != nil {
		return false, err
	}
	return true, nil
}

// presentOnOtherDevice returns true if any remote device announces a
// valid, non-deleted version of the named item.
func (f *folder) presentOnOtherDevice(name string) (bool, error) {
	devs, err := f.db.ListDevicesForFolder(f.folderID)
	if err != nil {
		return false, err
	}
	for _, dev := range devs {
		if dev == protocol.LocalDeviceID || dev == f.model.id {
			continue
		}
		rf, ok, err := f.db.GetDeviceFile(f.folderID, dev, name)
		if err != nil {
			return false, err
		}
		if ok && !rf.IsDeleted() && !rf.IsInvalid() {
			return true, nil
		}
	}
	return false, nil
}

// IgnoredDeletions lists the folder's deletion ledger, together with the
// devices that currently hold the global version of each item.
func (f *folder) IgnoredDeletions() ([]IgnoredDeletion, error) {
	items, err := f.ledger.All()
	if err != nil {
		return nil, err
	}
	res := make([]IgnoredDeletion, 0, len(items))
	for _, it := range items {
		avail, err := f.db.GetGlobalAvailability(f.folderID, it.Name)
		if err != nil {
			return nil, err
		}
		if avail == nil {
			avail = []protocol.DeviceID{}
		}
		res = append(res, IgnoredDeletion{
			Name:        osutil.NativeFilename(it.Name),
			Type:        fileInfoTypeString(it.Type),
			Size:        it.Size,
			ModTime:     it.ModTime(),
			IgnoredAt:   it.IgnoredAt(),
			AvailableOn: avail,
			RawType:     it.Type,
		})
	}
	return res, nil
}

// RestoreIgnoredDeletions removes the named items (including anything
// below them and their delete-ignored parent directories) from the ledger
// and pulls them back from the other devices.
func (f *folder) RestoreIgnoredDeletions(names []string) error {
	return f.doInSync(func(ctx context.Context) error {
		expanded, err := f.ledger.Expand(names)
		if err != nil || len(expanded) == 0 {
			return err
		}

		var drop []string
		for _, name := range expanded {
			cf, ok, err := f.db.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
			if err != nil {
				return err
			}
			if ok && cf.IsDeleteIgnored() {
				drop = append(drop, name)
			}
		}

		if err := f.ledger.Remove(expanded...); err != nil {
			return err
		}
		// Forgetting our delete-ignored entry makes the global version
		// needed again, so the puller fetches it.
		if len(drop) > 0 {
			if err := f.db.DropFilesNamed(f.folderID, protocol.LocalDeviceID, slices.Clone(drop)); err != nil {
				return err
			}
		}
		f.sl.InfoContext(ctx, "Restoring delete-ignored items", "count", len(expanded))
		f.SchedulePull()
		return nil
	})
}

// PropagateIgnoredDeletions turns the named local-only deletions into
// regular deletions, so the items are removed from the other devices too.
func (f *folder) PropagateIgnoredDeletions(names []string) error {
	return f.doInSync(func(ctx context.Context) error {
		expanded, err := f.ledger.Expand(names)
		if err != nil || len(expanded) == 0 {
			return err
		}

		var files []protocol.FileInfo
		for _, name := range expanded {
			if !osutil.IsDeleted(f.mtimefs, name) {
				// Put back by the user in the meantime; leave it alone.
				continue
			}
			nf, ok, err := f.db.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
			if err != nil {
				return err
			}
			switch {
			case ok && nf.IsDeleteIgnored():
			case !ok:
				// No local entry (e.g. after a database reset); base the
				// deletion on the global version.
				gf, gok, err := f.db.GetGlobalFile(f.folderID, name)
				if err != nil {
					return err
				}
				if !gok || gf.IsDeleted() {
					continue
				}
				nf = gf
			default:
				continue
			}
			nf.LocalFlags = f.localFlags
			nf.SetDeleted(f.shortID)
			nf.Sequence = 0
			files = append(files, nf)
		}

		if err := f.ledger.Remove(expanded...); err != nil {
			return err
		}
		if len(files) > 0 {
			f.sl.InfoContext(ctx, "Deleting previously delete-ignored items on all devices", "count", len(files))
			return f.updateLocalsFromScanning(files)
		}
		return nil
	})
}

func fileInfoTypeString(t protocol.FileInfoType) string {
	switch t {
	case protocol.FileInfoTypeDirectory:
		return "directory"
	case protocol.FileInfoTypeSymlink, protocol.FileInfoTypeSymlinkDirectory, protocol.FileInfoTypeSymlinkFile:
		return "symlink"
	default:
		return "file"
	}
}

// Model API

func (m *model) folderRunner(folder string) (service, error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	if err := m.checkFolderRunningRLocked(folder); err != nil {
		return nil, err
	}
	runner, _ := m.folderRunners.Get(folder)
	return runner, nil
}

func (m *model) IgnoredDeletions(folder string) ([]IgnoredDeletion, error) {
	runner, err := m.folderRunner(folder)
	if err != nil {
		return nil, err
	}
	return runner.IgnoredDeletions()
}

func (m *model) RestoreIgnoredDeletions(folder string, names []string) error {
	runner, err := m.folderRunner(folder)
	if err != nil {
		return err
	}
	return runner.RestoreIgnoredDeletions(names)
}

func (m *model) PropagateIgnoredDeletions(folder string, names []string) error {
	runner, err := m.folderRunner(folder)
	if err != nil {
		return err
	}
	return runner.PropagateIgnoredDeletions(names)
}
