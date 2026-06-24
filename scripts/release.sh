#!/usr/bin/env zsh
# Cut a signed + notarized release from this machine. Uses scripts/package-desktop.sh
# under the hood, then renames the artifacts under a versioned scheme and
# uploads them to a GitHub Release via `gh`.
#
#   zsh scripts/release.sh           # reads ./VERSION (canonical)
#   zsh scripts/release.sh v1.0.0    # verifies arg matches ./VERSION (drift guard)
#
# Prerequisites (one-time):
#   1. Apple Developer ID Application cert in your login Keychain. The script
#      auto-detects it; set OCTOBUDDY_SIGN_IDENTITY only when multiple "Developer
#      ID Application" certs are present and you want to pick one explicitly.
#   2. App Store Connect API key registered with notarytool under the profile
#      name "octobuddy-notary" (override with OCTOBUDDY_NOTARY_PROFILE):
#        xcrun notarytool store-credentials octobuddy-notary \
#          --key /path/AuthKey_XXXX.p8 --key-id ABCD1234EF --issuer <uuid>
#   3. `gh auth status` shows you logged in.
#
# No env exports in ~/.zshrc needed for the common case — the script picks
# both up automatically.
#
# What it builds + uploads (universal macOS .app + all daemon binaries):
#   - OctoBuddy-<ver>-macos-universal.zip   (signed + notarized + stapled)
#   - octobuddy-daemon-<ver>-darwin-arm64
#   - octobuddy-daemon-<ver>-darwin-amd64
#   - octobuddy-daemon-<ver>-linux-amd64
#   - octobuddy-daemon-<ver>-linux-arm64
#   - octobuddy-daemon-<ver>-windows-amd64.exe
#   - checksums.txt   (sha256 of every asset above)
#
# Re-runnable: the underlying tag must be unique (Apple's notary remembers
# digests), so bump the patch version if you need to retry.
set -euo pipefail

# Canonical version lives in /VERSION. Reading it here is the single source of
# truth — Info.plist's CFBundleShortVersionString gets stamped from this value
# at package time, so there is exactly one place to bump for a release.
repo_root="${0:A:h}/.."
if [[ ! -f "$repo_root/VERSION" ]]; then
  echo "✗ $repo_root/VERSION missing — create it (one line: MAJOR.MINOR.PATCH)" >&2
  exit 2
fi
file_ver=$(< "$repo_root/VERSION")
file_ver="${file_ver//[[:space:]]/}"

if [[ $# -eq 0 ]]; then
  ver="$file_ver"
  tag="v$ver"
elif [[ $# -eq 1 ]]; then
  tag="$1"
  if [[ "$tag" != v[0-9]*.[0-9]*.[0-9]* ]]; then
    echo "✗ tag must be vMAJOR.MINOR.PATCH (got $tag)" >&2
    exit 2
  fi
  ver="${tag#v}"
  # Drift guard: an explicit arg MUST match VERSION (the source of truth).
  # Bump VERSION first, commit, then run release.
  if [[ "$ver" != "$file_ver" ]]; then
    echo "✗ arg $tag does not match VERSION ($file_ver)" >&2
    echo "  bump VERSION first (single source of truth), commit, then re-run" >&2
    exit 2
  fi
else
  echo "usage: zsh scripts/release.sh [vX.Y.Z]" >&2
  echo "  no arg → read ./VERSION; arg → must match ./VERSION" >&2
  exit 2
fi
out="$repo_root/output"
stage="$out/release-$ver"

# --- resolve signing identity ---
# Auto-detect when exactly one "Developer ID Application" cert is in the
# Keychain — the typical single-developer case. An explicit OCTOBUDDY_SIGN_IDENTITY
# override wins (multi-team setups, switching personal/work certs).
if [[ -z "${OCTOBUDDY_SIGN_IDENTITY:-}" ]]; then
  identities=("${(@f)$(security find-identity -p codesigning -v 2>/dev/null \
    | awk -F'"' '/Developer ID Application/{print $2}')}")
  # awk emits nothing → array gets one empty element; strip.
  identities=("${(@)identities:#}")
  case ${#identities[@]} in
    0)
      echo "✗ no Developer ID Application identity found in Keychain." >&2
      echo "  Get one at developer.apple.com → Certificates, import it, then re-run." >&2
      exit 1
      ;;
    1)
      OCTOBUDDY_SIGN_IDENTITY="${identities[1]}"
      echo "▸ signing identity (auto): $OCTOBUDDY_SIGN_IDENTITY"
      ;;
 *)
      echo "✗ multiple Developer ID Application identities in Keychain:" >&2
      for id in "${identities[@]}"; do echo "    $id" >&2; done
      echo "  pass the one you want as OCTOBUDDY_SIGN_IDENTITY=… and re-run." >&2
      exit 1
      ;;
  esac
fi
export OCTOBUDDY_SIGN_IDENTITY

# --- resolve notary profile ---
# Default to a convention ("octobuddy-notary") set up once with
#   xcrun notarytool store-credentials octobuddy-notary --key … --key-id … --issuer …
# We don't probe the keychain item to verify — notarytool will surface a clear
# error at use-time, and probing would needlessly hit the network.
: "${OCTOBUDDY_NOTARY_PROFILE:=octobuddy-notary}"
export OCTOBUDDY_NOTARY_PROFILE
echo "▸ notary profile: $OCTOBUDDY_NOTARY_PROFILE"

command -v gh >/dev/null || { echo "✗ gh CLI required to publish releases"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "✗ run \`gh auth login\` first"; exit 1; }

# Refuse if the working tree is dirty — the release should reflect HEAD exactly.
if ! git -C "$repo_root" diff --quiet || ! git -C "$repo_root" diff --cached --quiet; then
  echo "✗ working tree has uncommitted changes — commit or stash before releasing" >&2
  exit 1
fi

# Tag (idempotent: if it already exists locally that's fine, but it MUST point at HEAD).
if git -C "$repo_root" rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  head_sha="$(git -C "$repo_root" rev-parse HEAD)"
  tag_sha="$(git -C "$repo_root" rev-parse "$tag^{commit}")"
  if [[ "$head_sha" != "$tag_sha" ]]; then
    echo "✗ tag $tag already exists at a different commit ($tag_sha) — bump the version or move the tag deliberately" >&2
    exit 1
  fi
else
  echo "▸ tagging HEAD as $tag"
  git -C "$repo_root" tag -a "$tag" -m "OctoBuddy $tag"
fi
git -C "$repo_root" push origin "$tag"

echo "▸ packaging (universal + sign + notarize)…"
OCTOBUDDY_UNIVERSAL=1 OCTOBUDDY_VERSION="$ver" zsh "$repo_root/scripts/package-desktop.sh"

echo "▸ staging release assets → $stage"
rm -rf "$stage"
mkdir -p "$stage"
cp "$out/OctoBuddy.zip"                "$stage/OctoBuddy-${ver}-macos-universal.zip"
cp "$out/octobuddy-daemon-darwin-arm64"      "$stage/octobuddy-daemon-${ver}-darwin-arm64"
cp "$out/octobuddy-daemon-darwin-amd64"      "$stage/octobuddy-daemon-${ver}-darwin-amd64"
cp "$out/octobuddy-daemon-linux-amd64"       "$stage/octobuddy-daemon-${ver}-linux-amd64"
cp "$out/octobuddy-daemon-linux-arm64"       "$stage/octobuddy-daemon-${ver}-linux-arm64"
cp "$out/octobuddy-daemon-windows-amd64.exe" "$stage/octobuddy-daemon-${ver}-windows-amd64.exe"
( cd "$stage" && shasum -a 256 ./* > checksums.txt )
ls -lh "$stage"

echo "▸ publishing GitHub Release $tag"
gh release create "$tag" \
  --repo "$(gh repo view --json nameWithOwner --jq .nameWithOwner)" \
  --title "OctoBuddy $tag" \
  --generate-notes \
  "$stage"/*

echo
echo "✓ released $tag"
echo "  $(gh release view "$tag" --json url --jq .url)"
