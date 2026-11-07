# Load & Correctness Test Plan — "Limited drop in the largest community"

This plan turns one concrete story into a set of tests. Every number traces back to the workload model in section 2. When a number changes, change it here first, then in the code.

## 1. The story
A popular artist releases **100 units** of limited goods inside their fan community at a pre-announced time. The community is the largest on a mid-size fan-community platform. Fans wait for the countdown, then hit "buy" at the same moment. Many fail, mash the button, or retry after timeouts. Some winners abandon checkout or fail payment, so their holds expire and that stock goes back on sale to fans who are still retrying.

**Questions the plan must answer**
1. Does the system ever sell more than 100 units, charge anyone twice, or lose or duplicate stock? (correctness, hard requirement)
2. How fast is it under the realistic spike? (latency, reported)
3. How much can one instance take before it degrades? (capacity, reported)
4. How sensitive are the conclusions to the assumptions? (sensitivity)

## 2. Workload model

### 2.1 Platform scale (public figures, rounded)
- ~3,800 communities, ~485,000 registered fans. The repo uses rounded wording: "~4,000 communities, ~500,000 fans".
- Average community: ~128 fans. Not useful for a flash sale; the distribution is heavily skewed.

### 2.2 Largest community size (Zipf model, size ∝ 1/rank^s)
| s (skew) | Largest community | 10th largest |
|---|---|---|
| 0.8 | ~23,000 | ~3,600 |
| **1.0 (baseline)** | **~55,000** | **~5,500** |
| 1.2 | ~105,000 | ~6,600 |

### 2.3 Buyers per drop
buyers = members × active share × participation

| Parameter | Baseline | Range tested | Rationale |
|---|---|---|---|
| Active share | 60% | — | Registrations include inactive and churned fans |
| Participation | 30% | 20–40% | Limited goods from a favorite artist; high intent |
| **Buyers (largest community, s=1.0)** | **~10,000** | 4,000–19,000 | 55,000 × 0.6 × 0.3 |
| Buyers (10th-largest community) | ~1,000 | — | 5,500 × 0.6 × 0.3 |
| Stock | 100 units | — | Typical limited drop |
| Per-user limit | 1 | — | Common for limited goods |

### 2.4 Behavior model
- **Arrival curve (first attempt of each buyer), seconds after release:**
  t0: 50% · t1: 15% · t2: 10% · t3: 7% · t4: 5% · t5–t9: 13% spread evenly (2.6% per second).
- **Attempts per buyer** (mean ≈ 2.7):
  1 attempt: 40% · 2: 25% · 3–5: 25% · 6–10: 10%.
  A buyer retries after a sold-out (409) response with 1–3 s think time, hoping for released stock. A buyer retries after a timeout or 5xx with the **same Idempotency-Key**.
- **Duplicate submissions:** 10% of attempts are double-clicks: the same request with the same key, sent twice within 50 ms.
- **Client timeout:** 2 s.
- **Checkout after a successful hold:**
  - 80% confirm within 1–5 s.
  - 15% abandon (never confirm), so their holds expire.
  - 5% hit a payment failure, so their holds are released immediately.
- **Hold TTL:** 10 minutes in the real design, **compressed to 15 s** in tests (configurable), so released stock returns while buyers are still retrying.
- **Mock payment latency:** 100–300 ms, uniformly distributed.

### 2.5 Derived load (baseline)
- ~10,000 buyers × ~2.7 attempts ≈ **27,000 reservation requests**, plus duplicates (~2,700) and ~100–120 confirm calls.
- Peak arrival ≈ **5,000 first attempts in the first second**, plus duplicates and fast retries.
- In-flight requests (Little's law) = arrival rate × latency. At 5,000 req/s and 50 ms that's ~250 in flight. If latency rises to 1 s, that becomes ~5,000. Report the observed in-flight peak.

## 3. Pass/fail rules

### 3.1 Hard criteria (any violation fails the test)
| ID | Invariant | Checked by |
|---|---|---|
| I1 | confirmed + active-held quantity ≤ total_stock, at all times | invariant checker + periodic sampling during the run |
| I2 | no user holds or owns more than the per-user limit per drop | DB constraint + checker |
| I3 | at most one successful payment per reservation, and one effect per Idempotency-Key | checker (payments, idempotency_keys) |
| I4 | every expired hold is released exactly once | checker (status transitions + stock ledger) |
| I5 | remaining + held + confirmed == total_stock after the run | checker |
| H1 | no 5xx except an explicit 503 under overload; no hung requests (every request ends within client timeout + 1 s) | loadgen |

### 3.2 Reported targets (measured and reported; do not fail the build)
- Reservation endpoint: p95 ≤ 200 ms, p99 ≤ 500 ms at the baseline scenario.
- Sold-out responses (409) count as **successful** handling, not errors.
- Fairness note: report the order in which winners arrived versus the hold order (informational only).

## 4. Correctness scenarios (`go test`, real Postgres, run with `-race`)

| ID | Scenario | Setup | Pass |
|---|---|---|---|
| C1 | No oversell under a burst | 1,000 goroutines behind a start barrier race for 10 units; run once per strategy | Strategies B/C: exactly 10 holds, I1/I5 hold. **Strategy A: test asserts it oversells** (documents the bug) |
| C2 | Per-user limit under concurrency | Same user, 20 concurrent reservation attempts | Exactly 1 hold; the rest get 409 |
| C3 | Idempotency: same key at the same time | 50 concurrent requests with one key | One effect; all 50 responses are identical |
| C4 | Idempotency: same key, different body | Reuse a key with a changed quantity | 422; no effect |
| C5 | Payment failure releases the hold | Mock payment forced to fail | Hold released, stock returns, I5 holds |
| C6 | Confirm retry never double-charges | Confirm with the same key ×20 concurrently, plus a retry after a simulated timeout | Exactly one payment |
| C7 | Expiry is exactly-once with multiple sweepers | 200 expired holds, 2–3 sweepers running at once | Each released once; stock restored exactly; I4/I5 hold |
| C8 | Confirm after expiry is rejected | Let a hold expire, then confirm | 409/410; no payment; stock already released |
| C9 | Sold-out is consistent | Drain stock, then 100 concurrent attempts | All 409; stock never negative |
| C10 | Mixed operations keep stock conserved | Random interleaving of reserve, confirm, fail and expire for 30 s | I1–I5 hold throughout |

## 5. Load scenarios (`cmd/loadgen`)
Each run writes JSON plus a markdown row to `docs/results.md`, including the machine spec (CPU, RAM, OS, Go and Postgres versions), strategy, pool size and git commit.

| ID | Name | Load | Purpose | Pass / report |
|---|---|---|---|---|
| L1 | Worst-case burst | 10,000 buyers released at the same instant, 1 attempt each, 100 units; run once per strategy | Correctness under maximum contention (harsher than reality, by design) | Hard: I1–I5, H1. Report: oversell count (A), p95/p99, throughput, duration |
| L2 | **Largest-community drop** (main scenario) | Full behavior model from 2.4: ~10,000 buyers, arrival curve, attempts, duplicates, timeouts, checkout, 15 s TTL, ~30 s run | The "as real as possible" result | Hard: I1–I5, H1. Report: peak req/s reached, p50/p95/p99, in-flight peak, units sold, units released and resold, double-charge count (must be 0) |
| L3 | Typical popular community | Same as L2 with ~1,000 buyers | Baseline latency without stress | Report against the 3.2 targets |
| L4 | Sensitivity sweep | L2 with 4,000 / 10,000 / 19,000 buyers (the s=0.8 / 1.0 / 1.2 cases) | Show the conclusion doesn't depend on one guess | Hard: I1–I5 at every level. Report a latency-vs-buyers table |
| L5 | Capacity ceiling | Step a constant rate 500 → 1,000 → 2,000 → 4,000 → 8,000 req/s, 20 s per step, stock large enough not to sell out | Find the max sustainable rate on one instance | Ceiling = last step with p99 ≤ 500 ms and errors < 1%. Compare it with L2's peak (see section 6) |
| L6 | Retry storm | L2 with 30% timeouts (inject latency) and 20% duplicate submissions | Idempotency under messy clients | Hard: I3 (0 double charges), I1–I5 |
| L7 | Pool-size comparison | L2 with pgxpool MaxConns 5 / 20 / 50 | Show how pool sizing affects latency, and that overload returns 503 instead of hanging | Report latency per size; H1 must hold |
| L8 | Soak (optional) | 70% of the L5 ceiling for 10 minutes, stock refilled per drop | Stability: no leaks, no drift | Hard: I1–I5. Report memory, goroutine count and DB connection count over time |

## 6. Interpreting the results
- **If the L5 ceiling ≥ L2's peak rate:** one instance handles the largest community's drop. Say so, and give the headroom.
- **If the L5 ceiling < L2's peak rate (likely on a laptop):** this is the more interesting result. Write in the README: "A single instance sustains X req/s, below the modeled peak of ~5,000/s. The bottleneck is the single stock row (writes are serialized). In production I would add an admission queue or waiting room to admit buyers at the rate the stock row can handle, and/or shard stock across N rows." Discussion only, not code.
- Strategy comparison (from L1/L2): state which strategy won, by how much, and why. This goes in the ADR.

## 7. Priority (fits the 2-day budget)
- **Must:** C1, C2, C3, C6, C7, L1, L2, L5.
- **Should:** C4, C5, C9, L3, L4, L6.
- **Could:** C8, C10, L7, L8.

## 8. Results template (`docs/results.md`)
```
### <scenario id> — <date> — commit <sha>
Machine: <CPU> / <RAM> / <OS> · Go <ver> · Postgres <ver> · pool=<n> · strategy=<A|B|C>
Buyers: <n> · Stock: <n> · Requests: <n> (dup <n>, retries <n>) · Duration: <s>
Latency ms: p50 <> · p95 <> · p99 <> · max <> · Peak req/s: <> · Peak in-flight: <>
Outcome: sold <n> · released <n> · resold <n> · 409 <n> · 503 <n> · 5xx <n>
Invariants: I1 <ok/n> · I2 <> · I3 <> · I4 <> · I5 <> · H1 <>
```

## 9. Implementation status (kept current)

| ID | Status | Where |
|---|---|---|
| C1 | ✅ done (A, B and C) | `internal/sale/concurrency_test.go`: `TestConcurrentCorrectStrategiesNeverOversell` (B, C), `TestConcurrentNaiveOversells` (A) |
| C2 | ✅ done (50 concurrent attempts, more than the 20 planned) | `TestConcurrentSameUserGetsOneHold` |
| C3 | ✅ done (both strategies) | `internal/sale/idempotency_test.go`: `TestIdempotentReserveConcurrentSameKey` |
| C4 | ✅ done (quantity, drop, and endpoint mismatches) | `TestIdempotencyKeyReuseWithDifferentRequest` |
| C5 | ✅ done | `TestPaymentFailureReleasesHold`, `TestConcurrentDeclinedConfirmReleasesOnce` |
| C6 | ✅ done (20 same-key + 20 different-key confirms; retry after a timeout mid-payment) | `TestConcurrentConfirmChargesOnce`, `TestConfirmRetryAfterTimeoutDoesNotDoubleCharge` |
| C7 | ⏳ milestone 5 (expiry sweeper) | |
| C8 | ✅ done (hold past `expires_at` → 410, nothing charged; also needs the sweeper case in milestone 5) | `TestConfirmAfterExpiryIsRejected` |
| C9 | ✅ done | `TestConcurrentSoldOutIsConsistent` |
| C10 | optional, after milestone 5 | |
| L1 | ✅ done, including H1, mid-run I1 sampling, per-outcome latency, peak in-flight, git commit | `make load` → `docs/results.md` |
| L2 | ⏳ after milestones 4 and 5. Needs same-key retries, confirm/payment, and hold expiry with resale; without them the "released and resold" numbers would be invented | |
| L3, L4 | ⏳ after L2 (they are L2 variants) | |
| L6 | 🟡 partial: `make load-messy` = L1 burst + 10% double-clicks + 30% same-key retries + confirm with 5% declines. Not yet on top of L2, and timeouts are not injected (retries follow a received answer) | `docs/results.md` 16:01 |
| L5 | ✅ done | `make load-ceiling` → `docs/results.md` |
| L7 | ⏳ milestone 5 (pool-size comparison); `-pool` already exists | |
| L8 | optional | |
| I4 | reported as "n/a" until the sweeper exists | |

**Note on section 6 (found while running L5):** L5 as specified keeps stock from selling out, so it measures the *write* path (about 500 req/s ceiling here). The modeled L2 peak of about 5,000 req/s is mostly *sold-out* traffic, which only needs the lock-free pre-check, and L1 measured that path at 4,422–7,489 req/s. Comparing L5's ceiling directly with L2's peak would understate capacity. Proposal: judge the main scenario by L2 itself, and keep L5 as the write-path ceiling. A pgbench baseline shows the single row alone handles about 4,900 updates/s in the same VM (see `docs/results.md`).
