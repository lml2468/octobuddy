# Releasing XClaw

This is the operator-facing checklist for cutting a signed + notarized
release. XClaw releases are built locally on your Mac (no CI involved) — the
operator already has the Developer ID cert + Apple notary credential on
hand, and a single-operator product doesn't get much from offloading the
build to GitHub runners.

Everything is wrapped in `scripts/release.sh`. Once the one-time setup below
is done, cutting a release is `zsh scripts/release.sh v1.0.0`.

## One-time setup

### 1. Apple Developer ID Application certificate

You need a paid Apple Developer account (`developer.apple.com`).

1. Generate a **Developer ID Application** certificate in
   `developer.apple.com → Certificates`. NOT "Apple Development" — that's
   testing-only and won't notarize.
2. Open the downloaded `.cer` to import into your login Keychain.
3. Find your identity string:
   ```bash
   security find-identity -p codesigning -v
   ```
   You want the line that looks like
   `Developer ID Application: Your Name (TEAMID)`.
4. Export it for the release script:
   ```bash
   echo 'export XCLAW_SIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"' >> ~/.zshrc
   ```

### 2. App Store Connect API key for notarization

1. Go to
   `appstoreconnect.apple.com → Users and Access → Integrations → App Store
   Connect API`.
2. Create a key with the **Developer** role; download the `.p8` (only
   downloadable ONCE — save it somewhere safe).
3. Note the **Key ID** (e.g. `ABCD1234EF`) and the **Issuer UUID**.
4. Register with `notarytool`:
   ```bash
   xcrun notarytool store-credentials xclaw-notary \
     --key /path/to/AuthKey_XXXX.p8 \
     --key-id ABCD1234EF \
     --issuer <issuer-uuid>
   ```
5. Export the profile name for the release script:
   ```bash
   echo 'export XCLAW_NOTARY_PROFILE=xclaw-notary' >> ~/.zshrc
   ```

### 3. GitHub CLI

```bash
brew install gh
gh auth login
```

## Cutting a release

1. Make sure `main` is green on CI.
2. Make sure the working tree is clean (the script refuses otherwise).
3. Pick a semver tag (`vMAJOR.MINOR.PATCH`).
4. Run:
   ```bash
   zsh scripts/release.sh v1.0.0
   ```
   The script will:
   - Tag HEAD (if not already tagged) and push the tag.
   - Build the universal `.app`, embed `xclawd` + `octo-cli`, sign inside-out
     with your Developer ID, notarize via the API key, staple, re-zip.
   - Cross-compile `xclawd` for darwin/linux/windows (5 binaries).
   - Stage everything under versioned filenames + a `checksums.txt`.
   - `gh release create` with auto-generated notes.
5. End-to-end takes ~5–15 min (mostly notary queue wait).
6. The release lands at
   `https://github.com/lml2468/xclaw/releases/tag/v1.0.0` with:
   - `XClaw-<ver>-macos-universal.zip` — signed + notarized .app (both archs)
   - `xclawd-<ver>-{darwin-arm64,darwin-amd64,linux-amd64,linux-arm64,windows-amd64.exe}`
     — headless daemon binaries for non-Mac platforms
   - `checksums.txt` — SHA256 of every asset above
7. Hand-edit the release body if you want a human summary at the top
   (auto-generated notes are usually good enough).

## Verifying a build locally before cutting a tag

If you want to sanity-check the .app without releasing it:

```bash
XCLAW_UNIVERSAL=1 zsh scripts/package-desktop.sh
spctl --assess --type execute -vv desktop/bin/xclaw.app
codesign --verify --deep --strict --verbose=2 desktop/bin/xclaw.app
```

## Troubleshooting

- **"User interaction is not allowed" during codesign** — your login keychain
  is locked or the cert's partition list excludes `codesign`. Unlock the
  keychain and retry.
- **`notarytool` returns `Invalid`** — download the log it points at and fix
  whichever helper failed. Most common: a helper (xclawd, octo-cli) wasn't
  signed with the hardened runtime; the script signs both inside-out so this
  usually means a stale build artifact crept in. Try
  `rm -rf desktop/bin && zsh scripts/release.sh …` again.
- **"The provided entity includes an attribute with a value that has already
  been used"** when re-tagging the same version — Apple's notary service
  remembers the .zip digest. Bump the patch version and re-tag.
- **Tag already exists at a different commit** — the script refuses to move
  a tag silently. Either bump the version or delete the tag locally and on
  origin first if you really mean to redo it.
