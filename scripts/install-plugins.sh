#!/usr/bin/env bash
# Replace each harness's installed copy of the adapter with this checkout.
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

# --- Claude Code: a versioned copy under the plugin cache, named in a registry
CC_SRC="$REPO/adapters/claude-code"
REG="$HOME/.claude/plugins/installed_plugins.json"
if [ -f "$REG" ]; then
  VER=$(python3 -c "import json;print(json.load(open('$CC_SRC/.claude-plugin/plugin.json'))['version'])")
  DST="$HOME/.claude/plugins/cache/shoulder-daemon/shoulder-daemon/$VER"
  rm -rf "$DST"; mkdir -p "$DST"; cp -r "$CC_SRC"/. "$DST"/
  python3 - "$VER" "$DST" "$CC_SRC" <<'PY'
import json, os, sys
ver, dst, src = sys.argv[1], sys.argv[2], sys.argv[3]
mk = os.path.expanduser('~/.claude/plugins/known_marketplaces.json')
d = json.load(open(mk))
d['shoulder-daemon'] = {'source': {'source': 'directory', 'path': src}, 'installLocation': src}
json.dump(d, open(mk, 'w'), indent=2)
reg = os.path.expanduser('~/.claude/plugins/installed_plugins.json')
d = json.load(open(reg))
d['plugins']['shoulder-daemon@shoulder-daemon'] = [
    {'scope': 'user', 'installPath': dst, 'version': ver}]
json.dump(d, open(reg, 'w'), indent=2)
PY
  echo "claude-code: installed $VER -> $DST"
else
  echo "claude-code: not installed, skipping"
fi

# --- OpenCode: one file, loaded in place
OC_DST="$HOME/.config/opencode/plugins"
if [ -d "$HOME/.config/opencode" ]; then
  mkdir -p "$OC_DST"
  cp "$REPO/adapters/opencode/shoulder-daemon.js" "$OC_DST/shoulder-daemon.js"
  echo "opencode:    installed -> $OC_DST/shoulder-daemon.js"
else
  echo "opencode:    not installed, skipping"
fi
