# Prefer the Compose v2 plugin; fall back to the standalone docker-compose binary.
COMPOSE ?= $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo docker-compose)

PG_PORT      ?= 5432
DATABASE_URL ?= postgres://flashsale:flashsale@localhost:$(PG_PORT)/flashsale?sslmode=disable
export DATABASE_URL

# Load test knobs: make load BUYERS=1000 STOCK=10 POOL=50 STRATEGIES=atomic
BUYERS     ?= 10000
STOCK      ?= 100
POOL       ?= 20
STRATEGIES ?= naive,atomic,pessimistic
REPEAT     ?= 1

# Messy clients (L6-style): make load-messy (10% double clicks, 30% same-key retries, winners pay, 5% declined)
DUP_RATIO   ?= 0.1
RETRY_RATIO ?= 0.3
PAY_FAIL    ?= 0.05

# Capacity ceiling knobs: make load-ceiling RATES=500,1000,2000 STEP=20s
RATES ?= 500,1000,2000,4000,8000
STEP  ?= 20s

.PHONY: up down reset run build test test-short vet load load-messy load-ceiling

up: ## Start Postgres and wait until it is healthy
	$(COMPOSE) up -d --wait postgres

down: ## Stop containers (keeps the data volume)
	$(COMPOSE) down

reset: ## Stop containers and delete the data volume
	$(COMPOSE) down -v

build:
	go build -o bin/api ./cmd/api
	go build -o bin/loadgen ./cmd/loadgen

run: ## Run the API locally (migrates on start)
	go run ./cmd/api

vet:
	go vet ./...

test: ## Full test suite with the race detector (needs `make up`)
	go test -race -count=1 ./...

test-short: ## Unit tests only, no database
	go test -short -count=1 ./...

load: build ## L1 burst: BUYERS released at once vs STOCK units, per strategy; appends to docs/results.md
	./bin/loadgen -scenario burst -api-bin bin/api -strategies $(STRATEGIES) -buyers $(BUYERS) -stock $(STOCK) -pool $(POOL) -repeat $(REPEAT)

load-messy: build ## L1 + duplicates, same-key retries, confirm with payment failures (strategy B)
	./bin/loadgen -scenario burst -api-bin bin/api -strategies atomic -buyers $(BUYERS) -stock $(STOCK) -pool $(POOL) -repeat $(REPEAT) \
		-dup-ratio $(DUP_RATIO) -retry-ratio $(RETRY_RATIO) -confirm -payment-failure-rate $(PAY_FAIL)

load-ceiling: build ## L5: stepped constant rate (strategy B) until p99 > 500 ms or errors >= 1%
	./bin/loadgen -scenario ceiling -api-bin bin/api -strategies atomic -rates $(RATES) -step $(STEP) -pool $(POOL)
