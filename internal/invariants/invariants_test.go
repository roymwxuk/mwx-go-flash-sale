package invariants_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

// The checker is the referee for every other test, so it gets tested first:
// hand-build broken states with raw SQL and make sure each one is reported.

func TestCheckerDetectsEachViolation(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup string // $1 = drop id
		want  []string
	}{
		{
			name: "clean",
			setup: `UPDATE drops SET remaining = 1 WHERE id = $1;
			        INSERT INTO reservations (drop_id, user_id, quantity, expires_at) VALUES ($1, 'a', 1, now());`,
			want: nil,
		},
		{
			name: "lost update oversell", // 3 holds, counter says 0 left
			setup: `UPDATE drops SET remaining = 0 WHERE id = $1;
			        INSERT INTO reservations (drop_id, user_id, quantity, expires_at)
			        VALUES ($1, 'a', 1, now()), ($1, 'b', 1, now()), ($1, 'c', 1, now());`,
			want: []string{invariants.NoOversell, invariants.StockConservation},
		},
		{
			name:  "double release", // 1 held but counter was refunded too
			setup: `INSERT INTO reservations (drop_id, user_id, quantity, expires_at) VALUES ($1, 'a', 1, now());`,
			want:  []string{invariants.StockConservation},
		},
		{
			name: "per-user quantity over limit", // the unique index cannot see quantity
			setup: `INSERT INTO reservations (drop_id, user_id, quantity, expires_at) VALUES ($1, 'a', 2, now());
			        UPDATE drops SET remaining = 0 WHERE id = $1;`,
			want: []string{invariants.PerUserLimit},
		},
		{
			name: "confirmed without payment",
			setup: `INSERT INTO reservations (drop_id, user_id, quantity, status, expires_at) VALUES ($1, 'a', 1, 'confirmed', now());
			        UPDATE drops SET remaining = 1 WHERE id = $1;`,
			want: []string{invariants.PaymentConsistency},
		},
	}

	pool := testdb.New(t, 4)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dropID int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO drops (name, total_stock, remaining) VALUES ($1, 2, 2) RETURNING id`, tc.name,
			).Scan(&dropID); err != nil {
				t.Fatal(err)
			}
			// pgx sends multi-statement SQL only without parameters, so
			// substitute the id as a literal (test-only, no user input).
			if _, err := pool.Exec(ctx, replaceID(tc.setup, dropID)); err != nil {
				t.Fatalf("setup: %v", err)
			}

			rep, err := invariants.Check(ctx, pool, dropID)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, v := range rep.Violations {
				got[v.Invariant] = true
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("missing violation %q; got %+v", w, rep.Violations)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("want exactly %v, got %+v", tc.want, rep.Violations)
			}
		})
	}
}

func replaceID(sql string, id int64) string {
	return strings.ReplaceAll(sql, "$1", strconv.FormatInt(id, 10))
}
