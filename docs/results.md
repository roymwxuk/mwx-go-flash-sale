# Measured results

Every number in the README must appear here first. `make load` (`cmd/loadgen`) appends one section per invocation with the machine spec. Raw JSON for each invocation goes to `results/` (gitignored).

## Summary: where each README number comes from

All runs were on one machine: Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, Go 1.26.5, PostgreSQL 16.15 in a Docker VM (2 CPU / 1.9 GB). Ranges are min–max across runs. The headline rows were re-run on a clean commit (`eeeb8cc`). Older sections recorded as `-dirty` stay as history.

| Claim | Number | Section below |
|---|---|---|
| In-process: A oversells, B does not (1,000 goroutines vs 10 units) | A oversold 10/10 runs (511–718 units); B exactly 10 holds 10/10 | 2026-11-07, milestone 2 |
| HTTP burst, 10,000 buyers vs 100 units, pool 20 (headline table) | A oversold 1,373–1,683; B and C exactly 100, 0 violations | L1 burst 18:43, commit `eeeb8cc` (clean) |
| B vs C write-path ceiling, same session | B 500 req/s (2/2 runs), C 500 and 250; past the ceiling B completed 551–566 req/s with 0 errors, C 402 req/s with 71.5% 503s | L5 18:45, commit `eeeb8cc` (clean) |
| B vs C, earlier session | B 750 req/s (2/2), C 500 and 250 | L5 16:15 (`afb3aea-dirty`) |
| B write-path ceiling, earlier sessions | 500 req/s (before and after milestone 4) | L5 15:27, L5 16:01 |
| Postgres alone, one hot row (pgbench) | 6,195 tps (1 client), 4,926 tps (20 clients) | supporting measurement 15:29 |
| Burst throughput, B (almost all sold-out answers) | 4,422–7,489 req/s | L1 burst 15:26 |
| Messy clients (10% double-click, 30% retry, 5% declined) | 14,164–14,169 req/run, 0 key violations, exactly 100 holds | L1 burst 18:44, commit `eeeb8cc` (clean) |
| Sweepers: 3 sweepers, 200 expired holds | exactly 200 released, 10/10 runs | Expiry sweeper 16:30 |
| Confirm racing the sweeper, 150 confirms | 0 charged-but-expired, 10/10 runs | Expiry sweeper 16:30 |

**Not measured (cut for time):** pool-size comparison (ADR-009), strategy D (ADR-007), SERIALIZABLE (ADR-003). CI (`.github/workflows/ci.yml`) runs a 1,000-buyer smoke test for correctness only; its numbers aren't recorded here, because shared runners are too noisy to compare.

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
- **Atomic latency** was p50 0.71–1.28 s and p95 1.26–2.54 s, at 3,568–6,954 req/s. Every request needs at least one query, and 10,000 requests share 20 connections to a Postgres in a 2-CPU VM. *Hypothesis, not measured:* most of this latency is waiting for a pool connection rather than waiting for the row lock. The pool-size comparison that would test it was cut for time (ADR-009).
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

---

## L1 burst: 2026-11-07 16:14, commit afb3aea-dirty: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies naive,atomic,pessimistic -buyers 10000 -stock 100 -pool 20 -repeat 3`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| naive | 20 | 1252 | 1152 | 2 | 4387 | 4361 | 0 | 4770.3 | 5248.6 | 5253.6 | 5254.9 | 2306.0 | 1903 | 2458.3 | ok |
| naive | 20 | 1242 | 1142 | 2 | 2614 | 6144 | 0 | 5097.4 | 5158.1 | 5161.1 | 5161.9 | 2426.2 | 1937 | 2549.9 | ok |
| naive | 20 | 1599 | 1499 | 2 | 2144 | 6257 | 0 | 5093.0 | 5156.0 | 5162.1 | 5163.0 | 2975.9 | 1937 | 3197.2 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 2547.4 | 4391.2 | 4566.8 | 4605.4 | 881.2 | 2171 | 892.3 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1057.1 | 1920.2 | 2107.5 | 2149.6 | 420.5 | 4652 | 429.4 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 954.5 | 1794.9 | 1950.1 | 1986.5 | 420.5 | 5034 | 430.4 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1077.5 | 3179.4 | 3498.4 | 3569.5 | 530.5 | 2802 | 551.1 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1024.3 | 1586.7 | 1876.4 | 1962.0 | 382.0 | 5097 | 390.6 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1190.5 | 3586.0 | 3917.9 | 3988.5 | 538.7 | 2507 | 546.3 | ok |

Per run (test-plan template):

**naive, run 1** (pool=20, drop 30)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.25 s
- Latency ms: p50 4770.3 · p95 5248.6 · p99 5253.6 · max 5254.9 · 201 only: p50 1556.0 / p95 2306.0 / p99 2421.0 · 409 only: p50 3824.8 / p95 5038.0 / p99 5137.8 · Peak in-flight: 10000
- Outcome (first attempts): held 1252 · 409 4387 · 503 4361 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1152 mid-run), 103 mid-run samples, peak active 1252 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1252 + confirmed 0 = 1252 > total_stock 100 (oversold by 1152)
  - `stock_conservation`: remaining 0 + held 1252 + confirmed 0 = 1252 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 2** (pool=20, drop 31)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.16 s
- Latency ms: p50 5097.4 · p95 5158.1 · p99 5161.1 · max 5161.9 · 201 only: p50 1637.7 / p95 2426.2 / p99 2514.3 · 409 only: p50 3773.2 / p95 4947.8 / p99 5058.5 · Peak in-flight: 10000
- Outcome (first attempts): held 1242 · 409 2614 · 503 6144 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1142 mid-run), 104 mid-run samples, peak active 1242 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1242 + confirmed 0 = 1242 > total_stock 100 (oversold by 1142)
  - `stock_conservation`: remaining 0 + held 1242 + confirmed 0 = 1242 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 3** (pool=20, drop 32)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.16 s
- Latency ms: p50 5093.0 · p95 5156.0 · p99 5162.1 · max 5163.0 · 201 only: p50 1893.0 / p95 2975.9 / p99 3124.7 · 409 only: p50 4090.0 / p95 4957.3 / p99 5035.4 · Peak in-flight: 10000
- Outcome (first attempts): held 1599 · 409 2144 · 503 6257 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1499 mid-run), 103 mid-run samples, peak active 1599 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1599 + confirmed 0 = 1599 > total_stock 100 (oversold by 1499)
  - `stock_conservation`: remaining 0 + held 1599 + confirmed 0 = 1599 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**atomic, run 1** (pool=20, drop 33)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 4.61 s
- Latency ms: p50 2547.4 · p95 4391.2 · p99 4566.8 · max 4605.4 · 201 only: p50 723.9 / p95 881.2 / p99 889.4 · 409 only: p50 2571.3 / p95 4393.2 / p99 4567.3 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 92 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 34)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 2.15 s
- Latency ms: p50 1057.1 · p95 1920.2 · p99 2107.5 · max 2149.6 · 201 only: p50 367.2 / p95 420.5 / p99 427.5 · 409 only: p50 1062.7 / p95 1921.9 / p99 2107.5 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 43 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 35)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 1.99 s
- Latency ms: p50 954.5 · p95 1794.9 · p99 1950.1 · max 1986.5 · 201 only: p50 326.1 / p95 420.5 / p99 426.8 · 409 only: p50 959.0 / p95 1797.5 / p99 1950.5 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 39 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**pessimistic, run 1** (pool=20, drop 36)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 3.57 s
- Latency ms: p50 1077.5 · p95 3179.4 · p99 3498.4 · max 3569.5 · 201 only: p50 425.4 / p95 530.5 / p99 548.8 · 409 only: p50 1080.9 / p95 3184.3 / p99 3499.3 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 71 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**pessimistic, run 2** (pool=20, drop 37)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 1.96 s
- Latency ms: p50 1024.3 · p95 1586.7 · p99 1876.4 · max 1962.0 · 201 only: p50 301.0 / p95 382.0 / p99 388.8 · 409 only: p50 1028.6 / p95 1587.4 / p99 1877.4 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 40 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**pessimistic, run 3** (pool=20, drop 38)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 3.99 s
- Latency ms: p50 1190.5 · p95 3586.0 · p99 3917.9 · max 3988.5 · 201 only: p50 429.7 / p95 538.7 / p99 544.7 · 409 only: p50 1194.7 / p95 3589.6 / p99 3919.0 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 79 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a (no expiry until M5) · I5 ok · H1 ok

**Analysis (written by hand):** first run with strategy C (pessimistic, `SELECT ... FOR NO KEY UPDATE` then UPDATE).
- **Correctness:** pessimistic granted exactly 100 holds in 3/3 runs, with 0 violations, I1 ok at every mid-run sample, and H1 ok. The same as atomic. Naive oversold by 1,142–1,499.
- **This burst cannot separate B from C on speed.** Only 100 of 10,000 requests per run write; the other 9,900 take the sold-out pre-check, which is identical for both. Atomic p95 1.79–4.39 s, pessimistic 1.59–3.59 s; winners' p95 0.42–0.88 s vs 0.38–0.54 s. The ranges overlap, and atomic's run 1 was a slow outlier for both kinds of request. The write-path ceiling below is the comparison that matters.

---

## L5 capacity ceiling: 2026-11-07 16:15, commit afb3aea-dirty

**Command:** `loadgen -scenario ceiling -api-bin bin/api -strategies atomic,pessimistic -rates 250,500,750,1000 -step 20s -pool 20 -repeat 2`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

**Strategy atomic, pool 20, drop 39 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 4.7 | 6.2 | 7.8 | 72.6 | 15 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 5.5 | 7.0 | 8.7 | 32.1 | 15 | ok | pass |
| 750 | 750 | 750 | 15000 | 15000 | 0 | 0 | 0.00% | 6.7 | 9.4 | 23.0 | 90.4 | 38 | ok | pass |
| 1000 | 1000 | 544 | 20000 | 20000 | 0 | 0 | 0.00% | 5929.8 | 15970.6 | 16611.4 | 16807.7 | 7877 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **750 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy atomic, pool 20, drop 40 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 4.8 | 6.1 | 7.1 | 58.9 | 12 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 5.7 | 7.3 | 10.8 | 94.4 | 28 | ok | pass |
| 750 | 750 | 750 | 15000 | 15000 | 0 | 0 | 0.00% | 7.1 | 14.7 | 51.3 | 136.2 | 51 | ok | pass |
| 1000 | 1000 | 536 | 20000 | 20000 | 0 | 0 | 0.00% | 8349.2 | 16471.5 | 17130.9 | 17332.7 | 9156 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **750 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy pessimistic, pool 20, drop 41 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.3 | 7.6 | 12.9 | 95.1 | 21 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 6.8 | 9.5 | 19.0 | 90.1 | 29 | ok | pass |
| 750 | 750 | 421 | 15000 | 5956 | 9044 | 0 | 60.29% | 6932.7 | 14893.4 | 15473.0 | 15631.9 | 6271 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy pessimistic, pool 20, drop 42 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.2 | 7.3 | 12.4 | 79.4 | 15 | ok | pass |
| 500 | 500 | 460 | 10000 | 10000 | 0 | 0 | 0.00% | 8.7 | 1590.8 | 1741.6 | 1980.8 | 715 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **250 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok


**Analysis (written by hand):** write-path ceiling, atomic (B) vs pessimistic (C), 2 runs each, back to back in one session.
- **Atomic: 750 req/s in both runs. Pessimistic: 500, then 250 req/s.** When overloaded, pessimistic completed 421–460 req/s (with 60% 503s at 750 req/s in run 1). Atomic completed 536–544 req/s at 1,000 req/s.
- **Why:** both take the same row lock on `drops` and hold it until COMMIT. Atomic takes it in its UPDATE, so the lock is held across UPDATE → COMMIT (2 round trips). Pessimistic takes it in the SELECT, one round trip earlier, so the lock is held across SELECT → UPDATE → COMMIT (3). On a single hot row, lock hold time limits throughput, and the measured gap points the same way. This also supports the round-trips-under-lock hypothesis from the earlier L5 runs.
- **Across sessions:** atomic passed 750 req/s here but failed it in the 16:01 session (500 ceiling). The laptop varies between sessions, so B vs C is compared only within this session.
- **Overload reduces goodput:** atomic completed about 750 req/s at the 750 step but only about 540 req/s at the 1,000 step. Past the ceiling, contention makes the system do *less* useful work, not just slower work. Admission control or a waiting room exists to prevent exactly this.

---

## 2026-11-07 16:30: Expiry sweeper correctness (milestone 5, tests repeated)

**Command:** `go test -race -count=10 -v -run 'TestConcurrentSweepersReleaseEachHoldOnce|TestConfirmRacingSweeper' ./internal/sweeper`. Same machine as above. Each run uses a fresh database.

**C7: 200 expired + 50 live holds, 3 sweepers started together, batch size 7:** passed **10/10**. Holds released per sweeper, per run (always summing to exactly 200):
`[42 42 116] [112 70 18] [35 102 63] [63 109 28] [14 74 112] [98 67 35] [49 56 95] [63 95 42] [49 32 119] [21 112 67]`.
Stock was restored exactly, the 50 live holds were untouched, and a second pass released 0.

**Confirm racing the sweeper:** 150 holds (1.5 s TTL), each confirmed at a random moment within ±60 ms of its own expiry, with a 20–80 ms provider and 3 sweepers polling every 2 ms. Passed **10/10**. Outcomes per run:

| Run | Sent after expiry → 410 | Sent before expiry → 200 (paid) | Sent before expiry → 410 (expired while waiting) | Charged but expired |
|---:|---:|---:|---:|---:|
| 1 | 86 | 46 | 18 | 0 |
| 2 | 86 | 61 | 3 | 0 |
| 3 | 87 | 16 | 47 | 0 |
| 4 | 86 | 59 | 5 | 0 |
| 5 | 87 | 45 | 18 | 0 |
| 6 | 92 | 34 | 24 | 0 |
| 7 | 90 | 25 | 35 | 0 |
| 8 | 87 | 50 | 13 | 0 |
| 9 | 87 | 59 | 4 | 0 |
| 10 | 87 | 58 | 5 | 0 |

The third column is the race itself: confirms sent before expiry that reached the database after it. Every one was rejected without a charge.

**Mutation checks** (code changed temporarily, then restored):

| Mutation | Result |
|---|---|
| Confirm does not extend `expires_at` when payment starts | **Caught:** "reservation 60 has a successful payment but is not confirmed" (charged, not confirmed) |
| Sweeper without `FOR UPDATE SKIP LOCKED` and without the `status='held'` guard | **Caught:** double refunds attempted, rejected by `CHECK (remaining <= total_stock)` |
| Only the `status='held'` guard removed | Passes: the row lock plus re-check suffice; the guard is defence in depth |

**Live run (API binary, dev database):** `HOLD_TTL=1s SWEEP_INTERVAL=200ms`, one hold taken over HTTP, back in stock within 1.6 s. The first live run exposed a stall: the dev database held drops oversold by the naive strategy in earlier load runs, and the CHECK correctly refused to refund them. After the per-drop fix, startup sweeps released 111,504 and then 86,022 legitimately expired holds left from earlier load runs, while 14 unrepairable naive drops were each logged once and skipped (ADR-008).

---

## L1 burst: 2026-11-07 18:43, commit eeeb8cc: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies naive,atomic,pessimistic -buyers 10000 -stock 100 -pool 20 -repeat 3`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| naive | 20 | 1678 | 1578 | 2 | 2005 | 6317 | 0 | 5132.5 | 5257.1 | 5265.7 | 5267.5 | 3362.6 | 1898 | 3581.2 | ok |
| naive | 20 | 1473 | 1373 | 2 | 3567 | 4960 | 0 | 5121.6 | 5189.4 | 5190.6 | 5190.9 | 2612.4 | 1926 | 2792.5 | ok |
| naive | 20 | 1783 | 1683 | 2 | 2315 | 5902 | 0 | 5137.3 | 5194.8 | 5202.6 | 5203.0 | 3377.3 | 1922 | 3651.1 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 981.6 | 1760.5 | 1912.2 | 1952.4 | 456.2 | 5122 | 461.4 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1180.7 | 2973.8 | 3163.3 | 3204.9 | 589.3 | 3120 | 595.3 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 864.5 | 1379.3 | 1545.5 | 1585.8 | 409.8 | 6306 | 418.0 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1360.1 | 3991.5 | 4331.4 | 4413.5 | 653.8 | 2266 | 661.0 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1236.5 | 3359.2 | 3664.5 | 3740.7 | 496.7 | 2673 | 513.5 | ok |
| pessimistic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1116.7 | 2409.6 | 2715.5 | 2794.5 | 442.8 | 3578 | 453.7 | ok |

Per run (test-plan template):

**naive, run 1** (pool=20, drop 46)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.27 s
- Latency ms: p50 5132.5 · p95 5257.1 · p99 5265.7 · max 5267.5 · 201 only: p50 2124.3 / p95 3362.6 / p99 3505.9 · 409 only: p50 4258.2 / p95 5024.3 / p99 5092.5 · Peak in-flight: 10000
- Outcome (first attempts): held 1678 · 409 2005 · 503 6317 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1578 mid-run), 105 mid-run samples, peak active 1678 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1678 + confirmed 0 = 1678 > total_stock 100 (oversold by 1578)
  - `stock_conservation`: remaining 0 + held 1678 + confirmed 0 = 1678 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 2** (pool=20, drop 47)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.19 s
- Latency ms: p50 5121.6 · p95 5189.4 · p99 5190.6 · max 5190.9 · 201 only: p50 1637.7 / p95 2612.4 / p99 2727.2 · 409 only: p50 3840.4 / p95 5029.9 / p99 5125.9 · Peak in-flight: 10000
- Outcome (first attempts): held 1473 · 409 3567 · 503 4960 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1373 mid-run), 103 mid-run samples, peak active 1473 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1473 + confirmed 0 = 1473 > total_stock 100 (oversold by 1373)
  - `stock_conservation`: remaining 0 + held 1473 + confirmed 0 = 1473 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**naive, run 3** (pool=20, drop 48)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 5.20 s
- Latency ms: p50 5137.3 · p95 5194.8 · p99 5202.6 · max 5203.0 · 201 only: p50 2081.1 / p95 3377.3 / p99 3562.6 · 409 only: p50 4261.9 / p95 5010.2 / p99 5087.5 · Peak in-flight: 10000
- Outcome (first attempts): held 1783 · 409 2315 · 503 5902 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 FAIL (oversold by 1683 mid-run), 104 mid-run samples, peak active 1783 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 FAIL (1) · H1 ok
  - `no_oversell`: held 1783 + confirmed 0 = 1783 > total_stock 100 (oversold by 1683)
  - `stock_conservation`: remaining 0 + held 1783 + confirmed 0 = 1783 != total_stock 100; surplus: holds without a matching decrement (lost update) or stock released twice

**atomic, run 1** (pool=20, drop 49)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 1.95 s
- Latency ms: p50 981.6 · p95 1760.5 · p99 1912.2 · max 1952.4 · 201 only: p50 379.1 / p95 456.2 / p99 460.1 · 409 only: p50 985.4 / p95 1761.4 / p99 1912.2 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 38 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 50)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 3.20 s
- Latency ms: p50 1180.7 · p95 2973.8 · p99 3163.3 · max 3204.9 · 201 only: p50 493.1 / p95 589.3 / p99 593.5 · 409 only: p50 1185.6 / p95 2975.1 / p99 3163.5 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 64 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 51)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 1.59 s
- Latency ms: p50 864.5 · p95 1379.3 · p99 1545.5 · max 1585.8 · 201 only: p50 351.5 / p95 409.8 / p99 417.4 · 409 only: p50 867.6 / p95 1379.8 / p99 1546.4 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 30 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**pessimistic, run 1** (pool=20, drop 52)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 4.41 s
- Latency ms: p50 1360.1 · p95 3991.5 · p99 4331.4 · max 4413.5 · 201 only: p50 524.1 / p95 653.8 / p99 659.7 · 409 only: p50 1364.8 / p95 3995.7 / p99 4332.2 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 87 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**pessimistic, run 2** (pool=20, drop 53)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 3.74 s
- Latency ms: p50 1236.5 · p95 3359.2 · p99 3664.5 · max 3740.7 · 201 only: p50 395.4 / p95 496.7 / p99 511.0 · 409 only: p50 1240.2 / p95 3362.8 / p99 3665.1 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 74 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**pessimistic, run 3** (pool=20, drop 54)
- Buyers: 10000 · Stock: 100 · Requests: 10000 (first attempts 10000, dup 0, retries 0, confirms 0) · Duration: 2.79 s
- Latency ms: p50 1116.7 · p95 2409.6 · p99 2715.5 · max 2794.5 · 201 only: p50 376.6 / p95 442.8 / p99 451.1 · 409 only: p50 1120.7 / p95 2416.1 / p99 2716.5 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Invariants: I1 ok, 54 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**Analysis (written by hand):** re-run of the headline burst on a **clean commit** (`eeeb8cc`, nothing uncommitted), so the README's headline numbers trace to exact code. Same command as the 16:14 run, which was recorded as `afb3aea-dirty`.
- **Same conclusion as before:** naive oversold by 1,373–1,683 units (16:14: 1,142–1,499). Atomic and pessimistic granted exactly 100 holds in 3/3 runs each, with 0 violations at every mid-run sample, 0 503s, and H1 ok.
- **Latency moved within run-to-run noise:** atomic p95 1.38–2.97 s (16:14: 1.79–4.39 s), pessimistic 2.41–3.99 s (16:14: 1.59–3.59 s). As before, the burst can't separate B from C on speed, because only 100 of 10,000 requests write.

---

## L1 burst: 2026-11-07 18:44, commit eeeb8cc: 10000 buyers vs 100 units

**Command:** `loadgen -scenario burst -api-bin bin/api -strategies atomic -buyers 10000 -stock 100 -pool 20 -repeat 3 -dup-ratio 0.1 -retry-ratio 0.3 -confirm -payment-failure-rate 0.05`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 1181.6 | 2471.5 | 2648.0 | 2703.5 | 612.0 | 3699 | 620.9 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 2227.4 | 4373.7 | 4573.7 | 4687.6 | 839.4 | 2133 | 4687.6 | ok |
| atomic | 20 | 100 | 0 | 0 | 9900 | 0 | 0 | 962.1 | 2155.9 | 2377.9 | 2623.6 | 524.4 | 3812 | 2623.6 | ok |

Per run (test-plan template):

**atomic, run 1** (pool=20, drop 55)
- Buyers: 10000 · Stock: 100 · Requests: 14164 (first attempts 10000, dup 983, retries 3081, confirms 129) · Duration: 2.70 s
- Latency ms: p50 1181.6 · p95 2471.5 · p99 2648.0 · max 2703.5 · 201 only: p50 547.1 / p95 612.0 / p99 619.0 · 409 only: p50 1188.3 / p95 2472.6 / p99 2648.3 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 34 · 409 4001 · 503 0 · other errors 0
- Confirm: 129 requests · confirmed 200 124 · declined 402 5 (hold released) · 503 0 · other 0 · latency ms p50 2567.1 / p95 2728.7 / p99 2794.2
- Invariants: I1 ok, 66 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**atomic, run 2** (pool=20, drop 56)
- Buyers: 10000 · Stock: 100 · Requests: 14167 (first attempts 10000, dup 983, retries 3084, confirms 132) · Duration: 4.69 s
- Latency ms: p50 2227.4 · p95 4373.7 · p99 4573.7 · max 4687.6 · 201 only: p50 735.8 / p95 839.4 / p99 4681.4 · 409 only: p50 2245.5 / p95 4375.1 / p99 4572.7 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 41 · 409 3994 · 503 0 · other errors 0
- Confirm: 132 requests · confirmed 200 121 · declined 402 11 (hold released) · 503 0 · other 0 · latency ms p50 4256.5 / p95 4493.4 / p99 4618.7
- Invariants: I1 ok, 104 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**atomic, run 3** (pool=20, drop 57)
- Buyers: 10000 · Stock: 100 · Requests: 14169 (first attempts 10000, dup 983, retries 3086, confirms 134) · Duration: 2.62 s
- Latency ms: p50 962.1 · p95 2155.9 · p99 2377.9 · max 2623.6 · 201 only: p50 408.3 / p95 524.4 / p99 532.5 · 409 only: p50 966.0 / p95 2157.1 / p99 2377.9 · Peak in-flight: 10000
- Outcome (first attempts): held 100 · 409 9900 · 503 0 · other 5xx 0 · transport errors 0 · hung 0
- Duplicate/retry reservation requests: 4035 · 201 41 · 409 3994 · 503 0 · other errors 0
- Confirm: 134 requests · confirmed 200 131 · declined 402 3 (hold released) · 503 0 · other 0 · latency ms p50 2350.7 / p95 2610.4 / p99 2650.3
- Invariants: I1 ok, 60 mid-run samples, peak active 100 · I2 ok · I3 ok · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 ok · H1 ok

**Analysis (written by hand):** re-run of the messy-client burst on clean commit `eeeb8cc` (the 16:01 run was `451a8dd-dirty`).
- **About 14,164–14,169 requests per run** (983 double-clicks, 3,081–3,086 same-key retries), **0 keys that produced two different effects** (I3 ok), exactly 100 holds, and every invariant held at every mid-run sample. 3–11 payments were declined per run, and their stock came back.
- Confirm p50 was again far above the mock's 100–300 ms (2.35–4.26 s), because winners' confirms queue in the same 20-connection pool as about 14,000 losing requests.

---

## L5 capacity ceiling: 2026-11-07 18:45, commit eeeb8cc

**Command:** `loadgen -scenario ceiling -api-bin bin/api -strategies atomic,pessimistic -rates 250,500,750,1000 -step 20s -pool 20 -repeat 2`

**Machine:** Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.2, go1.26.5, PostgreSQL 16.15 in Docker (engine VM: 2 CPU / 1.9 GB). Load generator, API, and DB on one machine.

**Strategy atomic, pool 20, drop 58 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 4.9 | 7.0 | 13.7 | 59.2 | 15 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 6.0 | 14.9 | 91.0 | 219.3 | 61 | ok | pass |
| 750 | 750 | 551 | 15000 | 15000 | 0 | 0 | 0.00% | 2465.8 | 6894.7 | 7161.4 | 7298.7 | 3648 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy atomic, pool 20, drop 59 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.1 | 8.3 | 43.5 | 134.6 | 29 | ok | pass |
| 500 | 500 | 500 | 10000 | 10000 | 0 | 0 | 0.00% | 6.0 | 10.7 | 46.9 | 116.4 | 36 | ok | pass |
| 750 | 750 | 566 | 15000 | 15000 | 0 | 0 | 0.00% | 1846.5 | 6177.3 | 6435.2 | 6635.6 | 3370 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy pessimistic, pool 20, drop 60 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.6 | 8.9 | 50.6 | 161.1 | 32 | ok | pass |
| 500 | 500 | 499 | 10000 | 10000 | 0 | 0 | 0.00% | 7.5 | 61.5 | 112.6 | 332.6 | 53 | ok | pass |
| 750 | 750 | 402 | 15000 | 4274 | 10726 | 0 | 71.51% | 8722.5 | 16662.8 | 17142.6 | 17362.7 | 7011 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **500 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Strategy pessimistic, pool 20, drop 61 (stock 51004, never sells out)**

| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| 250 | 250 | 250 | 5000 | 5000 | 0 | 0 | 0.00% | 5.6 | 10.2 | 52.7 | 124.5 | 21 | ok | pass |
| 500 | 500 | 426 | 10000 | 10000 | 0 | 0 | 0.00% | 35.9 | 3118.0 | 3378.1 | 3614.1 | 1188 | ok | fail |

Ceiling (last step with p99 within target and errors below the limit): **250 req/s** · Invariants: I1 ok · I2 ok · I5 ok · H1 ok

**Analysis (written by hand):** re-run of the B vs C write-path ceiling on clean commit `eeeb8cc` (the 16:15 run was `afb3aea-dirty`).
- **Ceilings: atomic 500 req/s in both runs; pessimistic 500, then 250.** In the 16:15 session atomic passed 750 twice. This session it failed 750 in both runs, so the 750 figure doesn't reproduce. The laptop varies between sessions, and the ceiling steps are coarse (250 req/s apart).
- **B was still ahead in every pairing, and the gap shows past the ceiling.** At the 750 step, atomic completed 551–566 req/s with **0 errors**. Pessimistic completed 402 req/s with **71.5% 503s** (run 1); in run 2 it already failed at 500 (426 req/s completed, p99 3.4 s). At the 500 step, which both passed in run 1, p99 was 47–91 ms for atomic vs 113 ms for pessimistic.
- **Conclusion for ADR-007 is unchanged, but stated more carefully:** across 4 runs in 2 sessions, B's ceiling was never below C's in the same session, and B always completed more requests once overloaded. The size of the gap depends on the session.

