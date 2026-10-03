#!/usr/bin/env bash
# Read and write the daemon's env file without ever putting a secret on a
# terminal. The setup skill runs inside a coding harness, so everything a
# command prints is kept in that session's transcript - and shoulder-daemon
# reads transcripts. So a key is copied from the environment by name here
# rather than passed as an argument, and nothing prints a value back.
#
# Reads and writes go through `shoulderd env`, so the file is read and written
# with the one grammar the daemon, the CLI and compose all read it with: a
# setting is replaced where it first stood and any later copy dropped, a new
# one is appended, and every other line - comments, SHOULDER_TOKEN, whatever
# the user put there by hand - is carried across untouched. Everything but `path` therefore needs the binary, which
# the plugin's ensure-daemon.sh --fetch installs.
#
#   env-set.sh path                 print the file the daemon will read
#   env-set.sh get NAME             print a non-secret value, empty if unset
#   env-set.sh has NAME             exit 0 if set in the file or the environment,
#                                   1 if not, 2 if it cannot tell
#   env-set.sh set NAME VALUE       set a non-secret setting
#   env-set.sh copy NAME            copy $NAME from this environment into the file
#   env-set.sh unset NAME           remove a setting
set -euo pipefail

# $SHOULDER_ENV_FILE, a leading ~/ being home, or the conventional location.
# The same order the daemon and the CLI resolve, so what this writes is what
# they read.
file() {
  case "${SHOULDER_ENV_FILE:-}" in
    "~/"*) echo "$HOME/${SHOULDER_ENV_FILE#??}" ;;
    "") echo "${XDG_CONFIG_HOME:-$HOME/.config}/shoulder-daemon/env" ;;
    *) echo "$SHOULDER_ENV_FILE" ;;
  esac
}

# The binary, wherever the plugin or the user put it, and one that knows
# `env`: an older one would answer every question here with a usage error,
# which a caller reading "is it set" would take for "no". A checkout's binary
# is in neither place; `make install-plugins` records it in the env file as
# SHOULDER_BIN, a plain path it writes single-quoted, so a grep reads it.
remedy="from a checkout, run make install-plugins there; otherwise run the plugin's scripts/ensure-daemon.sh --fetch"
shoulderd_bin() {
  local bin recorded
  recorded="$(sed -n "s/^SHOULDER_BIN='\(.*\)'\$/\1/p" "$(file)" 2>/dev/null | tail -n 1)"
  for bin in "${SHOULDER_BIN:-}" "$recorded" "$(command -v shoulderd 2>/dev/null)" "$HOME/.local/bin/shoulderd" \
             "${XDG_DATA_HOME:-$HOME/.local/share}/shoulder-daemon/bin/shoulderd"; do
    [ -n "$bin" ] && [ -x "$bin" ] && break
    bin=""
  done
  if [ -z "$bin" ]; then
    echo "shoulderd is not installed yet; $remedy" >&2
    exit 2
  fi
  if ! "$bin" env path >/dev/null 2>&1; then
    echo "$bin has no 'shoulderd env', which this skill needs. From a checkout, run make install-plugins there. A fetched binary is replaced by deleting it and running the plugin's scripts/ensure-daemon.sh --fetch, but a release from before 'env' shipped lacks it too; until one has it, install from a checkout." >&2
    exit 2
  fi
  echo "$bin"
}

[ "${1:-}" = path ] || [ -z "${1:-}" ] || BIN="$(shoulderd_bin)" || exit 2

env_cmd() {
  SHOULDER_ENV_FILE="$(file)" "$BIN" env "$@"
}

read_file() {
  env_cmd get "$1"
}

case "${1:-}" in
  path) file ;;
  get)  read_file "${2:?usage: env-set.sh get NAME}" ;;
  has)
    name="${2:?usage: env-set.sh has NAME}"
    [ -n "${!name:-}" ] && exit 0
    [ -n "$(read_file "$name")" ] && exit 0
    exit 1
    ;;
  set)
    # An empty value removes the line: NAME= reads as set, and a key written
    # that way reports as configured and fails on the first prompt.
    if [ -n "${3?usage: env-set.sh set NAME VALUE}" ]; then
      env_cmd set "${2:?usage: env-set.sh set NAME VALUE}" "$3"
    else
      env_cmd unset "${2:?usage: env-set.sh set NAME VALUE}"
    fi
    echo "set ${2} in $(file)"
    ;;
  copy)
    name="${2:?usage: env-set.sh copy NAME}"
    if [ -z "${!name:-}" ]; then
      echo "${name} is not in this environment; nothing copied" >&2
      exit 1
    fi
    printf '%s' "${!name}" | env_cmd set "$name"
    echo "copied ${name} into $(file) from the environment; its value was not printed"
    ;;
  unset)
    env_cmd unset "${2:?usage: env-set.sh unset NAME}"
    echo "removed ${2} from $(file)"
    ;;
  *)
    sed -n 's/^#   //p' "$0"
    exit 2
    ;;
esac
