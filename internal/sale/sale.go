// Package sale holds the drop and reservation logic: creating drops, reading
// stock state, and taking holds through a pluggable StockStrategy.
package sale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/payment"
)

// Domain errors. The HTTP layer maps these to status codes; errors.Is works
// even when they are wrapped.
var (
	ErrNotFound       = errors.New("drop not found")
	ErrSoldOut        = errors.New("sold out")
	ErrPerUserLimit   = errors.New("per-user limit reached")
	ErrSaleNotStarted = errors.New("sale has not started")
	ErrInvalidInput   = errors.New("invalid input")

	// ErrIdempotencyMismatch: the key was already used for a different
	// request (other endpoint, other drop or reservation, other quantity).
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	ErrNotHeld             = errors.New("reservation is not held")
	ErrHoldExpired         = errors.New("hold has expired")
	// ErrPaymentPending: the provider's answer was lost (timeout, client
	// gone). Retrying with the same Idempotency-Key is safe and finishes it.
	ErrPaymentPending = errors.New("payment outcome unknown; retry with the same Idempotency-Key")
)

// Result is a successful (or stored) response, exactly as sent to the
// client. Body is stored verbatim with the idempotency key, so a replay is
// byte-for-byte identical to the original.
type Result struct {
	Status      int
	Body        []byte
	Replayed    bool
	Reservation Reservation
}

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
	payments payment.Gateway
}

// NewService wires the service. A nil gateway means an instant, always
// approving mock, which is what most tests want.
func NewService(pool *pgxpool.Pool, strategy StockStrategy, holdTTL time.Duration, payments payment.Gateway) *Service {
	if payments == nil {
		payments = payment.NewMock(payment.MockConfig{})
	}
	return &Service{pool: pool, strategy: strategy, holdTTL: holdTTL, payments: payments}
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

const reserveEndpoint = "POST /drops/{id}/reservations"

// Reserve takes a hold of qty units for userID, idempotently under key.
// See ADR-003 (transaction shape) and ADR-005 (idempotency):
//
//  1. One lock-free read returns both the drop's pre-check fields and any
//     stored response for this key. A stored key replays (or 422s on a
//     different request). Not found, not started, over the per-user limit,
//     or already sold out → reject without opening a transaction. Rejections
//     have no side effect, so they are not stored and a retry re-evaluates.
//  2. In one READ COMMITTED transaction: INSERT the idempotency key first.
//     A concurrent request with the same key blocks on its unique index
//     until we commit, then replays our response. INSERT the reservation
//     (partial UNIQUE index enforces one active hold per user), store the
//     response on the key, and take stock from the hot row last so its
//     row lock is held as briefly as possible.
func (s *Service) Reserve(ctx context.Context, dropID int64, userID string, qty int, key string) (Result, error) {
	if err := validateCaller(userID, key); err != nil {
		return Result{}, err
	}
	if qty < 1 {
		return Result{}, fmt.Errorf("%w: quantity must be >= 1", ErrInvalidInput)
	}
	hash := requestHash(reserveEndpoint, dropID, qty)

	var limit, remaining int
	var started bool
	var stored storedResponse
	err := s.pool.QueryRow(ctx, `
		SELECT d.per_user_limit, d.remaining, d.sale_starts_at <= now(),
		       k.request_hash, k.response_status, k.response_body
		FROM drops d
		LEFT JOIN idempotency_keys k ON k.user_id = $2 AND k.key = $3
		WHERE d.id = $1`, dropID, userID, key,
	).Scan(&limit, &remaining, &started, &stored.hash, &stored.status, &stored.body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Result{}, ErrNotFound
	case err != nil:
		return Result{}, fmt.Errorf("load drop: %w", err)
	case stored.hash != nil:
		return stored.replay(hash)
	case !started:
		return Result{}, ErrSaleNotStarted
	case qty > limit:
		return Result{}, ErrPerUserLimit
	case remaining < qty:
		return Result{}, ErrSoldOut
	}

	var res Result
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := claimKey(ctx, tx, userID, key, reserveEndpoint, hash); err != nil {
			return err
		}
		r := &res.Reservation
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
		res.Status = http.StatusCreated
		if res.Body, err = json.Marshal(r); err != nil {
			return err
		}
		if err := storeResponse(ctx, tx, userID, key, res.Status, res.Body); err != nil {
			return err
		}
		return s.strategy.Take(ctx, tx, dropID, qty)
	})
	if errors.Is(err, errKeyTaken) {
		// Another request with this key committed first: replay its answer.
		return s.replayStored(ctx, userID, key, hash)
	}
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
