# Architecture Decision Records

Short ADRs: context, options, decision, why, trade-offs. Newest at the bottom.

---

## ADR-001: Schema and where each invariant is enforced

**Status:** accepted (milestone 1). Will be refined as strategies land.

### Context
The project has to prove five invariants under concurrent load: no oversell, per-user limit, no double charge, exactly-once release, and stock conservation. Application code can be raced, but database constraints cannot. So any invariant that can be expressed as a constraint should be one.

### Options considered
1. **Compute stock from `reservations` only** (no counter column). This removes drift between a counter and the rows, but every reservation would need `SUM(quantity)` over a growing set plus a lock to stop two buyers seeing the same sum. That throws away the main thing we want to compare: different ways of updating one counter.
2. **Denormalised `drops.remaining` counter + reservation rows** (chosen). The counter is the hot row that strategies A–D decrement. The rows are the audit trail. Conservation (`remaining + held + confirmed = total_stock`) cross-checks them.

### Decision
| Table | Key points |
|---|---|
| `drops` | `remaining` counter with `CHECK (remaining >= 0 AND remaining <= total_stock)`; `per_user_limit` (default 1); `price_minor`; `version` (strategy D only). |
| `reservations` | `status ∈ {held, confirmed, expired, cancelled}` via CHECK; **partial UNIQUE index `(drop_id, user_id) WHERE status IN ('held','confirmed')`**; partial index on `expires_at WHERE status='held'` for the sweeper. |
| `idempotency_keys` | PK `(user_id, key)`; `request_hash` (SHA-256 of method+path+body); `response_status`/`response_body` NULL while in flight. |
| `payments` | `reservation_id UNIQUE`, `idempotency_key UNIQUE`; status `pending/succeeded/failed`. |

Identifiers are `BIGINT GENERATED ALWAYS AS IDENTITY`: compact, ordered, and good enough with no auth or public enumeration concerns. A production system would use UUIDs or an opaque ID.

### How each invariant maps to the schema
| Invariant | Enforced by |
|---|---|
| 1. No oversell | Query shape of the stock strategy (atomic `UPDATE ... WHERE remaining >= n`, or a row lock), backed by `CHECK (remaining >= 0)`. |
| 2. Per-user limit | Partial UNIQUE index: one active reservation per (drop, user). `quantity <= per_user_limit` is checked inside the reservation INSERT. |
| 3. No double charge | `payments.reservation_id UNIQUE` + `payments.idempotency_key UNIQUE`. |
| 4. Exactly-once release | Sweeper changes `held → expired` with `FOR UPDATE SKIP LOCKED` and returns stock in the same transaction (milestone 5). The status transition happens only once. |
| 5. Conservation | Checked after every run by `internal/invariants` (milestone 2). |

### Why the CHECK does not hide strategy A's bug
Strategy A (naive) reads `remaining`, computes `remaining - n` in Go, and writes the **absolute** value back. Two buyers who both read `1` both write `0`. Neither write breaks `remaining >= 0`, so the constraint cannot stop it. This is a classic lost update, and it shows up as `held + confirmed > total_stock`, which is exactly what the invariant checker detects. That is intended, because A exists to demonstrate the failure.

### Trade-offs
- `quantity <= per_user_limit` is not a pure constraint, since a CHECK cannot read another table. A trigger could enforce it, but that hides logic. Checking it in the INSERT's `WHERE` clause is visible and covered by tests. With the default limit of 1 and one active row per user, the UNIQUE index alone already guarantees the limit.
- Any `per_user_limit > 1` model allows one reservation of up to N units, not N separate reservations of 1. This keeps the constraint a simple partial UNIQUE index.
- The counter and the rows can drift if a code path updates one without the other. Mitigation: they are always written in one transaction, and the conservation check runs after every test and load run.

---

## ADR-002: Migrations as embedded plain SQL

**Status:** accepted (milestone 1)

### Context
We need repeatable schema setup for local runs, tests (one fresh database per test), and CI. Allowed options are `goose` or plain embedded SQL.

### Decision
Plain `.sql` files under `internal/db/migrations/`, compiled in with `//go:embed`, and applied in lexical order by a runner of about 60 lines (`internal/db/db.go`):
- `schema_migrations(version)` records what has been applied.
- Each file and its bookkeeping row run in **one transaction**, so a failure leaves no partial schema.
- `pg_advisory_lock` serialises concurrent `Migrate` calls (several API instances or parallel tests starting together).
- The API migrates on start-up.

### Why
There is no extra dependency or CLI to install. The binary carries its own schema. The behaviour is small enough to explain line by line in an interview.

### Trade-offs
- No down-migrations. For this project, `make reset` (drop the volume) is the rollback.
- Statements that cannot run inside a transaction (`CREATE INDEX CONCURRENTLY`) are unsupported. That doesn't matter at this scale. On a live production table, `goose` with `-- +goose NO TRANSACTION` would be the next step.

### Note on Go version
`pgx/v5` v5.12 requires Go ≥ 1.25, so `go.mod` declares `go 1.25.0`. That satisfies the project's "Go 1.22+" rule.

---

## ADR-003: Stock strategies, transaction shape, and isolation level

**Status:** accepted (milestone 2). Strategies A and B are implemented; C (FOR UPDATE) and D (optimistic) come later.

### Context
Every buyer decrements the same `drops.remaining` row, so this is a single hot row. We want to compare several decrement strategies under identical conditions. Measured results must reflect the strategy, not unrelated differences in the surrounding code.

### Decision: one interface, one transaction shape
```go
type StockStrategy interface {
    Name() string
    Take(ctx context.Context, tx pgx.Tx, dropID int64, n int) error // ErrSoldOut if < n left
}
```
The strategy is chosen with `STRATEGY=naive|atomic`. `sale.Service.Reserve` owns everything else, so every strategy runs inside the same flow:

1. **Lock-free pre-check** (no transaction): drop exists, sale started, `qty <= per_user_limit`, `remaining >= qty`. Otherwise it rejects immediately.
2. `BEGIN` (READ COMMITTED)
3. `INSERT` the reservation. The partial UNIQUE index rejects a second active hold for the same user (maps to `ErrPerUserLimit`).
4. `strategy.Take(...)` on the hot row, as the **last** statement.
5. `COMMIT`. Any error rolls back both the row and the decrement.

### Why this order
- **Hot-row lock last:** the row lock taken by `UPDATE drops` is held until `COMMIT`. Putting it after the INSERT means the lock is held for one statement plus the commit, not for the whole transaction. Every microsecond a lock is held on the hot row is a microsecond every other buyer waits.
- **Per-user check first:** concurrent requests from the same user collide on the unique index, which is a per-user row rather than the global hot row. So duplicate clicks from one user don't add contention for everybody else.
- **Pre-check is a hint, not a guard.** It reads without locks, so it can be stale. A stale read can only cause an extra trip into the transaction, or a "sold out" answer slightly early if stock is about to be released. It can never cause an oversell, because the real check is step 4. After sell-out, about 99% of flash-sale traffic is answered by this one cheap SELECT without touching the hot-row lock. It applies equally to every strategy, so the comparison stays fair.
- **Error precedence:** because the pre-check runs first, a sold-out drop answers `409 sold_out` even to a user who already holds a unit. That is acceptable: both statements are true, and the cheaper check wins.

### Strategy A: naive read-then-write (intentionally broken)
`SELECT remaining`, then compare in Go, then `UPDATE drops SET remaining = $computed`. At READ COMMITTED, the plain SELECT takes no lock. N transactions can read the same value, and each writes `value - 1`. The writes serialize on the row lock, but each one overwrites the previous one: a **lost update**. `CHECK (remaining >= 0)` cannot catch it, because no write goes negative. Measured: it oversold in 10/10 runs (see `results.md`).

### Strategy B: atomic conditional UPDATE (primary choice)
`UPDATE drops SET remaining = remaining - $n WHERE id = $1 AND remaining >= $n`. Zero rows affected means sold out.

**Why it is correct at READ COMMITTED:** when the UPDATE finds the row locked by another transaction, it waits. After that transaction commits, Postgres re-reads the **latest committed version** of the row and re-evaluates the `WHERE` clause against it before writing. (In the Postgres source this is EvalPlanQual. The docs call it "the second updater ... re-evaluate[s] its search condition".) So `remaining >= n` is always judged against the true current value. The check and the write are one atomic step on the row.

### Isolation level
| Strategy | Level | Why |
|---|---|---|
| A naive | READ COMMITTED | The production default. We want to show the bug that real code with default settings has. |
| B atomic | READ COMMITTED | Correct as described above. A stronger level would add serialization failures and retries for no gain. |
| C FOR UPDATE (later) | READ COMMITTED | The explicit row lock gives the guarantee. |
| D optimistic (later) | READ COMMITTED | The version column gives the guarantee, and conflicts are retried in app code. |

**Rejected: SERIALIZABLE for everything.** It would make A correct too, but by aborting conflicting transactions (`40001`), which then need retry loops. On a single hot row almost every transaction conflicts, so throughput would collapse into retries. This is a candidate for an extra measured comparison if time allows.

### Error mapping (HTTP)
| Error | Status |
|---|---|
| invalid input / missing `X-User-Id` / missing `Idempotency-Key` | 400 |
| drop not found | 404 |
| sold out, per-user limit, sale not started | 409 (with distinct `error` codes) |
| context deadline exceeded (including waiting for a pool connection) | **503**, never a hang |

### Trade-offs
- Inserting before decrementing means sold-out buyers who pass the stale pre-check do a wasted INSERT and rollback. That is cheap and only happens in the instant around sell-out.
- Rolled-back INSERTs consume identity values, so reservation IDs have gaps. That is harmless.
- `Idempotency-Key` is required by the API now but not stored until milestone 4.

---

## ADR-004: Load generator design

**Status:** accepted (milestone 3)

### Context
The load test must show what happens when 10,000 buyers arrive *at the same instant*. If the client spreads the requests out (queueing, connection setup, slow dials), the burst softens, race windows shrink, and the measurements flatter the server.

### Decisions
1. **One pre-opened TCP connection per buyer.** Setup dials all N connections before the barrier, with at most 64 dials in flight so the listen backlog (`somaxconn` = 128 on macOS) never overflows. A dropped SYN would add a ~1 s retransmit that looks like server latency. Each buyer gets its own `http.Transport` that can only use its own connection.
   - *Rejected:* one shared client with `MaxConnsPerHost = N`. A shared pool gives an idle connection to whichever request asks first, so a warm-up cannot guarantee N distinct sockets. Requests without one queue in the client or dial during the burst. That is client-side throttling, the opposite of what we want to measure.
2. **Same start barrier as the tests** (`close(start)`). Requests and headers are built *before* the barrier, so only the send is timed.
3. **A fresh API process per run.** loadgen starts `bin/api` with the run's `STRATEGY` and `DB_MAX_CONNS`, waits for `/healthz`, runs, then sends SIGINT. Strategies never share a warm process, and pool size is a real start-up setting.
4. **The DB is the referee.** "Oversold" in the results comes from `invariants.Check` against Postgres, not from counting HTTP 201s.
5. **`SO_LINGER=0` on client sockets.** Closing 10,000 sockets normally leaves them in TIME_WAIT (~30 s on macOS). The ephemeral range is only 16,384 ports, and the second run in our first attempt failed with `can't assign requested address`. Linger 0 closes with RST, which is fine for a load generator and wrong for a real client.
6. **Timeouts:** server `REQUEST_TIMEOUT` = 5 s (pool wait included, then 503). Client timeout = 30 s, so a server 503 is recorded as a 503, not as a client timeout.
7. **Exit code:** 1 if any strategy except `naive` violates an invariant. Naive is expected to fail. CI (milestone 6) relies on this.

### Trade-offs and limits
- The client, API, and Postgres share one laptop. The load generator's 10,000 goroutines compete for the same CPU as the API. Numbers are for comparison, not capacity.
- On macOS, Postgres runs in a Docker VM (here 2 CPU / 1.9 GB) behind port forwarding. The machine spec in `results.md` records the VM size.
- Percentiles are exact (all samples sorted). That is fine at 10,000 samples per run; a streaming histogram would be needed at millions.

### Addendum (test plan L1/L5)
8. **H1 is checked per run:** any 5xx other than 503, any transport error, or any request slower than client timeout + 1 s (or a client timeout) fails the run. All of these count, including for `naive`, because H1 is about overload behavior, not stock correctness.
9. **I1 is sampled during the burst**, every 50 ms, through loadgen's own 4-connection pool (never the API's), plus once at the end. "At all times" is checked as "at every sample", which is as close as an external observer can get.
10. **Capacity ceiling (L5) is open-loop.** Requests are scheduled in 10 ms slots at the target rate whether or not earlier ones have finished, and latency is measured from the *scheduled* time. A closed-loop client (send the next request only after the previous returns) slows down exactly when the server does and hides the queue. This is "coordinated omission": it makes overloaded steps look fast.
11. **L5 uses a shared keep-alive pool capped at 2,000 connections**, unlike the burst. A constant rate is what a reusable pool is for, and the cap stops an overloaded step from exhausting ports. The cost is that, past the ceiling, the backlog queues in the client rather than reaching the server as 503s. It is still counted as latency (point 10), and the step fails either way.
12. **Every result records `git rev-parse --short HEAD`**, suffixed `-dirty` when tracked files have uncommitted changes, so each number traces back to the code that produced it.
