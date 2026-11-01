#!/usr/bin/env bash
# One release is three tags. vX.Y.Z is what the pipelines build from and what
# a person reads. relay/vX.Y.Z and advisor-echo/vX.Y.Z are what the Go module
# proxy reads: each module lives in a subdirectory, and Go only recognises a
# tag for a nested module when the tag carries the directory as its prefix.
# Without those two, `go install ...@latest` keeps resolving to a pseudo-version
# of whatever main happens to be, and `shoulderd doctor` never sees a release.
#
# Runs check-version first, then the lint and the tests CI runs, creates
# whichever of the three tags does not exist yet, and pushes all three to every
# remote. Safe to rerun: a rerun only adds the module tags at the commit already
# tagged, so the checks run once, before the release tag exists.
set -euo pipefail
cd "$(dirname "$0")/.."

tag="${1:-}"
[ -n "$tag" ] || { echo "usage: scripts/tag-release.sh vX.Y.Z" >&2; exit 2; }
scripts/check-version.sh "$tag"

# The module tags follow the release tag, so a rerun that adds the missing two
# lands them on the commit that was released, not on whatever HEAD is now.
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  base="$(git rev-parse "$tag^{commit}")"
else
  # A tag is cut only from a tree that passes what the pipeline runs. Five
  # releases were tagged from commits the lint job then rejected, each one
  # checked beforehand with something cheaper than the pipeline's own tools.
  if [ -n "$(git status --porcelain)" ]; then
    echo "the working tree is not clean; a tag is cut from a commit, not from what is on disk" >&2
    exit 1
  fi
  make lint test
  base="$(git rev-parse HEAD)"
fi

for t in "$tag" "relay/$tag" "advisor-echo/$tag"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    echo "$t: exists at $(git rev-parse --short "$t^{commit}")"
  else
    git tag -a "$t" -m "$t" "$base"
    echo "$t: created at $(git rev-parse --short "$base")"
  fi
done

for remote in $(git remote); do
  git push "$remote" "$tag" "relay/$tag" "advisor-echo/$tag"
done
