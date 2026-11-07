package sale

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// StockStrategy takes n units from a drop's `remaining` counter inside the
// caller's transaction. It returns ErrSoldOut when fewer than n units are
// left. Implementations must not commit or roll back: the caller owns the tx,
// so a failure here also undoes the reservation row inserted before it.
//
// Every strategy runs at READ COMMITTED (see ADR-003). The strategies differ
// only in the SQL they send to the single hot `drops` row.
type StockStrategy interface {
	Name() string
	Take(ctx context.Context, tx pgx.Tx, dropID int64, n int) error
}

var strategies = map[string]StockStrategy{
	Naive{}.Name():  Naive{},
	Atomic{}.Name(): Atomic{},
}

// ParseStrategy maps a config value such as "atomic" to its implementation.
func ParseStrategy(name string) (StockStrategy, error) {
	s, ok := strategies[name]
	if !ok {
		return nil, fmt.Errorf("unknown stock strategy %q (want one of %v)", name, StrategyNames())
	}
	return s, nil
}

func StrategyNames() []string {
	names := make([]string, 0, len(strategies))
	for n := range strategies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Naive is strategy A: read, decide in Go, then write an absolute value back.
//
// INTENTIONALLY BROKEN. Under READ COMMITTED, two transactions can both read
// remaining=1, both decide "enough stock", and both write remaining=0. The
// second write silently overwrites the first (a lost update), so two holds
// exist for one unit. The CHECK (remaining >= 0) cannot catch this because
// no write ever goes negative. It exists so the load test can show the bug.
type Naive struct{}

func (Naive) Name() string { return "naive" }

func (Naive) Take(ctx context.Context, tx pgx.Tx, dropID int64, n int) error {
	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT remaining FROM drops WHERE id = $1`, dropID,
	).Scan(&remaining); err != nil {
		return fmt.Errorf("naive: read remaining: %w", err)
	}
	if remaining < n {
		return ErrSoldOut
	}
	// The decision above was made on a value that may already be stale.
	if _, err := tx.Exec(ctx,
		`UPDATE drops SET remaining = $2 WHERE id = $1`, dropID, remaining-n,
	); err != nil {
		return fmt.Errorf("naive: write remaining: %w", err)
	}
	return nil
}

// Atomic is strategy B: one conditional UPDATE does the check and the
// decrement together.
//
// Why it is safe at READ COMMITTED: if another transaction holds the row
// lock, this UPDATE waits. When the lock is released, Postgres re-reads the
// *latest committed* row version and re-evaluates the WHERE clause against it
// before writing. So `remaining >= n` is always checked against the true
// current value, and zero rows affected means sold out.
type Atomic struct{}

func (Atomic) Name() string { return "atomic" }

func (Atomic) Take(ctx context.Context, tx pgx.Tx, dropID int64, n int) error {
	tag, err := tx.Exec(ctx,
		`UPDATE drops SET remaining = remaining - $2
		 WHERE id = $1 AND remaining >= $2`, dropID, n)
	if err != nil {
		return fmt.Errorf("atomic: decrement: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoldOut
	}
	return nil
}
