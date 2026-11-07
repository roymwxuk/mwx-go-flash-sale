# mwx-go-flash-sale

A flash-sale API for limited drops (event tickets, limited fan goods) in Go and PostgreSQL. It is built to **prove, with measured numbers, that it never oversells**: 10,000 buyers hit 100 units at the same instant, and no stock is oversold, no buyer is charged twice, and expired holds are released exactly once.

> **Status: milestone 3 + test-plan scenarios L1 and L5.** Two stock strategies (A naive, B atomic), an invariant checker, barrier-start concurrency tests, and an HTTP load generator (burst and capacity-ceiling scenarios). Next: idempotency keys, confirm + mock payment (milestone 4), then the main "largest-community drop" scenario L2 once holds expire (milestone 5).

## Headline results: 10,000 buyers vs 100 units over HTTP

Test plan scenario **L1** ([`docs/test-plan.md`](docs/test-plan.md)). Every buyer has a connection open and is released at the same instant. Oversell is counted from the database, and I1 is also sampled every 50 ms *during* the run. 3 runs per strategy, pool of 20 connections. Ranges are min–max across runs. Per-run numbers and analysis are in [`docs/results.md`](docs/results.md) (run of 2026-11-07 15:26).

| Strategy | Oversold (units) | 201 holds | 503 | p50 | p95 | p99 | Winners (201) p95 | Throughput | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| A. naive read-then-write | **1,444–2,016** | 1,544–2,116 | 3,067–5,860 | 4.42–5.19 s | 5.20–5.29 s* | 5.20–5.29 s* | 2.62–3.61 s | 1,889–1,921 req/s | ok |
| B. atomic conditional UPDATE | **0** | **exactly 100** | 0 | 0.74–1.15 s | 1.20–2.13 s | 1.31–2.23 s | 0.37–0.60 s | 4,422–7,489 req/s | ok |

Latency covers every response (201, 409, 503). \*Capped by the API's 5 s `REQUEST_TIMEOUT`: slower requests were cut off as 503, so naive's true tail is worse. H1 = no 5xx except 503, no hung requests, no transport errors.

**Capacity ceiling (scenario L5, strategy B):** **500 req/s** of reservation *writes* (stock never sells out) with p99 ≤ 500 ms. At 1,000 req/s the server completed about 554 req/s and the queue grew. For comparison, Postgres alone updated the same kind of single hot row at 4,926 tx/s (pgbench, 20 clients), so the limit is the reserve path's round trips while holding the row lock, not the row. Details in `docs/results.md`.

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, Go 1.26.5, PostgreSQL 16.15 in a Docker VM (2 CPU / 1.9 GB). The load generator, API, and DB all run on one laptop, so the numbers are for comparison, not capacity planning.

What the numbers say:
- The broken strategy is also the slow one. Lost updates keep stock "available" longer, so more buyers queue on the locked row, the pool saturates, and requests time out as 503. They never hang.
- With atomic, the winners are the fast ones (p95 0.37–0.60 s). The 9,900 sold-out answers wait behind them for one of 20 DB connections.
- The in-process concurrency test agrees: with 1,000 goroutines vs 10 units, A oversold in 10/10 runs and B granted exactly 10 in 10/10 runs.

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
make load          # L1: naive vs atomic, 10,000 buyers vs 100 units; appends to docs/results.md
make load-ceiling  # L5: stepped constant rate 500→8,000 req/s until p99 > 500 ms or errors ≥ 1%
```

`make test-short` runs the unit tests only, with no database.

Load test knobs: `make load BUYERS=1000 STOCK=10 POOL=50 STRATEGIES=atomic REPEAT=3`. `make load` and `make load-ceiling` exit with status 1 if H1 fails, or if any strategy other than `naive` violates an invariant. Raw JSON goes to `results/`.

**OS limits for 10,000 buyers (one machine):**
- **File descriptors:** each process (loadgen and API) needs about one per buyer. Go raises the soft `ulimit -n` to the hard limit at start-up. On macOS, `kern.maxfilesperproc` (61,440 here) is the real cap. On Linux, check `ulimit -Hn`.
- **Ephemeral ports:** macOS gives 16,384 (`net.inet.ip.portrange.first`–`last`), so keep `BUYERS` below about 15,000. loadgen closes sockets with `SO_LINGER=0` so back-to-back runs don't run out of ports while old ones sit in TIME_WAIT.
- **Listen backlog:** `kern.ipc.somaxconn` is 128 on macOS. loadgen opens connections at most 64 at a time before the burst, so the backlog never overflows.

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
- ADR-004: load generator design (one pre-opened connection per buyer, fresh API per run, DB as referee)

## Why `go test -race` is not enough

The race detector finds data races in memory inside one Go process. Oversell is a lost update between *database transactions*. The goroutines share no memory, so `-race` passes even for strategy A. DB correctness is proven by constraints, the invariant checker, and measured runs.
