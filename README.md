# mwx-go-flash-sale

A flash-sale API for limited drops (event tickets, limited fan goods) in Go and PostgreSQL. It is built to **prove, with measured numbers, that it never oversells**: 10,000 buyers hit 100 units at the same instant, and no stock is oversold, no buyer is charged twice, and expired holds are released exactly once.

> **Status: milestone 2.** Reservations work with two stock strategies (A naive, B atomic), backed by an invariant checker and barrier-start concurrency tests. Next: the HTTP load generator with latency and throughput numbers.

## First result: naive vs atomic (correctness only)

1,000 goroutines released together by a start barrier, racing for **10 units**, repeated 10 times. Full table and machine spec in [`docs/results.md`](docs/results.md).

| Strategy | Runs that oversold | Holds granted per run | Invariant violations |
|---|---:|---:|---:|
| A. naive read-then-write | **10 / 10** | 521–728 (for 10 units) | every run |
| B. atomic conditional UPDATE | **0 / 10** | exactly 10 | 0 |

Latency and throughput come in milestone 3 (HTTP load generator).

## Invariants and how they are enforced

| # | Invariant | Enforced by | Tested by |
|---|---|---|---|
| 1 | No oversell | Query shape: `UPDATE ... WHERE remaining >= n` (strategy B), plus `CHECK (remaining >= 0)` | `TestConcurrentAtomicNeverOversells`; `TestConcurrentNaiveOversells` shows the failure mode |
| 2 | Per-user limit | Partial UNIQUE index on `(drop_id, user_id) WHERE status IN ('held','confirmed')` | `TestConcurrentSameUserGetsOneHold` (50 concurrent requests from one user → 1 hold, under both strategies) |
| 3 | No double charge | `payments.reservation_id UNIQUE` | _milestone 4_ |
| 4 | Exactly-once release | _milestone 5 (sweeper with `SKIP LOCKED`)_ | _milestone 5_ |
| 5 | Stock conservation | Checked by `internal/invariants` after every test run | all concurrency tests; `TestCheckerDetectsEachViolation` tests the checker itself |

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/drops` | `{"name","total_stock","per_user_limit"?,"price_minor"?,"sale_starts_at"?}` → 201 |
| `GET` | `/drops/{id}` | stock state: `remaining`, `held`, `confirmed` |
| `POST` | `/drops/{id}/reservations` | headers `X-User-Id`, `Idempotency-Key` (required; replay lands in milestone 4); body `{"quantity"}` optional → 201, 409 `sold_out` / `per_user_limit` / `sale_not_started`, 503 on timeout |
| `GET` | `/healthz` | 200, or 503 if the DB is unreachable |

## How to run

Requirements: Go 1.25+, Docker (Compose v2 plugin or standalone `docker-compose`).

```sh
make up      # start Postgres 16 and wait until healthy
make test    # go test -race ./... (integration tests use the compose Postgres)
make run     # start the API on :8080 (migrates on start)
make load    # load generator (milestone 3)
```

`make test-short` runs the unit tests only, with no database.

Configuration (environment variables):

| Var | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://flashsale:flashsale@localhost:5432/flashsale?sslmode=disable` | Postgres DSN |
| `HTTP_ADDR` | `:8080` | listen address |
| `STRATEGY` | `atomic` | stock strategy: `naive` or `atomic` |
| `HOLD_TTL` | `5m` | how long a hold reserves stock |
| `DB_MAX_CONNS` | `20` | pgxpool MaxConns |
| `REQUEST_TIMEOUT` | `5s` | per-request deadline, including pool wait |
| `SHUTDOWN_TIMEOUT` | `10s` | graceful shutdown limit |
| `PG_PORT` | `5432` | host port for the compose Postgres (Makefile) |

## Design decisions

See [`docs/decisions.md`](docs/decisions.md):
- ADR-001: schema, and where each invariant is enforced
- ADR-002: migrations as embedded plain SQL
- ADR-003: stock strategies, transaction shape (hot-row lock last), and isolation level

## Why `go test -race` is not enough

The race detector finds data races in memory inside one Go process. Oversell is a lost update between *database transactions*. The goroutines share no memory, so `-race` passes even for strategy A. DB correctness is proven by constraints, the invariant checker, and measured runs.
