// Package invariants checks the database for violations of the five
// properties this project proves. It reads final DB state only and trusts no
// application counters, so tests and the load generator can both use it as an
// independent referee.
package invariants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Invariant names used in Violation.Invariant.
const (
	NoOversell         = "no_oversell"
	PerUserLimit       = "per_user_limit"
	NoDoubleCharge     = "no_double_charge"
	PaymentConsistency = "payment_consistency"
	StockConservation  = "stock_conservation"
)

// DB is the subset of *pgxpool.Pool (or pgx.Tx) the checker needs. Taking an
// interface keeps the checker usable inside a test transaction too.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type Stats struct {
	DropID     int64 `json:"drop_id"`
	TotalStock int   `json:"total_stock"`
	Remaining  int   `json:"remaining"`
	Held       int   `json:"held"`
	Confirmed  int   `json:"confirmed"`
}

// Oversold is how many units were promised beyond total stock (0 if none).
func (s Stats) Oversold() int {
	return max(0, s.Held+s.Confirmed-s.TotalStock)
}

type Violation struct {
	Invariant string `json:"invariant"`
	Detail    string `json:"detail"`
}

type Report struct {
	Stats      Stats       `json:"stats"`
	Violations []Violation `json:"violations"`
}

func (r Report) OK() bool { return len(r.Violations) == 0 }

// maxListed caps how many offending rows are listed per invariant.
const maxListed = 5

// Check inspects one drop. Exactly-once release (invariant 4) has no separate
// query: releasing a hold twice returns its units twice, which shows up as a
// conservation *surplus* (remaining + held + confirmed > total_stock). A lost
// update also produces a surplus; no_oversell tells the two apart, since only
// the lost update pushes held + confirmed above total_stock.
func Check(ctx context.Context, db DB, dropID int64) (Report, error) {
	var rep Report
	st := &rep.Stats
	st.DropID = dropID
	err := db.QueryRow(ctx, `
		SELECT d.total_stock, d.remaining,
		       COALESCE(SUM(r.quantity) FILTER (WHERE r.status = 'held'), 0),
		       COALESCE(SUM(r.quantity) FILTER (WHERE r.status = 'confirmed'), 0)
		FROM drops d
		LEFT JOIN reservations r ON r.drop_id = d.id
		WHERE d.id = $1
		GROUP BY d.id`, dropID,
	).Scan(&st.TotalStock, &st.Remaining, &st.Held, &st.Confirmed)
	if err != nil {
		return Report{}, fmt.Errorf("load stats for drop %d: %w", dropID, err)
	}

	add := func(inv, format string, args ...any) {
		rep.Violations = append(rep.Violations, Violation{Invariant: inv, Detail: fmt.Sprintf(format, args...)})
	}

	// 1. No oversell.
	if n := st.Oversold(); n > 0 {
		add(NoOversell, "held %d + confirmed %d = %d > total_stock %d (oversold by %d)",
			st.Held, st.Confirmed, st.Held+st.Confirmed, st.TotalStock, n)
	}

	// 5. Stock conservation (also catches double release, see above).
	if sum := st.Remaining + st.Held + st.Confirmed; sum != st.TotalStock {
		// Surplus: more units accounted for than exist. Either holds were
		// created without their decrement landing (lost update) or a release
		// returned stock twice. Deficit: remaining was decremented with no
		// matching reservation (e.g. a decrement committed without its row).
		kind := "deficit: remaining decremented without a matching reservation"
		if sum > st.TotalStock {
			kind = "surplus: holds without a matching decrement (lost update) or stock released twice"
		}
		add(StockConservation, "remaining %d + held %d + confirmed %d = %d != total_stock %d; %s",
			st.Remaining, st.Held, st.Confirmed, sum, st.TotalStock, kind)
	}

	// 2. Per-user limit: active rows per user and their total quantity.
	if err := eachRow(ctx, db, func(user string, rows, qty, limit int) {
		add(PerUserLimit, "user %q has %d active reservations totalling %d (limit %d)", user, rows, qty, limit)
	}, `
		SELECT r.user_id, COUNT(*)::int, SUM(r.quantity)::int, MAX(d.per_user_limit)
		FROM reservations r JOIN drops d ON d.id = r.drop_id
		WHERE r.drop_id = $1 AND r.status IN ('held', 'confirmed')
		GROUP BY r.user_id
		HAVING COUNT(*) > 1 OR SUM(r.quantity) > MAX(d.per_user_limit)
		ORDER BY r.user_id
		LIMIT $2`, dropID, maxListed); err != nil {
		return Report{}, err
	}

	// 3. No double charge: at most one successful payment per reservation.
	if err := eachID(ctx, db, func(resID int64, n int) {
		add(NoDoubleCharge, "reservation %d has %d successful payments", resID, n)
	}, `
		SELECT p.reservation_id, COUNT(*)::int
		FROM payments p JOIN reservations r ON r.id = p.reservation_id
		WHERE r.drop_id = $1 AND p.status = 'succeeded'
		GROUP BY p.reservation_id HAVING COUNT(*) > 1
		LIMIT $2`, dropID, maxListed); err != nil {
		return Report{}, err
	}

	// Payment and confirmation agree: confirmed <=> exactly one succeeded payment.
	if err := eachID(ctx, db, func(resID int64, paid int) {
		if paid == 0 {
			add(PaymentConsistency, "reservation %d is confirmed but has no successful payment", resID)
		} else {
			add(PaymentConsistency, "reservation %d has a successful payment but is not confirmed", resID)
		}
	}, `
		SELECT r.id, COUNT(p.id)::int
		FROM reservations r
		LEFT JOIN payments p ON p.reservation_id = r.id AND p.status = 'succeeded'
		WHERE r.drop_id = $1
		GROUP BY r.id, r.status
		HAVING (r.status = 'confirmed') <> (COUNT(p.id) > 0)
		LIMIT $2`, dropID, maxListed); err != nil {
		return Report{}, err
	}

	return rep, nil
}

func eachRow(ctx context.Context, db DB, fn func(user string, rows, qty, limit int), sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var user string
		var n, qty, limit int
		if err := rows.Scan(&user, &n, &qty, &limit); err != nil {
			return err
		}
		fn(user, n, qty, limit)
	}
	return rows.Err()
}

func eachID(ctx context.Context, db DB, fn func(id int64, n int), sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		fn(id, n)
	}
	return rows.Err()
}
