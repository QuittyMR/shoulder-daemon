#!/bin/sh
# What `make up`, `make update` and `make up-check` do about the relay. The
# Makefile passes everything it needs: DOCKER, COMPOSE_BIN, PROJECT,
# COMPOSE_FILES, ENV_FILE, ENV_HASH, LEGACY_ENV and STATE_DIR.
#
# up      recreates the relay alone when its env file changed, then brings up
#         what is not running and leaves the rest as it is
# update  recreates the whole stack from the images just built
# check   prints absent, current or stale, and changes nothing
#
# A container keeps the environment it was created with, so an edited env file
# would otherwise reach the daemon only after a `make down` nobody knows to run,
# and the daemon goes on without the model or the store the file names, saying
# nothing. The Makefile hashes the file and compose puts the hash in the relay's
# environment, so the two are compared here; a relay without one predates this
# and is stale too. It is recreated with --no-deps, so the memory service keeps
# running. Everything else `up` leaves alone, because it is what
# SHOULDER_START_CMD runs at every session start, and podman-compose recreates
# whatever it is told to bring up - which would bounce a healthy relay and make
# the store pay its model load each time.
set -eu

files=""
for f in $COMPOSE_FILES; do
	files="$files -f $f"
done
# The lock's descriptor is closed for compose, or the containers it starts
# inherit it and hold the lock for as long as they run.
compose() {
	# shellcheck disable=SC2086 # the -f pairs are meant to split
	SHOULDER_ENV_FILE="$ENV_FILE" SHOULDER_ENV_HASH="$ENV_HASH" "$COMPOSE_BIN" -p "$PROJECT" $files "$@" 9>&-
}

relay=$("$DOCKER" ps -aq --filter "label=com.docker.compose.project=$PROJECT" \
	--filter label=com.docker.compose.service=shoulderd | head -n 1)
verdict=absent
if [ -n "$relay" ]; then
	have=$("$DOCKER" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$relay" | sed -n 's/^SHOULDER_ENV_HASH=//p')
	verdict=stale
	[ "$have" = "$ENV_HASH" ] && verdict=current
fi

if [ "$1" = check ]; then
	echo "$verdict"
	exit 0
fi

# deploy/.env is where the relay's settings used to go, and compose no longer
# hands it to the relay. Its names - never its values - are said on every run
# until it is cleaned up, because a setting left there is one the daemon lost.
if [ -f "$LEGACY_ENV" ]; then
	names=$(sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*=.*/\2/p' "$LEGACY_ENV" |
		grep -v -x -e SHOULDER_IMAGE -e SHOULDER_MEMORY_IMAGE | sort -u | tr '\n' ' ')
	[ -z "$names" ] ||
		echo "warning: $LEGACY_ENV is no longer read for the relay, and still sets: ${names% }. Move any daemon setting among them to $ENV_FILE." >&2
fi

# Two sessions starting at once would otherwise both find the relay stale and
# recreate it twice, the second under the first.
mkdir -p "$STATE_DIR"
if command -v flock >/dev/null 2>&1; then
	exec 9>"$STATE_DIR/$PROJECT.lock"
	flock 9
else
	echo "warning: no flock here, so two starts at once are not kept apart" >&2
fi

# The memory service is behind a profile so that an install that never asked
# for it never gets it, and compose acts only on the services its profiles
# name: once an install has the store, everything that brings the stack up has
# to go on selecting it, or the relay comes up pointing at a store that is not
# there. The volume is asked after rather than the container because `make
# down` removes the container and keeps the volume, and the first `up` after a
# `down` is exactly when the store is missing.
profile=""
"$DOCKER" volume ls --format '{{.Name}}' | grep -qx "${PROJECT}_memory-data" && profile="--profile memory"

if [ "$1" = update ]; then
	# An install that runs the bare binary has no stack, and that is not a
	# failed update; one that has a relay and could not recreate it is.
	if [ -z "$relay" ]; then
		echo "no relay container in project $PROJECT; nothing to recreate"
		exit 0
	fi
	# shellcheck disable=SC2086
	compose $profile up -d --force-recreate
	exit 0
fi

if [ "$verdict" = stale ]; then
	echo "The relay was created from another version of $ENV_FILE; recreating it."
	compose up -d --no-deps --force-recreate shoulderd
fi
# shellcheck disable=SC2086
compose $profile up -d --no-recreate
