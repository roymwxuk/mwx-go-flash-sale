# mwx-go-flash-sale

A flash-sale API for limited drops (event tickets, limited fan goods) in Go and PostgreSQL. It is built to **prove, with measured numbers, that it never oversells**: 10,000 buyers hit 100 units at the same instant, and no stock is oversold, no buyer is charged twice, and expired holds are released exactly once.

**Why it's hard:** every buyer decrements the same database row. A plain "read stock, then write stock" lets thousands of buyers see the same last unit (measured below: it oversold 100 units by up to 1,499). Retries and double-clicks can charge a card twice, and a background job that releases unpaid holds can return the same unit twice if two copies of it run. This project builds the API, then proves each guarantee with database constraints, concurrency tests, and a load generator that checks the database after every run.

> **Status:** three stock strategies (A naive, B atomic, C pessimistic), idempotency keys, confirm with a mock payment provider (failure injection), an expiry sweeper safe with N instances, an invariant checker, barrier-start concurrency tests, and an HTTP load generator (burst, messy-client, and capacity-ceiling scenarios). **Not done yet:** CI, and the pool-size comparison (see [Known limitations](#known-limitations)).

## Headline results: 10,000 buyers vs 100 units over HTTP

Test plan scenario **L1** ([`docs/test-plan.md`](docs/test-plan.md)). Every buyer has a connection open and is released at the same instant. Oversell is counted from the database, and I1 is also sampled every 50 ms *during* the run. 3 runs per strategy, pool of 20 connections. Ranges are min–max across runs. Per-run numbers and analysis are in [`docs/results.md`](docs/results.md) (run of 2026-11-07 16:14).

| Strategy | Oversold (units) | 201 holds | 503 | p50 | p95 | p99 | Winners (201) p95 | Throughput | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| A. naive read-then-write | **1,142–1,499** | 1,242–1,599 | 4,361–6,257 | 4.77–5.10 s | 5.16–5.25 s* | 5.16–5.25 s* | 2.31–2.98 s | 1,903–1,937 req/s | ok |
| B. atomic conditional UPDATE | **0** | **exactly 100** | 0 | 0.95–2.55 s | 1.79–4.39 s | 1.95–4.57 s | 0.42–0.88 s | 2,171–5,034 req/s | ok |
| C. pessimistic `FOR NO KEY UPDATE` | **0** | **exactly 100** | 0 | 1.02–1.19 s | 1.59–3.59 s | 1.88–3.92 s | 0.38–0.54 s | 2,507–5,097 req/s | ok |

Latency covers every response (201, 409, 503). \*Capped by the API's 5 s `REQUEST_TIMEOUT`: slower requests were cut off as 503 (Service Unavailable), so naive's true tail is worse. H1 = no 5xx except 503, no hung requests, no transport errors.

**Messy clients (strategy B, run of 16:01):** the same burst plus 10% double-clicks, 30% same-key retries, and every winner paying with 5% of payments declined. About 14,160 requests per run (983 duplicates, about 3,077 retries). Across 3 runs: **0 keys that produced two different effects**, exactly 100 holds, 0 invariant violations, and 4–10 declined payments whose stock came back. p95 1.96–3.59 s, p99 2.17–3.79 s, winners p95 0.49–0.89 s. Finding: confirm p50 was 2.3–3.5 s although the mock payment takes 100–300 ms, because winners' confirms queue in the same 20-connection pool as about 14,000 losing requests (see `docs/results.md`).

**B vs C on the write path (L5, same session, 2 runs each):** atomic sustained **750 req/s** in both runs; pessimistic reached **500 and 250 req/s**, because it holds the hot-row lock one round trip longer (SELECT → UPDATE → COMMIT instead of UPDATE → COMMIT). In the burst above they can't be told apart, since only 100 of 10,000 requests write. Decision and reasoning: ADR-007.

**Capacity ceiling (scenario L5, strategy B), earlier sessions:** **500 req/s** of reservation *writes* (stock never sells out) with p99 ≤ 500 ms, before and after milestone 4. Above it, the server completes only about 554–562 req/s and the queue grows. Postgres alone updated the same kind of single hot row at 4,926 tx/s (pgbench, 20 clients). Milestone 4 added two round trips per reserve, but *before* the row lock, and the ceiling did not move. So the limit tracks round trips made while holding the row lock (consistent with the data, not yet proven). Details in `docs/results.md`.

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, Go 1.26.5, PostgreSQL 16.15 in a Docker VM (2 CPU / 1.9 GB). The load generator, API, and DB all run on one laptop, so the numbers are for comparison, not capacity planning.

What the numbers say:
- The broken strategy is also the slow one. Lost updates keep stock "available" longer, so more buyers queue on the locked row, the pool saturates, and requests time out as 503. They never hang.
- With atomic, the winners are the fast ones (p95 0.42–0.88 s). The 9,900 sold-out answers wait behind them for one of 20 DB connections.
- The in-process concurrency test agrees: with 1,000 goroutines vs 10 units, A oversold in 10/10 runs and B granted exactly 10 in 10/10 runs.

## Invariants and how they are enforced

| # | Invariant | Enforced by | Tested by |
|---|---|---|---|
| 1 | No oversell | Query shape: `UPDATE ... WHERE remaining >= n` (B) or a `FOR NO KEY UPDATE` row lock (C), plus `CHECK (remaining >= 0)` | `TestConcurrentCorrectStrategiesNeverOversell` (B, C); `TestConcurrentNaiveOversells` shows the failure mode |
| 2 | Per-user limit | Partial UNIQUE index on `(drop_id, user_id) WHERE status IN ('held','confirmed')` | `TestConcurrentSameUserGetsOneHold` (50 concurrent requests from one user → 1 hold, under both strategies) |
| 3 | No double charge | `payments.reservation_id UNIQUE`; provider idempotency key = reservation id; `Idempotency-Key` stored in the same transaction as the effect | `TestConcurrentConfirmChargesOnce` (40 concurrent confirms → 1 payment, 1 charge), `TestConfirmRetryAfterTimeoutDoesNotDoubleCharge`, `TestIdempotentReserveConcurrentSameKey` (50 same-key requests → 1 hold, 50 identical responses); `make load-messy` (0 key violations) |
| 4 | Exactly-once release | Sweeper claims holds with `FOR UPDATE SKIP LOCKED`, transitions `held→expired` and refunds in one statement guarded by `status='held'`; `CHECK (remaining <= total_stock)` rejects over-refunds; confirm extends `expires_at` so a paying hold is never expired | `TestConcurrentSweepersReleaseEachHoldOnce` (3 sweepers, 200 holds, 10/10 runs exact), `TestConfirmRacingSweeper` (150 confirms around expiry, 10/10 runs, 0 charged-but-expired), `TestPoisonedDropDoesNotBlockOthers` |
| 5 | Stock conservation | Checked by `internal/invariants` after every test run; declined payments release stock with a conditional `UPDATE ... WHERE status='held'` (exactly once) | all concurrency tests; `TestConcurrentDeclinedConfirmReleasesOnce`; `TestCheckerDetectsEachViolation` tests the checker itself |

## Architecture

```mermaid
flowchart LR
    subgraph clients["Clients"]
        LG["cmd/loadgen<br/>N buyers, start barrier"]
        T["go test<br/>concurrency tests"]
    end

    subgraph api["cmd/api (one or more instances)"]
        H["httpapi<br/>per-request deadline → 503"]
        S["sale.Service<br/>Reserve / Confirm"]
        K["Idempotency-Key<br/>claimed in the same tx"]
        ST{{"StockStrategy<br/>A naive · B atomic · C pessimistic"}}
        PAY["payment.Mock<br/>latency + failure rate<br/>provider key = reservation id"]
        SW["sweeper goroutine<br/>FOR UPDATE SKIP LOCKED"]
        POOL[("pgxpool<br/>MaxConns 20")]
    end

    subgraph pg["PostgreSQL 16"]
        DR["drops<br/>remaining (hot row)<br/>CHECK 0 ≤ remaining ≤ total"]
        RS["reservations<br/>partial UNIQUE (drop, user)<br/>WHERE held/confirmed"]
        IK["idempotency_keys<br/>PK (user, key)"]
        PM["payments<br/>UNIQUE reservation_id"]
    end

    INV["internal/invariants<br/>checks I1–I5 from the DB"]

    LG -->|HTTP| H
    T --> S
    H --> S
    S --> K
    S --> ST
    S -->|"outside any tx"| PAY
    K --> POOL
    ST --> POOL
    SW --> POOL
    POOL --> DR
    POOL --> RS
    POOL --> IK
    POOL --> PM
    LG -.->|after each run and every 50 ms| INV
    T -.-> INV
    INV -.-> pg
```

**Reserve** (one transaction, READ COMMITTED): lock-free pre-check (answers sold-out with one query) → `BEGIN` → claim the idempotency key → INSERT the reservation (the partial UNIQUE index enforces the per-user limit) → take stock on the hot row **last**, so its lock is held as briefly as possible → `COMMIT`.
**Confirm** (two short transactions around the payment call): lock the hold and record a `pending` payment → charge outside any transaction, with the provider key `res-<reservation id>` → conditional state transitions that are safe to run twice.
**Sweeper** (in every instance): per drop, one statement claims expired holds with `SKIP LOCKED`, flips them `held → expired`, and refunds the stock.

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
| `STRATEGY` | `atomic` | stock strategy: `naive`, `atomic`, or `pessimistic` |
| `HOLD_TTL` | `5m` | how long a hold reserves stock |
| `DB_MAX_CONNS` | `20` | pgxpool MaxConns |
| `REQUEST_TIMEOUT` | `5s` | per-request deadline, including pool wait |
| `SHUTDOWN_TIMEOUT` | `10s` | graceful shutdown limit |
| `PAYMENT_MIN_LATENCY` / `PAYMENT_MAX_LATENCY` | `100ms` / `300ms` | mock payment provider latency (uniform) |
| `PAYMENT_FAILURE_RATE` | `0` | share of payments the mock declines (failure injection) |
| `SWEEP_INTERVAL` | `1s` | how often each instance's sweeper looks for expired holds |
| `SWEEP_BATCH` | `500` | holds per sweeper statement (per drop) |
| `IDEMPOTENCY_KEY_TTL` | `24h` | idempotency keys older than this are deleted by the sweeper |
| `PG_PORT` | `5432` | host port for the compose Postgres (Makefile) |

## Key decisions

Full ADRs (context, options, decision, why, trade-offs) are in [`docs/decisions.md`](docs/decisions.md).

| ADR | Decision | One-line why |
|---|---|---|
| [001](docs/decisions.md#adr-001-schema-and-where-each-invariant-is-enforced) | `drops.remaining` counter + reservation rows; every invariant that can be a constraint is one | Application code can be raced; constraints can't |
| [002](docs/decisions.md#adr-002-migrations-as-embedded-plain-sql) | Plain SQL migrations embedded with `//go:embed`, advisory-locked | No extra dependency; explainable line by line |
| [003](docs/decisions.md#adr-003-stock-strategies-transaction-shape-and-isolation-level) | One `StockStrategy` interface, one transaction shape, READ COMMITTED for all | Measured differences come from the strategy alone; hot-row lock taken last |
| [004](docs/decisions.md#adr-004-load-generator-design) | One pre-opened connection per buyer, fresh API per run, DB is the referee | A client that spreads out the burst flatters the server |
| [005](docs/decisions.md#adr-005-idempotency-keys) | Key claimed *inside* the effect's transaction; only successes stored | Duplicates are serialized by Postgres with no polling and no lease |
| [006](docs/decisions.md#adr-006-confirm-and-mock-payment) | Confirm is a re-runnable state machine; provider key = reservation id | No transaction held across a network call, and no double charge |
| [007](docs/decisions.md#adr-007-choosing-the-stock-strategy-a-vs-b-vs-c) | **B, the atomic conditional UPDATE**; D cut | Shortest lock hold on the hot row: 750 vs 500/250 req/s against C |
| [008](docs/decisions.md#adr-008-expiry-sweeper) | `SKIP LOCKED` sweeper in every instance, per drop | Exactly-once in three independent layers |
| [009](docs/decisions.md#adr-009-connection-pool-sizing-and-behavior-under-pool-exhaustion) | Pool of 20; one 5 s deadline including the pool wait → 503 | Exhaustion must end in an answer, never a hang |
| [010](docs/decisions.md#adr-010-the-hot-row-at-100-scale-written-discussion-not-built) | 100× scale: written discussion only | Sold-out reads, not writes, are what grows |

Bugs found along the way, each with a regression test: a stale joined row under READ COMMITTED (ADR-006), a `FOR UPDATE` vs foreign-key deadlock (ADR-007), a confirm-vs-sweeper race, and a poisoned drop stalling the sweeper (ADR-008).

## Why `go test -race` is not enough

The race detector finds data races in memory inside one Go process. Oversell is a lost update between *database transactions*. The goroutines share no memory, so `-race` passes even for strategy A. DB correctness is proven by constraints, the invariant checker, and measured runs.

## Known limitations

- **One laptop.** The load generator, API and Postgres share one machine (Postgres in a 2-CPU Docker VM), so the numbers compare strategies; they aren't capacity figures. Throughput varied up to about 2× between runs of the same code.
- **CI is not built yet** (GitHub Actions with `go vet`, `go test -race` and a load smoke test). `make test` and `make load` exit non-zero on failure, so a CI job only needs to call them.
- **Pool size was not compared.** Every run used 20 connections. The hypothesis that burst latency is mostly pool wait, not lock wait, is untested ([ADR-009]({D}adr-009-connection-pool-sizing-and-behavior-under-pool-exhaustion)).
- **Strategy D (optimistic version column) and a SERIALIZABLE run were cut.** The reasoning against them in ADR-003 and ADR-007 is argument, not measurement.
- **Payments that stay pending forever** (provider outage, and the client never retries) keep their stock held. Production needs a reconciliation job that asks the provider for the outcome.
- **The mock payment provider lives in memory, per API process.** With several instances, a real provider would be the shared source of truth.
- **No auth.** `X-User-Id` is trusted as given.

## What I'd change at 100× scale

Written discussion only, not built ([ADR-010]({D}adr-010-the-hot-row-at-100-scale-written-discussion-not-built)). In a 100-unit drop only about 100 requests ever write, so 100× buyers mostly means 100× **sold-out reads**, each of which still takes a DB connection today.

1. **Answer sold-out before taking a DB connection**: a per-drop "sold out" flag cached for 100–500 ms. It can only say *no*, so it can never cause an oversell; the atomic UPDATE stays the guard.
2. **Hold the row lock for fewer round trips**: reserve in one statement (CTE or stored function). Our full path tops out at 500–750 req/s, while Postgres alone did 4,926 single-row updates/s in the same VM.
3. **Admission control / waiting room**: past the ceiling, goodput *fell* (750 → about 540 req/s), so let buyers in at the rate the row can absorb.
4. **Stock buckets (sharded counters)** only for large drops where the write rate itself is the limit; **queue-based allocation** where exact arrival order must decide winners.

## 日本語サマリー

限定ドロップ（チケット・限定グッズ）向けのフラッシュセールAPIを Go と PostgreSQL で実装し、1万人が100個の在庫に同時アクセスしても売り越さないことを計測で示した。素朴な read-then-write 方式は1,142〜1,499個を売り越した一方、条件付きアトミックUPDATE（採用）と悲観ロックはどの試行でも正確に100個だった。同一セッションの書き込み上限試験ではアトミックUPDATEが750 req/s、悲観ロックが500・250 req/sで、ホット行のロック保持時間の差が出た。冪等キーは効果と同一トランザクションで確保し、二重クリックと再送を含む1回あたり約14,160リクエストで、二重の効果はゼロ。期限切れ予約は `FOR UPDATE SKIP LOCKED` のスイーパー3台で10回中10回、各予約をちょうど1回だけ解放した。

