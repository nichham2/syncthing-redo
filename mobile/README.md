# Photo Sync mobile apps

* [`ARCHITECTURE.md`](ARCHITECTURE.md): how the deletion ledger, the mobile bridge and the dashboard fit together.
* `../lib/mobile`: Go bridge, built with gomobile (see below).
* `ios/PhotoSync`: Swift sources for the iOS app target (PhotoKit bridge, background tasks, dashboard host, Info.plist keys).
* `android/app`: Kotlin sources and manifest for the Android app (MediaStore bridge, WorkManager, foreground service, dashboard host).
* `../gui/default/mobile`: the shared dashboard UI, served by the core at `/mobile/`.

## Building the bridge

```sh
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init
gomobile bind -target=ios     -o Syncthing.xcframework ./lib/mobile   # needs Xcode
gomobile bind -target=android -o syncthing.aar         ./lib/mobile   # needs the Android NDK
```

Add `Syncthing.xcframework` to the Xcode target with the Swift files in `ios/PhotoSync`,
or add `syncthing.aar` as a dependency of the Android module with the Kotlin files
(dependencies: `androidx.work:work-runtime-ktx`, `androidx.activity`, `androidx.core`,
`androidx.lifecycle:lifecycle-runtime-ktx`).

The native sources are a starting point for the app projects. They are not compiled
in this repository's CI.

## Trying the dashboard on a desktop build

```sh
go run build.go build syncthing
./syncthing serve
# open http://127.0.0.1:8384/mobile/ in a phone-sized browser window
```

Enable "Keep photos on other devices" for a folder in the dashboard's Settings tab,
or set `nonDestructiveDeletes` in the folder config.
