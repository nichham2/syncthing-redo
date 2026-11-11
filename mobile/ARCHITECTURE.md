# Keepsake: architecture

Keepsake is a fork of Syncthing that mirrors photos between an iPhone, an
Android phone and an always-on hub, with deletions that stay local ("ignore
tag" logic) and a mobile-first dashboard. This document explains how the
Syncthing core was adapted. Installation is covered in
[`INSTALL.md`](INSTALL.md), naming and icons in [`branding/`](branding/).

In the code, the sync engine is still called Syncthing (package names,
config keys, REST paths). "Keepsake" is the product name shown to users.

```
 ┌──────────── iPhone ─────────────┐            ┌──────────── Android ────────────┐
 │ Apple Photos (PhotoKit)         │            │ Gallery / MediaStore            │
 │        ▲  export originals      │            │        ▲ scanFile / insert      │
 │        │  import new files      │            │        │                        │
 │ PhotoLibraryBridge.swift        │            │ MediaStoreBridge.kt             │
 │        │                        │            │        │                        │
 │ staging folder (app container)  │            │ DCIM/Camera (direct) or staging │
 │        │                        │            │        │                        │
 │ lib/mobile (gomobile .xcframework)  ◄─BEP/TLS─►  lib/mobile (gomobile .aar)     │
 │   Syncthing core + SQLite DB    │  LAN, relay │   Syncthing core + SQLite DB    │
 │   + deletion ledger             │  or via hub │   + deletion ledger             │
 │ WKWebView ─► /mobile/ dashboard │            │ WebView ─► /mobile/ dashboard   │
 └─────────────────────────────────┘            └─────────────────────────────────┘
```

## 1. Sync state and the ignore list ("deletion ledger")

### The problem with plain deletions

In Syncthing every device keeps an index of every file, each with a version
vector. Deleting a file creates a new version with `Deleted=true`, which is
newer than everyone else's copy, so every other device deletes it too. That
is the behaviour the requirements rule out.

### What changes

A new per-folder option, `nonDestructiveDeletes` (config key, REST and
dashboard toggle), turns on the following.

**New local flag.** `protocol.FlagLocalDeleteIgnored` (bit 7, shown as `x`
in debug output) marks a file entry as "deliberately absent here". It is one
of the `LocalInvalidFlags`, so it is announced to peers as *invalid*, exactly
like files matched by `.stignore`. It is also part of the new
`LocalHiddenFlags`, which keep it out of local counts and out of the local
need list.

**New table.** Each folder database gets `ignored_deletions`
(`internal/db/sqlite/sql/schema/folder/60-ignored-deletions.sql`):

| column           | purpose                                                  |
|------------------|----------------------------------------------------------|
| `name`           | path in the folder (primary key)                         |
| `type`           | file / directory / symlink                               |
| `size`, `modified` | what the item looked like when it was deleted          |
| `blocklist_hash` | content hash, indexed, to catch renamed copies           |
| `ignored_at`     | when the deletion was recorded                           |

Schema files are applied on every start with `CREATE TABLE IF NOT EXISTS`,
so existing databases get the table without a migration. The model keeps an
in-memory index of the table (`lib/model/deletionledger.go`) that writes
through on every change.

### The flow

1. **User deletes a photo on device A.** The scanner
   (`scanSubdirsDeletedAndIgnored`) finds a tracked item missing on disk and
   calls `handleNonDestructiveDelete` (`lib/model/nondestructive.go`). If
   some other device still announces a valid copy, it
   * writes a ledger row (before touching the file entry, so a crash can
     only cause the deletion to be detected again, never a flagged entry
     the ledger does not know about), and
   * replaces the local entry with the **same version** plus
     `FlagLocalDeleteIgnored`. No deletion record is created.

   Items that only ever existed on A are deleted the normal way.

2. **Device B is unaffected.** B receives A's entry flagged invalid. Its own
   copy stays the global version, so B keeps the photo and shows A as 100%
   in sync.

3. **A does not download it again.** A's entry has the same version as the
   global one, so the database does not mark it as needed. If B later edits
   the photo (newer version) or adds the same bytes under another name, the
   puller (`pullerIteration` in `folder_sendrecv.go`) checks the ledger by
   name and by blocklist hash and records that version as delete-ignored
   instead of fetching it. Rescans skip delete-ignored entries.

4. **Restore** (`POST /rest/folder/ignoreddeletions/restore`) removes the
   ledger rows for the selected items, everything beneath them and their
   delete-ignored parent albums, drops the local entries, and schedules a
   pull. The photo comes back byte-for-byte from B.

5. **Delete everywhere** (`POST /rest/folder/ignoreddeletions/propagate`)
   turns the selected local-only deletions into regular deletions (a bumped
   version with `Deleted=true`), which then removes them from B.

6. **Putting a file back by hand** (copying it into the folder again) is
   picked up by the next scan as a new local version; the ledger row is
   removed automatically.

Turning the option off stops recording new local-only deletions. Existing
ledger entries keep their protection until they are restored or deleted
everywhere.

`GET /rest/folder/ignoreddeletions?folder=ID` lists the ledger with, for each
item, the devices that still hold it (`availableOn`).

### Why this design

* It reuses the "invalid" mechanism the protocol already has for ignored
  files, so **no protocol change** is needed and unmodified Syncthing
  desktops on the other end interoperate (they just see A as not having
  the file).
* Keeping the version unchanged means B never sees anything newer than its
  own copy, so nothing on B can be deleted by this feature.
* Matching by content hash covers the common "same photo, new name" cases
  (re-exports, de-duplication tools, `IMG_0001 (1).jpg`).
* The existing `ignoreDelete` option is receiver-side ("ignore deletions
  coming from others"); it leaves devices permanently out of sync and gives
  the deleting device no way to list or restore. `nonDestructiveDeletes` is
  sender-side and keeps everyone in a consistent state.

## 2. The cross-platform mobile bridge

### Go side: `lib/mobile`

A small gomobile-compatible package that embeds the full Syncthing core:

| call | used for |
|------|----------|
| `NewNode(homeDir)`, `Start()`, `Stop()` | app lifecycle; `Stop` closes the DB before suspension |
| `EnsurePhotoFolder(id, label, path)` | send-receive folder, `nonDestructiveDeletes`, permissions ignored |
| `AddPeer(deviceID, name)` | pairing; shares every photo folder |
| `Rescan(folder, relPath)` | the native side signals photo library changes |
| `RunBackgroundSync(maxSeconds)` | one bounded OS background window; returns status JSON |
| `StatusJSON()`, `SetPaused(bool)`, `DeviceID()`, `DashboardURL()` | UI and notifications |
| `Platform` callback interface | `OnRemoteChange` (import into the photo library), `OnStateChanged` |

`applyMobileDefaults` pins the GUI/REST API to loopback with an API key,
disables self-upgrade (stores own updates) and the usage-report prompt, and
keeps global discovery and relays on, since phones move between networks.

`RunBackgroundSync` starts the node, waits until every folder is idle with
nothing to pull and every connected peer is at 100% (held for 3 s, after a
10 s grace period for connections), then stops it again.

Build:

```sh
mobile/build-bridge.sh ios       # -> mobile/ios/Mobile.xcframework
mobile/build-bridge.sh android   # -> mobile/android/app/libs/keepsake-core.aar
```

### iOS: `mobile/ios/Keepsake`

Apple Photos is a database, not a folder, so the bridge keeps a **staging
folder** in the app container plus a `localIdentifier ↔ path` mapping.

* **Export:** new assets are written with
  `PHAssetResourceManager.writeData` from the *original* resource (full
  quality, EXIF/GPS, HEIC, iCloud originals downloaded on demand). The file
  modification time is set to the capture date.
* **Import:** files arriving from Android are added with
  `PHAssetCreationRequest.addResource(with:fileURL:)` (original bytes, not
  re-encoded); `creationDate` comes from EXIF `DateTimeOriginal` or the
  preserved mtime. The mapping prevents re-exporting them.
* **Deletion in Photos:** detected by diffing the mapping against the
  library; the staging file is removed and Syncthing records a local-only
  deletion. "Delete everywhere" from the other phone deletes the asset,
  which iOS confirms with the user.
* **Background:** no persistent background execution on iOS.
  `BGAppRefreshTask` (~30 s), `BGProcessingTask` (minutes, usually overnight
  while charging) and the ~30 s grace period after backgrounding each call
  `RunBackgroundSync`; the expiration handler stops the node.
* **Permissions:** `NSPhotoLibraryUsageDescription`,
  `NSPhotoLibraryAddUsageDescription`, `NSLocalNetworkUsageDescription`;
  LAN discovery broadcasts need Apple's multicast entitlement (without it,
  global discovery and relays still work).

Trade-off: the staging folder doubles storage for synced photos on the
iPhone. A later step can replace it with a virtual `fs.Filesystem` backed
by PhotoKit, which Syncthing's filesystem abstraction allows.

### Android: `mobile/android/app/src/main/java/app/keepsake`

* **Direct mode** (recommended, needs *All files access*, which Google Play
  allows for sync apps): Syncthing syncs `DCIM/Camera` in place. Files keep
  their original bytes and mtimes. New files are announced to the gallery
  with `MediaScannerConnection.scanFile`. Deleting in the gallery removes the
  file, and Syncthing records it as a local-only deletion.
* **Staging mode** (only `READ_MEDIA_IMAGES/VIDEO`): mirrors MediaStore into
  app storage like the iOS bridge, using `MediaStore.setRequireOriginal` (with
  `ACCESS_MEDIA_LOCATION`) so GPS EXIF is kept, and inserts incoming files
  with `IS_PENDING`.
* **Background:** `WorkManager` periodic work every 15 minutes or more, with
  Wi-Fi/charging constraints, calling `RunBackgroundSync(8 min)`. There is an
  optional `dataSync` foreground service for continuous sync (Android 14+
  requires the type; Android 15 limits it to 6 h/day and `onTimeout` stops it).

### The constraint to plan around

Two phones only sync when **both are awake and online at the same time**.
Background windows on iOS are short and up to the OS. Syncing works reliably
while one app is in the foreground, or overnight when both phones are
charging. That is why Keepsake's recommended setup has a third device: the
**hub**, an always-on desktop, NAS or small VPS running `keepsake-hub` (the
plain engine binary) and sharing the same `photos` folder. Each phone then
only needs to reach the hub. The hub keeps `nonDestructiveDeletes` **off**:
it never deletes anything itself, and since the phones announce their
deletions as invalid rather than deleted, the hub keeps every photo. A
photo deleted on the iPhone stays on the hub and on the Android phone.

```
   iPhone ◄──────► hub (computer/NAS, always on) ◄──────► Android
      └───────────── direct, when both are awake ─────────────┘
```

## 3. Mobile dashboard (`gui/default/mobile/`)

Branded Keepsake (icon `keepsake-icon.svg`, accent blue `#2457D6`, amber
ribbon `#F5A524`).

A dependency-free page served by the core at `/mobile/`. The native apps
load it in a WebView (`DashboardURL()` passes the API key in the URL
fragment), and it also works in any phone browser through the normal GUI
login.

* **Sync:** progress ring, status in plain language ("Up to date", "Syncing
  · 12 items left · 34 MB", "Waiting for your devices"), last sync, photo
  count, "kept elsewhere" count, *Sync now*.
* **Devices:** connection dot, LAN/internet/relay, per-peer completion bar,
  last seen, pause/resume, own ID with QR code, add-device form that shares
  the photo folders.
* **Ignored:** searchable list of local-only deletions with "On Pixel 9"
  chips, multi-select, sticky *Restore* / *Delete everywhere* bar (the
  destructive action asks for confirmation).
* **Settings:** per-folder "keep photos on other devices" switch, pause all,
  link to the full interface.

Live updates use the `/rest/events` long-poll. The layout uses a bottom tab
bar within thumb reach, 48 px touch targets, safe-area insets, light and dark
themes from one set of tokens, and reduced-motion support.
