#!/bin/zsh
# Dev runner: build the Go core, place it where the Swift app's CorePaths can
# find it, then launch the app. The app's CoreSupervisor spawns the daemon in
# multi-bot config mode (~/.xclaw/config.json), connects the control bus, and
# the console window lists bots + streams sessions.
#
#   zsh scripts/run-dev.sh                 # launch the app (needs ~/.xclaw/config.json)
#   zsh scripts/run-dev.sh --seed-config   # write a starter ~/.xclaw config, then launch
#
# Without a config the app shows a "needs-config" state instead of running an
# empty daemon — seed one (or edit it by hand) to add bots.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
seed=false
[[ "${1:-}" == "--seed-config" ]] && seed=true

if $seed; then
  cfg_dir="$HOME/.xclaw"
  mkdir -p "$cfg_dir/default"
  if [[ ! -f "$cfg_dir/config.json" ]]; then
    cat > "$cfg_dir/config.json" <<'JSON'
{
  "apiUrl": "https://your-octo-server.example",
  "sdk": { "driver": "claude" },
  "bots": [ { "id": "default" } ]
}
JSON
    echo "▸ wrote $cfg_dir/config.json (edit apiUrl)"
  fi
  if [[ ! -f "$cfg_dir/default/config.json" ]]; then
    cat > "$cfg_dir/default/config.json" <<'JSON'
{ "octoToken": "bf_replace_me" }
JSON
    echo "▸ wrote $cfg_dir/default/config.json (edit octoToken)"
  fi
fi

echo "▸ building xclawd…"
( cd "$repo_root/core" && go build -o "$repo_root/core/.xclawd-dev" ./cmd/xclawd )
echo "  → $repo_root/core/.xclawd-dev"

echo "▸ launching XClawApp…"
# Run from repo root so CorePaths' dev-walk finds core/.xclawd-dev. Also export
# an explicit override for robustness.
export XCLAWD_BIN="$repo_root/core/.xclawd-dev"
cd "$repo_root"
swift run --package-path app XClawApp
