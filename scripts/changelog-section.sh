#!/usr/bin/env bash
# Print one version's section of CHANGELOG.md, without its heading, for use as
# release notes. Both release pipelines read it, so the notes on GitHub and
# GitLab are the same text.
set -euo pipefail
cd "$(dirname "$0")/.."

ver="${1#v}"
if [ -z "$ver" ]; then
  echo "usage: scripts/changelog-section.sh vX.Y.Z" >&2
  exit 2
fi

awk -v ver="$ver" '
  /^## \[/      { inside = index($0, "## [" ver "]") == 1; next }
  /^\[.*\]: /   { inside = 0 }
  inside        { print }
' CHANGELOG.md | sed -e '1{/^$/d}' -e '${/^$/d}'
