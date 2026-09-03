#!/usr/bin/env bash
# A release is one number in three places: the git tag, the Claude Code plugin
# manifest, and the changelog. Any two agreeing while the third drifts ships a
# plugin that reports the wrong version or a release with no notes, and nothing
# else catches it. Run with the tag; exits non-zero on the first mismatch.
set -euo pipefail
cd "$(dirname "$0")/.."

tag="${1:-}"
if [ -z "$tag" ]; then
  echo "usage: scripts/check-version.sh vX.Y.Z" >&2
  exit 2
fi
case "$tag" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "tag $tag is not vX.Y.Z" >&2; exit 1 ;;
esac
ver="${tag#v}"

plugin=$(python3 -c "import json;print(json.load(open('adapters/claude-code/.claude-plugin/plugin.json'))['version'])")
if [ "$plugin" != "$ver" ]; then
  echo "adapters/claude-code/.claude-plugin/plugin.json says $plugin, tag says $ver" >&2
  exit 1
fi

if ! grep -q "^## \[$ver\]" CHANGELOG.md; then
  echo "CHANGELOG.md has no '## [$ver]' section" >&2
  exit 1
fi

echo "$tag: plugin manifest and changelog agree"
