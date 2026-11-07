# Interview notes

Short answers in English and Japanese. Updated as each milestone lands, and backed by numbers in `results.md`.

## Why atomic UPDATE over FOR UPDATE (or the reverse)? What did the numbers show?
- EN (A vs B so far): With 1,000 goroutines racing for 10 units, naive read-then-write oversold in 10/10 runs (511–718 extra units). The atomic `UPDATE ... WHERE remaining >= n` granted exactly 10 holds in 10/10 runs. B is correct at READ COMMITTED because a blocked UPDATE re-checks its WHERE clause against the latest committed row before writing. So check-and-decrement is one atomic step, with no explicit lock and no retry loop. _FOR UPDATE comparison and latency numbers pending (milestones 3 and 5)._
- JA（A対Bのみ）: 10個に対し1,000 goroutineを同時発射した結果、素朴な read-then-write は10回中10回売り越し（超過511〜718個）。`UPDATE ... WHERE remaining >= n` の条件付きUPDATEは10回とも正確に10件のみ確保。READ COMMITTEDでも、ロック待ちしたUPDATEはコミット済み最新行でWHERE句を再評価するため、在庫確認と減算が1文で原子的に行われる。明示ロックもリトライも不要。_FOR UPDATEとの比較・レイテンシは後続マイルストーンで追記。_

## What happens on the hot row at 100× traffic, and what would you change?
- EN: _TBD_
- JA: _未記入_

## How does idempotency behave when two identical requests arrive at the same millisecond?
- EN: _TBD (milestone 4)_
- JA: _未記入_

## Why can't the race detector prove there is no oversell?
- EN: `-race` instruments memory accesses inside one Go process. Oversell is two *database transactions* interleaving (a lost update on `drops.remaining`). The Go side may have no shared memory at all, since each request has its own goroutine and its own connection. So the detector has nothing to flag. DB correctness is proven by constraints, the invariant checker, and load results.
- JA: `-race` は単一プロセス内のメモリアクセス競合のみ検出する。売り越しは DB トランザクション同士の lost update であり、Go 側に共有メモリが存在しないため検出対象外。DB の正しさは制約・不変条件チェッカー・負荷試験結果で証明する。

## How do you know the sweeper never releases stock twice?
- EN: _TBD (milestone 5)_
- JA: _未記入_

## What would break first if you deployed this as-is?
- EN: _TBD_
- JA: _未記入_
