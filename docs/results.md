# Measured results

Every number in the README must appear here first. `make load` (`cmd/loadgen`) appends one section per invocation with the machine spec. Raw JSON for each invocation goes to `results/` (gitignored).

---

## 2026-11-07: In-process concurrency test, strategy A vs B (milestone 2)

**What ran:** `go test -race -count=10 -v -run 'TestConcurrent(Atomic|Naive)' ./internal/sale`

**Setup:**
- 1,000 goroutines released together by a start barrier (`close(start)`), each reserving 1 unit from a drop of 10.
- Calls go straight to `sale.Service.Reserve` (no HTTP). pgxpool `MaxConns=50`. Each run uses a fresh database.
- The final state is judged by `internal/invariants.Check`.

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, Go 1.26.5 (`-race` on), PostgreSQL 16.15 in Docker (Colima). Client, API, and DB are all on one machine.

| Run | A: holds granted | A: oversold units | A: final `remaining` | B: holds granted | B: sold-out | B: violations |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 708 | 698 | 0 | 10 | 990 | 0 |
| 2 | 697 | 687 | 1 | 10 | 990 | 0 |
| 3 | 581 | 571 | 0 | 10 | 990 | 0 |
| 4 | 597 | 587 | 0 | 10 | 990 | 0 |
| 5 | 728 | 718 | 0 | 10 | 990 | 0 |
| 6 | 688 | 678 | 2 | 10 | 990 | 0 |
| 7 | 541 | 531 | 0 | 10 | 990 | 0 |
| 8 | 521 | 511 | 0 | 10 | 990 | 0 |
| 9 | 610 | 600 | 0 | 10 | 990 | 0 |
| 10 | 648 | 638 | 0 | 10 | 990 | 0 |

**Summary:**
- **Strategy A oversold in 10/10 runs**, by 511–718 units (mean 621.9) on a 10-unit drop. Every run was flagged by `no_oversell` and `stock_conservation`.
- **Strategy B granted exactly 10 holds in 10/10 runs**, with 990 sold-out responses, 0 unexpected errors, and 0 invariant violations.
- In runs 2 and 6, A ended with `remaining > 0` while hundreds of holds existed. Lost updates are not just "the last write wins at 0": a stale write of 1 or 2 can land after others have written 0.

**Caveats:** These are correctness numbers, not performance numbers. Latency and throughput need the HTTP load generator (milestone 3). The race detector slows Go code roughly 2–10×, which widens A's read-to-write window, so A's oversell size is not comparable to production. That A oversells at all is the point.

---

## 2026-11-07 15:16: HTTP load test, 10000 buyers vs 100 units

**Command:** `loadgen -api-bin bin/api -strategies naive,atomic -buyers 10000 -stock 100 -pool 20 -repeat 3`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other / transport errors | p50 ms | p95 ms | p99 ms | max ms | Throughput req/s | Sell-out ms | Retries |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| naive | 20 | 1629 | 1529 | 2 | 4579 | 3792 | 0 | 4505.9 | 5221.4 | 5232.3 | 5245.9 | 1904 | 3146.5 | 0 |
| naive | 20 | 1570 | 1470 | 2 | 6830 | 1600 | 0 | 3781.8 | 5192.2 | 5203.7 | 5214.2 | 1917 | 2805.3 | 0 |
| naive | 20 | 2058 | 1958 | 2 | 4454 | 3488 | 0 | 4454.3 | 5186.4 | 5203.4 | 5235.8 | 1909 | 3497.2 | 0 |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 792.0 | 2002.1 | 2119.7 | 2246.1 | 4452 | 517.1 | 0 |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1284.6 | 2540.4 | 2679.0 | 2756.9 | 3568 | 764.1 | 0 |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 712.3 | 1261.6 | 1362.5 | 1382.5 | 6954 | 399.4 | 0 |

Violations for **naive** (drop 7):
- `no_oversell`: held 1629 + confirmed 0 = 1629 > total_stock 100 (oversold by 1529)
- `stock_conservation`: remaining 0 + held 1629 + confirmed 0 = 1629 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

Violations for **naive** (drop 8):
- `no_oversell`: held 1570 + confirmed 0 = 1570 > total_stock 100 (oversold by 1470)
- `stock_conservation`: remaining 0 + held 1570 + confirmed 0 = 1570 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

Violations for **naive** (drop 9):
- `no_oversell`: held 2058 + confirmed 0 = 2058 > total_stock 100 (oversold by 1958)
- `stock_conservation`: remaining 0 + held 2058 + confirmed 0 = 2058 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**Analysis (written by hand, from the table above):**
- **Correctness:** atomic (B) granted exactly 100 holds in 3/3 runs, with 0 violations, 0 503s, and 0 errors. Naive (A) oversold in 3/3 runs, by 1,470–1,958 units: 15–20× the stock.
- **Naive is also the slowest.** Its p95 of about 5.2 s equals the API's 5 s `REQUEST_TIMEOUT`, and 1,600–3,792 requests per run got 503. Lost updates keep `remaining` high for longer (sell-out took 2.8–3.5 s vs 0.4–0.76 s for atomic). So the lock-free pre-check keeps admitting buyers, and each one opens a transaction that queues on the hot row while holding one of the 20 pool connections. Everyone else then waits for a connection until the deadline. The 503s show the intended behavior under pool exhaustion: a timeout, not a hang.
- **Atomic latency** was p50 0.71–1.28 s and p95 1.26–2.54 s, at 3,568–6,954 req/s. Every request needs at least one query, and 10,000 requests share 20 connections to a Postgres in a 2-CPU VM. *Hypothesis, not yet measured:* most of this latency is waiting for a pool connection rather than waiting for the row lock. The pool-size comparison (milestone 5) will test it.
- **Variance:** atomic throughput varied about 2× across runs on the same laptop. Treat the numbers as relative (A vs B on the same machine and the same minute), not as capacity figures.

---

## L1 burst: 2026-11-07 15:26, commit a53bf6a-dirty: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies naive,atomic -buyers 10000 -stock 100 -pool 20 -repeat 3`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| naive | 20 | 1647 | 1547 | 2 | 2493 | 5860 | 0 | 5188.5 | 5285.8 | 5291.7 | 5293.3 | 3605.7 | 1889 | 3904.4 | ok |
| naive | 20 | 2116 | 2016 | 2 | 4817 | 3067 | 0 | 4418.9 | 5202.4 | 5224.0 | 5229.1 | 3250.5 | 1912 | 3457.9 | ok |
| naive | 20 | 1544 | 1444 | 2 | 2876 | 5580 | 0 | 5130.5 | 5201.5 | 5204.1 | 5204.8 | 2623.7 | 1921 | 2749.7 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 915.0 | 1574.5 | 1681.5 | 1707.1 | 519.9 | 5858 | 528.3 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 739.2 | 1195.9 | 1305.0 | 1335.3 | 369.4 | 7489 | 378.5 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1145.3 | 2130.7 | 2232.5 | 2261.4 | 604.9 | 4422 | 610.1 | ok |

Per run (test-plan template):

**naive, run 1** (pool=20, drop 13)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 5.29 s
- Latency ms: p50 5188.5 · p95 5285.8 · p99 5291.7 · max 5293.3 · 201 only: p50 2513.6 / p95 3605.7 / p99 3822.9 · 409 only: p50 4353.4 / p95 5115.3 / p99 5155.6 · Peak in-flight: 10000
- Outcome: sold 1647 · released n/a · resold n/a · 409 2493 · 503 5860 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1547 mid-run), 103 mid-run samples, peak active 1647 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1647 + confirmed 0 = 1647 > total_stock 100 (oversold by 1547)
  - `stock_conservation`: remaining 0 + held 1647 + confirmed 0 = 1647 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 2** (pool=20, drop 14)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 5.23 s
- Latency ms: p50 4418.9 · p95 5202.4 · p99 5224.0 · max 5229.1 · 201 only: p50 1987.8 / p95 3250.5 / p99 3401.1 · 409 only: p50 4247.1 / p95 5042.5 / p99 5134.0 · Peak in-flight: 10000
- Outcome: sold 2116 · released n/a · resold n/a · 409 4817 · 503 3067 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 2016 mid-run), 104 mid-run samples, peak active 2116 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 2116 + confirmed 0 = 2116 > total_stock 100 (oversold by 2016)
  - `stock_conservation`: remaining 0 + held 2116 + confirmed 0 = 2116 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 3** (pool=20, drop 15)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 5.20 s
- Latency ms: p50 5130.5 · p95 5201.5 · p99 5204.1 · max 5204.8 · 201 only: p50 1669.3 / p95 2623.7 / p99 2719.5 · 409 only: p50 3883.3 / p95 4993.0 / p99 5088.9 · Peak in-flight: 10000
- Outcome: sold 1544 · released n/a · resold n/a · 409 2876 · 503 5580 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1444 mid-run), 103 mid-run samples, peak active 1544 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1544 + confirmed 0 = 1544 > total_stock 100 (oversold by 1444)
  - `stock_conservation`: remaining 0 + held 1544 + confirmed 0 = 1544 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**atomic, run 1** (pool=20, drop 16)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 1.71 s
- Latency ms: p50 915.0 · p95 1574.5 · p99 1681.5 · max 1707.1 · 201 only: p50 428.4 / p95 519.9 / p99 527.1 · 409 only: p50 918.1 / p95 1575.6 / p99 1682.1 · Peak in-flight: 10000
- Outcome: sold 100 · released n/a · resold n/a · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 32 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 17)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 1.34 s
- Latency ms: p50 739.2 · p95 1195.9 · p99 1305.0 · max 1335.3 · 201 only: p50 294.7 / p95 369.4 / p99 376.3 · 409 only: p50 743.1 / p95 1196.7 / p99 1305.9 · Peak in-flight: 10000
- Outcome: sold 100 · released n/a · resold n/a · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 25 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 18)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (dup 0, retries 0) · Duration: 2.26 s
- Latency ms: p50 1145.3 · p95 2130.7 · p99 2232.5 · max 2261.4 · 201 only: p50 530.2 / p95 604.9 / p99 608.9 · 409 only: p50 1148.3 / p95 2132.4 / p99 2232.6 · Peak in-flight: 10000
- Outcome: sold 100 · released n/a · resold n/a · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 46 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**Analysis (written by hand):**
- **H1 held in all 6 runs:** no 5xx other than 503, no hung requests, no transport errors. Naive's 3,067–5,860 503s per run are the allowed overload answer.
- **I1 checked mid-run, not just at the end:** about 100 DB samples per run, every 50 ms. Naive was already oversold while the burst was in flight (peak active 1,544–2,116 vs 100 stock). Atomic never exceeded 100 at any sample.
- **Winners are faster than losers.** Atomic 201s: p95 369–605 ms. Atomic 409s: p95 1,196–2,131 ms. Latency here comes from queue position for one of 20 pool connections, not from request cost. Winners are by definition the first served, and the 9,900 sold-out requests wait behind them.
- Compared with the 15:16 run (same code path, before H1 and sampling were added): same picture, and atomic throughput again varied about 1.7× between runs (4,422–7,489 req/s).

---

## L5 capacity ceiling: 2026-11-07 15:27, commit a53bf6a-dirty

**Command:** `loadgen -scenario ceiling -api-bin bin/api -strategies atomic -rates 500,1000,2000,4000,8000 -step 20s -pool 20`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

**Strategy atomic, pool 20, drop 19 (stock 311005, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 5.1 | 124.1 | 394.4 | 589.6 | 237 | ok | pass |
| 1000 | 1000 | 554 | 20000 | 19995 | 5 | 0 | 0.03% | 5815.5 | 15398.3 | 16018.1 | 16153.7 | 7677 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok


**Analysis (written by hand):**
- **Ceiling: 500 req/s** for the reservation *write* path: every request takes a unit, because stock never runs out. At 1,000 req/s the server completed only about 554 req/s, so the queue grew for the whole step and p99 reached 16 s. H1 and invariants held even while overloaded.
- **Why the overloaded step shows few 503s but huge latency:** the client caps connections at 2,000, so most of the backlog waited *in the client*. Latency is measured from each request's scheduled send time, not the moment it got a connection, so that wait is counted (no coordinated omission). With one connection per user, the same backlog would have reached the server and come back as 503s.
- **The row itself is not the 550/s limit.** A bare single-row conditional UPDATE inside the same Postgres VM (pgbench, below) sustained 4,926 tx/s with 20 clients. Our reserve path costs about 5 DB round trips (pre-check, BEGIN, INSERT, UPDATE, COMMIT) from the host into the VM, and the row lock is held from the UPDATE until COMMIT returns. *Hypothesis, not yet measured:* the lock hold time across round trips is the limiter, and a single-statement reserve (INSERT + UPDATE in one CTE, one round trip) would raise the ceiling.
- **Reading this against the test plan (section 6):** the modeled peak of about 5,000 req/s (L2) is mostly *sold-out* traffic after the first 100 units, which only needs the lock-free pre-check. L1 measured that path at 4,422–7,489 req/s, the same order as the modeled peak. L5 as specified measures the write path, which a 100-unit drop barely uses (100 writes plus resales). The verdict for the main scenario should therefore come from L2 once it exists (milestone 5), not from L5 alone.

### Supporting measurement: single-row UPDATE ceiling inside the DB VM (2026-11-07 15:29)

Shows what Postgres alone can do on one hot row, with no API and no host-to-VM networking.

```
docker exec flashsale-postgres pgbench -n -U flashsale -d flashsale -f hot.sql -c <clients> -j 2 -T 8
hot.sql: UPDATE bench_hot SET remaining = remaining - 1 WHERE id = 1 AND remaining >= 1;
```

| Clients | tps | Avg latency |
|---:|---:|---:|
| 1 | 6,195 | 0.161 ms |
| 20 | 4,926 | 4.060 ms |

Same machine and VM as above (Postgres 16.15, 2 CPU / 1.9 GB). Adding clients *lowers* throughput: all 20 queue on one row lock, so concurrency adds contention, not capacity.

---

## L1 burst: 2026-11-07 16:00, commit 451a8dd-dirty: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies naive,atomic -buyers 10000 -stock 100 -pool 20 -repeat 3`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| naive | 20 | 1379 | 1279 | 2 | 2522 | 6099 | 0 | 5149.6 | 5250.4 | 5255.5 | 5256.2 | 3123.8 | 1903 | 3359.3 | ok |
| naive | 20 | 1607 | 1507 | 2 | 2064 | 6329 | 0 | 5184.7 | 5327.1 | 5337.9 | 5338.4 | 3579.6 | 1873 | 3741.2 | ok |
| naive | 20 | 1353 | 1253 | 2 | 2398 | 6249 | 0 | 5134.5 | 5234.9 | 5238.7 | 5239.5 | 2976.5 | 1909 | 3116.8 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 999.4 | 2033.2 | 2190.1 | 2226.8 | 450.7 | 4491 | 458.1 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1037.1 | 1755.9 | 1920.6 | 1959.0 | 419.6 | 5105 | 424.1 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1483.8 | 2780.2 | 2971.4 | 3025.9 | 691.6 | 3305 | 700.8 | ok |

Per run (test-plan template):

**naive, run 1** (pool=20, drop 20)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.26 s
- Latency ms: p50 5149.6 · p95 5250.4 · p99 5255.5 · max 5256.2 · 201 only: p50 2049.4 / p95 3123.8 / p99 3293.8 · 409 only: p50 4116.6 / p95 5031.7 / p99 5117.3 · Peak in-flight: 10000
- Outcome (first attempts): held 1379 · 409 2522 · 503 6099 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1279 mid-run), 104 mid-run samples, peak active 1379 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1379 + confirmed 0 = 1379 > total_stock 100 (oversold by 1279)
  - `stock_conservation`: remaining 0 + held 1379 + confirmed 0 = 1379 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 2** (pool=20, drop 21)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.34 s
- Latency ms: p50 5184.7 · p95 5327.1 · p99 5337.9 · max 5338.4 · 201 only: p50 2421.7 / p95 3579.6 / p99 3700.2 · 409 only: p50 4675.5 / p95 5092.4 / p99 5129.0 · Peak in-flight: 10000
- Outcome (first attempts): held 1607 · 409 2064 · 503 6329 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1507 mid-run), 103 mid-run samples, peak active 1607 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1607 + confirmed 0 = 1607 > total_stock 100 (oversold by 1507)
  - `stock_conservation`: remaining 0 + held 1607 + confirmed 0 = 1607 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 3** (pool=20, drop 22)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.24 s
- Latency ms: p50 5134.5 · p95 5234.9 · p99 5238.7 · max 5239.5 · 201 only: p50 2030.3 / p95 2976.5 / p99 3068.4 · 409 only: p50 4133.7 / p95 5028.2 / p99 5107.2 · Peak in-flight: 10000
- Outcome (first attempts): held 1353 · 409 2398 · 503 6249 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1253 mid-run), 103 mid-run samples, peak active 1353 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1353 + confirmed 0 = 1353 > total_stock 100 (oversold by 1253)
  - `stock_conservation`: remaining 0 + held 1353 + confirmed 0 = 1353 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**atomic, run 1** (pool=20, drop 23)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 2.23 s
- Latency ms: p50 999.4 · p95 2033.2 · p99 2190.1 · max 2226.8 · 201 only: p50 395.3 / p95 450.7 / p99 456.9 · 409 only: p50 1002.6 / p95 2035.7 / p99 2191.1 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 43 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 24)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 1.96 s
- Latency ms: p50 1037.1 · p95 1755.9 · p99 1920.6 · max 1959.0 · 201 only: p50 365.2 / p95 419.6 / p99 423.2 · 409 only: p50 1041.1 / p95 1756.9 / p99 1920.8 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 38 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 25)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 3.03 s
- Latency ms: p50 1483.8 · p95 2780.2 · p99 2971.4 · max 3025.9 · 201 only: p50 618.1 / p95 691.6 / p99 698.8 · 409 only: p50 1488.8 / p95 2783.7 / p99 2972.3 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 60 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**Analysis (written by hand):** milestone 4 code (idempotency keys and confirm added), same L1 burst as before, as a regression check.
- **Correctness unchanged:** atomic granted exactly 100 holds in 3/3 runs, with 0 violations and H1 ok. Naive oversold by 1,253–1,507.
- **Latency within run-to-run noise of the 15:26 run:** atomic p95 1.76–2.78 s here vs 1.20–2.13 s before, and winners (201) p95 0.42–0.69 s vs 0.37–0.60 s. The sold-out path still costs one query: the pre-check now also LEFT JOINs the caller's idempotency key in the same statement. Atomic throughput again varied about 1.5× across runs (3,305–5,105 req/s).

---

## L1 burst: 2026-11-07 16:01, commit 451a8dd-dirty: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies atomic -buyers 10000 -stock 100 -pool 20 -repeat 3 -dup-ratio 0.1 -retry-ratio 0.3 -confirm -payment-failure-rate 0.05`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1364.4 | 3379.4 | 3566.1 | 3625.0 | 735.8 | 2759 | 3062.8 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1164.9 | 1962.5 | 2166.2 | 2217.1 | 494.8 | 4510 | 1714.4 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1462.7 | 3587.2 | 3786.2 | 3836.7 | 890.2 | 2606 | 3172.2 | ok |

Per run (test-plan template):

**atomic, run 1** (pool=20, drop 26)
- Buyers: 10000 · Stock: 100 · Requests: 14160 (first attempts 10000, dup 983, retries 3077, confirms 125) · Duration: 3.63 s
- Latency ms: p50 1364.4 · p95 3379.4 · p99 3566.1 · max 3625.0 · 201 only: p50 588.5 / p95 735.8 / p99 876.8 · 409 only: p50 1369.0 / p95 3381.7 / p99 3567.0 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 46 · 409 3989 · 503 0 · other errors 0
- Confirm: 125 requests · confirmed 200 120 · declined 402 5 (hold released) · 503 0 · other 0 · latency ms p50 3394.4 / p95 3622.3 / p99 3687.1
- Invariants: I1 ok, 83 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 27)
- Buyers: 10000 · Stock: 100 · Requests: 14160 (first attempts 10000, dup 983, retries 3077, confirms 125) · Duration: 2.22 s
- Latency ms: p50 1164.9 · p95 1962.5 · p99 2166.2 · max 2217.1 · 201 only: p50 373.3 / p95 494.8 / p99 1578.6 · 409 only: p50 1168.0 / p95 1964.7 / p99 2166.5 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 45 · 409 3990 · 503 0 · other errors 0
- Confirm: 125 requests · confirmed 200 121 · declined 402 4 (hold released) · 503 0 · other 0 · latency ms p50 2291.9 / p95 2479.7 / p99 2521.6
- Invariants: I1 ok, 56 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 28)
- Buyers: 10000 · Stock: 100 · Requests: 14162 (first attempts 10000, dup 983, retries 3079, confirms 127) · Duration: 3.84 s
- Latency ms: p50 1462.7 · p95 3587.2 · p99 3786.2 · max 3836.7 · 201 only: p50 758.8 / p95 890.2 / p99 897.7 · 409 only: p50 1467.4 / p95 3588.0 / p99 3786.4 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 35 · 409 4000 · 503 0 · other errors 0
- Confirm: 127 requests · confirmed 200 117 · declined 402 10 (hold released) · 503 0 · other 0 · latency ms p50 3491.8 / p95 3719.8 / p99 3851.6
- Invariants: I1 ok, 86 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**Analysis (written by hand):** messy clients on top of the L1 burst (test plan 2.4 / L6-style). 10% of buyers double-click (same key, own connection, within 50 ms), 30% resend with the same key after their answer, and every winner confirms (pays) with 5% of payments declined. 30% of winners also retry the confirm.
- **About 14,160 requests per run, 0 key violations:** no Idempotency-Key ever produced two different successful responses. Exactly 100 holds in 3/3 runs, and every invariant held at every mid-run sample and at the end. H1 held across reserves, duplicates, retries and confirms.
- **Declined payments released their stock:** 4–10 declines per run (402s, including replays of retried confirms). The DB check confirms conservation after the releases.
- **Finding: winners wait behind losers.** Confirm latency p50 was 2.3–3.5 s although the mock payment takes 100–300 ms. Confirms share the 20-connection pool with about 14,000 sold-out and retried reservation requests, so a buyer who *won* waits in the same queue as everyone who lost. Possible fixes, not implemented: answer sold-out without a DB connection (cached flag), or give confirm its own small pool so paying customers are never starved.

---

## L5 capacity ceiling: 2026-11-07 16:01, commit 451a8dd-dirty

**Command:** `loadgen -scenario ceiling -api-bin bin/api -strategies atomic -rates 250,500,750,1000 -step 20s -pool 20`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

**Strategy atomic, pool 20, drop 29 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.2 | 8.7 | 32.9 | 130.9 | 26 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 5.9 | 9.7 | 40.3 | 186.8 | 35 | ok | pass |
| 750 | 750 | 562 | 15000 | 14999 | 1 | 0 | 0.01% | 2459.2 | 6370.8 | 6667.4 | 6743.3 | 3134 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok


**Analysis (written by hand):** milestone 4 code, finer steps (250 / 500 / 750 / 1,000).
- **Ceiling still 500 req/s.** At 750 req/s the server completed 562 req/s, the same saturation point as before (554 req/s at the 15:27 run), although each successful reserve now makes about 7 DB round trips instead of 5.
- This **fits a sharper version of the earlier hypothesis:** the two added round trips (claim key, store response) happen *before* the stock row is locked, and the lock is still held only from the stock UPDATE to COMMIT. So the limit tracks round trips made *while holding the row lock*, not total round trips. Consistent with the data, not proven; a one-statement reserve would be the direct test.
- At 500 req/s, p99 was 40 ms here vs 394 ms in the 15:27 run, under similar load. Latency at a passing step is sensitive to laptop noise, so the ceiling (pass/fail) is the stable number, not the p99 below it.
