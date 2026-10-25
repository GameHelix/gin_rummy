GOCACHE ?= $(CURDIR)/.cache/go-build
export GOCACHE
CONFIG ?= config/agent.json

.PHONY: test check build init dry-run run daemon status

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...

build:
	go build -o bin/trader ./cmd/trader

init:
	go run ./cmd/trader init --config $(CONFIG)

# Live market data, no model call, nothing saved.
dry-run:
	go run ./cmd/trader run --dry-run --config $(CONFIG)

# Makes paid Anthropic requests, bounded by the configured daily and monthly caps.
run:
	go run ./cmd/trader run --config $(CONFIG)

daemon:
	go run ./cmd/trader daemon --config $(CONFIG)

status:
	go run ./cmd/trader status --config $(CONFIG)

# Docker runner: the daemon as a service, sharing ./data with the local binary.
.PHONY: docker-up docker-logs docker-status docker-down
docker-up:
	docker compose up -d --build

docker-logs:
	docker compose logs -f

docker-status:
	docker compose run --rm trader status

docker-down:
	docker compose down
