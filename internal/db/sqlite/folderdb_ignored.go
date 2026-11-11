// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sqlite

import (
	"context"
	"iter"
	"slices"

	"github.com/jmoiron/sqlx"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/itererr"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

func (s *folderDB) PutIgnoredDeletions(items []db.IgnoredDeletion) error {
	if len(items) == 0 {
		return nil
	}

	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	tx, err := s.sql.BeginTxx(context.Background(), nil)
	if err != nil {
		return wrap(err)
	}
	defer tx.Rollback() //nolint:errcheck

	//nolint:sqlclosecheck
	stmt, err := tx.Preparex(`
		INSERT OR REPLACE INTO ignored_deletions (name, type, size, modified, blocklist_hash, ignored_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return wrap(err)
	}
	for _, it := range items {
		var hash any
		if len(it.BlocklistHash) > 0 {
			hash = it.BlocklistHash
		}
		name := osutil.NormalizedFilename(it.Name)
		if _, err := stmt.Exec(name, it.Type, it.Size, it.ModNanos, hash, it.IgnoredNanos); err != nil {
			return wrap(err)
		}
	}
	return wrap(tx.Commit())
}

func (s *folderDB) DeleteIgnoredDeletions(names []string) error {
	if len(names) == 0 {
		return nil
	}
	normalized := make([]string, len(names))
	for i, n := range names {
		normalized[i] = osutil.NormalizedFilename(n)
	}

	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	// Chunk to stay well below the SQLite variable limit.
	for chunk := range slices.Chunk(normalized, 1000) {
		query, args, err := sqlx.In(`DELETE FROM ignored_deletions WHERE name IN (?)`, chunk)
		if err != nil {
			return wrap(err)
		}
		if _, err := s.sql.Exec(query, args...); err != nil {
			return wrap(err)
		}
	}
	return nil
}

type ignoredDeletionRow struct {
	Name          string
	Type          int32
	Size          int64
	Modified      int64
	BlocklistHash []byte `db:"blocklist_hash"`
	IgnoredAt     int64  `db:"ignored_at"`
}

func (s *folderDB) AllIgnoredDeletions() (iter.Seq[db.IgnoredDeletion], func() error) {
	it, errFn := iterStructs[ignoredDeletionRow](s.stmt(`
		SELECT name, type, size, modified, blocklist_hash, ignored_at FROM ignored_deletions
		ORDER BY name
	`).Queryx())
	return itererr.Map(it, errFn, func(r ignoredDeletionRow) (db.IgnoredDeletion, error) {
		return db.IgnoredDeletion{
			Name:          r.Name,
			Type:          protocol.FileInfoType(r.Type),
			Size:          r.Size,
			ModNanos:      r.Modified,
			BlocklistHash: r.BlocklistHash,
			IgnoredNanos:  r.IgnoredAt,
		}, nil
	})
}
