SHELL := /bin/bash
BIN := bin
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: build test bench lint docker-build up down logs doctor e2e clean install-plugins update

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

lint:
	cd relay && go vet ./...
	cd advisor-echo && go vet ./...
	gofmt -l relay advisor-echo

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
