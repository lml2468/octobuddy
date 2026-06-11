#!/bin/zsh
# Package XClaw into a distributable macOS .app: build the Go core (xclawd) and
# the Swift app in release, assemble the bundle (xclawd in Contents/Helpers/),
# write Info.plist, sign (Developer ID if configured, else ad-hoc), and produce
# a .zip and (if create-dmg is available) a .dmg.
#
#   zsh scripts/package-app.sh
#
# Env:
#   XCLAW_SIGN_IDENTITY   Developer ID Application identity (else ad-hoc)
#   XCLAW_NOTARY_PROFILE  notarytool keychain profile (requires signing)
#   XCLAW_VERSION         CFBundleShortVersionString (default 0.1.0)
#   XCLAW_UNIVERSAL=true  build a universal (arm64 + x86_64) xclawd + app
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "XClaw packaging runs only on macOS." >&2
    exit 1
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
app_name="XClaw"
bundle_id="${XCLAW_BUNDLE_ID:-app.xclaw.dev}"
version="${XCLAW_VERSION:-0.1.0}"
build_number="${XCLAW_BUILD_NUMBER:-$(git -C "$repo_root" rev-list --count HEAD 2>/dev/null || echo 1)}"
sign_identity="${XCLAW_SIGN_IDENTITY:-}"
notary_profile="${XCLAW_NOTARY_PROFILE:-}"
universal="${XCLAW_UNIVERSAL:-false}"

out_dir="$repo_root/output"
bundle_dir="$out_dir/$app_name.app"
zip_path="$out_dir/$app_name.zip"
dmg_path="$out_dir/$app_name.dmg"
entitlements="$repo_root/app/Packaging/XClawApp.entitlements"

echo "▸ building xclawd (Go core, release)…"
if [[ "$universal" == "true" ]]; then
    ( cd "$repo_root/core" && \
      CGO_ENABLED=0 GOARCH=arm64 go build -ldflags "-s -w" -o "$out_dir/xclawd-arm64" ./cmd/xclawd && \
      CGO_ENABLED=0 GOARCH=amd64 go build -ldflags "-s -w" -o "$out_dir/xclawd-amd64" ./cmd/xclawd )
    lipo -create -output "$out_dir/xclawd" "$out_dir/xclawd-arm64" "$out_dir/xclawd-amd64"
    rm -f "$out_dir/xclawd-arm64" "$out_dir/xclawd-amd64"
else
    ( cd "$repo_root/core" && CGO_ENABLED=0 go build -ldflags "-s -w" -o "$out_dir/xclawd" ./cmd/xclawd )
fi
xclawd_bin="$out_dir/xclawd"

echo "▸ building XClawApp (Swift, release)…"
arch_flags=()
[[ "$universal" == "true" ]] && arch_flags=(--arch arm64 --arch x86_64)
( cd "$repo_root/app" && swift build -c release "${arch_flags[@]}" --product XClawApp )
app_bin="$(cd "$repo_root/app" && swift build -c release "${arch_flags[@]}" --show-bin-path)/XClawApp"

echo "▸ assembling $app_name.app…"
rm -rf "$bundle_dir" "$zip_path" "$dmg_path"
mkdir -p "$bundle_dir/Contents/MacOS" "$bundle_dir/Contents/Helpers" "$bundle_dir/Contents/Resources"
cp "$app_bin" "$bundle_dir/Contents/MacOS/XClawApp"
cp "$xclawd_bin" "$bundle_dir/Contents/Helpers/xclawd"
chmod +x "$bundle_dir/Contents/MacOS/XClawApp" "$bundle_dir/Contents/Helpers/xclawd"

# App icon (generate with app/Packaging/make-appicon.sh).
app_icon="$repo_root/app/Packaging/AppIcon.icns"
icon_key=""
if [[ -f "$app_icon" ]]; then
    cp "$app_icon" "$bundle_dir/Contents/Resources/AppIcon.icns"
    icon_key=$'\n    <key>CFBundleIconFile</key><string>AppIcon</string>'
fi

cat > "$bundle_dir/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleDevelopmentRegion</key><string>en</string>
    <key>CFBundleDisplayName</key><string>$app_name</string>
    <key>CFBundleExecutable</key><string>XClawApp</string>$icon_key
    <key>CFBundleIdentifier</key><string>$bundle_id</string>
    <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
    <key>CFBundleName</key><string>$app_name</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>$version</string>
    <key>CFBundleVersion</key><string>$build_number</string>
    <key>LSMinimumSystemVersion</key><string>14.0</string>
    <key>LSUIElement</key><true/>
    <key>NSHighResolutionCapable</key><true/>
    <key>NSPrincipalClass</key><string>NSApplication</string>
</dict>
</plist>
EOF
plutil -lint "$bundle_dir/Contents/Info.plist" >/dev/null

# --- verify structure ---
for required in "Contents/MacOS/XClawApp" "Contents/Helpers/xclawd" "Contents/Info.plist"; do
    [[ -e "$bundle_dir/$required" ]] || { echo "ERROR: missing $required" >&2; exit 1; }
done
echo "  bundle structure verified."

# --- sign (helpers inside-out: xclawd → app) ---
if [[ -n "$sign_identity" ]]; then
    echo "▸ signing with $sign_identity…"
    codesign --force --options runtime --timestamp --sign "$sign_identity" \
        "$bundle_dir/Contents/Helpers/xclawd"
    codesign --force --options runtime --timestamp \
        --entitlements "$entitlements" --sign "$sign_identity" "$bundle_dir"
    codesign --verify --deep --strict --verbose=2 "$bundle_dir"
else
    echo "▸ ad-hoc signing (no XCLAW_SIGN_IDENTITY)…"
    codesign --force --sign - "$bundle_dir/Contents/Helpers/xclawd" 2>/dev/null || true
    codesign --force --sign - "$bundle_dir" 2>/dev/null || true
fi

# --- zip ---
ditto -c -k --keepParent "$bundle_dir" "$zip_path"

# --- notarize (signed only) ---
if [[ -n "$sign_identity" && -n "$notary_profile" ]]; then
    echo "▸ notarizing…"
    xcrun notarytool submit "$zip_path" --keychain-profile "$notary_profile" --wait
    xcrun stapler staple -v "$bundle_dir"
    rm -f "$zip_path"
    ditto -c -k --keepParent "$bundle_dir" "$zip_path"
fi

# --- DMG (optional) ---
if command -v create-dmg >/dev/null 2>&1; then
    echo "▸ building DMG…"
    create-dmg --volname "$app_name" --window-size 540 360 --icon-size 96 \
        --icon "$app_name.app" 140 180 --app-drop-link 400 180 \
        --no-internet-enable "$dmg_path" "$bundle_dir" || \
        echo "  (create-dmg failed; .zip is still available)"
    if [[ -f "$dmg_path" && -n "$sign_identity" ]]; then
        codesign --force --sign "$sign_identity" --timestamp "$dmg_path"
        [[ -n "$notary_profile" ]] && { xcrun notarytool submit "$dmg_path" --keychain-profile "$notary_profile" --wait; xcrun stapler staple -v "$dmg_path"; }
    fi
else
    echo "  (create-dmg not installed — skipping DMG; install with: brew install create-dmg)"
fi

echo
echo "✓ packaged:"
echo "  bundle: $bundle_dir"
echo "  zip:    $zip_path"
[[ -f "$dmg_path" ]] && echo "  dmg:    $dmg_path"
[[ -z "$sign_identity" ]] && echo "  (ad-hoc signed — for distribution set XCLAW_SIGN_IDENTITY + XCLAW_NOTARY_PROFILE)"
