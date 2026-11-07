# Resume bullets (Japanese, 体言止め)

Every number below is in `docs/results.md`. All were measured on one laptop (Apple M1 Pro, PostgreSQL 16 in a 2-CPU Docker VM).

- Go・PostgreSQLによる限定ドロップ向けフラッシュセールAPIの設計・実装。1万人が100個の在庫へ同時アクセスする負荷試験で、素朴なread-then-write方式の売り越し（1,142〜1,499個）を再現し、条件付きアトミックUPDATEで全試行の売り越しゼロ・在庫保存則の成立を実証
- 在庫確保3方式（read-then-write／アトミックUPDATE／悲観ロック）を共通インターフェースで比較できる負荷試験基盤の構築。同一セッションの書き込み上限試験でアトミックUPDATE 750 req/s（2回とも）、悲観ロック 500・250 req/sを計測し、ホット行のロック保持時間を根拠とする方式選定をADRに記録
- 冪等キー・二重課金防止・期限切れ予約の解放処理の実装と検証。二重クリック10％・同一キー再送30％・決済失敗5％を含む1回あたり約14,160リクエストで二重の効果ゼロ、`FOR UPDATE SKIP LOCKED` によるスイーパー3台の同時実行で10回中10回、各予約をちょうど1回だけ解放
