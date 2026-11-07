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

**Status:** accepted (milestone 2). A and B implemented in milestone 2, C (pessimistic) later. D (optimistic) was cut. The final choice and the numbers are in ADR-007.

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
| C pessimistic | READ COMMITTED | The explicit row lock (`FOR NO KEY UPDATE`, see ADR-007) gives the guarantee. |
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

---

## ADR-005: Idempotency keys

**Status:** accepted (milestone 4)

### Context
Clients time out and retry, and users double-click. Every reservation and confirm request carries an `Idempotency-Key`. The guarantee: **the same key and the same request produce at most one effect and the same response; the same key with a different request gets 422.** Two identical requests can arrive in the same millisecond, on different connections.

### Options considered
1. **Check, then insert** (SELECT the key; if missing, do the work and store it). Two concurrent requests both see "missing" and both do the work. Rejected: this is the naive strategy's race again.
2. **Insert the key first in its own transaction, then do the work, then store the response** (insert-first with a status column). Duplicates see an "in progress" row and must poll or wait. A crash between the key insert and the work leaves a stuck key that needs a lease and takeover logic.
3. **Insert the key inside the same transaction as the effect** (chosen for reserve). Concurrent duplicates block on the key's primary key until the first transaction ends. On COMMIT, the duplicate's `INSERT ... ON CONFLICT DO NOTHING` affects 0 rows, and it replays the stored response. On ROLLBACK, the duplicate's insert succeeds and it does the work itself.

### Decision
- **Storage:** `idempotency_keys`, PK `(user_id, key)`, so keys are scoped per user and two users can't collide. It also stores `endpoint`, `request_hash`, `response_status`, and `response_body` (the exact bytes sent).
- **Request identity:** `request_hash` = SHA-256 of the endpoint template plus the request's *meaning* (drop id and quantity; reservation id). `{}` and `{"quantity":1}` therefore count as the same request, while another drop, another quantity, or another endpoint is a different one and gets **422 `idempotency_key_reused`**.
- **Reserve:** the replay lookup is folded into the existing pre-check query (`drops LEFT JOIN idempotency_keys`), so the sold-out path still costs **one** query. Inside the transaction, the order is: claim key → insert reservation → store response → take stock (hot row last, as in ADR-003). Concurrent duplicates are serialized by the database, with no polling and no lease.
- **Only successful effects are stored.** Validation errors, not found, sale not started, sold out, and per-user limit are *not* stored. They had no side effect, so re-running them is harmless, and storing them would add a write to the sold-out path, which carries 99% of flash-sale traffic. Consequence (tested by `TestRejectionsAreNotStored`): a sold-out answer under key K doesn't stick to K, so if a declined payment returns a unit, retrying K can succeed. For a buyer, that's the desirable behavior.
- **Replays** are byte-identical and carry `Idempotent-Replayed: true`.
- **Confirm** uses the same table, but can't keep a transaction open across the payment call (ADR-006). It stores the final response after the payment settles. Its correctness doesn't depend on the key at all (confirm is idempotent by state), so a duplicate that slips past the key check just re-drives the same state machine to the same answer.
- **TTL:** keys are meant to live for 24 h. Deleting old keys is a job for the background worker in milestone 5. Until then, keys don't expire.

### Evidence
- `TestIdempotentReserveConcurrentSameKey` (C3): 50 identical requests at the same instant give 1 reservation, 50 byte-identical responses, and 49 replays, under both strategies. A mutation test (removing the key claim) makes it fail.
- `TestIdempotencyKeyReuseWithDifferentRequest` (C4): different quantity, drop, or endpoint gives 422 with no effect.
- Load (`make load-messy`, 10,000 buyers): about 14,160 requests per run, including 983 double-clicks and about 3,077 same-key retries, with **0 keys that produced two different successful responses**.

### Trade-offs
- A duplicate waiting on the key holds a pool connection for as long as the first transaction runs (a few ms). A storm of same-key duplicates could therefore use up pool connections. Accepted: per-key duplicates are few, and the alternative (polling) costs more queries overall.
- The success path makes about 7 round trips instead of 5. The extra ones (claim, store) happen before the stock row is locked, so the hot-row lock time doesn't change. Measured: the write ceiling stayed at 500 req/s.

---

## ADR-006: Confirm and mock payment

**Status:** accepted (milestone 4)

### Context
Confirming means charging money through an external provider: 100–300 ms, may decline, may time out with an unknown outcome. The invariants: **never charge twice** (I3), **a confirmed reservation always has exactly one successful payment and vice versa**, and **a declined payment returns the stock exactly once** (I5).

### Options considered
1. **One transaction around the payment call.** That holds a pool connection and the reservation's row lock for the whole payment latency, and a timeout mid-call rolls back the DB record of a charge that may have happened. Rejected.
2. **Separate steps, with the provider's idempotency key derived from the reservation** (chosen).

### Decision: a re-runnable state machine
1. **tx1:** `SELECT ... FOR UPDATE` on the reservation (owner, status, expiry), then INSERT `payments(status='pending')`. `payments.reservation_id UNIQUE` allows one payment row per reservation, ever.
2. **Charge** the provider with key `res-<reservation id>`, outside any transaction. Every confirm attempt for a reservation (retries, double-clicks, *different* client keys) sends the same provider key, so the provider charges at most once.
3. **tx2:** conditional transitions only. Approved: `pending→succeeded` and `held→confirmed`. Declined: `pending→failed` and `held→cancelled`, plus `remaining += quantity` only when that `UPDATE ... WHERE status='held'` actually matched a row. Running tx2 twice changes nothing the second time, so stock is released **exactly once**.
4. **Store** the response under the client's Idempotency-Key (200 with the confirmed reservation, or 402 `payment_failed`).

Calling Confirm again, concurrently or later, just drives the same reservation to the same final state.

### Failure handling
| Situation | Behavior |
|---|---|
| Provider declines | 402, hold cancelled, stock returned once. A retry with the same key replays the 402. |
| Provider call times out or the client disconnects | Payment stays `pending`. The API answers **503 `payment_pending`**: retry with the same key. The retry asks the provider again with the same provider key and gets the recorded outcome, with no second charge. |
| Client disconnects *after* the provider answered | tx2 runs with `context.WithoutCancel` plus a 5 s timeout, so the DB records what the provider did even though nobody is listening. |
| Hold expired before confirm | 410 `hold_expired`, nothing charged (C8). |
| Hold expires *while* a payment is pending | Confirm resumes it (the money may have moved). The milestone 5 sweeper must skip reservations with a pending payment. |
| Another user's reservation | 404, so other users' reservation IDs aren't revealed. |

### The mock provider
`internal/payment.Mock`: configurable latency range and failure rate (`PAYMENT_MIN_LATENCY`, `PAYMENT_MAX_LATENCY`, `PAYMENT_FAILURE_RATE`), idempotent per key, thread-safe. It records the outcome **before** the simulated latency, so a caller that times out has really been charged. That's the ambiguous case every real integration has to handle, and the tests exercise it. It's in-memory per API process; with several instances, a real provider would be the shared source of truth.

### Bug found during this milestone: a stale joined row under READ COMMITTED
The first version of tx1 read the reservation **and** its payment in one statement with `FOR UPDATE OF r`. `TestConcurrentDeclinedConfirmReleasesOnce` (20 concurrent confirms of a declined payment) failed 3 times in 5 runs: some callers got 409 "not held" instead of 402. **Cause:** when a READ COMMITTED statement waits for a row lock, Postgres re-reads *only the locked row* at its newest version after the wait; rows joined from other tables still come from the snapshot taken *before* the wait. A waiter could therefore see a fresh `cancelled` reservation next to a stale `pending` payment, a combination that never existed. **Fix:** take the lock first, then read the payment in a separate statement, which gets a fresh snapshot. The test then passed 15/15 under `-race`. Stock was never released twice, even before the fix, because tx2's conditional UPDATEs guarded it. Only the response code was wrong.

### Evidence
- `TestConcurrentConfirmChargesOnce` (C6): 20 confirms with the same key plus 20 with different keys, all concurrent with a slow provider, give 1 payment row and 1 charge.
- `TestConfirmRetryAfterTimeoutDoesNotDoubleCharge` (C6): the client gives up mid-payment, gets 503, retries, and gets 200 with 1 charge over 2 provider calls.
- `TestPaymentFailureReleasesHold` (C5), `TestConcurrentDeclinedConfirmReleasesOnce`, `TestConfirmAfterExpiryIsRejected` (C8).
- A mutation test (removing the `status='held'` guard on release) makes the release-once test fail.
- Load: all winners confirm with 5% declined, and the provider's total charges equal the confirmed reservations (`TestRunMessyClients`). Every invariant held at 10,000 buyers.

---

## ADR-007: Choosing the stock strategy (A vs B vs C)

**Status:** accepted. **Decision: B, the atomic conditional UPDATE.** D (optimistic version column) was cut, per the cut order in the project plan.

### The three candidates (all at READ COMMITTED, same transaction shape; ADR-003)
| | SQL on the hot row | Lock held on `drops` row | Correct? |
|---|---|---|---|
| A. naive | `SELECT remaining` → decide in Go → `UPDATE SET remaining = $abs` | UPDATE → COMMIT, but the read happened *before* the lock | **No:** lost updates |
| B. atomic | `UPDATE ... SET remaining = remaining - n WHERE id = $1 AND remaining >= n` | UPDATE → COMMIT (2 round trips) | Yes |
| C. pessimistic | `SELECT remaining ... FOR NO KEY UPDATE` → decide in Go → `UPDATE` | SELECT → UPDATE → COMMIT (3 round trips) | Yes |

### What the numbers showed (`docs/results.md`, runs of 16:14 and 16:15)
- **Correctness:** in-process, 1,000 goroutines vs 10 units: A oversold in every run; B and C granted exactly 10 every time. Over HTTP, 10,000 buyers vs 100 units, 3 runs each: A oversold by 1,142–1,499; B and C exactly 100, with 0 violations at every mid-run sample.
- **Burst latency:** B and C can't be told apart. Only 100 of 10,000 requests write, and the rest share the same sold-out pre-check.
- **Write-path ceiling, same session:** **B 750 req/s (2/2 runs), C 500 and 250 req/s.** Overloaded goodput: B about 540 req/s, C about 420–460 req/s.

### Why B
1. **Shortest lock hold on the hot row.** The check and the decrement are one statement, so the lock is taken as late as possible. On a single row every buyer has to pass, hold time sets throughput, and B measured higher.
2. **Simplest correct code.** One statement, with no business decision in Go between the read and the write. "Zero rows affected" means sold out.
3. **Fewer lock-mode subtleties.** C's first version used `FOR UPDATE` and **deadlocked under load** (999 of 1,000 requests failed with `40P01`, see below). B's plain UPDATE takes the right lock (`FOR NO KEY UPDATE`) implicitly.

### When C would be the better choice
When the decision needs more than one row or logic that can't be written as a `WHERE` clause. For example: check stock *and* a per-drop budget, read several rows and choose between them, or call business rules in Go with the row held. C locks first and lets Go decide safely. The price is a longer lock hold, so keep the work between SELECT and COMMIT minimal, with no network calls inside it.

### Bug found: `FOR UPDATE` deadlocks with the foreign-key check
The reserve transaction inserts the reservation *before* taking stock (ADR-003). The INSERT's foreign key (`reservations.drop_id → drops.id`) takes **`FOR KEY SHARE`** on the drop row, so the drop can't be deleted mid-insert. `SELECT ... FOR UPDATE` then requests a lock that **conflicts with other transactions' KEY SHARE**. Two buyers who each hold KEY SHARE and each wait for FOR UPDATE wait on each other, and Postgres aborts one with `deadlock detected`. The barrier test caught it at once: 999 of 1,000 requests failed.

**Fix:** `FOR NO KEY UPDATE`, the mode a plain UPDATE of a non-key column takes. It blocks other writers but not KEY SHARE. It's the lock you actually mean when you're about to change `remaining`. After the fix: 5/5 test runs pass under `-race`.

*Lesson:* `FOR UPDATE` is stronger than most code needs. Unless you're changing a key column or deleting the row, `FOR NO KEY UPDATE` is the right lock, and with foreign keys pointing at the row the stronger mode can deadlock.

### Not chosen: D (optimistic version column with retry)
Cut for time, as planned. Expectation, not measured: on a single hot row, almost every optimistic attempt conflicts, so retries multiply the load exactly when it's highest. Optimistic concurrency suits low-contention rows, which a flash sale's stock row is not.
