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

dry-run:
	go run ./cmd/trader run --dry-run --config $(CONFIG)

run:
	go run ./cmd/trader run --config $(CONFIG)

daemon:
	go run ./cmd/trader daemon --config $(CONFIG)

status:
	go run ./cmd/trader status --config $(CONFIG)

.PHONY: docker-up docker-logs docker-status docker-down
docker-up:
	docker compose up -d --build

docker-logs:
	docker compose logs -f

docker-status:
	docker compose run --rm trader status

docker-down:
	docker compose down
