#!/usr/bin/env bash
# scripts/lib/build-common.sh — shared build pipeline for the package +
# release scripts. Sourced (not executed) by both zsh (package-desktop.sh,
# release.sh) and bash (package-linux-appimage.sh, CI), so every helper
# below sticks to POSIX-compatible syntax.
#
# Why this file exists: before extraction, every script that built the
# daemon, resolved the canonical version, or waited on background jobs
# carried its own copy. The first drift (CI's macOS .app version stamp
# vs. the Linux AppImage filename version) was already fixed once by
# hand. This file is the answer to "where do I change that next time?"
#
# Conventions:
# - Functions echo progress to stderr (>&2) so caller-captured stdout
#   (e.g. `ver=$(resolve_version "$root")`) stays clean.
# - No global state; every function takes its inputs as args.

# resolve_version returns the canonical version string for the build:
#   1. $OCTOBUDDY_VERSION (env override; release.sh sets this).
#   2. The contents of $repo_root/VERSION (single source of truth).
#   3. "0.0.0-dev" fallback (clean checkout, no release stamp).
# Whitespace is trimmed so a stray trailing newline in VERSION doesn't
# corrupt downstream filename + Info.plist stamping.
resolve_version() {
  local repo_root="${1:?repo_root required}"
  local v="${OCTOBUDDY_VERSION:-}"
  if [ -z "$v" ] && [ -f "$repo_root/VERSION" ]; then
    v="$(< "$repo_root/VERSION")"
    v="${v//[[:space:]]/}"
  fi
  if [ -z "$v" ]; then
    v="0.0.0-dev"
  fi
  printf '%s\n' "$v"
}

# build_octobuddy_daemon cross-compiles the daemon. Identical flags across
# every caller — anything that needs to change ("-trimpath", "-ldflags",
# embed a version, etc.) changes once here. Zero cgo so the output runs
# everywhere; -trimpath strips the operator's $HOME / module-cache from
# binary paths and -buildvcs=false omits the local git-dirty flag, both
# required for byte-reproducible release artifacts.
#
# Args: $1=core_dir $2=GOOS $3=GOARCH $4=out_path
build_octobuddy_daemon() {
  local core_dir="${1:?core_dir required}"
  local goos="${2:?GOOS required}"
  local goarch="${3:?GOARCH required}"
  local out_path="${4:?out_path required}"
  ( cd "$core_dir" \
    && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
       go build -trimpath -buildvcs=false -ldflags "-s -w" \
       -o "$out_path" ./cmd/octobuddy-daemon )
}

# build_frontend runs `npm ci && npm run build` in the desktop frontend
# directory. Extracted so it can be invoked from a `&` background job
# without callers learning npm semantics.
#
# Args: $1=desktop_dir
build_frontend() {
  local desktop_dir="${1:?desktop_dir required}"
  ( cd "$desktop_dir/frontend" && npm ci --silent && npm run build )
}

# wait_for_pids waits on an explicit list of background PIDs and aborts (exit 1)
# if any returned non-zero. Pass the label as $1, then the PIDs.
#
# Why explicit PIDs instead of `jobs -p`: in zsh, `$(jobs -p)` runs the builtin
# in a command-substitution SUBSHELL that sees NONE of the parent shell's
# background jobs, so the old `for pid in $(jobs -p)` loop silently waited on
# nothing and returned immediately — letting a downstream `lipo`/`cp` race
# against still-building outputs (stale daemon embedded in the .app). Capturing
# `$!` right after each `&` at the call site is correct in both bash and zsh.
wait_for_pids() {
  local label="${1:-background jobs}"; shift
  local fail=0 pid
  for pid in "$@"; do
    wait "$pid" || fail=1
  done
  if [ "$fail" -ne 0 ]; then
    echo "✗ one or more $label failed" >&2
    exit 1
  fi
}
