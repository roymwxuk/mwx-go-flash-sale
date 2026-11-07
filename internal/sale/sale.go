// Package sale holds the drop and reservation logic: creating drops, reading
// stock state, and taking holds through a pluggable StockStrategy.
package sale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Domain errors. The HTTP layer maps these to status codes; errors.Is works
// even when they are wrapped.
var (
	ErrNotFound       = errors.New("drop not found")
	ErrSoldOut        = errors.New("sold out")
	ErrPerUserLimit   = errors.New("per-user limit reached")
	ErrSaleNotStarted = errors.New("sale has not started")
	ErrInvalidInput   = errors.New("invalid input")
)

type Drop struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	TotalStock   int       `json:"total_stock"`
	Remaining    int       `json:"remaining"`
	PerUserLimit int       `json:"per_user_limit"`
	PriceMinor   int64     `json:"price_minor"`
	SaleStartsAt time.Time `json:"sale_starts_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// DropState is a drop plus the quantities currently held and confirmed.
type DropState struct {
	Drop
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
}

type Reservation struct {
	ID        int64     `json:"id"`
	DropID    int64     `json:"drop_id"`
	UserID    string    `json:"user_id"`
	Quantity  int       `json:"quantity"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type Service struct {
	pool     *pgxpool.Pool
	strategy StockStrategy
	holdTTL  time.Duration
}

func NewService(pool *pgxpool.Pool, strategy StockStrategy, holdTTL time.Duration) *Service {
	return &Service{pool: pool, strategy: strategy, holdTTL: holdTTL}
}

func (s *Service) Strategy() StockStrategy { return s.strategy }

type CreateDropInput struct {
	Name         string     `json:"name"`
	TotalStock   int        `json:"total_stock"`
	PerUserLimit int        `json:"per_user_limit"` // 0 means default (1)
	PriceMinor   *int64     `json:"price_minor"`    // nil means default (1000)
	SaleStartsAt *time.Time `json:"sale_starts_at"` // nil means now
}

// Validate applies defaults and checks the input. It is pure, so it is unit
// tested without a database.
func (in *CreateDropInput) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.PerUserLimit == 0 {
		in.PerUserLimit = 1
	}
	if in.PriceMinor == nil {
		p := int64(1000)
		in.PriceMinor = &p
	}
	switch {
	case in.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	case in.TotalStock < 1:
		return fmt.Errorf("%w: total_stock must be >= 1", ErrInvalidInput)
	case in.PerUserLimit < 1 || in.PerUserLimit > in.TotalStock:
		return fmt.Errorf("%w: per_user_limit must be between 1 and total_stock", ErrInvalidInput)
	case *in.PriceMinor < 0:
		return fmt.Errorf("%w: price_minor must be >= 0", ErrInvalidInput)
	}
	return nil
}

func (s *Service) CreateDrop(ctx context.Context, in CreateDropInput) (Drop, error) {
	if err := in.Validate(); err != nil {
		return Drop{}, err
	}
	var d Drop
	// COALESCE lets a nil sale_starts_at fall back to the database clock.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO drops (name, total_stock, remaining, per_user_limit, price_minor, sale_starts_at)
		VALUES ($1, $2, $2, $3, $4, COALESCE($5, now()))
		RETURNING id, name, total_stock, remaining, per_user_limit, price_minor, sale_starts_at, created_at`,
		in.Name, in.TotalStock, in.PerUserLimit, *in.PriceMinor, in.SaleStartsAt,
	).Scan(&d.ID, &d.Name, &d.TotalStock, &d.Remaining, &d.PerUserLimit, &d.PriceMinor, &d.SaleStartsAt, &d.CreatedAt)
	if err != nil {
		return Drop{}, fmt.Errorf("insert drop: %w", err)
	}
	return d, nil
}

func (s *Service) GetDrop(ctx context.Context, id int64) (DropState, error) {
	var st DropState
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.name, d.total_stock, d.remaining, d.per_user_limit, d.price_minor,
		       d.sale_starts_at, d.created_at,
		       COALESCE(SUM(r.quantity) FILTER (WHERE r.status = 'held'), 0),
		       COALESCE(SUM(r.quantity) FILTER (WHERE r.status = 'confirmed'), 0)
		FROM drops d
		LEFT JOIN reservations r ON r.drop_id = d.id
		WHERE d.id = $1
		GROUP BY d.id`, id,
	).Scan(&st.ID, &st.Name, &st.TotalStock, &st.Remaining, &st.PerUserLimit, &st.PriceMinor,
		&st.SaleStartsAt, &st.CreatedAt, &st.Held, &st.Confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return DropState{}, ErrNotFound
	}
	if err != nil {
		return DropState{}, fmt.Errorf("get drop: %w", err)
	}
	return st, nil
}

// Reserve takes a hold of qty units for userID. See ADR-003 for the shape:
//
//  1. Pre-check without locks: not found, not started, over the per-user
//     limit, or already sold out → reject without opening a transaction.
//     A stale read here can only cause a too-early "sold out", never an
//     oversell, because the real check happens in step 3.
//  2. INSERT the reservation row. The partial UNIQUE index rejects a second
//     active hold for the same (drop, user).
//  3. strategy.Take on the hot drops row, as the last statement, so the row
//     lock is held for as short a time as possible before COMMIT.
func (s *Service) Reserve(ctx context.Context, dropID int64, userID string, qty int) (Reservation, error) {
	if userID == "" {
		return Reservation{}, fmt.Errorf("%w: user id is required", ErrInvalidInput)
	}
	if qty < 1 {
		return Reservation{}, fmt.Errorf("%w: quantity must be >= 1", ErrInvalidInput)
	}

	var limit, remaining int
	var started bool
	err := s.pool.QueryRow(ctx,
		`SELECT per_user_limit, remaining, sale_starts_at <= now() FROM drops WHERE id = $1`, dropID,
	).Scan(&limit, &remaining, &started)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Reservation{}, ErrNotFound
	case err != nil:
		return Reservation{}, fmt.Errorf("load drop: %w", err)
	case !started:
		return Reservation{}, ErrSaleNotStarted
	case qty > limit:
		return Reservation{}, ErrPerUserLimit
	case remaining < qty:
		return Reservation{}, ErrSoldOut
	}

	var r Reservation
	// BeginTxFunc commits if the callback returns nil and rolls back otherwise,
	// so a sold-out Take also removes the reservation row inserted above.
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO reservations (drop_id, user_id, quantity, expires_at)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4))
			RETURNING id, drop_id, user_id, quantity, status, expires_at, created_at`,
			dropID, userID, qty, s.holdTTL.Seconds(),
		).Scan(&r.ID, &r.DropID, &r.UserID, &r.Quantity, &r.Status, &r.ExpiresAt, &r.CreatedAt)
		if isUniqueViolation(err, "reservations_one_active_per_user") {
			return ErrPerUserLimit
		}
		if err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
		return s.strategy.Take(ctx, tx, dropID, qty)
	})
	if err != nil {
		return Reservation{}, err
	}
	return r, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
