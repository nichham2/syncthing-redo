# Installing Keepsake on your three devices

Keepsake runs on three devices:

| Device | Role | What you install |
|---|---|---|
| **Computer or NAS** (Windows, Mac, Linux, Raspberry Pi) | The **hub**: always on, keeps a full copy of every photo, and passes photos between the phones when only one of them is awake | `keepsake-hub` program |
| **iPhone** | Phone app | Keepsake iOS app, sideloaded from Windows/Linux, via TestFlight, or from Xcode on a Mac |
| **Android phone** | Phone app | Keepsake APK |

Why a hub? Phones only sync while both are awake and online, and iOS gives
apps short background windows. With a hub, each phone only needs to reach the
hub, whenever it gets the chance. The hub also acts as a backup.

You do the steps in this order: hub, Android, iPhone, then pairing.

---

## Getting the builds

Every push to the repository runs the **Build Keepsake** workflow
(`.github/workflows/build-keepsake.yaml`). To download what it built:

1. Open the repository on GitHub, then **Actions**, then **Build Keepsake**.
2. Open the latest green run and scroll to **Artifacts**.
3. Download:
   * `keepsake-android-apk` for the Android phone
   * `keepsake-hub-windows-amd64`, `-darwin-arm64` (Apple Silicon Mac), `-linux-amd64` or `-linux-arm64` (Raspberry Pi 4/5, most NAS boxes) for the hub
   * `keepsake-ios-unsigned-ipa` for the iPhone (option A in step 3)

You can also run the workflow by hand with **Run workflow**.

The iPhone download is unsigned. Apple requires every app to be signed with
an Apple account before it runs, so step 3 covers signing it, with or
without a Mac.

---

## 1. Hub (computer or NAS)

1. Unzip the `keepsake-hub-…` download into a folder of your choice.
2. Start it:
   * **Windows:** double-click `keepsake-hub.exe`. If SmartScreen warns about an unknown app, choose *More info*, then *Run anyway*.
   * **Mac:** in Terminal, run `xattr -d com.apple.quarantine keepsake-hub && ./keepsake-hub`.
   * **Linux / Raspberry Pi:** `chmod +x keepsake-hub && ./keepsake-hub`.
3. A browser opens the engine settings at <http://127.0.0.1:8384>. If it doesn't, open that address yourself.
   * Set a GUI password when it asks (*Settings*, then *GUI*).
4. Note the hub's device ID: *Actions*, then *Show ID*. Leave this page open, because the phones need the ID (as text or QR code).
5. Start the hub automatically:
   * **Windows:** Task Scheduler, *Create Basic Task*, trigger *When I log on*, action *Start a program* pointing at `keepsake-hub.exe --no-browser`.
   * **Mac:** *System Settings*, *General*, *Login Items*, add `keepsake-hub`.
   * **Linux:** copy `etc/linux-systemd/user/syncthing.service` to `~/.config/systemd/user/keepsake-hub.service`, change `ExecStart` to the path of `keepsake-hub`, then run `systemctl --user enable --now keepsake-hub`.

The hub needs no port forwarding. It finds the phones through global discovery and relays.

Building the hub yourself instead: install Go, then run `go run build.go build syncthing` in the repository. The program is `./syncthing`.

---

## 2. Android phone

1. Unzip `keepsake-android-apk` and copy `app-debug.apk` to the phone (USB, Google Drive, email to yourself…).
2. Open the APK on the phone. Android asks you to allow installing unknown apps for the app you opened it with (Files, Chrome, …). Allow it, then tap **Install**.
   * Or, with USB debugging on: `adb install app-debug.apk`.
3. Open **Keepsake** and allow:
   * **Photos and videos**: needed to read your photos.
   * **All files access** (*Settings*, *Apps*, *Keepsake*, *Permissions*, *Files and media*, *Allow management of all files*). Recommended: Keepsake then syncs `DCIM/Camera` directly with no extra copy. Without it, Keepsake keeps its own copy and adds incoming photos to `DCIM/Keepsake`.
   * **Notifications**: for the "keeping your photos in sync" notice.
4. For reliable background syncing: *Settings*, *Apps*, *Keepsake*, *Battery*, **Unrestricted**.

This is a debug build signed with a development key. That's fine for your own phones. To publish on Google Play, see "Publishing" below.

Building the APK yourself instead: install Android Studio with the NDK, plus Go, then run:

```sh
mobile/build-bridge.sh android
cd mobile/android && gradle assembleDebug   # APK in app/build/outputs/apk/debug/
```

---

## 3. iPhone

There are three ways to install the iPhone app. Only the last one needs a Mac.

| Option | You need | How long an install lasts |
|---|---|---|
| **A. Sideload from Windows or Linux** (recommended to start) | A free Apple ID, a Windows PC (or Mac/Linux) and a USB cable | 7 days, then refresh (AltStore can do it automatically) |
| **B. TestFlight, built in the cloud** | A paid Apple Developer account ($99/year) | 90 days per build; every push to `main` uploads a new one |
| **C. Xcode on a Mac** | A Mac with Xcode | 7 days with a free Apple ID, 1 year with a paid one |

Whichever you use, finish with the [first launch](#first-launch-on-the-iphone) steps.

### A. Sideload from Windows or Linux (free Apple ID)

GitHub builds the app on a Mac in the cloud, and a sideloading tool on your computer signs it with your Apple ID and copies it to the phone.

1. Download the **`keepsake-ios-unsigned-ipa`** artifact (see [Getting the builds](#getting-the-builds)) and unzip it to get `Keepsake-unsigned.ipa`.
2. Install a sideloading tool:
   * **Sideloadly** (Windows, macOS): <https://sideloadly.io>. On Windows, also install **iTunes** and **iCloud** from Apple's website (not the Microsoft Store versions) so the PC can talk to the iPhone.
   * **AltStore** (Windows, macOS): <https://altstore.io>. AltServer on your PC re-signs the app every few days over Wi-Fi, so it doesn't expire as long as the PC is on now and then.
   * On Linux, use **AltServer-Linux** or **SideStore** (<https://sidestore.io>), which run without iTunes.
3. Connect the iPhone by USB, unlock it and tap **Trust** on the phone.
4. Drop `Keepsake-unsigned.ipa` into the tool, enter your Apple ID, and start the install.
   * Use a spare Apple ID if you prefer. Apple only uses it to sign the app.
   * If the tool reports that the bundle ID is taken, let it change it (Sideloadly: *Advanced options*, *Bundle ID*). Keepsake keeps working under any bundle ID.
5. On the iPhone, turn on **Developer Mode** (*Settings*, *Privacy & Security*, *Developer Mode*; the phone restarts), then trust your Apple ID: *Settings*, *General*, *VPN & Device Management*, your Apple ID, **Trust**.

Limits of a free Apple ID: the app stops opening after **7 days** until it is re-signed (open the tool and install again, or let AltStore refresh it). Your photos and settings stay. You can have at most three sideloaded apps at a time.

### B. TestFlight, built in the cloud (paid developer account)

GitHub signs the app with your developer account and uploads it to TestFlight. You install it with Apple's **TestFlight** app, like a normal App Store app. Everything is done in a web browser.

One-time setup:

1. Join the [Apple Developer Program](https://developer.apple.com/programs/) ($99/year). Note your **Team ID** (*Membership details* on developer.apple.com).
2. Register the app ID: on developer.apple.com, open *Certificates, Identifiers & Profiles*, then *Identifiers*, and add an App ID with bundle ID `app.keepsake.photos`.
   * If that ID is taken, pick your own (for example `com.yourname.keepsake`) and set it as the repository **variable** `IOS_BUNDLE_ID` (GitHub, *Settings*, *Secrets and variables*, *Actions*, *Variables*).
3. In [App Store Connect](https://appstoreconnect.apple.com), go to *Apps* and add a **New App**: platform iOS, name Keepsake, and the bundle ID from step 2. You don't have to submit it to the App Store.
4. In App Store Connect, go to *Users and Access*, then *Integrations*, then *App Store Connect API*. Create a **team key** with the **Admin** role and download the `.p8` file. Xcode needs Admin to create the signing certificate itself. Note the **Key ID** and **Issuer ID**.
5. In the GitHub repository, go to *Settings*, *Secrets and variables*, *Actions*, and add these **secrets**:

   | Secret | Value |
   |---|---|
   | `APPLE_TEAM_ID` | Your Team ID |
   | `ASC_KEY_ID` | The Key ID |
   | `ASC_ISSUER_ID` | The Issuer ID |
   | `ASC_KEY_P8` | The whole contents of the `.p8` file, including the `BEGIN` and `END` lines |

Then:

1. Run **Build Keepsake** from the Actions tab with **testflight** ticked. After that, every push to `main` uploads a new build.
2. After 5 to 15 minutes the build appears in App Store Connect under *TestFlight*. The first time, App Store Connect asks an export compliance question. Keepsake only uses standard encryption (TLS) for its connections; Apple's questionnaire explains which answer applies to that.
3. Under *TestFlight*, *Internal Testing*, add yourself (and anyone else on your team) as a tester.
4. On the iPhone, install **TestFlight** from the App Store, open the invite, and tap **Install**.

TestFlight builds expire after 90 days. A new push to `main` replaces them.

### C. Xcode on a Mac

You need a **Mac** with **Xcode 16** or newer, [Homebrew](https://brew.sh) and Go (`brew install go`).

1. Get the code: `git clone https://github.com/nichham2/syncthing-redo && cd syncthing-redo`
2. Build the sync engine and the Xcode project:
   ```sh
   mobile/build-bridge.sh ios
   brew install xcodegen
   cd mobile/ios && xcodegen && open Keepsake.xcodeproj
   ```
3. In Xcode, select the **Keepsake** target, open **Signing & Capabilities**, and set **Team** to your Apple ID.
   * Add the account under *Xcode*, *Settings*, *Accounts* if it isn't listed.
   * If Xcode says the bundle identifier is taken, change `app.keepsake.photos` to something unique, such as `com.yourname.keepsake`.
4. Connect the iPhone by USB and unlock it. Turn on **Developer Mode** (*Settings*, *Privacy & Security*, *Developer Mode*; the phone restarts).
5. Pick the iPhone as the run destination in Xcode's toolbar and press **Run** (▶).
6. With a free Apple ID, the first launch is blocked until you trust yourself as the developer: *Settings*, *General*, *VPN & Device Management*, your Apple ID, **Trust**.

With a free Apple ID the app stops opening after 7 days; press Run again to renew it.

### First launch on the iPhone

* Open **Keepsake** and allow **Full Access** to Photos.
* Allow **Local Network** when asked, so Keepsake can reach your other devices directly on Wi-Fi.
* Keep **Background App Refresh** on for Keepsake (*Settings*, *General*, *Background App Refresh*).

---

## 4. Pair everything

You pair each phone with the hub. The phones don't need to be paired with each other.

1. **On each phone:** open Keepsake, then **Devices**, then **Add a device**.
   * Paste the hub's device ID. To get it, point the camera at the QR code on the hub's *Show ID* page and copy the text.
   * Name it "Hub" and tap **Add and share photos**.
2. **On the hub** (<http://127.0.0.1:8384>), a banner appears for each phone:
   * "*Device … wants to connect*": click **Add device**, then **Save**.
   * "*… wants to share folder Camera Roll (photos)*": click **Add**.
     * For the first phone, choose where the hub stores the photos (for example `~/Keepsake Photos`) and leave *Keep photos on other devices when I delete them here* **off** on the hub, so it keeps a full archive.
     * For the second phone, the banner offers to add it to the existing `photos` folder. Accept.
3. Wait for the first sync. The **Keepsake** tab on each phone shows progress, then "Up to date".

The first sync copies every photo. Do it on Wi-Fi with the phones plugged in and the app open.

---

## Everyday use

* **Take photos as usual.** They reach the hub and then the other phone the next time each phone syncs: right away while the app is open, otherwise in the background when the OS allows it (Android about every 15 minutes, iPhone mostly overnight while charging).
* **Delete a photo on one phone.** It disappears only from that phone. It stays on the other phone and on the hub, and Keepsake never downloads it back. It's listed under **Ignored**.
* **Ignored tab:**
  * **Restore** brings photos back to this phone.
  * **Delete everywhere** removes them from every device. It asks for confirmation first, and the iPhone asks again when it has to delete from Photos.
* **Settings tab:** *Keep photos on other devices* is the switch for the "delete here, keep there" behaviour, and you can pause all syncing.

## Troubleshooting

| Problem | What to check |
|---|---|
| Phone stays "Waiting for your devices" | Is the hub running? Is the hub's ID correct on the phone? Did you accept the phone on the hub? |
| iPhone app won't open after a week | The free Apple ID 7-day limit. Install it again with your sideloading tool (or let AltStore refresh it), or press Run again in Xcode. |
| Android photos don't appear in Gallery | Grant *All files access*, or look in the `DCIM/Keepsake` album. |
| Android syncs rarely | Set battery usage to *Unrestricted*; keep Wi-Fi on. |
| Sync is slow on cellular | Pause syncing in Settings, or set Wi-Fi-only (default on Android). |

## Publishing (optional, later)

* **Google Play:** create a release signing key, add a `signingConfig` to `mobile/android/app/build.gradle.kts`, build with `gradle bundleRelease`, and declare the *All files access* use ("file sync / backup") in the Play Console.
* **App Store:** needs the paid developer account, App Store screenshots, and a privacy policy (Keepsake sends photos only to your own paired devices).
