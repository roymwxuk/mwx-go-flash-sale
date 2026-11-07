package sale

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const maxKeyLen = 255

// errKeyTaken is internal: another request committed this key first.
var errKeyTaken = errors.New("idempotency key already used")

func validateCaller(userID, key string) error {
	switch {
	case userID == "":
		return fmt.Errorf("%w: user id is required", ErrInvalidInput)
	case key == "":
		return fmt.Errorf("%w: Idempotency-Key is required", ErrInvalidInput)
	case len(key) > maxKeyLen:
		return fmt.Errorf("%w: Idempotency-Key longer than %d bytes", ErrInvalidInput, maxKeyLen)
	}
	return nil
}

// requestHash fingerprints the *meaning* of a request (endpoint template and
// its parameters), not the raw bytes, so `{}` and `{"quantity":1}` hash the
// same while a different drop, reservation or quantity does not.
func requestHash(endpoint string, params ...any) []byte {
	h := sha256.New()
	fmt.Fprint(h, endpoint)
	for _, p := range params {
		fmt.Fprintf(h, "|%v", p)
	}
	return h.Sum(nil)
}

type storedResponse struct {
	hash   []byte
	status *int
	body   []byte
}

// replay returns the stored response, or ErrIdempotencyMismatch if the key
// was first used for a different request.
func (s storedResponse) replay(hash []byte) (Result, error) {
	if !bytes.Equal(s.hash, hash) {
		return Result{}, ErrIdempotencyMismatch
	}
	if s.status == nil {
		// Only possible if a row was written outside this package: keys are
		// inserted and completed in the same transaction.
		return Result{}, errors.New("idempotency key has no stored response")
	}
	res := Result{Status: *s.status, Body: s.body, Replayed: true}
	_ = json.Unmarshal(s.body, &res.Reservation) // best effort; Body is what clients get
	return res, nil
}

// claimKey inserts the key inside the caller's transaction. If another
// transaction has inserted the same key but not committed yet, Postgres makes
// this INSERT wait on the primary key until that transaction ends:
//   - it commits → ON CONFLICT DO NOTHING, 0 rows → errKeyTaken (replay it)
//   - it rolls back → our INSERT succeeds and we do the work ourselves
//
// So concurrent duplicates are serialised by the database, without polling.
func claimKey(ctx context.Context, tx pgx.Tx, userID, key, endpoint string, hash []byte) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, key, endpoint, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, key) DO NOTHING`, userID, key, endpoint, hash)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errKeyTaken
	}
	return nil
}

func storeResponse(ctx context.Context, tx pgx.Tx, userID, key string, status int, body []byte) error {
	_, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET response_status = $3, response_body = $4, completed_at = now()
		WHERE user_id = $1 AND key = $2`, userID, key, status, body)
	if err != nil {
		return fmt.Errorf("store idempotent response: %w", err)
	}
	return nil
}

func (s *Service) replayStored(ctx context.Context, userID, key string, hash []byte) (Result, error) {
	var st storedResponse
	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, response_status, response_body
		FROM idempotency_keys WHERE user_id = $1 AND key = $2`, userID, key,
	).Scan(&st.hash, &st.status, &st.body)
	if err != nil {
		return Result{}, fmt.Errorf("load idempotency key: %w", err)
	}
	return st.replay(hash)
}
