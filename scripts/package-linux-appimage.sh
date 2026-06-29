#!/usr/bin/env bash
# Package the Linux desktop build as an AppImage.
#
# Output: output/OctoBuddy-${arch}.AppImage  (and .AppDir on the side)
#
# Inputs (auto-detected; override via env):
#   OCTOBUDDY_VERSION  Default reads /VERSION; used for AppImage filename + .desktop X-AppImage-Version.
#   ARCH               Default: $(uname -m). Drives the appimagetool runtime arch + filename suffix.
#
# Prerequisites on the host:
#   - wails3 CLI (only for the optional `wails3 task build`; we do the
#     equivalent inline via `go build` to keep the script self-contained).
#   - GTK4 + libwebkitgtk-6.0 dev packages (Wails runtime deps; ubuntu CI
#     installs them, see .github/workflows/ci.yml).
#   - The script downloads appimagetool on first run if not already present
#     under .bin/ (pinned version for reproducible CI).
#
# Why an AppImage at all: Linux users today need to either build from
# source or run the raw octobuddy binary alongside octobuddy-daemon. An
# AppImage is a single executable file that bundles both binaries, the
# .desktop launcher, the icon, and a small launcher wrapper — double-click
# parity with the macOS .app and the Windows .exe distribution.
#
# Distribution: this script produces an UNSIGNED AppImage. The AppImage
# format does support GPG signing; that's a follow-on (PR-?). For now,
# downstream consumers should verify the SHA-256 alongside the artifact.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
desktop_dir="$repo_root/desktop"
core_dir="$repo_root/core"
out_dir="$repo_root/output"

# Shared build helpers (resolve_version, build_octobuddy_daemon,
# build_frontend, wait_for_pids). Each previously had its own copy here.
# shellcheck source=lib/build-common.sh
source "$repo_root/scripts/lib/build-common.sh"
version="$(resolve_version "$repo_root")"

arch="${ARCH:-$(uname -m)}"
# appimagetool's runtime naming uses x86_64 / aarch64 — translate amd64 / arm64
# so a `GOARCH=amd64` invocation lands the right file.
case "$arch" in
  amd64) arch=x86_64 ;;
  arm64) arch=aarch64 ;;
esac

mkdir -p "$out_dir"
tool_dir="$out_dir/.bin"
mkdir -p "$tool_dir"

# Resolve appimagetool — fetch on first run, cache under output/.bin for
# subsequent CI runs (cached by actions/cache or just re-downloaded).
tool="$tool_dir/appimagetool-$arch.AppImage"
if [[ ! -x "$tool" ]]; then
  echo "▸ fetching appimagetool ($arch)…"
  url="https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-$arch.AppImage"
  curl -fsSL -o "$tool" "$url"
  chmod +x "$tool"
fi

# Daemon (zero-cgo so it's a fully static ELF that runs anywhere) and the
# Wails frontend bundle are independent — run them in parallel and wait,
# same idiom as package-desktop.sh's 5-arch daemon fan-out. The Wails Go
# binary depends on the frontend bundle (it //go:embed-s frontend/dist),
# so the cgo desktop build still follows.
goarch="${GOARCH:-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
echo "▸ building octobuddy-daemon + frontend in parallel…"
build_pids=()
build_octobuddy_daemon "$core_dir" linux "$goarch" "$out_dir/octobuddy-daemon-linux" & build_pids+=($!)
build_frontend "$desktop_dir" & build_pids+=($!)
wait_for_pids "linux build phase" "${build_pids[@]}"

# Desktop binary (cgo on — links GTK4 + WebKitGTK natively).
echo "▸ building octobuddy (Wails desktop)…"
( cd "$desktop_dir" && CGO_ENABLED=1 GOOS=linux \
    go build -tags production -trimpath -buildvcs=false -ldflags "-s -w" \
    -o "$out_dir/octobuddy-linux" . )

# Assemble the AppDir. The freedesktop layout is rigid: AppRun (executed
# when the AppImage runs), <id>.desktop at the top level, and the icon at
# the top level. usr/bin/ holds the actual binaries.
appdir="$out_dir/OctoBuddy.AppDir"
rm -rf "$appdir"
mkdir -p "$appdir/usr/bin"

cp "$out_dir/octobuddy-linux" "$appdir/usr/bin/octobuddy"
cp "$out_dir/octobuddy-daemon-linux" "$appdir/usr/bin/octobuddy-daemon"
chmod +x "$appdir/usr/bin/octobuddy" "$appdir/usr/bin/octobuddy-daemon"

# Icon: top-level + usr/share/icons/hicolor/512x512/apps for desktop
# integration when a user uses AppImageLauncher or similar to install it.
cp "$desktop_dir/build/appicon.png" "$appdir/octobuddy.png"
mkdir -p "$appdir/usr/share/icons/hicolor/512x512/apps"
cp "$desktop_dir/build/appicon.png" "$appdir/usr/share/icons/hicolor/512x512/apps/octobuddy.png"

cat > "$appdir/octobuddy.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=OctoBuddy
GenericName=Agent Gateway
Comment=Cross-platform agent gateway desktop
Exec=octobuddy
Icon=octobuddy
Terminal=false
Categories=Development;Utility;
StartupWMClass=octobuddy
X-AppImage-Version=$version
X-AppImage-Name=OctoBuddy
EOF

# AppRun: the script the AppImage runs when launched. Points OCTOBUDDY_DAEMON_BIN
# at the bundled daemon so the desktop binary's supervisor finds it without
# expecting a system-wide install or sibling-file layout (Linux ELF binaries
# can't introspect their own location via $0 reliably the way macOS bundles
# can — set the env var here so the supervisor's lookup is deterministic).
cat > "$appdir/AppRun" <<'EOF'
#!/usr/bin/env bash
HERE="$(dirname "$(readlink -f "${0}")")"
export PATH="$HERE/usr/bin:$PATH"
export OCTOBUDDY_DAEMON_BIN="$HERE/usr/bin/octobuddy-daemon"
exec "$HERE/usr/bin/octobuddy" "$@"
EOF
chmod +x "$appdir/AppRun"

# Build the AppImage. ARCH= is what appimagetool stamps into the runtime;
# matches the arch suffix in the filename. --no-appstream skips the
# appstream metadata validation (we don't ship a .appdata.xml yet —
# follow-on if we ever target distro repos).
echo "▸ packaging AppImage…"
artifact="$out_dir/OctoBuddy-${version}-${arch}.AppImage"
ARCH="$arch" "$tool" --no-appstream "$appdir" "$artifact" >/dev/null

# Sidecar checksum so consumers can verify the integrity of the unsigned
# binary. (Signing is a follow-on PR.)
( cd "$out_dir" && shasum -a 256 "$(basename "$artifact")" > "$(basename "$artifact").sha256" )

echo
echo "✓ AppImage built:"
echo "  $artifact"
echo "  $artifact.sha256"
