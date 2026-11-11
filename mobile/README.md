# Keepsake mobile apps

Keepsake keeps the photos on your iPhone and your Android phone in sync, with an
always-on hub (computer or NAS) in between. Deleting a photo on one phone removes it
**only from that phone**: the other phone and the hub keep it, and it is not sent back.

* [`INSTALL.md`](INSTALL.md): installing on the hub, the Android phone and the iPhone, then pairing them.
* [`ARCHITECTURE.md`](ARCHITECTURE.md): how the deletion ledger, the mobile bridge and the dashboard fit together.
* [`branding/`](branding/): name, icon, colours, and where each is set.
* `../lib/mobile`: Go bridge, built with gomobile by `build-bridge.sh`.
* `ios/`: the iOS app. `project.yml` (XcodeGen) generates `Keepsake.xcodeproj`; Swift sources are in `ios/Keepsake`.
* `android/`: the Android app (Gradle). Kotlin sources are in `android/app/src/main/java/app/keepsake`.
* `../gui/default/mobile`: the dashboard UI both apps show, served by the core at `/mobile/`.
* `../.github/workflows/build-keepsake.yaml`: CI that builds the hub binaries, the Android APK, an unsigned iPhone IPA for sideloading, and (with Apple secrets) uploads a signed build to TestFlight.

## How the app works

1. Each phone runs the sync engine (Syncthing core) inside the app and shares one
   folder, ID `photos`, with the hub.
2. **iPhone:** new photos in Apple Photos are exported as originals (HEIC/JPEG/MOV
   with EXIF, GPS and capture date) into a staging folder the engine syncs. Photos
   arriving from elsewhere are imported into Apple Photos with their original bytes.
3. **Android:** the engine syncs `DCIM/Camera` directly (with *All files access*), or a
   staging folder if you don't grant that. New files are announced to the gallery.
4. When you delete a photo on a phone, Keepsake records it in that phone's
   **Ignored** list. The deletion is not sent to other devices, and the photo is
   never downloaded to that phone again.
5. The **Ignored** tab lets you **Restore** a photo (download it again) or
   **Delete everywhere** (send the deletion to every device, after a confirmation).
6. Sync runs while the app is open, and in background windows the OS grants
   (WorkManager every ~15 min on Android; BGTaskScheduler on iOS, mostly overnight
   while charging).

The dashboard has four tabs: **Sync** (status, last synced, photo count, Sync now),
**Devices** (connection status per device, your device ID and QR code, add a device),
**Ignored** (restore or delete everywhere), and **Settings** (the "Keep photos on other
devices" switch per folder, pause all, link to the advanced engine settings).

## Building

```sh
mobile/build-bridge.sh android   # -> mobile/android/app/libs/keepsake-core.aar (needs the Android NDK)
mobile/build-bridge.sh ios       # -> mobile/ios/Mobile.xcframework (needs Xcode, macOS only)

cd mobile/android && gradle assembleDebug            # APK in app/build/outputs/apk/debug
cd mobile/ios && xcodegen generate && open Keepsake.xcodeproj
```

The hub is the normal engine binary: `go run build.go build syncthing`.
The CI workflow does all of this; see [`INSTALL.md`](INSTALL.md) for downloading its output.

## Trying the dashboard on a desktop build

```sh
go run build.go build syncthing
./syncthing serve
# open http://127.0.0.1:8384/mobile/ in a phone-sized browser window
```

Enable "Keep photos on other devices" for a folder in the dashboard's Settings tab,
or set `nonDestructiveDeletes` in the folder config.
