package sale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const confirmEndpoint = "POST /reservations/{id}/confirm"

// paymentWindow is how far a hold's expiry is pushed when its payment
// starts, so the sweeper cannot expire it while the provider is being called.
// It only needs to exceed the provider's worst-case latency plus a retry.
const paymentWindow = 2 * time.Minute

// paymentKey is the idempotency key sent to the provider. It is derived from
// the reservation, not from the client's Idempotency-Key, so *every* confirm
// attempt for a reservation (retries, double clicks, different client keys)
// presents the same key and the provider charges at most once.
func paymentKey(reservationID int64) string {
	return "res-" + strconv.FormatInt(reservationID, 10)
}

// Confirm pays for a held reservation and confirms it. See ADR-006.
//
// The payment call takes 100–300 ms and can fail or time out, so it must not
// run inside a DB transaction (that would hold a pool connection and the
// reservation's row lock the whole time). Confirm is instead a small,
// re-runnable state machine; calling it again, concurrently or later, just
// drives the same reservation to the same final state:
//
//  1. tx1: lock the reservation row, check owner/status/expiry, and record a
//     `pending` payment (payments.reservation_id is UNIQUE: one row ever).
//  2. Charge the provider with paymentKey(id), outside any transaction.
//  3. tx2: conditional transitions (pending→succeeded, held→confirmed; or
//     pending→failed, held→cancelled plus stock released). A WHERE on the
//     current status makes each transition happen exactly once.
//  4. Store the final response under the client's Idempotency-Key.
func (s *Service) Confirm(ctx context.Context, reservationID int64, userID, key string) (Result, error) {
	if err := validateCaller(userID, key); err != nil {
		return Result{}, err
	}
	hash := requestHash(confirmEndpoint, reservationID)

	var stored storedResponse
	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body
		FROM idempotency_keys WHERE user_id = $1 AND key = $2`, userID, key,
	).Scan(&stored.hash, &stored.status, &stored.body)
	if err == nil {
		return stored.replay(hash)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("load idempotency key: %w", err)
	}

	needCharge, amount, err := s.startPayment(ctx, reservationID, userID)
	if err != nil {
		return Result{}, err
	}

	if needCharge {
		approved, err := s.payments.Charge(ctx, paymentKey(reservationID), amount)
		if err != nil {
			// The provider may or may not have charged. Leave the payment
			// `pending`; a retry with the same key asks the provider again
			// and gets the recorded outcome without a second charge.
			return Result{}, fmt.Errorf("%w: %v", ErrPaymentPending, err)
		}
		// From here the money has moved (or been declined). Finish recording
		// it even if the client has hung up: WithoutCancel keeps the values
		// of ctx but drops its cancellation, and the timeout bounds the work.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.finishPayment(ctx, reservationID, approved); err != nil {
			return Result{}, err
		}
	}
	return s.confirmResponse(ctx, reservationID, userID, key, hash)
}

// startPayment is tx1. It reports whether the provider must be called.
func (s *Service) startPayment(ctx context.Context, id int64, userID string) (needCharge bool, amount int64, err error) {
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var owner, status string
		var live bool
		var qty int
		var price int64
		var payStatus *string
		// FOR UPDATE OF r serialises concurrent confirms of this reservation
		// (and, in milestone 5, the expiry sweeper) on its row only.
		err := tx.QueryRow(ctx, `
			SELECT r.user_id, r.status, r.expires_at > now(), r.quantity, d.price_minor
			FROM reservations r
			JOIN drops d ON d.id = r.drop_id
			WHERE r.id = $1
			FOR UPDATE OF r`, id,
		).Scan(&owner, &status, &live, &qty, &price)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock reservation: %w", err)
		}
		// Read the payment in a SEPARATE statement, after the lock is held.
		// Under READ COMMITTED, a statement that waited for a row lock
		// re-reads only the locked row at its newest version; rows it joined
		// from other tables still come from the snapshot taken *before* the
		// wait. Joining payments above could therefore pair a fresh
		// "cancelled" reservation with a stale "pending" payment, a
		// combination that never existed. Found by TestConcurrentDeclined-
		// ConfirmReleasesOnce under -count=5 (see ADR-006).
		err = tx.QueryRow(ctx, `SELECT status FROM payments WHERE reservation_id = $1`, id).Scan(&payStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load payment: %w", err)
		}
		if owner != userID {
			return ErrNotFound // don't reveal other users' reservations
		}
		amount = int64(qty) * price

		switch {
		case status == "confirmed", status == "cancelled" && payStatus != nil && *payStatus == "failed":
			return nil // already final; just answer
		case status == "cancelled":
			return ErrNotHeld
		case status == "expired":
			return ErrHoldExpired
		case payStatus != nil && *payStatus == "pending":
			// An earlier attempt started paying and lost the answer. Resume
			// it even if the hold has since run out: the money may have moved.
			needCharge = true
			return nil
		case !live:
			return ErrHoldExpired
		}
		needCharge = true
		if _, err = tx.Exec(ctx, `
			INSERT INTO payments (reservation_id, idempotency_key, status, amount_minor)
			VALUES ($1, $2, 'pending', $3)
			ON CONFLICT (reservation_id) DO NOTHING`, id, paymentKey(id), amount); err != nil {
			return err
		}
		// Give the payment time to finish by pushing the hold's expiry
		// forward. This is a write to the reservation row *itself*, and that
		// is the point: a sweeper that took its snapshot before this commit
		// would not see the pending payment (another table, old snapshot),
		// but when it locks this row Postgres re-checks `expires_at <= now()`
		// against the row's newest version and skips it (ADR-008).
		_, err = tx.Exec(ctx, `
			UPDATE reservations
			SET expires_at = GREATEST(expires_at, now() + make_interval(secs => $2))
			WHERE id = $1`, id, paymentWindow.Seconds())
		return err
	})
	return needCharge, amount, err
}

// finishPayment is tx2. Each UPDATE only matches the expected current status,
// so running it twice (concurrent confirms) changes nothing the second time,
// and a declined hold returns its stock exactly once.
func (s *Service) finishPayment(ctx context.Context, id int64, approved bool) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if approved {
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET status = 'succeeded', updated_at = now()
				WHERE reservation_id = $1 AND status = 'pending'`, id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE reservations SET status = 'confirmed', updated_at = now()
				WHERE id = $1 AND status = 'held'`, id)
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE payments SET status = 'failed', updated_at = now()
			WHERE reservation_id = $1 AND status = 'pending'`, id); err != nil {
			return err
		}
		var dropID int64
		var qty int
		err := tx.QueryRow(ctx, `
			UPDATE reservations SET status = 'cancelled', updated_at = now()
			WHERE id = $1 AND status = 'held'
			RETURNING drop_id, quantity`, id).Scan(&dropID, &qty)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another attempt already released it
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE drops SET remaining = remaining + $2 WHERE id = $1`, dropID, qty)
		return err
	})
}

// confirmResponse builds the answer from the reservation's final state and
// stores it under the client's key. If a concurrent request with the same key
// stored first, its (identical) response is replayed instead.
func (s *Service) confirmResponse(ctx context.Context, id int64, userID, key string, hash []byte) (Result, error) {
	var res Result
	r := &res.Reservation
	var payStatus *string
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.drop_id, r.user_id, r.quantity, r.status, r.expires_at, r.created_at, p.status
		FROM reservations r LEFT JOIN payments p ON p.reservation_id = r.id
		WHERE r.id = $1`, id,
	).Scan(&r.ID, &r.DropID, &r.UserID, &r.Quantity, &r.Status, &r.ExpiresAt, &r.CreatedAt, &payStatus)
	if err != nil {
		return Result{}, fmt.Errorf("load reservation: %w", err)
	}

	switch {
	case r.Status == "confirmed":
		res.Status = http.StatusOK
		res.Body, err = json.Marshal(r)
	case r.Status == "cancelled" && payStatus != nil && *payStatus == "failed":
		res.Status = http.StatusPaymentRequired
		res.Body, err = json.Marshal(map[string]string{
			"error":   "payment_failed",
			"message": "payment was declined; the hold was released",
		})
	default:
		return Result{}, fmt.Errorf("reservation %d in unexpected state %q after payment", id, r.Status)
	}
	if err != nil {
		return Result{}, err
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys
		  (user_id, key, endpoint, request_hash, response_status, response_body, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (user_id, key) DO NOTHING`,
		userID, key, confirmEndpoint, hash, res.Status, res.Body)
	if err != nil {
		return Result{}, fmt.Errorf("store idempotent response: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.replayStored(ctx, userID, key, hash)
	}
	return res, nil
}
