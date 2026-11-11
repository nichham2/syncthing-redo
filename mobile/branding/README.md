# Keepsake brand

| | |
|---|---|
| Name | **Keepsake** |
| Tagline | Your photos on all your phones. Delete here, keep there. |
| Primary | `#2457D6` (Keepsake blue) |
| Accent | `#F5A524` (ribbon amber) |
| Icon | `keepsake-icon.svg`: a photo frame with a bookmark ribbon, meaning "kept" |

The icon is used for the Android adaptive launcher icon
(`android/app/src/main/res/drawable/ic_launcher_foreground.xml`), the iOS app
icon (`ios/Keepsake/Assets.xcassets/AppIcon.appiconset`) and the dashboard
favicon (`gui/default/mobile/keepsake-icon.svg`).

To rename the app, change `keepsake.appName` / `keepsake.applicationId` in
`android/gradle.properties`, `PRODUCT_NAME` / `PRODUCT_BUNDLE_IDENTIFIER` /
`CFBundleDisplayName` in `ios/project.yml`, and the `<title>` and brand text in
`gui/default/mobile/index.html`.

Keepsake is built on the open-source Syncthing engine (MPL-2.0). "Syncthing"
is a trademark of the Syncthing Foundation, which is why the app ships under
its own name; keep the "built on Syncthing" credit and the license notices.
