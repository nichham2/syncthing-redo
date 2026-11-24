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
	"strings"
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

// recordHiddenCopy makes sure a remote item that the puller keeps away
// from this device because of the ledger is itself in the ledger. Items
// matched by content under a new name would otherwise be flagged
// delete-ignored without being listed anywhere, and could never be
// restored.
func (f *folder) recordHiddenCopy(file protocol.FileInfo) error {
	if f.ledger.Has(file.Name) {
		return nil
	}
	return f.ledger.Add(db.IgnoredDeletion{
		Name:          file.Name,
		Type:          file.Type,
		Size:          file.Size,
		ModNanos:      file.ModTime().UnixNano(),
		BlocklistHash: file.BlocksHash,
		IgnoredNanos:  time.Now().UnixNano(),
	})
}

// repairLedger adds to the ledger any local entry that is flagged
// delete-ignored but missing from it. Builds before recordHiddenCopy
// existed left such entries behind for content matches.
func (f *folder) repairLedger() error {
	var missing []db.IgnoredDeletion
	it, errFn := f.db.AllLocalFiles(f.folderID, protocol.LocalDeviceID)
	for fi := range it {
		if fi.IsDeleteIgnored() && !f.ledger.Has(fi.Name) {
			missing = append(missing, db.IgnoredDeletion{
				Name:          fi.Name,
				Type:          fi.Type,
				Size:          fi.Size,
				ModNanos:      fi.ModTime().UnixNano(),
				BlocklistHash: fi.BlocksHash,
				IgnoredNanos:  time.Now().UnixNano(),
			})
		}
	}
	if err := errFn(); err != nil {
		return err
	}
	if len(missing) > 0 {
		f.sl.Info("Adding delete-ignored items missing from the ledger", "count", len(missing))
	}
	return f.ledger.Add(missing...)
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
	items, _, err := f.IgnoredDeletionsPage("", 1, 0)
	return items, err
}

// IgnoredDeletionsPage returns one page of the ledger, sorted by name, and
// the number of entries that match filter (a case-insensitive substring of
// the name; "" matches everything). A perpage of zero or less returns every
// match. Availability is only looked up for the entries on the page, so a
// large ledger costs no more than the page that is shown.
func (f *folder) IgnoredDeletionsPage(filter string, page, perpage int) ([]IgnoredDeletion, int, error) {
	items, err := f.ledger.All()
	if err != nil {
		return nil, 0, err
	}
	if filter != "" {
		needle := strings.ToLower(filter)
		items = slices.DeleteFunc(items, func(it db.IgnoredDeletion) bool {
			return !strings.Contains(strings.ToLower(osutil.NativeFilename(it.Name)), needle)
		})
	}
	total := len(items)
	if perpage > 0 {
		page = max(page, 1)
		start := min((page-1)*perpage, total)
		end := min(start+perpage, total)
		items = items[start:end]
	}
	res := make([]IgnoredDeletion, 0, len(items))
	for _, it := range items {
		avail, err := f.db.GetGlobalAvailability(f.folderID, it.Name)
		if err != nil {
			return nil, 0, err
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
	return res, total, nil
}

// IgnoredDeletionsCount returns the number of entries in the ledger.
func (f *folder) IgnoredDeletionsCount() (int, error) {
	if err := f.ledger.load(); err != nil {
		return 0, err
	}
	return f.ledger.Len(), nil
}

// RestoreIgnoredDeletions removes the named items (including anything
// below them and their delete-ignored parent directories) from the ledger
// and pulls them back from the other devices.
func (f *folder) RestoreIgnoredDeletions(names []string) error {
	return f.doInSync(func(ctx context.Context) error {
		expanded, err := f.ledger.Expand(names, true)
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
		f.ledger.MarkRestored(expanded...)
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
		expanded, err := f.ledger.Expand(names, false)
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

func (m *model) IgnoredDeletionsPage(folder, filter string, page, perpage int) ([]IgnoredDeletion, int, error) {
	runner, err := m.folderRunner(folder)
	if err != nil {
		return nil, 0, err
	}
	return runner.IgnoredDeletionsPage(filter, page, perpage)
}

func (m *model) IgnoredDeletionsCount(folder string) (int, error) {
	runner, err := m.folderRunner(folder)
	if err != nil {
		return 0, err
	}
	return runner.IgnoredDeletionsCount()
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
