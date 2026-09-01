#!/usr/bin/env bash
# Make sure the relay is answering, exactly once, however many editors launch.
#
# This is the only hook here that runs a command rather than posting HTTP,
# because Claude Code refuses HTTP hooks for SessionStart. It starts one thing:
# the daemon this plugin exists to talk to. It never stops it - the daemon
# exits on its own once no session has used it for a while, which is the only
# way to get that right with several editors open.
#
# Always exits 0 and never blocks. A session that cannot reach the relay simply
# has no advisory context, and that is not a reason to hold up somebody's work.
set -u

ADDR="${SHOULDER_ADDR:-127.0.0.1:8787}"
answering() { curl -sf --max-time 1 -o /dev/null "http://${ADDR}/healthz" 2>/dev/null; }

answering && exit 0

# Two editors launched together would otherwise both see nothing listening and
# both start one. mkdir is atomic on every filesystem this runs on; whoever
# loses the race waits briefly for the winner rather than racing to bind.
LOCK="${XDG_RUNTIME_DIR:-/tmp}/shoulder-daemon.start.lock"
if ! mkdir "${LOCK}" 2>/dev/null; then
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    sleep 0.3
    answering && exit 0
  done
  exit 0
fi
trap 'rmdir "${LOCK}" 2>/dev/null || true' EXIT

# A stale lock from a killed launch would otherwise wedge every later start.
( sleep 30; rmdir "${LOCK}" 2>/dev/null || true ) >/dev/null 2>&1 &

if [ -n "${SHOULDER_START_CMD:-}" ]; then
  ( eval "${SHOULDER_START_CMD}" ) >/dev/null 2>&1 &
elif command -v shoulderd >/dev/null 2>&1; then
  ( nohup shoulderd >/dev/null 2>&1 & ) >/dev/null 2>&1
else
  echo "shoulder-daemon: nothing answering at ${ADDR}, and no 'shoulderd' on PATH." >&2
  echo "shoulder-daemon: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest" >&2
fi
exit 0
