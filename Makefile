SHELL := /bin/bash
BIN := bin
COMPOSE := docker compose -f deploy/docker-compose.yml

GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2

.PHONY: build test cover bench lint vulncheck release-check docker-build up down logs doctor e2e clean install-plugins update

build:
	@mkdir -p $(BIN)
	cd relay && go build -o ../$(BIN)/shoulderd ./cmd/shoulderd
	cd advisor-echo && go build -o ../$(BIN)/advisor-echo .

test:
	cd relay && go test ./...
	cd advisor-echo && go test ./...

# The hook round trip is the number the whole design rests on. Anything that
# puts network or synchronous disk I/O on the hook path shows up here first.
bench:
	cd relay && go test ./internal/pipeline/ -run '^$$' -bench BenchmarkHookRoundTrip -benchtime 20000x

# Same linter, same version, same config as CI; a clean run here is a clean
# run there.
lint:
	cd relay && $(GOLANGCI) run ./...
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

# What a harness runs is the copy of the adapter it took when the plugin was
# installed, not this checkout. Editing the adapter here changes nothing the
# harness loads until that copy is replaced, and the failure is silent: the
# stale copy goes on posting to whatever address and header it was built
# against. `shoulderd doctor` reports it; this fixes it.
install-plugins:
	@scripts/install-plugins.sh

# Everything an update needs, in the order it needs it.
update: build docker-build install-plugins
	@$(COMPOSE) up -d --force-recreate >/dev/null 2>&1 || true
	@echo
	@echo "Daemon rebuilt and restarted, adapters reinstalled."
	@echo "Restart your editor so it reloads the plugin, then: ./$(BIN)/shoulderd doctor"

docker-build:
	$(COMPOSE) build

up:
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

doctor: build
	./$(BIN)/shoulderd doctor

clean:
	rm -rf $(BIN)
