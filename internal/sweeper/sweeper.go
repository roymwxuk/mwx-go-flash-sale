// Package sweeper is the background worker that returns the stock of expired
// holds and deletes idempotency keys past their TTL. It is safe to run in any
// number of instances at once (see ADR-008):
//
//   - Each batch claims rows with FOR UPDATE SKIP LOCKED, so two sweepers
//     never wait for each other and never claim the same reservation.
//   - The held→expired transition and the stock refund happen in one
//     statement, and the UPDATE only matches rows still 'held'. A row can
//     leave 'held' exactly once, so its stock comes back exactly once.
package sweeper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Interval  time.Duration // pause between sweeps when there is nothing left to do
	BatchSize int           // rows per statement; small batches keep each transaction short
	KeyTTL    time.Duration // idempotency keys older than this are deleted
}

type Sweeper struct {
	pool *pgxpool.Pool
	cfg  Config
	log  *slog.Logger

	mu         sync.Mutex
	lastWarned map[int64]time.Time // drop id -> last time its failure was logged
}

func New(pool *pgxpool.Pool, cfg Config, log *slog.Logger) *Sweeper {
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 500
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.KeyTTL <= 0 {
		cfg.KeyTTL = 24 * time.Hour
	}
	return &Sweeper{pool: pool, cfg: cfg, log: log, lastWarned: map[int64]time.Time{}}
}

// Stats counts what one sweep did.
type Stats struct {
	Released    int // reservations moved held → expired
	Units       int // stock units returned
	KeysDeleted int
	FailedDrops int // drops whose release failed this sweep (others still ran)
}

// Run sweeps every Interval until ctx is cancelled, then returns. A failed
// sweep is logged and retried on the next tick: a transient DB error must not
// stop the worker for good.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		st, err := s.SweepOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil && st.FailedDrops == 0:
			s.log.Warn("sweep failed", "err", err)
		case st.Released > 0 || st.KeysDeleted > 0:
			// Per-drop failures were already logged (throttled) by SweepOnce.
			s.log.Info("sweep", "released", st.Released, "units", st.Units, "keys_deleted", st.KeysDeleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce drains everything that is due right now, drop by drop and one
// batch at a time. A batch smaller than BatchSize means nothing unclaimed is
// left for that drop.
//
// Why per drop: one drop in a state that cannot be repaired (e.g. a drop
// oversold by the naive strategy, where returning stock would exceed
// total_stock and the CHECK rejects it) must not block every other drop.
// With one batch across all drops, the bad rows sort first every time and
// the whole sweeper stalls forever. Found in a live run against a database
// that still held old naive load-test drops.
func (s *Sweeper) SweepOnce(ctx context.Context) (Stats, error) {
	var total Stats
	drops, err := s.dueDrops(ctx)
	if err != nil {
		return total, err
	}
	var errs []error
	for _, dropID := range drops {
		for {
			n, units, err := s.releaseBatch(ctx, dropID)
			if err != nil {
				if ctx.Err() != nil {
					return total, ctx.Err()
				}
				total.FailedDrops++
				errs = append(errs, err)
				s.warnThrottled(dropID, err)
				break // skip this drop, keep sweeping the others
			}
			total.Released += n
			total.Units += units
			if n < s.cfg.BatchSize {
				break
			}
		}
	}
	for {
		n, err := s.deleteKeysBatch(ctx)
		if err != nil {
			return total, errors.Join(append(errs, err)...)
		}
		total.KeysDeleted += n
		if n < s.cfg.BatchSize {
			break
		}
	}
	return total, errors.Join(errs...)
}

// dueDrops lists drops that have at least one expired hold.
func (s *Sweeper) dueDrops(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT drop_id FROM reservations
		WHERE status = 'held' AND expires_at <= now()`)
	if err != nil {
		return nil, fmt.Errorf("list drops with expired holds: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// warnThrottled logs a failing drop at most once a minute, so a drop that
// keeps failing does not flood the log every Interval.
func (s *Sweeper) warnThrottled(dropID int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastWarned[dropID]) < time.Minute {
		return
	}
	s.lastWarned[dropID] = time.Now()
	s.log.Warn("sweep: cannot release expired holds for drop; skipping it", "drop_id", dropID, "err", err)
}

// releaseBatch expires up to BatchSize holds of one drop and returns their stock, as one
// statement (one implicit transaction, one round trip). The CTEs:
//
//   - expired: held rows past expires_at, oldest first (served by the partial
//     index reservations_held_expiry). SKIP LOCKED passes over rows another
//     sweeper or a confirm has locked, instead of waiting for them.
//   - Holds with a pending payment are skipped: the money may already have
//     moved, so only confirm may finish them (ADR-006). Confirm also pushes
//     expires_at forward when it starts paying, which this query sees even
//     when the NOT EXISTS reads an older snapshot (see ADR-008).
//   - released: held → expired. The status condition repeats the guard so the
//     transition, and with it the refund, can only happen once per row.
//   - restock: add the released quantity back to this drop.
func (s *Sweeper) releaseBatch(ctx context.Context, dropID int64) (released, units int, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH expired AS (
			SELECT r.id
			FROM reservations r
			WHERE r.drop_id = $2
			  AND r.status = 'held'
			  AND r.expires_at <= now()
			  AND NOT EXISTS (
				SELECT 1 FROM payments p
				WHERE p.reservation_id = r.id AND p.status = 'pending')
			ORDER BY r.expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), released AS (
			UPDATE reservations r
			SET status = 'expired', updated_at = now()
			FROM expired e
			WHERE r.id = e.id AND r.status = 'held'
			RETURNING r.drop_id, r.quantity
		), restock AS (
			UPDATE drops d
			SET remaining = d.remaining + s.qty
			FROM (SELECT drop_id, SUM(quantity) AS qty FROM released GROUP BY drop_id) s
			WHERE d.id = s.drop_id
			RETURNING d.id
		)
		SELECT COUNT(*)::int, COALESCE(SUM(quantity), 0)::int FROM released`,
		s.cfg.BatchSize, dropID,
	).Scan(&released, &units)
	if err != nil {
		return 0, 0, fmt.Errorf("release expired holds for drop %d: %w", dropID, err)
	}
	return released, units, nil
}

// deleteKeysBatch removes up to BatchSize idempotency keys older than KeyTTL.
// After that, a retry with an old key is treated as a new request; for
// reserve, the per-user limit still prevents a second hold.
func (s *Sweeper) deleteKeysBatch(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE (user_id, key) IN (
			SELECT user_id, key FROM idempotency_keys
			WHERE created_at < now() - make_interval(secs => $1)
			LIMIT $2
			FOR UPDATE SKIP LOCKED)`,
		s.cfg.KeyTTL.Seconds(), s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("delete old idempotency keys: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
