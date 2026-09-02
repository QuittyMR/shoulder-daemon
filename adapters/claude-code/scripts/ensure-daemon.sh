#!/usr/bin/env bash
# Make sure the relay is answering, exactly once, however many editors launch.
#
# It runs at session start and again before every prompt. The second is the
# whole recovery story: the daemon stops when the last session it knows about
# ends, and a daemon that restarted a moment ago knows about one editor however
# many are open, so it can and does exit under a session that is still working.
# Probing a port costs a millisecond and puts the daemon back rather than
# leaving the rest of the session unobserved. It is a command hook because
# Claude Code refuses HTTP hooks for SessionStart, and because an HTTP hook
# cannot start anything.
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

# A lock left behind by a launch that died wedges every start that follows, for
# good: the daemon never comes back and nothing says why. It cannot be cleaned
# up by a background reaper either - the harness kills a hook's process group
# the moment the hook returns, so anything sleeping in the background dies with
# it. So the lock is broken on age instead: one older than a minute belongs to a
# launch that is not coming back.
take() {
  mkdir "${LOCK}" 2>/dev/null && return 0
  if [ -n "$(find "${LOCK}" -maxdepth 0 -mmin +1 2>/dev/null)" ]; then
    rmdir "${LOCK}" 2>/dev/null || true
    mkdir "${LOCK}" 2>/dev/null && return 0
  fi
  return 1
}

if ! take; then
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    sleep 0.3
    answering && exit 0
  done
  exit 0
fi
trap 'rmdir "${LOCK}" 2>/dev/null || true' EXIT

if [ -n "${SHOULDER_START_CMD:-}" ]; then
  ( eval "${SHOULDER_START_CMD}" ) >/dev/null 2>&1 &
elif command -v shoulderd >/dev/null 2>&1; then
  ( nohup shoulderd >/dev/null 2>&1 & ) >/dev/null 2>&1
else
  echo "shoulder-daemon: nothing answering at ${ADDR}, and no 'shoulderd' on PATH." >&2
  echo "shoulder-daemon: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest" >&2
fi
exit 0
