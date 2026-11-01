#!/usr/bin/env bash
# Post one hook event to the relay and hand its answer back to the harness.
#
# Every per-event hook runs this instead of an HTTP hook, because an HTTP hook
# that cannot connect only prints an error and nothing acts on it. Here the
# error is the trigger: when the relay does not answer, the start script runs,
# so the first event that finds the relay gone is also the one that brings it
# back. A relay that is up costs one request and a print.
#
# Always exits 0 and never blocks. Only a 200 body is printed: that is the
# relay's hook response, and anything else on stdout would be read as one.
set -u

ADDR="${SHOULDER_ADDR:-127.0.0.1:8787}"
EVENT="${1:?hook event name}"
HERE="$(cd "$(dirname "$0")" && pwd)"
TOKEN="${SHOULDER_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  TOKEN="$(cat "${XDG_DATA_HOME:-$HOME/.local/share}/shoulder-daemon/token" 2>/dev/null)"
fi

# The prompt hook is the one the relay may hold while it asks the model about
# the prompt; every other hook answers from memory. The hook's own timeout in
# hooks.json is 5 seconds, and this stays under it.
MAX_TIME=2
if [ "$EVENT" = UserPromptSubmit ]; then
  MAX_TIME=4
fi

answer="$(curl -s --max-time "$MAX_TIME" -w '\n%{http_code}' \
  -H "X-Shoulder-Token: ${TOKEN}" -H 'Content-Type: application/json' \
  --data-binary @- "http://${ADDR}/v1/hooks/claude-code/${EVENT}" 2>/dev/null)"
rc=$?
code="$(printf '%s' "$answer" | tail -n 1)"

if [ "$rc" -ne 0 ]; then
  "$HERE/ensure-daemon.sh" >/dev/null 2>&1 || true
  exit 0
fi
if [ "$code" = 200 ]; then
  printf '%s' "$answer" | sed '$d'
fi
exit 0
