#!/usr/bin/env zsh
# Package the OctoBuddy desktop app (Wails v3) into a distributable bundle with the
# octobuddy-daemon daemon embedded and signed inside-out.
#
#   zsh scripts/package-desktop.sh                 # current macOS arch, ad-hoc signed
#   OCTOBUDDY_SIGN_IDENTITY="Apple Development: …" zsh scripts/package-desktop.sh
#   OCTOBUDDY_UNIVERSAL=1 …                            # mac universal (arm64+amd64)
#   OCTOBUDDY_VERSION=1.0.0 …                          # stamp CFBundleVersion + CFBundleShortVersionString in the bundle's Info.plist (release.sh sets this)
#   OCTOBUDDY_NOTARY_PROFILE=my-profile …              # notarize via keychain profile (local dev)
#   OCTOBUDDY_NOTARY_KEY_PATH=/path/AuthKey.p8 …       # notarize via App Store Connect API key (CI)
#     OCTOBUDDY_NOTARY_KEY_ID=ABCD1234EF                  + key id
#     OCTOBUDDY_NOTARY_ISSUER=12345678-...                + issuer uuid
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
app_name="octobuddy"
bundle="$desktop/bin/${app_name}.app"
sign_identity="${OCTOBUDDY_SIGN_IDENTITY:-}"
notary_profile="${OCTOBUDDY_NOTARY_PROFILE:-}"
notary_key_path="${OCTOBUDDY_NOTARY_KEY_PATH:-}"
notary_key_id="${OCTOBUDDY_NOTARY_KEY_ID:-}"
notary_issuer="${OCTOBUDDY_NOTARY_ISSUER:-}"
universal="${OCTOBUDDY_UNIVERSAL:-}"
version="${OCTOBUDDY_VERSION:-}"
# Fall back to the canonical /VERSION (single source of truth) so a manual
# `zsh scripts/package-desktop.sh` from a clean checkout stamps Info.plist
# with the same value the release script would use. Explicit env wins.
if [[ -z "$version" ]] && [[ -f "$repo_root/VERSION" ]]; then
  version=$(< "$repo_root/VERSION")
  version="${version//[[:space:]]/}"
fi
entitlements="$desktop/build/darwin/entitlements.plist"

export PATH="$(go env GOPATH)/bin:$PATH"
mkdir -p "$out"

echo "▸ cross-compiling octobuddy-daemon (zero-cgo)…"
build_octobuddy-daemon() { # $1=GOOS $2=GOARCH $3=out
  # -trimpath strips the operator's $HOME / module-cache from binary paths,
  # -buildvcs=false omits the local git-dirty flag — both required for any
  # third party trying to reproduce a release artifact byte-for-byte.
  ( cd "$core" && CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$3" ./cmd/octobuddy-daemon )
}
# Daemon binaries for all four platforms (CI picks these up for win/linux).
# Run the five cross-compiles in parallel — independent inputs, independent
# outputs, idle CPU cores otherwise. `wait` collects all PIDs and aborts the
# script via `set -e` if any background build returns non-zero.
build_octobuddy-daemon darwin  arm64 "$out/octobuddy-daemon-darwin-arm64"   &
build_octobuddy-daemon darwin  amd64 "$out/octobuddy-daemon-darwin-amd64"   &
build_octobuddy-daemon windows amd64 "$out/octobuddy-daemon-windows-amd64.exe" &
build_octobuddy-daemon linux   amd64 "$out/octobuddy-daemon-linux-amd64"    &
build_octobuddy-daemon linux   arm64 "$out/octobuddy-daemon-linux-arm64"    &
fail=0
for pid in $(jobs -p); do wait "$pid" || fail=1; done
(( fail == 0 )) || { echo "✗ one or more octobuddy-daemon cross-compiles failed"; exit 1; }
lipo -create -output "$out/octobuddy-daemon" "$out/octobuddy-daemon-darwin-arm64" "$out/octobuddy-daemon-darwin-amd64"
echo "  ✓ octobuddy-daemon (mac universal + win/linux in $out)"

# Bundle the latest octo-cli release (companion CLI the agent calls). Universal
# mac binary, sha256-verified against the release checksums.txt. Set
# OCTOBUDDY_SKIP_OCTO=1 to skip (e.g. offline). Needs the `gh` CLI.
octo_repo="Mininglamp-OSS/octo-cli"
octo_tag=""
if [[ -z "${OCTOBUDDY_SKIP_OCTO:-}" ]]; then
  command -v gh >/dev/null || { echo "✗ gh CLI required to bundle octo-cli (or set OCTOBUDDY_SKIP_OCTO=1)"; exit 1; }
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
  ( cd "$desktop" && wails3 task darwin:package:universal )
else
  ( cd "$desktop" && wails3 task darwin:package )
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

echo "▸ embedding octobuddy-daemon at Contents/Helpers/octobuddy-daemon…"
mkdir -p "$bundle/Contents/Helpers"
cp "$out/octobuddy-daemon" "$bundle/Contents/Helpers/octobuddy-daemon"
chmod +x "$bundle/Contents/Helpers/octobuddy-daemon"
if [[ -n "$octo_tag" ]]; then
  echo "▸ embedding octo-cli $octo_tag at Contents/Helpers/octo-cli…"
  cp "$out/octo-cli" "$bundle/Contents/Helpers/octo-cli"
  chmod +x "$bundle/Contents/Helpers/octo-cli"
  # Version marker goes in Resources (sealed as data) — NOT Helpers, where a
  # non-code file would break the bundle signature.
  mkdir -p "$bundle/Contents/Resources"
  cp "$out/octo-cli.version" "$bundle/Contents/Resources/octo-cli.version"
  # The SHA-256 sidecar is computed AFTER the inside-out codesign step below —
  # signing rewrites the binary's signature region, so hashing it pre-sign
  # would store a digest that the installed binary no longer matches and
  # EnsureInstalled would fail-closed on every launch.
fi

if [[ -n "$sign_identity" ]]; then
  echo "▸ signing inside-out with: $sign_identity"
  # Helpers first (hardened runtime), then the app (hardened runtime + entitlements).
  codesign --force --options runtime --timestamp --sign "$sign_identity" \
    "$bundle/Contents/Helpers/octobuddy-daemon"
  [[ -n "$octo_tag" ]] && codesign --force --options runtime --timestamp --sign "$sign_identity" \
    "$bundle/Contents/Helpers/octo-cli"
  # SHA-256 sidecar (round 11 Sec) — EnsureInstalled verifies the helper
  # against this before copying it to ~/.octobuddy/bin. Without it, anyone with
  # write access to Helpers/ (admin, tampered .zip, post-install attacker)
  # could swap the binary and have it silently executed as the user on
  # next launch. Sits in Resources so it's sealed as data, not signed code.
  # MUST run after the Helpers codesign above and before the outer app
  # codesign so the digest matches the on-disk signed binary AND gets sealed
  # into the bundle's outer signature.
  [[ -n "$octo_tag" ]] && \
    ( cd "$bundle/Contents/Helpers" && shasum -a 256 octo-cli ) > "$bundle/Contents/Resources/octo-cli.sha256"
  codesign --force --options runtime --timestamp \
    --entitlements "$entitlements" --sign "$sign_identity" "$bundle"
  codesign --verify --deep --strict --verbose=2 "$bundle"
else
  echo "▸ ad-hoc signing (no OCTOBUDDY_SIGN_IDENTITY)…"
  codesign --force --sign - "$bundle/Contents/Helpers/octobuddy-daemon" 2>/dev/null || true
  [[ -n "$octo_tag" ]] && codesign --force --sign - "$bundle/Contents/Helpers/octo-cli" 2>/dev/null || true
  # Sidecar after ad-hoc signing, same reason as the Developer-ID branch.
  [[ -n "$octo_tag" ]] && \
    ( cd "$bundle/Contents/Helpers" && shasum -a 256 octo-cli ) > "$bundle/Contents/Resources/octo-cli.sha256"
  codesign --force --sign - "$bundle" 2>/dev/null || true
fi

echo "▸ zipping → $out/OctoBuddy.zip"
rm -f "$out/OctoBuddy.zip"
ditto -c -k --keepParent "$bundle" "$out/OctoBuddy.zip"

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
  xcrun notarytool submit "$out/OctoBuddy.zip" "${notarize_args[@]}" --wait
  xcrun stapler staple -v "$bundle"
  rm -f "$out/OctoBuddy.zip"
  ditto -c -k --keepParent "$bundle" "$out/OctoBuddy.zip"
fi

echo
echo "✓ packaged:"
echo "  app:    $bundle"
echo "  zip:    $out/OctoBuddy.zip"
echo "  daemon: $out/octobuddy-daemon-{darwin-*,windows-amd64.exe,linux-amd64}"
echo
echo "Windows/Linux GUI: run 'cd desktop && wails3 task package' on the target OS,"
echo "then place the matching octobuddy-daemon binary beside the app (Contents/Helpers on mac,"
echo "alongside the .exe on Windows, in the AppImage resources on Linux)."
