#!/usr/bin/env zsh
# Cut a signed + notarized release from this machine. Uses scripts/package-desktop.sh
# under the hood, then renames the artifacts under a versioned scheme and
# uploads them to a GitHub Release via `gh`.
#
#   zsh scripts/release.sh v1.0.0
#
# Prerequisites (one-time):
#   1. Apple Developer ID Application cert in your login Keychain.
#      Identity string lives in $XCLAW_SIGN_IDENTITY (or pass via env each run):
#        export XCLAW_SIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
#   2. App Store Connect API key registered with notarytool:
#        xcrun notarytool store-credentials xclaw-notary \
#          --key /path/AuthKey_XXXX.p8 --key-id ABCD1234EF --issuer <uuid>
#      Then: export XCLAW_NOTARY_PROFILE=xclaw-notary
#   3. `gh auth status` shows you logged in.
#
# What it builds + uploads (universal macOS .app + all daemon binaries):
#   - XClaw-<ver>-macos-universal.zip   (signed + notarized + stapled)
#   - xclawd-<ver>-darwin-arm64
#   - xclawd-<ver>-darwin-amd64
#   - xclawd-<ver>-linux-amd64
#   - xclawd-<ver>-linux-arm64
#   - xclawd-<ver>-windows-amd64.exe
#   - checksums.txt   (sha256 of every asset above)
#
# Re-runnable: the underlying tag must be unique (Apple's notary remembers
# digests), so bump the patch version if you need to retry.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: zsh scripts/release.sh vX.Y.Z" >&2
  exit 2
fi

tag="$1"
if [[ "$tag" != v[0-9]*.[0-9]*.[0-9]* ]]; then
  echo "✗ tag must be vMAJOR.MINOR.PATCH (got $tag)" >&2
  exit 2
fi
ver="${tag#v}"

repo_root="${0:A:h}/.."
out="$repo_root/output"
stage="$out/release-$ver"

: "${XCLAW_SIGN_IDENTITY:?set XCLAW_SIGN_IDENTITY to your Developer ID Application identity string}"
: "${XCLAW_NOTARY_PROFILE:?set XCLAW_NOTARY_PROFILE to the keychain profile from \`xcrun notarytool store-credentials\`}"

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
  git -C "$repo_root" tag -a "$tag" -m "XClaw $tag"
fi
git -C "$repo_root" push origin "$tag"

echo "▸ packaging (universal + sign + notarize)…"
XCLAW_UNIVERSAL=1 XCLAW_VERSION="$ver" zsh "$repo_root/scripts/package-desktop.sh"

echo "▸ staging release assets → $stage"
rm -rf "$stage"
mkdir -p "$stage"
cp "$out/XClaw.zip"                "$stage/XClaw-${ver}-macos-universal.zip"
cp "$out/xclawd-darwin-arm64"      "$stage/xclawd-${ver}-darwin-arm64"
cp "$out/xclawd-darwin-amd64"      "$stage/xclawd-${ver}-darwin-amd64"
cp "$out/xclawd-linux-amd64"       "$stage/xclawd-${ver}-linux-amd64"
cp "$out/xclawd-linux-arm64"       "$stage/xclawd-${ver}-linux-arm64"
cp "$out/xclawd-windows-amd64.exe" "$stage/xclawd-${ver}-windows-amd64.exe"
( cd "$stage" && shasum -a 256 ./* > checksums.txt )
ls -lh "$stage"

echo "▸ publishing GitHub Release $tag"
gh release create "$tag" \
  --repo "$(gh repo view --json nameWithOwner --jq .nameWithOwner)" \
  --title "XClaw $tag" \
  --generate-notes \
  "$stage"/*

echo
echo "✓ released $tag"
echo "  $(gh release view "$tag" --json url --jq .url)"
