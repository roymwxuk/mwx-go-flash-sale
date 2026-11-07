# Measured results

Every number in the README must appear here first. `cmd/loadgen` (milestone 3) will append one section per run, with the machine spec.

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
