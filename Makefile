SHELL := /bin/bash
BIN := bin
# The stack is podman's: the relay runs as the user who owns the transcripts
# through userns keep-id, which docker does not have. podman-compose is named
# rather than reached through `podman compose`, which picks docker-compose first
# when it is installed.
DOCKER := podman
COMPOSE_BIN := podman-compose
PROJECT := shoulder-daemon
COMPOSE_FILES := deploy/docker-compose.yml
# The daemon's one env file, resolved as the daemon, the CLI and the adapters
# resolve it, a leading ~/ included. Compose cannot spell the fallback itself -
# podman-compose has no nested defaults - so it is resolved here and handed to
# every compose command, with the hash of what it holds: scripts/up.sh compares
# that with the relay's to tell whether the relay is running on an older file.
ENV_FILE := $(shell f="$$SHOULDER_ENV_FILE"; case "$$f" in ("~/"*) f="$$HOME/$${f#??}";; esac; printf '%s' "$${f:-$${XDG_CONFIG_HOME:-$$HOME/.config}/shoulder-daemon/env}")
ENV_HASH := $(shell f='$(ENV_FILE)'; if [ ! -e "$$f" ]; then echo absent; elif command -v sha256sum >/dev/null 2>&1; then sha256sum <"$$f" | cut -c1-64; elif command -v shasum >/dev/null 2>&1; then shasum -a 256 <"$$f" | cut -c1-64; fi)
ifeq ($(ENV_HASH),)
$(error cannot hash $(ENV_FILE): it is unreadable, or neither sha256sum nor shasum is installed)
endif
LEGACY_ENV := deploy/.env
STATE_DIR := $(or $(XDG_STATE_HOME),$(HOME)/.local/state)/shoulder-daemon
COMPOSE = SHOULDER_ENV_FILE='$(ENV_FILE)' SHOULDER_ENV_HASH='$(ENV_HASH)' $(COMPOSE_BIN) -p '$(PROJECT)' $(addprefix -f ,$(COMPOSE_FILES))
UP = DOCKER='$(DOCKER)' COMPOSE_BIN='$(COMPOSE_BIN)' PROJECT='$(PROJECT)' COMPOSE_FILES='$(COMPOSE_FILES)' \
	ENV_FILE='$(ENV_FILE)' ENV_HASH='$(ENV_HASH)' LEGACY_ENV='$(LEGACY_ENV)' STATE_DIR='$(STATE_DIR)' scripts/up.sh

GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

.PHONY: build test adapter-test cover bench lint vulncheck release-check release docker-build up down logs memory doctor e2e clean install-plugins update up-check

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
install-plugins: build
	@scripts/install-plugins.sh

# Everything an update needs, in the order it needs it.
update: build docker-build install-plugins
	@$(UP) update
	@echo
	@echo "Daemon rebuilt and restarted, adapters reinstalled."
	@echo "Restart your editor so it reloads the plugin, then: ./$(BIN)/shoulderd doctor"

docker-build:
	$(COMPOSE) build

# What SHOULDER_START_CMD runs every time the daemon idles out and a session
# brings it back: starts what is not running, recreates the relay alone when its
# env file changed, and leaves everything else as it is. scripts/up.sh says why
# each of those matters.
up:
	@$(UP) up

# What `up` would decide about the relay, and why, without doing it.
up-check:
	@$(UP) check

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
