# mwx-go-flash-sale

A flash-sale API for limited drops (event tickets, limited fan goods) in Go and PostgreSQL. It is built to **prove, with measured numbers, that it never oversells**: 10,000 buyers hit 100 units at the same instant, and no stock is oversold, no buyer is charged twice, and expired holds are released exactly once.

> **Status: milestone 4.** Two stock strategies (A naive, B atomic), idempotency keys, confirm with a mock payment provider (failure injection), an invariant checker, barrier-start concurrency tests, and an HTTP load generator (burst, messy-client, and capacity-ceiling scenarios). Next: the expiry sweeper (`SKIP LOCKED`, several instances), then the main "largest-community drop" scenario L2 (milestone 5).

## Headline results: 10,000 buyers vs 100 units over HTTP

Test plan scenario **L1** ([`docs/test-plan.md`](docs/test-plan.md)). Every buyer has a connection open and is released at the same instant. Oversell is counted from the database, and I1 is also sampled every 50 ms *during* the run. 3 runs per strategy, pool of 20 connections. Ranges are min–max across runs. Per-run numbers and analysis are in [`docs/results.md`](docs/results.md) (run of 2026-11-07 16:00, milestone 4 code).

| Strategy | Oversold (units) | 201 holds | 503 | p50 | p95 | p99 | Winners (201) p95 | Throughput | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| A. naive read-then-write | **1,253–1,507** | 1,353–1,607 | 6,099–6,329 | 5.13–5.18 s | 5.23–5.33 s* | 5.24–5.34 s* | 2.98–3.58 s | 1,873–1,909 req/s | ok |
| B. atomic conditional UPDATE | **0** | **exactly 100** | 0 | 1.00–1.48 s | 1.76–2.78 s | 1.92–2.97 s | 0.42–0.69 s | 3,305–5,105 req/s | ok |

Latency covers every response (201, 409, 503). \*Capped by the API's 5 s `REQUEST_TIMEOUT`: slower requests were cut off as 503 (Service Unavailable), so naive's true tail is worse. H1 = no 5xx except 503, no hung requests, no transport errors.

**Messy clients (strategy B, run of 16:01):** the same burst plus 10% double-clicks, 30% same-key retries, and every winner paying with 5% of payments declined. About 14,160 requests per run (983 duplicates, about 3,077 retries). Across 3 runs: **0 keys that produced two different effects**, exactly 100 holds, 0 invariant violations, and 4–10 declined payments whose stock came back. p95 1.96–3.59 s, p99 2.17–3.79 s, winners p95 0.49–0.89 s. Finding: confirm p50 was 2.3–3.5 s although the mock payment takes 100–300 ms, because winners' confirms queue in the same 20-connection pool as about 14,000 losing requests (see `docs/results.md`).

**Capacity ceiling (scenario L5, strategy B):** **500 req/s** of reservation *writes* (stock never sells out) with p99 ≤ 500 ms, before and after milestone 4. Above it, the server completes only about 554–562 req/s and the queue grows. Postgres alone updated the same kind of single hot row at 4,926 tx/s (pgbench, 20 clients). Milestone 4 added two round trips per reserve, but *before* the row lock, and the ceiling did not move. So the limit tracks round trips made while holding the row lock (consistent with the data, not yet proven). Details in `docs/results.md`.

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, Go 1.26.5, PostgreSQL 16.15 in a Docker VM (2 CPU / 1.9 GB). The load generator, API, and DB all run on one laptop, so the numbers are for comparison, not capacity planning.

What the numbers say:
- The broken strategy is also the slow one. Lost updates keep stock "available" longer, so more buyers queue on the locked row, the pool saturates, and requests time out as 503. They never hang.
- With atomic, the winners are the fast ones (p95 0.42–0.69 s). The 9,900 sold-out answers wait behind them for one of 20 DB connections.
- The in-process concurrency test agrees: with 1,000 goroutines vs 10 units, A oversold in 10/10 runs and B granted exactly 10 in 10/10 runs.

## Invariants and how they are enforced

| # | Invariant | Enforced by | Tested by |
|---|---|---|---|
| 1 | No oversell | Query shape: `UPDATE ... WHERE remaining >= n` (strategy B), plus `CHECK (remaining >= 0)` | `TestConcurrentAtomicNeverOversells`; `TestConcurrentNaiveOversells` shows the failure mode |
| 2 | Per-user limit | Partial UNIQUE index on `(drop_id, user_id) WHERE status IN ('held','confirmed')` | `TestConcurrentSameUserGetsOneHold` (50 concurrent requests from one user → 1 hold, under both strategies) |
| 3 | No double charge | `payments.reservation_id UNIQUE`; provider idempotency key = reservation id; `Idempotency-Key` stored in the same transaction as the effect | `TestConcurrentConfirmChargesOnce` (40 concurrent confirms → 1 payment, 1 charge), `TestConfirmRetryAfterTimeoutDoesNotDoubleCharge`, `TestIdempotentReserveConcurrentSameKey` (50 same-key requests → 1 hold, 50 identical responses); `make load-messy` (0 key violations) |
| 4 | Exactly-once release | _milestone 5 (sweeper with `SKIP LOCKED`)_ | _milestone 5_ |
| 5 | Stock conservation | Checked by `internal/invariants` after every test run; declined payments release stock with a conditional `UPDATE ... WHERE status='held'` (exactly once) | all concurrency tests; `TestConcurrentDeclinedConfirmReleasesOnce`; `TestCheckerDetectsEachViolation` tests the checker itself |

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/drops` | `{"name","total_stock","per_user_limit"?,"price_minor"?,"sale_starts_at"?}` → 201 |
| `GET` | `/drops/{id}` | stock state: `remaining`, `held`, `confirmed` |
| `POST` | `/drops/{id}/reservations` | headers `X-User-Id`, `Idempotency-Key` (required); body `{"quantity"}` optional → 201 (replays carry `Idempotent-Replayed: true`), 409 `sold_out` / `per_user_limit` / `sale_not_started`, 422 `idempotency_key_reused`, 503 on timeout |
| `POST` | `/reservations/{id}/confirm` | headers `X-User-Id`, `Idempotency-Key` → 200 confirmed, 402 `payment_failed` (hold released), 409 `reservation_not_held`, 410 `hold_expired`, 422, 503 `payment_pending` (retry with the same key) |
| `GET` | `/healthz` | 200, or 503 if the DB is unreachable |

## How to run

Requirements: Go 1.25+, Docker (Compose v2 plugin or standalone `docker-compose`).

```sh
make up      # start Postgres 16 and wait until healthy
make test    # go test -race ./... (integration tests use the compose Postgres)
make run     # start the API on :8080 (migrates on start)
make load          # L1: naive vs atomic, 10,000 buyers vs 100 units; appends to docs/results.md
make load-messy    # L1 + 10% double-clicks, 30% same-key retries, winners pay (5% declined)
make load-ceiling  # L5: stepped constant rate 500→8,000 req/s until p99 > 500 ms or errors ≥ 1%
```

`make test-short` runs the unit tests only, with no database.

Load test knobs: `make load BUYERS=1000 STOCK=10 POOL=50 STRATEGIES=atomic REPEAT=3`. `make load`, `make load-messy` and `make load-ceiling` exit with status 1 if H1 fails, or if any strategy other than `naive` violates an invariant. Raw JSON goes to `results/`.

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
| `PAYMENT_MIN_LATENCY` / `PAYMENT_MAX_LATENCY` | `100ms` / `300ms` | mock payment provider latency (uniform) |
| `PAYMENT_FAILURE_RATE` | `0` | share of payments the mock declines (failure injection) |
| `PG_PORT` | `5432` | host port for the compose Postgres (Makefile) |

## Design decisions

See [`docs/decisions.md`](docs/decisions.md):
- ADR-001: schema, and where each invariant is enforced
- ADR-002: migrations as embedded plain SQL
- ADR-003: stock strategies, transaction shape (hot-row lock last), and isolation level
- ADR-004: load generator design (one pre-opened connection per buyer, fresh API per run, DB as referee)
- ADR-005: idempotency keys (key claimed inside the effect's transaction; only successes stored)
- ADR-006: confirm and mock payment (re-runnable state machine; a stale-join bug under READ COMMITTED, found and fixed)

## Why `go test -race` is not enough

The race detector finds data races in memory inside one Go process. Oversell is a lost update between *database transactions*. The goroutines share no memory, so `-race` passes even for strategy A. DB correctness is proven by constraints, the invariant checker, and measured runs.
