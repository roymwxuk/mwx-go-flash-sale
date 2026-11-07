# Prefer the Compose v2 plugin; fall back to the standalone docker-compose binary.
COMPOSE ?= $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo docker-compose)

PG_PORT      ?= 5432
DATABASE_URL ?= postgres://flashsale:flashsale@localhost:$(PG_PORT)/flashsale?sslmode=disable
export DATABASE_URL

.PHONY: up down reset run build test test-short vet load

up: ## Start Postgres and wait until it is healthy
	$(COMPOSE) up -d --wait postgres

down: ## Stop containers (keeps the data volume)
	$(COMPOSE) down

reset: ## Stop containers and delete the data volume
	$(COMPOSE) down -v

build:
	go build -o bin/api ./cmd/api

run: ## Run the API locally (migrates on start)
	go run ./cmd/api

vet:
	go vet ./...

test: ## Full test suite with the race detector (needs `make up`)
	go test -race -count=1 ./...

test-short: ## Unit tests only, no database
	go test -short -count=1 ./...

load: ## Load generator (implemented in milestone 3)
	@echo "cmd/loadgen is not implemented yet (milestone 3)." && exit 1
