#!/usr/bin/env zsh
# Package the XClaw desktop app (Wails v3) into a distributable bundle with the
# xclawd daemon embedded and signed inside-out.
#
#   zsh scripts/package-desktop.sh                 # current macOS arch, ad-hoc signed
#   XCLAW_SIGN_IDENTITY="Apple Development: …" zsh scripts/package-desktop.sh
#   XCLAW_UNIVERSAL=1 …                            # mac universal (arm64+amd64)
#   XCLAW_NOTARY_PROFILE=my-profile …             # notarize (needs a Developer ID cert)
#
# macOS is the fully-supported target here. The Go daemon is zero-cgo and
# cross-compiles to win/linux too (built below for CI), but the Wails GUI binary
# for Windows/Linux must be produced on those platforms (native webview) — run
# `wails3 task package` there. See the tail of this script.
set -euo pipefail

repo_root="${0:A:h}/.."
desktop="$repo_root/desktop"
core="$repo_root/core"
out="$repo_root/output"
app_name="xclaw"
bundle="$desktop/bin/${app_name}.app"
sign_identity="${XCLAW_SIGN_IDENTITY:-}"
notary_profile="${XCLAW_NOTARY_PROFILE:-}"
universal="${XCLAW_UNIVERSAL:-}"
entitlements="$desktop/build/darwin/entitlements.plist"

export PATH="$(go env GOPATH)/bin:$PATH"
mkdir -p "$out"

echo "▸ cross-compiling xclawd (zero-cgo)…"
build_xclawd() { # $1=GOOS $2=GOARCH $3=out
  ( cd "$core" && CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -ldflags "-s -w" -o "$3" ./cmd/xclawd )
}
# Daemon binaries for all three platforms (CI picks these up for win/linux).
build_xclawd darwin  arm64 "$out/xclawd-darwin-arm64"
build_xclawd darwin  amd64 "$out/xclawd-darwin-amd64"
build_xclawd windows amd64 "$out/xclawd-windows-amd64.exe"
build_xclawd linux   amd64 "$out/xclawd-linux-amd64"
lipo -create -output "$out/xclawd" "$out/xclawd-darwin-arm64" "$out/xclawd-darwin-amd64"
echo "  ✓ xclawd (mac universal + win/linux in $out)"

echo "▸ building the Wails app (.app bundle)…"
if [[ -n "$universal" ]]; then
  ( cd "$desktop" && wails3 task package:universal )
else
  ( cd "$desktop" && wails3 task package )
fi
[[ -d "$bundle" ]] || { echo "✗ bundle not produced at $bundle"; exit 1; }

echo "▸ embedding xclawd at Contents/Helpers/xclawd…"
mkdir -p "$bundle/Contents/Helpers"
cp "$out/xclawd" "$bundle/Contents/Helpers/xclawd"
chmod +x "$bundle/Contents/Helpers/xclawd"

if [[ -n "$sign_identity" ]]; then
  echo "▸ signing inside-out with: $sign_identity"
  # Helper first (hardened runtime), then the app (hardened runtime + entitlements).
  codesign --force --options runtime --timestamp --sign "$sign_identity" \
    "$bundle/Contents/Helpers/xclawd"
  codesign --force --options runtime --timestamp \
    --entitlements "$entitlements" --sign "$sign_identity" "$bundle"
  codesign --verify --deep --strict --verbose=2 "$bundle"
else
  echo "▸ ad-hoc signing (no XCLAW_SIGN_IDENTITY)…"
  codesign --force --sign - "$bundle/Contents/Helpers/xclawd" 2>/dev/null || true
  codesign --force --sign - "$bundle" 2>/dev/null || true
fi

echo "▸ zipping → $out/XClaw.zip"
rm -f "$out/XClaw.zip"
ditto -c -k --keepParent "$bundle" "$out/XClaw.zip"

if [[ -n "$sign_identity" && -n "$notary_profile" ]]; then
  echo "▸ notarizing…"
  xcrun notarytool submit "$out/XClaw.zip" --keychain-profile "$notary_profile" --wait
  xcrun stapler staple -v "$bundle"
  rm -f "$out/XClaw.zip"
  ditto -c -k --keepParent "$bundle" "$out/XClaw.zip"
fi

echo
echo "✓ packaged:"
echo "  app:    $bundle"
echo "  zip:    $out/XClaw.zip"
echo "  daemon: $out/xclawd-{darwin-*,windows-amd64.exe,linux-amd64}"
echo
echo "Windows/Linux GUI: run 'cd desktop && wails3 task package' on the target OS,"
echo "then place the matching xclawd binary beside the app (Contents/Helpers on mac,"
echo "alongside the .exe on Windows, in the AppImage resources on Linux)."
