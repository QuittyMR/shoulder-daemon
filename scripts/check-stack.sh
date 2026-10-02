#!/usr/bin/env bash
# Brings the compose stack up through `make up`, the way an install runs it,
# with a model and a store given through its env file, and checks what unit
# tests cannot see: which file the Makefile and compose hand the relay, that
# `make up` leaves a current relay alone and recreates one whose file changed
# or that predates the hash, that it names - never shows - what deploy/.env
# still sets, and that `shoulderd doctor` reports what the file configured.
#
# It must never touch an install. The relay normally listens on 127.0.0.1:8787
# with host networking, so this one gets a compose project of its own, a free
# port, its own image tag, facts volume, env file, deploy/.env and state
# directory, and an override that drops the host mounts. Everything it creates
# is removed on exit, and nothing else.
set -euo pipefail

cd "$(dirname "$0")/.."

tmp=$(mktemp -d "${TMPDIR:-/tmp}/shoulder-check-stack.XXXXXX")
project="shoulder-daemon-check-$$"
image="localhost/shoulder-daemon:check-stack-$$"
compose=(env SHOULDER_ENV_FILE="$tmp/env" SHOULDER_ENV_HASH=check podman-compose -p "$project"
	-f deploy/docker-compose.yml -f "$tmp/override.yml")
# `make up` and `make up-check` exactly as SHOULDER_START_CMD runs them, pointed
# at this project and its files.
mk=(make -s PROJECT="$project" COMPOSE_FILES="deploy/docker-compose.yml $tmp/override.yml"
	SHOULDER_ENV_FILE="$tmp/env" LEGACY_ENV="$tmp/legacy.env" STATE_DIR="$tmp/state")
stub_pid=""

cleanup() {
	if [ -f "$tmp/override.yml" ]; then
		"${compose[@]}" down -v >/dev/null 2>&1 || true
	fi
	podman rmi "$image" >/dev/null 2>&1 || true
	[ -z "$stub_pid" ] || kill "$stub_pid" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT

fail() {
	echo "check-stack: FAIL: $*" >&2
	exit 1
}

pass() {
	echo "check-stack: $*"
}

listening() {
	python3 -c 'import socket, sys; s = socket.socket(); sys.exit(s.connect_ex(("127.0.0.1", int(sys.argv[1]))) != 0)' "$1"
}

relay_id() {
	podman ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=shoulderd
}

relay_env() {
	podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(relay_id)" | sed -n "s/^$1=//p"
}

verdict() {
	"${mk[@]}" up-check
}

# The Go reader's table of cases, against the python-dotenv podman-compose
# reads the file with, which is installed wherever this runs.
(cd relay && SHOULDER_DOTENV_REQUIRED=1 go test -count=1 -run TestTheTableIsWhatPythonDotenvSays ./internal/config >/dev/null) ||
	fail "the env file grammar in relay/internal/config disagrees with python-dotenv"
pass "the daemon reads the env file as python-dotenv does"

# The file the Makefile hands compose; -n prints the recipe without running it.
resolved() {
	env -u SHOULDER_ENV_FILE -u XDG_CONFIG_HOME "$@" make -s -n up-check | sed -n "s/.*ENV_FILE='\([^']*\)'.*/\1/p"
}
home="$tmp/home dir"
[ "$(resolved HOME="$home")" = "$home/.config/shoulder-daemon/env" ] || fail "the default is not under HOME"
[ "$(resolved HOME="$home" XDG_CONFIG_HOME="$tmp/xdg")" = "$tmp/xdg/shoulder-daemon/env" ] || fail "XDG_CONFIG_HOME is not followed"
[ "$(resolved HOME="$home" SHOULDER_ENV_FILE="~/x/env")" = "$home/x/env" ] || fail "a leading ~/ is not home"
[ "$(resolved HOME="$home" XDG_CONFIG_HOME="$tmp/xdg" SHOULDER_ENV_FILE="$tmp/env")" = "$tmp/env" ] ||
	fail "SHOULDER_ENV_FILE does not replace the default"
pass "the Makefile resolves the env file as the daemon does"

cat >"$tmp/stub.py" <<'EOF'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class Stub(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.path.endswith("/chat/completions"):
            body = {"choices": [{"message": {"content": json.dumps({"inject": "", "keywords": [], "facts": []})}}]}
        elif self.path == "/api/search":
            body = {"results": []}
        else:
            self.send_error(404)
            return
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args):
        pass

srv = HTTPServer(("127.0.0.1", 0), Stub)
with open(sys.argv[1], "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
EOF
python3 "$tmp/stub.py" "$tmp/stub.port" &
stub_pid=$!
for _ in $(seq 50); do
	[ -s "$tmp/stub.port" ] && break
	sleep 0.1
done
[ -s "$tmp/stub.port" ] || fail "the stub did not start"
CHECK_STACK_STUB="http://127.0.0.1:$(cat "$tmp/stub.port")"
export CHECK_STACK_STUB

port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
listening "$port" && fail "127.0.0.1:$port was free a moment ago and is taken now; run it again"
base="http://127.0.0.1:$port"

# Written in the forms the grammar in relay/internal/config/dotenv.go covers, so
# that compose building the container and doctor reading the file are seen to
# agree. The stub's address comes through ${...} from this script's
# environment, which compose and doctor both inherit.
token=$(python3 -c 'import secrets; print(secrets.token_hex(16))')
cat >"$tmp/env" <<'EOF'
# written by scripts/check-stack.sh

export SHOULDER_LLM=local   # a comment compose drops
SHOULDER_LLM_BASE_URL="${CHECK_STACK_STUB}/v1" # and one after quotes
SHOULDER_LLM_MODEL='stub'
SHOULDER_MEMORY_URL=${CHECK_STACK_STUB}
EOF
echo "SHOULDER_TOKEN=$token" >>"$tmp/env"
printf 'SHOULDER_LLM=gemini\nSHOULDER_TOKEN=%s\n' "$token" >"$tmp/env-other"

cat >"$tmp/override.yml" <<EOF
services:
  shoulderd:
    image: $image
    environment:
      SHOULDER_ADDR: "127.0.0.1:$port"
    volumes: !override
      - facts:/home/nonroot
    healthcheck:
      test: ["CMD", "/shoulderd", "doctor", "-liveness", "-addr", "$base"]
EOF

"${compose[@]}" config >"$tmp/config.yml" 2>&1 || { cat "$tmp/config.yml" >&2; fail "compose config failed"; }
paths=$(sed -n '/^ *env_file:/,/^ *[a-z_]*:$/s/^ *- *\(path: *\)\{0,1\}\([^ ]*\)$/\2/p' "$tmp/config.yml")
[ "$paths" = "$tmp/env" ] || fail "compose reads env files [$paths], want only $tmp/env"
pass "compose reads $tmp/env and nothing else"

pass "building $image"
"${compose[@]}" build shoulderd >"$tmp/build.log" 2>&1 || { cat "$tmp/build.log" >&2; fail "the image did not build"; }

# A relay from before the hash: created with SHOULDER_ENV_HASH taken out.
printf 'services:\n  shoulderd:\n    environment:\n      SHOULDER_ENV_HASH: !reset null\n' >"$tmp/old.yml"
"${compose[@]}" -f "$tmp/old.yml" up -d --no-deps shoulderd >"$tmp/old.log" 2>&1 || { cat "$tmp/old.log" >&2; fail "the old relay did not start"; }
[ -n "$(relay_id)" ] && [ -z "$(relay_env SHOULDER_ENV_HASH)" ] || fail "the stand-in for an old relay carries a hash"
[ "$(verdict)" = stale ] || fail "a relay without a hash is not called stale"

# deploy/.env is named, never shown, and never stops anything.
printf 'SHOULDER_IMAGE=x\nSHOULDER_LLM=secret-looking\nECHO_MODE=also-secret\n' >"$tmp/legacy.env"
before=$(relay_id)
"${mk[@]}" up >"$tmp/up.log" 2>&1 || { cat "$tmp/up.log" >&2; fail "make up failed"; }
grep -q "still sets: ECHO_MODE SHOULDER_LLM\. Move" "$tmp/up.log" && ! grep -q "secret\|SHOULDER_IMAGE" "$tmp/up.log" ||
	{ cat "$tmp/up.log" >&2; fail "make up does not name what deploy/.env sets, by name alone"; }
rm "$tmp/legacy.env"
[ "$(relay_id)" != "$before" ] && [ -n "$(relay_env SHOULDER_ENV_HASH)" ] || fail "make up left the relay without a hash in place"
pass "make up recreates a relay that predates the hash, and names what deploy/.env still sets without its values"

for _ in $(seq 60); do
	listening "$port" && break
	sleep 0.5
done
listening "$port" || { "${compose[@]}" logs shoulderd >&2 || true; fail "the relay never listened on $base"; }

[ "$(verdict)" = current ] || fail "a relay make up created from the file is called stale"
before=$(relay_id)
"${mk[@]}" up >"$tmp/up.log" 2>&1 || { cat "$tmp/up.log" >&2; fail "make up failed on a current relay"; }
[ "$(relay_id)" = "$before" ] || fail "make up recreated a current relay"
hash=$(relay_env SHOULDER_ENV_HASH)
echo "SHOULDER_PICKINESS=strict" >>"$tmp/env"
[ "$(verdict)" = stale ] || fail "a changed file does not make the relay stale"
"${mk[@]}" up >"$tmp/up.log" 2>&1 || { cat "$tmp/up.log" >&2; fail "make up did not recreate the stale relay"; }
[ "$(relay_id)" != "$before" ] && [ "$(relay_env SHOULDER_ENV_HASH)" != "$hash" ] || fail "make up left the stale relay in place"
[ "$(relay_env SHOULDER_PICKINESS)" = strict ] || fail "the recreated relay does not have the new setting"
[ "$(verdict)" = current ] || fail "the relay make up recreated is called stale"
pass "make up leaves a current relay alone and recreates one whose file changed, with the new hash"

for _ in $(seq 60); do
	listening "$port" && break
	sleep 0.5
done
listening "$port" || { "${compose[@]}" logs shoulderd >&2 || true; fail "the recreated relay never listened on $base"; }

doctor() {
	env -u SHOULDER_LLM -u SHOULDER_LLM_MODEL -u SHOULDER_MEMORY -u SHOULDER_MEMORY_URL -u SHOULDER_TOKEN \
		SHOULDER_ENV_FILE="$1" ./bin/shoulderd doctor --json --addr "$base" || true
}

# The rest of doctor's exit code is about hooks and plugins, which a stack no
# session has used cannot satisfy, so the verdict is read off the fields.
check() {
	python3 - "$1" "$2" <<'EOF'
import json, sys
got, want = json.loads(sys.argv[1]), json.loads(sys.argv[2])
bad = []
for k, v in want.items():
    g = got.get(k)
    if v is True and not (isinstance(g, str) and g.startswith("MISMATCH")):
        bad.append(f"{k}: got {g!r}, want a MISMATCH")
    elif v is not True and g != v:
        bad.append(f"{k}: got {g!r}, want {v!r}")
print("\n".join(bad))
EOF
}

out=$(doctor "$tmp/env")
bad=$(check "$out" '{"llm": "local", "llm_model": "stub", "llm_source": "env file", "llm_model_source": "env file",
	"llm_mismatch": null, "llm_note": null, "llm_model_mismatch": null,
	"memory": "ok", "memory_name": "mcp-memory-service", "memory_source": "env file",
	"memory_mismatch": null, "memory_note": null}')
[ -z "$bad" ] || { echo "$out" >&2; fail "doctor does not report the configured model and store:"$'\n'"$bad"; }
pass "doctor reports llm local (stub) and memory ok (mcp-memory-service), both from the env file"

out=$(doctor "$tmp/env-other")
bad=$(check "$out" '{"llm": "local", "llm_mismatch": true, "memory_name": "mcp-memory-service", "memory_mismatch": true}')
[ -z "$bad" ] || { echo "$out" >&2; fail "doctor does not catch a daemon running other than its env file says:"$'\n'"$bad"; }
pass "doctor fails a model and a store other than the env file's"
pass "ok"
