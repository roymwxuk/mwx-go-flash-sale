# Interview notes

Short answers in English and Japanese. Updated as each milestone lands, and backed by numbers in `results.md`.

## Why atomic UPDATE over FOR UPDATE (or the reverse)? What did the numbers show?
- EN (A vs B so far): With 1,000 goroutines racing for 10 units, naive read-then-write oversold in 10/10 runs (511–718 extra units). The atomic `UPDATE ... WHERE remaining >= n` granted exactly 10 holds in 10/10 runs. B is correct at READ COMMITTED because a blocked UPDATE re-checks its WHERE clause against the latest committed row before writing. So check-and-decrement is one atomic step, with no explicit lock and no retry loop. _FOR UPDATE comparison and latency numbers pending (milestones 3 and 5)._
- JA（A対Bのみ）: 10個に対し1,000 goroutineを同時発射した結果、素朴な read-then-write は10回中10回売り越し（超過511〜718個）。`UPDATE ... WHERE remaining >= n` の条件付きUPDATEは10回とも正確に10件のみ確保。READ COMMITTEDでも、ロック待ちしたUPDATEはコミット済み最新行でWHERE句を再評価するため、在庫確認と減算が1文で原子的に行われる。明示ロックもリトライも不要。_FOR UPDATEとの比較・レイテンシは後続マイルストーンで追記。_

## What happens on the hot row at 100× traffic, and what would you change?
- EN (draft, from L1/L5 data): Two different paths matter. Sold-out traffic only needs a lock-free read, and handled 4,422–7,489 req/s on a laptop. Writes to the single stock row are serialized: our full reserve path tops out at about 500 req/s (L5). Postgres alone did about 4,900 single-row updates/s in the same VM (pgbench), so the gap is round trips made while the row lock is held. In a 100-unit drop only about 100 requests ever write, so 100× buyers mostly stresses the read path and the connection pool. Changes, in order: (1) answer sold-out from a cached flag before taking a DB connection; (2) shorten the lock hold, with one-statement reserve (INSERT + UPDATE in a single CTE) and no network round trip between UPDATE and COMMIT; (3) if writes themselves become the limit (big drops), split stock into N bucket rows and take from a random one; (4) admission queue / waiting room so buyers enter at the rate the stock row can absorb.
- JA（暫定・L1/L5の計測より）: 経路が2つある。売り切れ応答はロック不要の読み取りだけで、ノートPCでも4,422〜7,489 req/s。在庫行への書き込みは直列化され、予約処理全体で約500 req/sが上限（L5）。同じVM内のPostgres単体では1行UPDATEが約4,900回/秒（pgbench）なので、差は行ロック保持中のラウンドトリップ。100個のドロップで書き込むのは約100リクエストだけなので、100倍の購入者が圧迫するのは主に読み取り経路と接続プール。対策の順序：(1) 売り切れはDB接続を取る前にキャッシュしたフラグで返す、(2) ロック保持時間の短縮（INSERTとUPDATEを1つのCTEで1往復に、UPDATEとCOMMITの間に往復を挟まない）、(3) 書き込み自体が上限になる大規模ドロップでは在庫をN行のバケットに分割し、ランダムな行から取る、(4) 在庫行が捌ける速度で入場させる待合室／アドミッションキュー。

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
- EN (draft, milestone 3): The connection pool, not correctness. In the 10,000-buyer test with a 20-connection pool, the atomic strategy never oversold, but p95 reached 1.3–2.5 s because every request queues for one of 20 connections. With the naive strategy, 1,600–3,792 requests per run hit the 5 s deadline and got 503. The fix order: shed sold-out traffic before it needs a DB connection (a cached "sold out" flag), size the pool against measured numbers (milestone 5), then admission control in front of the hot row. _Expiry, payments, and idempotency gaps to be added after milestone 4–5._
- JA（暫定・M3時点）: 先に壊れるのは正しさではなくコネクションプール。10,000人・プール20の試験で、atomic戦略は売り越しゼロだったが、全リクエストが20本の接続を奪い合うためp95は1.3〜2.5秒。naive戦略では1回あたり1,600〜3,792件が5秒のタイムアウトで503に。対策の順序：売り切れ後のトラフィックをDB接続前に返す（売り切れフラグのキャッシュ）、計測に基づくプールサイズ調整（M5）、ホット行の前段にアドミッション制御。_期限切れ・決済・冪等性の論点はM4〜5後に追記。_
