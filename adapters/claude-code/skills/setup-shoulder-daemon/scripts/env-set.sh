#!/usr/bin/env bash
# Read and write the daemon's env file without ever putting a secret on a
# terminal. The setup skill runs inside a coding harness, so everything a
# command prints is kept in that session's transcript - and shoulder-daemon
# reads transcripts. So a key is copied from the environment by name here
# rather than passed as an argument, and nothing prints a value back.
#
# Writes are line-wise and atomic: an existing setting is replaced in place, a
# new one is appended, and every other line - comments, SHOULDER_TOKEN, whatever
# the user put there by hand - is carried across untouched.
#
#   env-set.sh path                 print the file the daemon will read
#   env-set.sh get NAME             print a non-secret value, empty if unset
#   env-set.sh has NAME             exit 0 if set in the file or the environment
#   env-set.sh set NAME VALUE       set a non-secret setting
#   env-set.sh copy NAME            copy $NAME from this environment into the file
#   env-set.sh unset NAME           remove a setting
set -euo pipefail

# $SHOULDER_ENV_FILE, or the conventional location. The same order the daemon
# and the CLI resolve, so what this writes is what they read.
file() {
  if [ -n "${SHOULDER_ENV_FILE:-}" ]; then
    echo "$SHOULDER_ENV_FILE"
  else
    echo "${XDG_CONFIG_HOME:-$HOME/.config}/shoulder-daemon/env"
  fi
}

read_file() {
  local name="$1" path
  path="$(file)"
  [ -f "$path" ] || return 0
  sed -n "s/^[[:space:]]*${name}=//p" "$path" | tail -n 1
}

write() {
  local name="$1" value="$2" path tmp
  path="$(file)"
  mkdir -p "$(dirname "$path")"
  [ -f "$path" ] || : > "$path"
  chmod 600 "$path"
  tmp="$(mktemp "${path}.XXXXXX")"
  chmod 600 "$tmp"
  # grep rather than sed -i: the value is arbitrary text and must never be
  # read as a replacement expression.
  grep -v "^[[:space:]]*${name}=" "$path" > "$tmp" || true
  if [ -n "$value" ]; then
    printf '%s=%s\n' "$name" "$value" >> "$tmp"
  fi
  mv "$tmp" "$path"
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
    write "${2:?usage: env-set.sh set NAME VALUE}" "${3?usage: env-set.sh set NAME VALUE}"
    echo "set ${2} in $(file)"
    ;;
  copy)
    name="${2:?usage: env-set.sh copy NAME}"
    if [ -z "${!name:-}" ]; then
      echo "${name} is not in this environment; nothing copied" >&2
      exit 1
    fi
    write "$name" "${!name}"
    echo "copied ${name} into $(file) from the environment; its value was not printed"
    ;;
  unset)
    write "${2:?usage: env-set.sh unset NAME}" ""
    echo "removed ${2} from $(file)"
    ;;
  *)
    sed -n 's/^#   //p' "$0"
    exit 2
    ;;
esac
