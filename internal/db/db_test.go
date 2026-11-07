package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/db"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t, 4)
	// testdb.New already migrated once; a second run must be a no-op.
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// The per-user limit is a schema guarantee, so it is tested at the schema
// level, without any application code in between.
func TestOneActiveReservationPerUserPerDrop(t *testing.T) {
	pool := testdb.New(t, 4)
	ctx := context.Background()

	var dropID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO drops (name, total_stock, remaining) VALUES ('t', 10, 10) RETURNING id`,
	).Scan(&dropID); err != nil {
		t.Fatal(err)
	}

	insert := func(status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO reservations (drop_id, user_id, quantity, status, expires_at)
			VALUES ($1, 'u1', 1, $2, now() + interval '5 minutes')`, dropID, status)
		return err
	}

	if err := insert("held"); err != nil {
		t.Fatal(err)
	}
	if err := insert("held"); !isUniqueViolation(err) {
		t.Fatalf("second active hold: want unique violation, got %v", err)
	}
	// Inactive rows do not count towards the limit.
	if err := insert("expired"); err != nil {
		t.Fatalf("expired row should be allowed: %v", err)
	}
}

func TestStockChecks(t *testing.T) {
	pool := testdb.New(t, 4)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO drops (name, total_stock, remaining) VALUES ('t', 10, -1)`); !isCheckViolation(err) {
		t.Fatalf("negative remaining: want check violation, got %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO drops (name, total_stock, remaining) VALUES ('t', 10, 11)`); !isCheckViolation(err) {
		t.Fatalf("remaining > total: want check violation, got %v", err)
	}
}

func isUniqueViolation(err error) bool { return pgCode(err) == "23505" }
func isCheckViolation(err error) bool  { return pgCode(err) == "23514" }

func pgCode(err error) string {
	// errors.As walks the wrap chain looking for a *pgconn.PgError.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
