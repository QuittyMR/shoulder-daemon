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

# --- Environment: the part that has to know where this checkout is.
# Every path-dependent setting is rewritten from $REPO on each install, so a
# moved or renamed checkout is repaired by running this again rather than by
# hunting stale absolute paths through two config files.
ENV_FILE="${SHOULDER_ENV_FILE:-$HOME/.config/shoulder-daemon/env}"
mkdir -p "$(dirname "$ENV_FILE")"
touch "$ENV_FILE"; chmod 600 "$ENV_FILE"

set_var() {
  local key="$1" val="$2"
  if grep -q "^$key=" "$ENV_FILE"; then
    python3 - "$ENV_FILE" "$key" "$val" <<'PY'
import sys
path, key, val = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(path).read().splitlines()
out = [f'{key}="{val}"' if l.startswith(key + '=') else l for l in lines]
open(path, 'w').write("\n".join(out) + "\n")
PY
  else
    printf '%s="%s"\n' "$key" "$val" >> "$ENV_FILE"
  fi
}

set_var SHOULDER_START_CMD "make -C $REPO up"
if ! grep -q '^SHOULDER_TOKEN=' "$ENV_FILE"; then
  # The daemon injects text into a live coding session, and anything that can
  # reach 127.0.0.1 can post to it - including a page open in your browser, for
  # which localhost is not special. Generated rather than prompted for, because
  # a setup step nobody performs is a daemon running with no authentication.
  set_var SHOULDER_TOKEN "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  echo "env:         generated SHOULDER_TOKEN"
fi
echo "env:         $ENV_FILE"

# Claude Code interpolates ${SHOULDER_TOKEN} into its hook URLs from its own env
# block, and reads SHOULDER_START_CMD from there to boot the daemon, so those
# values have to exist in both places.
if [ -f "$HOME/.claude/settings.json" ]; then
  python3 - "$ENV_FILE" <<'PY'
import collections, json, os, re, sys
envfile = sys.argv[1]
want = {}
for line in open(envfile):
    m = re.match(r'^\s*(SHOULDER_[A-Z0-9_]+)\s*=\s*(.*)$', line)
    if m:
        want[m.group(1)] = m.group(2).strip().strip('"\'')
want['SHOULDER_ENV_FILE'] = envfile

p = os.path.expanduser('~/.claude/settings.json')
d = json.load(open(p), object_pairs_hook=collections.OrderedDict)
env = d.setdefault('env', collections.OrderedDict())
changed = []
for k in ('SHOULDER_TOKEN', 'SHOULDER_START_CMD', 'SHOULDER_ENV_FILE'):
    if k in want and env.get(k) != want[k]:
        env[k] = want[k]
        changed.append(k)
if changed:
    json.dump(d, open(p, 'w'), indent=2)
    open(p, 'a').write("\n")
print("claude-code: settings env " + (", ".join(changed) if changed else "already current"))
PY
fi
