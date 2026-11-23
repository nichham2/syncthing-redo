#!/usr/bin/env bash
# Builds the Keepsake sync engine (./lib/mobile) for the phone apps.
#
#   mobile/build-bridge.sh android   -> mobile/android/app/libs/keepsake-core.aar
#   mobile/build-bridge.sh ios       -> mobile/ios/Mobile.xcframework   (macOS + Xcode)
#
# Needs Go (version from go.mod), plus the Android NDK (ANDROID_HOME /
# ANDROID_NDK_HOME) for android or Xcode for ios.
set -euo pipefail

target="${1:-}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

# Pinned so builds are reproducible and a new upstream commit can't change
# the app unnoticed. Bump deliberately.
XMOBILE_VERSION=v0.0.0-20261117202452-47699113848a

go install "golang.org/x/mobile/cmd/gomobile@$XMOBILE_VERSION" "golang.org/x/mobile/cmd/gobind@$XMOBILE_VERSION"
export PATH="$(go env GOPATH)/bin:$PATH"
gomobile init

# gomobile needs golang.org/x/mobile/bind in the module graph. Add it for
# the build only and restore go.mod/go.sum afterwards.
cp go.mod go.mod.bak; cp go.sum go.sum.bak
trap 'mv go.mod.bak go.mod; mv go.sum.bak go.sum' EXIT
go get "golang.org/x/mobile/bind@$XMOBILE_VERSION"

# Embed the dashboard (gui/) into the binary.
go run build.go assets

case "$target" in
android)
    mkdir -p mobile/android/app/libs
    gomobile bind -target=android/arm64,android/amd64 -androidapi 29 \
        -ldflags="-s -w -checklinkname=0" -o mobile/android/app/libs/keepsake-core.aar ./lib/mobile
    echo "Built mobile/android/app/libs/keepsake-core.aar"
    ;;
ios)
    gomobile bind -target=ios,iossimulator -iosversion 16.0 \
        -ldflags="-s -w" -o mobile/ios/Mobile.xcframework ./lib/mobile
    echo "Built mobile/ios/Mobile.xcframework"
    ;;
*)
    echo "usage: $0 android|ios" >&2
    exit 2
    ;;
esac
