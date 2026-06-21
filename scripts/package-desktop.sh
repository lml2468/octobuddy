#!/usr/bin/env zsh
# Package the XClaw desktop app (Wails v3) into a distributable bundle with the
# xclawd daemon embedded and signed inside-out.
#
#   zsh scripts/package-desktop.sh                 # current macOS arch, ad-hoc signed
#   XCLAW_SIGN_IDENTITY="Apple Development: …" zsh scripts/package-desktop.sh
#   XCLAW_UNIVERSAL=1 …                            # mac universal (arm64+amd64)
#   XCLAW_VERSION=1.0.0 …                          # stamp CFBundleVersion + CFBundleShortVersionString in the bundle's Info.plist (release.sh sets this)
#   XCLAW_NOTARY_PROFILE=my-profile …              # notarize via keychain profile (local dev)
#   XCLAW_NOTARY_KEY_PATH=/path/AuthKey.p8 …       # notarize via App Store Connect API key (CI)
#     XCLAW_NOTARY_KEY_ID=ABCD1234EF                  + key id
#     XCLAW_NOTARY_ISSUER=12345678-...                + issuer uuid
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
notary_key_path="${XCLAW_NOTARY_KEY_PATH:-}"
notary_key_id="${XCLAW_NOTARY_KEY_ID:-}"
notary_issuer="${XCLAW_NOTARY_ISSUER:-}"
universal="${XCLAW_UNIVERSAL:-}"
version="${XCLAW_VERSION:-}"
entitlements="$desktop/build/darwin/entitlements.plist"

export PATH="$(go env GOPATH)/bin:$PATH"
mkdir -p "$out"

echo "▸ cross-compiling xclawd (zero-cgo)…"
build_xclawd() { # $1=GOOS $2=GOARCH $3=out
  ( cd "$core" && CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -ldflags "-s -w" -o "$3" ./cmd/xclawd )
}
# Daemon binaries for all four platforms (CI picks these up for win/linux).
build_xclawd darwin  arm64 "$out/xclawd-darwin-arm64"
build_xclawd darwin  amd64 "$out/xclawd-darwin-amd64"
build_xclawd windows amd64 "$out/xclawd-windows-amd64.exe"
build_xclawd linux   amd64 "$out/xclawd-linux-amd64"
build_xclawd linux   arm64 "$out/xclawd-linux-arm64"
lipo -create -output "$out/xclawd" "$out/xclawd-darwin-arm64" "$out/xclawd-darwin-amd64"
echo "  ✓ xclawd (mac universal + win/linux in $out)"

# Bundle the latest octo-cli release (companion CLI the agent calls). Universal
# mac binary, sha256-verified against the release checksums.txt. Set
# XCLAW_SKIP_OCTO=1 to skip (e.g. offline). Needs the `gh` CLI.
octo_repo="Mininglamp-OSS/octo-cli"
octo_tag=""
if [[ -z "${XCLAW_SKIP_OCTO:-}" ]]; then
  command -v gh >/dev/null || { echo "✗ gh CLI required to bundle octo-cli (or set XCLAW_SKIP_OCTO=1)"; exit 1; }
  echo "▸ fetching latest octo-cli release…"
  octo_tag="$(gh api repos/$octo_repo/releases/latest --jq .tag_name)"
  octo_ver="${octo_tag#v}"
  octo_tmp="$out/octo-tmp"; rm -rf "$octo_tmp"; mkdir -p "$octo_tmp"
  gh release download "$octo_tag" --repo "$octo_repo" --pattern checksums.txt --dir "$octo_tmp" --clobber
  fetch_octo() { # $1=arch
    local name="octo-cli_${octo_ver}_darwin_$1.tar.gz"
    gh release download "$octo_tag" --repo "$octo_repo" --pattern "$name" --dir "$octo_tmp" --clobber
    ( cd "$octo_tmp" && grep -F "  $name" checksums.txt | shasum -a 256 -c - )
    mkdir -p "$octo_tmp/$1"
    tar -xzf "$octo_tmp/$name" -C "$octo_tmp/$1" octo-cli
  }
  fetch_octo arm64
  fetch_octo amd64
  lipo -create -output "$out/octo-cli" "$octo_tmp/arm64/octo-cli" "$octo_tmp/amd64/octo-cli"
  printf '%s\n' "$octo_tag" > "$out/octo-cli.version"
  echo "  ✓ octo-cli $octo_tag (mac universal)"
fi

echo "▸ building the Wails app (.app bundle)…"
if [[ -n "$universal" ]]; then
  ( cd "$desktop" && wails3 task package:universal )
else
  ( cd "$desktop" && wails3 task package )
fi
[[ -d "$bundle" ]] || { echo "✗ bundle not produced at $bundle"; exit 1; }

# Stamp the bundle's Info.plist with the release version so Finder → Get Info
# and About dialogs reflect what we actually shipped (the source Info.plist
# carries a fallback that we leave alone). Done BEFORE signing so the seal
# covers the final plist content. plutil's -replace is idempotent.
if [[ -n "$version" ]]; then
  echo "▸ stamping Info.plist with version $version"
  plutil -replace CFBundleShortVersionString -string "$version" "$bundle/Contents/Info.plist"
  plutil -replace CFBundleVersion            -string "$version" "$bundle/Contents/Info.plist"
fi

echo "▸ embedding xclawd at Contents/Helpers/xclawd…"
mkdir -p "$bundle/Contents/Helpers"
cp "$out/xclawd" "$bundle/Contents/Helpers/xclawd"
chmod +x "$bundle/Contents/Helpers/xclawd"
if [[ -n "$octo_tag" ]]; then
  echo "▸ embedding octo-cli $octo_tag at Contents/Helpers/octo-cli…"
  cp "$out/octo-cli" "$bundle/Contents/Helpers/octo-cli"
  chmod +x "$bundle/Contents/Helpers/octo-cli"
  # Version marker goes in Resources (sealed as data) — NOT Helpers, where a
  # non-code file would break the bundle signature.
  mkdir -p "$bundle/Contents/Resources"
  cp "$out/octo-cli.version" "$bundle/Contents/Resources/octo-cli.version"
fi

if [[ -n "$sign_identity" ]]; then
  echo "▸ signing inside-out with: $sign_identity"
  # Helpers first (hardened runtime), then the app (hardened runtime + entitlements).
  codesign --force --options runtime --timestamp --sign "$sign_identity" \
    "$bundle/Contents/Helpers/xclawd"
  [[ -n "$octo_tag" ]] && codesign --force --options runtime --timestamp --sign "$sign_identity" \
    "$bundle/Contents/Helpers/octo-cli"
  codesign --force --options runtime --timestamp \
    --entitlements "$entitlements" --sign "$sign_identity" "$bundle"
  codesign --verify --deep --strict --verbose=2 "$bundle"
else
  echo "▸ ad-hoc signing (no XCLAW_SIGN_IDENTITY)…"
  codesign --force --sign - "$bundle/Contents/Helpers/xclawd" 2>/dev/null || true
  [[ -n "$octo_tag" ]] && codesign --force --sign - "$bundle/Contents/Helpers/octo-cli" 2>/dev/null || true
  codesign --force --sign - "$bundle" 2>/dev/null || true
fi

echo "▸ zipping → $out/XClaw.zip"
rm -f "$out/XClaw.zip"
ditto -c -k --keepParent "$bundle" "$out/XClaw.zip"

# Notarize via either a stored keychain profile (local dev, set up with
# `xcrun notarytool store-credentials`) or an App Store Connect API key trio
# (CI: a .p8 + key id + issuer uuid, with the key file dropped on disk just for
# this run). On success we staple the ticket and re-zip — the stapled .app
# survives offline first-launch Gatekeeper checks.
notarize_args=()
if [[ -n "$sign_identity" ]]; then
  if [[ -n "$notary_key_path" && -n "$notary_key_id" && -n "$notary_issuer" ]]; then
    notarize_args=(--key "$notary_key_path" --key-id "$notary_key_id" --issuer "$notary_issuer")
  elif [[ -n "$notary_profile" ]]; then
    notarize_args=(--keychain-profile "$notary_profile")
  fi
fi
if (( ${#notarize_args[@]} > 0 )); then
  echo "▸ notarizing…"
  xcrun notarytool submit "$out/XClaw.zip" "${notarize_args[@]}" --wait
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
