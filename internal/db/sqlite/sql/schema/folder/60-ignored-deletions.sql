-- Copyright (C) 2026 The Syncthing Authors.
--
-- This Source Code Form is subject to the terms of the Mozilla Public
-- License, v. 2.0. If a copy of the MPL was not distributed with this file,
-- You can obtain one at https://mozilla.org/MPL/2.0/.

--- Non-destructive deletion ledger.
--
-- When an item is deleted on this device in a folder with
-- nonDestructiveDeletes enabled, the deletion is not announced to other
-- devices. Instead the item is recorded here and the local file entry gets
-- the FlagLocalDeleteIgnored bit. The puller consults this table (by name,
-- and by blocklist hash for renamed copies of the same content) so the item
-- is never pulled back to this device, while other devices keep their copy.
CREATE TABLE IF NOT EXISTS ignored_deletions (
    name TEXT NOT NULL PRIMARY KEY COLLATE BINARY,
    type INTEGER NOT NULL, -- protocol.FileInfoType
    size INTEGER NOT NULL,
    modified INTEGER NOT NULL, -- unix nanos, of the item when it was deleted
    blocklist_hash BLOB, -- null for directories and empty files
    ignored_at INTEGER NOT NULL -- unix nanos
) STRICT, WITHOUT ROWID
;
CREATE INDEX IF NOT EXISTS ignored_deletions_blocklist_hash ON ignored_deletions (blocklist_hash)
    WHERE blocklist_hash IS NOT NULL
;
