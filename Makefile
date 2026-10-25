GOCACHE ?= $(CURDIR)/.cache/go-build
export GOCACHE
CONFIG ?= config/agent.json

.PHONY: test check smoke build verify init preview decide run status

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...

smoke:
	TRADER_TESTNET=1 go test -run TestTestnetRoundTrip -count=1 -v ./internal/agent/

build:
	go build -o bin/trader ./cmd/trader

verify:
	go run ./cmd/trader check --config $(CONFIG)

init:
	go run ./cmd/trader init --config $(CONFIG)

preview:
	go run ./cmd/trader decide --dry-run --config $(CONFIG)

decide:
	go run ./cmd/trader decide --config $(CONFIG)

run:
	go run ./cmd/trader run --config $(CONFIG)

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
