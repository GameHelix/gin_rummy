GOCACHE ?= $(CURDIR)/.cache/go-build
export GOCACHE

.PHONY: test check build radar docker-up docker-logs docker-down

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...

build:
	go build -o bin/trader ./cmd/trader

radar:
	go run ./cmd/trader radar

docker-up:
	docker compose up -d --build

docker-logs:
	docker compose logs -f

docker-down:
	docker compose down
