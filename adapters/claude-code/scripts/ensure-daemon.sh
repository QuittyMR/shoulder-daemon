#!/usr/bin/env bash
# Make sure the relay is answering and can reach its store, exactly once,
# however many editors launch.
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
# With --fetch, which only SessionStart passes, a machine with no shoulderd at
# all gets the release binary for its platform, checksum-verified, under the
# user's data directory. That is what makes the plugin the whole install. The
# per-prompt hook never downloads: its timeout is five seconds and a fetch that
# ran there would be killed half-way, every prompt, forever.
#
# Always exits 0 and never blocks. A session that cannot reach the relay simply
# has no advisory context, and that is not a reason to hold up somebody's work.
set -u

ADDR="${SHOULDER_ADDR:-127.0.0.1:8787}"
DATA="${XDG_DATA_HOME:-$HOME/.local/share}/shoulder-daemon"
BIN="$DATA/bin/shoulderd"
RELEASES="${SHOULDER_RELEASE_BASE:-https://github.com/QuittyMR/shoulder-daemon/releases/latest/download}"
FETCH=0
[ "${1:-}" = "--fetch" ] && FETCH=1
TMP=""

# The relay answers /healthz with an untroubled ok whether or not it can still
# reach the memory store behind it, so a relay whose store has gone away serves
# on happily while every recall and every write behind it fails; real sessions
# have run for days in that state with nothing to show for themselves. /readyz
# is the question this hook is actually asking. The status code is read rather
# than left to curl's exit status because up-but-unwell and not-there-at-all
# want different things here, and -f reports both as the same failure. The body
# comes back in the same breath because 503 itself covers two faults that want
# opposite things, and asking a second time to tell them apart would put another
# second in front of every prompt somebody types.
probe() { curl -s -w '\n%{http_code}' --max-time 1 "http://${ADDR}/readyz" 2>/dev/null; }
say() { echo "shoulder-daemon: $*" >&2; }

# recently answers whether a stamp names a moment inside the last n seconds. The
# moment is the file's contents and not its mtime, because reading an mtime
# portably needs a stat whose flags differ between GNU and BSD, and a floor that
# cannot be read is no floor at all. Anything that is not a number reads as long
# ago, as does a stamp dated in the future: that is a clock that moved rather
# than something that just happened, and waiting one of those out could take
# hours. Both are overwritten by the next attempt, so neither wedges anything.
recently() {
  local last since
  last="$(cat "$1" 2>/dev/null)"
  case "${last:-none}" in
    *[!0-9]*) last=0 ;;
  esac
  since=$(( $(date +%s) - last ))
  [ "$since" -ge 0 ] && [ "$since" -lt "$2" ]
}

mark() { date +%s >"$1" 2>/dev/null || true; }

ANSWER="$(probe)"
STATE="$(printf '%s' "$ANSWER" | tail -n 1)"

# A 404 is a relay built before /readyz existed. It may be in perfect health and
# nothing out here can tell, so guessing that it is not would hand everybody who
# has not upgraded yet a start command before every prompt they type. An unknown
# readiness is left alone exactly as a known-good one is, which is what this
# hook did back when all it ever asked about was /healthz.
case "$STATE" in
  200|404) exit 0 ;;
esac

# 503 is two faults wearing one status code. A store that is unreachable is
# something that died and the start command below puts it back. A relay with no
# memory backend configured has nothing to put back: it is a machine that was
# never finished being set up, and starting the stack on it every thirty seconds
# for the rest of the session is the same storm the floor exists to prevent,
# only slower and with no chance of ever ending. So this one is said and left
# alone - no lock, no start, no stamp. The body is matched as a string because a
# hook with a five second budget has no business loading a JSON parser to read a
# one-word answer; the relay writes it through encoding/json and so puts no
# space after the colon, but a body is somebody else's output and the spaced
# form costs one more pattern to accept.
#
# Saying it before every prompt would be its own kind of noise, so the same
# stamp mechanism holds it to once every five minutes: often enough that it is
# still on screen when somebody goes looking for why nothing is being
# remembered, rare enough to be ignorable while they finish what they are doing.
case "$ANSWER" in
  *'"memory":"none"'*|*'"memory": "none"'*)
    NAG="${XDG_RUNTIME_DIR:-/tmp}/shoulder-daemon.unconfigured.stamp"
    if ! recently "$NAG" 300; then
      say "${ADDR} has no memory backend configured, so nothing this session does is kept; set SHOULDER_MEMORY_URL or SHOULDER_MEMORY_PATH and restart it - 'shoulderd doctor' says which"
      mark "$NAG"
    fi
    exit 0
    ;;
esac

# Any other answer is a relay that is up and unwell, and the start command is
# still the fix, since it brings the whole stack back up, store included. But a
# store that is never coming back answers 503 forever, and this runs before
# every single prompt, so recovering unconditionally here would mean a start
# command per prompt for the rest of the session - a storm that outlives the
# thing it was meant to repair. One attempt every thirty seconds at most, and
# the stamp is written before the attempt rather than after it, so that a start
# which dies on its way up still counts as the try it was. Nothing listening at
# all is deliberately exempt: a relay that is genuinely down has to be back by
# the very next prompt, not half a minute into the work.
STAMP="${XDG_RUNTIME_DIR:-/tmp}/shoulder-daemon.recover.stamp"
if [ -n "$STATE" ] && [ "$STATE" != "000" ]; then
  recently "$STAMP" 30 && exit 0
  mark "$STAMP"
fi

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
    case "$(probe | tail -n 1)" in 200|404) exit 0 ;; esac
  done
  exit 0
fi
trap '[ -n "$TMP" ] && rm -rf "$TMP"; rmdir "${LOCK}" 2>/dev/null || true' EXIT

# Which binary. Something the user put on PATH wins over what the plugin
# fetched, so `go install` or a package manager is never second-guessed.
daemon() {
  if command -v shoulderd >/dev/null 2>&1; then
    command -v shoulderd
  elif [ -x "$BIN" ]; then
    echo "$BIN"
  fi
}

# fetch downloads the newest release for this platform and verifies it against
# the checksum file published beside it. Anything short of a verified binary
# leaves no file behind, so a half-download is never what runs next time.
fetch() {
  local os arch name
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) say "no release binary for $(uname -s); install with: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest"; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) say "no release binary for $(uname -m); install with: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest"; return 1 ;;
  esac
  name="shoulderd_${os}_${arch}"
  TMP="$(mktemp -d "${TMPDIR:-/tmp}/shoulder-daemon.XXXXXX")" || return 1

  say "no shoulderd found; fetching the latest release for ${os}/${arch}"
  if ! curl -fsSL --max-time 45 -o "$TMP/$name" "$RELEASES/$name" \
     || ! curl -fsSL --max-time 15 -o "$TMP/SHA256SUMS" "$RELEASES/SHA256SUMS"; then
    say "download failed; install with: go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest"
    return 1
  fi
  local want got
  want="$(grep " $name\$" "$TMP/SHA256SUMS" | cut -d' ' -f1)"
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$TMP/$name" | cut -d' ' -f1)"
  else
    got="$(shasum -a 256 "$TMP/$name" | cut -d' ' -f1)"
  fi
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    say "checksum mismatch for $name; refusing to install it"
    return 1
  fi
  mkdir -p "$DATA/bin" && chmod +x "$TMP/$name" && mv "$TMP/$name" "$BIN" || return 1
  say "installed $("$BIN" version 2>/dev/null || echo "$BIN")"
}

# link puts the fetched binary where a shell finds it, so `shoulderd monitor`
# and `shoulderd doctor` are the same word in a terminal as in this script.
# ~/.local/bin is on PATH by default on most Linux and macOS setups; when it is
# not, the one line that fixes it is said once, at fetch time.
link() {
  local dir="$HOME/.local/bin"
  mkdir -p "$dir" && ln -sf "$BIN" "$dir/shoulderd" || return 0
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) say "add $dir to your PATH to run shoulderd from a terminal: export PATH=\"$dir:\$PATH\"" ;;
  esac
}

if [ -n "${SHOULDER_START_CMD:-}" ]; then
  ( eval "${SHOULDER_START_CMD}" ) >/dev/null 2>&1 &
  exit 0
fi

exe="$(daemon)"
if [ -z "$exe" ] && [ "$FETCH" = 1 ]; then
  fetch && exe="$BIN" && link
fi
if [ -z "$exe" ]; then
  [ "$FETCH" = 1 ] || say "no relay ready at ${ADDR}, and no 'shoulderd' on PATH."
  exit 0
fi
( nohup "$exe" >/dev/null 2>&1 & ) >/dev/null 2>&1
exit 0
