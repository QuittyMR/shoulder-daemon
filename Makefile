SHELL := /bin/bash
BIN := bin
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: build test bench lint docker-build up down logs doctor e2e clean

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
