#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Builds Diskspace.app (universal), then dist/diskspace-desktop_macos_universal.{dmg,zip}.
# Requires macOS with Xcode command line tools, Go, rcodesign, and web/dist
# already built. Signs, notarizes and staples the app and the dmg; the zip is
# made from the stapled app. Env: VERSION, plus the credentials documented in
# sign-macos.sh and notarize-macos.sh (all required: there is no unsigned mode).
#
# Usage: package-mac.sh build     compile and assemble the unsigned app
#        package-mac.sh package   sign, notarize, staple, make dmg + zip
# (split so the workflow can keep credentials off disk while third-party
# build steps run).
set -euo pipefail

mode="${1:-all}"
case "$mode" in build | package | all) ;; *) echo "usage: package-mac.sh [build|package]" >&2; exit 2 ;; esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [[ ! -d gui ]] || ! ls gui/*.go >/dev/null 2>&1; then
  echo "error: ./gui package not found; cannot build the macOS app" >&2
  exit 1
fi
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "error: package-mac.sh must run on macOS" >&2
  exit 1
fi

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
version="${version#v}"
build="$root/dist/mac-build"
app="$build/Diskspace.app"

if [[ "$mode" != package ]]; then
rm -rf "$build"
mkdir -p "$build" "$root/dist"

ldflags="-s -w -X main.version=$version -X main.defaultRelay=https://diskspace.cc"
export CGO_ENABLED=1
export CGO_LDFLAGS="-framework UniformTypeIdentifiers"

echo "Building arm64"
GOOS=darwin GOARCH=arm64 go build -trimpath -tags desktop,production -ldflags "$ldflags" -o "$build/Diskspace-arm64" ./gui
echo "Building amd64"
CC="clang -arch x86_64" GOOS=darwin GOARCH=amd64 go build -trimpath -tags desktop,production -ldflags "$ldflags" -o "$build/Diskspace-amd64" ./gui

mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
lipo -create -output "$app/Contents/MacOS/Diskspace" "$build/Diskspace-arm64" "$build/Diskspace-amd64"
chmod +x "$app/Contents/MacOS/Diskspace"

icon_key=""
if [[ -f gui/build/appicon.icns ]]; then
  cp gui/build/appicon.icns "$app/Contents/Resources/icon.icns"
  icon_key="<key>CFBundleIconFile</key><string>icon</string>"
fi

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>cc.diskspace.app</string>
  <key>CFBundleName</key><string>Diskspace</string>
  <key>CFBundleDisplayName</key><string>Diskspace</string>
  <key>CFBundleExecutable</key><string>Diskspace</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
  $icon_key
</dict>
</plist>
PLIST
fi

if [[ "$mode" == build ]]; then
  echo "Built unsigned app: $app"
  exit 0
fi
if [[ ! -d "$app" ]]; then
  echo "error: $app missing; run package-mac.sh build first" >&2
  exit 1
fi

"$root/scripts/sign-macos.sh" "$app"
"$root/scripts/notarize-macos.sh" --staple "$app"
codesign --verify --deep --strict --verbose=2 "$app"
xcrun stapler validate "$app"
spctl --assess --type execute --verbose=2 "$app"

dmg="$root/dist/diskspace-desktop_macos_universal.dmg"
zip="$root/dist/diskspace-desktop_macos_universal.zip"
rm -f "$dmg" "$zip"

# Both containers are built from the stapled app.
stage="$build/dmg"
mkdir -p "$stage"
cp -R "$app" "$stage/"
ln -s /Applications "$stage/Applications"
hdiutil create -volname Diskspace -srcfolder "$stage" -ov -format UDZO "$dmg"
"$root/scripts/sign-macos.sh" "$dmg"
"$root/scripts/notarize-macos.sh" --staple "$dmg"
xcrun stapler validate "$dmg"

ditto -c -k --keepParent "$app" "$zip"

rm -rf "$build"
echo "Built: $dmg $zip"
