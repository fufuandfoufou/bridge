#!/bin/bash
# Builds a universal Bridge.app and dist/Bridge-<version>.dmg.
# SIGN_IDENTITY selects the code signing identity; the default "-" is ad-hoc,
# which runs on this Mac. Distribution needs a Developer ID and notarization:
# set NOTARY_KEY_FILE, NOTARY_KEY_ID and NOTARY_ISSUER (App Store Connect API
# key) to submit the dmg to Apple. VERSION overrides the version in Info.plist.
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(/usr/libexec/PlistBuddy -c 'Print CFBundleShortVersionString' app/Info.plist)}"
SIGN_IDENTITY="${SIGN_IDENTITY:--}"
# Signing fails on extended attributes macOS adds under ~/Documents, so the
# bundle is assembled in a temporary directory.
BUILD=$(mktemp -d)
trap 'rm -rf "$BUILD"' EXIT
APP="$BUILD/Bridge.app"
DMG="dist/Bridge-$VERSION.dmg"

rm -f "$DMG"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" dist

echo "==> core"
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$BUILD/bridge-$arch" ./cmd/bridge
done
lipo -create -output "$APP/Contents/Resources/bridge" "$BUILD/bridge-arm64" "$BUILD/bridge-amd64"

echo "==> ui"
for target in arm64-apple-macos13.0 x86_64-apple-macos13.0; do
  swiftc -O -swift-version 5 -parse-as-library -target "$target" -sdk "$(xcrun --show-sdk-path)" \
    -framework SwiftUI -framework AppKit -o "$BUILD/Bridge-$target" app/Sources/*.swift
done
lipo -create -output "$APP/Contents/MacOS/Bridge" "$BUILD"/Bridge-*-apple-macos13.0
cp app/Info.plist "$APP/Contents/Info.plist"
cp -R app/en.lproj app/zh-Hans.lproj "$APP/Contents/Resources/"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" -c "Set :CFBundleVersion $VERSION" "$APP/Contents/Info.plist"

echo "==> icon"
ICONSET="$BUILD/AppIcon.iconset"
mkdir -p "$ICONSET"
swift scripts/make-icon.swift app/logo.png "$BUILD/icon.png" "$APP/Contents/Resources/MenuBarIcon.png"
for s in 16 32 128 256 512; do
  sips -z $s $s "$BUILD/icon.png" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  sips -z $((s * 2)) $((s * 2)) "$BUILD/icon.png" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns -o "$APP/Contents/Resources/AppIcon.icns" "$ICONSET"

echo "==> sign ($SIGN_IDENTITY)"
xattr -cr "$APP"
# Notarization requires a secure timestamp; ad-hoc signatures can't have one.
TIMESTAMP=(--timestamp=none)
[ "$SIGN_IDENTITY" != "-" ] && TIMESTAMP=(--timestamp)
codesign --force --options runtime "${TIMESTAMP[@]}" -s "$SIGN_IDENTITY" "$APP/Contents/Resources/bridge"
codesign --force --options runtime "${TIMESTAMP[@]}" -s "$SIGN_IDENTITY" "$APP"
codesign --verify --strict "$APP"

echo "==> dmg"
STAGE="$BUILD/dmg"
mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
hdiutil create -quiet -volname "Bridge" -srcfolder "$STAGE" -format UDZO -ov "$DMG"
hdiutil verify -quiet "$DMG"

if [ "$SIGN_IDENTITY" != "-" ]; then
  codesign --force "${TIMESTAMP[@]}" -s "$SIGN_IDENTITY" "$DMG"
fi

if [ -n "${NOTARY_KEY_FILE:-}" ]; then
  echo "==> notarize"
  xcrun notarytool submit "$DMG" --key "$NOTARY_KEY_FILE" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" --wait --timeout 30m
  xcrun stapler staple "$DMG"
  xcrun stapler validate "$DMG"
  spctl --assess --type open --context context:primary-signature --verbose "$DMG"
fi

echo "built $DMG"
