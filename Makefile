SHELL := /bin/bash
BIN := bin
DOCKER := docker
COMPOSE := $(DOCKER) compose -f deploy/docker-compose.yml

# The memory service is behind a profile so that an install that never asked
# for it never gets it. Once an install has asked for it, though, every command
# that brings the stack up has to go on selecting it: compose acts only on the
# services its profiles name, so without this an `up` or an `update` starts the
# relay and leaves it pointing at a store that is not there - and the daemon
# calls that healthy, one failed search at a time. `up` is what
# SHOULDER_START_CMD runs, so that mistake repeats on every session and stays
# quiet for days.
#
# What is tested is the volume rather than the container, because `make down`
# removes the container and keeps the volume. Asking after the container means
# answering no for the whole time that matters - the first `up` after a `down`,
# when the store is exactly what is missing - and the store would never come
# back on its own. The volume is created the first time the memory service
# starts and not before, so a checkout that only ever wanted the built-in store
# still gets the relay alone. The name is the one compose derives from `name:`
# in deploy/docker-compose.yml; an install that overrides the project name
# falls back to relay-only, which is the safe direction to be wrong in.
MEMORY_PROFILE = $(shell $(DOCKER) volume ls --format '{{.Name}}' 2>/dev/null | grep -qx 'shoulder-daemon_memory-data' && echo --profile memory)

GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

.PHONY: build test adapter-test cover bench lint vulncheck release-check release docker-build up down logs memory doctor e2e clean install-plugins update

build:
	@mkdir -p $(BIN)
	cd relay && go build -o ../$(BIN)/shoulderd ./cmd/shoulderd
	cd advisor-echo && go build -o ../$(BIN)/advisor-echo .

test:
	cd relay && go test ./...
	cd advisor-echo && go test ./...

# The OpenCode adapter decides whether a session is observed at all, and it is
# the one part of this repository `go test` cannot see. Kept out of `test` so
# that node is not a requirement of the default suite; CI runs both.
adapter-test:
	node --test adapters/opencode/shoulder-daemon.test.js

# The hook round trip is the number the whole design rests on. Anything that
# puts network or synchronous disk I/O on the hook path shows up here first.
bench:
	cd relay && go test ./internal/pipeline/ -run '^$$' -bench BenchmarkHookRoundTrip -benchtime 20000x

# Same linter, same version, same config as CI; a clean run here is a clean
# run there.
lint:
	cd relay && $(GOLANGCI) run --build-tags integration,compare,minilm,scenario ./...
	cd advisor-echo && $(GOLANGCI) run ./...

cover:
	cd relay && go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1
	cd advisor-echo && go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

vulncheck:
	cd relay && go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	cd advisor-echo && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Before tagging: the tag, the plugin manifest and the changelog have to agree.
release-check:
	@test -n "$(TAG)" || { echo "usage: make release-check TAG=vX.Y.Z"; exit 2; }
	scripts/check-version.sh $(TAG)
	scripts/changelog-section.sh $(TAG)

# Three tags, every remote. See scripts/tag-release.sh for why three.
release:
	@test -n "$(TAG)" || { echo "usage: make release TAG=vX.Y.Z"; exit 2; }
	scripts/tag-release.sh $(TAG)

# What a harness runs is the copy of the adapter it took when the plugin was
# installed, not this checkout. Editing the adapter here changes nothing the
# harness loads until that copy is replaced, and the failure is silent: the
# stale copy goes on posting to whatever address and header it was built
# against. `shoulderd doctor` reports it; this fixes it.
install-plugins:
	@scripts/install-plugins.sh

# Everything an update needs, in the order it needs it.
update: build docker-build install-plugins
	@$(COMPOSE) $(MEMORY_PROFILE) up -d --force-recreate >/dev/null 2>&1 || true
	@echo
	@echo "Daemon rebuilt and restarted, adapters reinstalled."
	@echo "Restart your editor so it reloads the plugin, then: ./$(BIN)/shoulderd doctor"

docker-build:
	$(COMPOSE) build

# --no-recreate because this is what SHOULDER_START_CMD runs, every time the
# daemon idles out and a session brings it back: podman-compose recreates
# whatever `up` selects even when it is already healthy, so without it a
# session start bounces the relay out from under itself and makes the store pay
# its model load, and its 300-second start period, once an hour for nothing.
# No service is named so that the profile above decides: an install with the
# store gets both, one without gets the relay.
up:
	$(COMPOSE) $(MEMORY_PROFILE) up -d --no-recreate

# mcp-memory-service, for an install that wants it instead of the store the
# daemon keeps itself. First start pulls an embedding model and takes a few
# minutes. Set SHOULDER_MEMORY_URL too, or the daemon goes on using its own file.
memory:
	$(COMPOSE) --profile memory up -d memory

down:
	$(COMPOSE) --profile memory down

logs:
	$(COMPOSE) logs -f

doctor: build
	./$(BIN)/shoulderd doctor

clean:
	rm -rf $(BIN)
