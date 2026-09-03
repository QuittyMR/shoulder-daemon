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

answering() { curl -sf --max-time 1 -o /dev/null "http://${ADDR}/healthz" 2>/dev/null; }
say() { echo "shoulder-daemon: $*" >&2; }

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

if [ -n "${SHOULDER_START_CMD:-}" ]; then
  ( eval "${SHOULDER_START_CMD}" ) >/dev/null 2>&1 &
  exit 0
fi

exe="$(daemon)"
if [ -z "$exe" ] && [ "$FETCH" = 1 ]; then
  fetch && exe="$BIN"
fi
if [ -z "$exe" ]; then
  [ "$FETCH" = 1 ] || say "nothing answering at ${ADDR}, and no 'shoulderd' on PATH."
  exit 0
fi
( nohup "$exe" >/dev/null 2>&1 & ) >/dev/null 2>&1
exit 0
