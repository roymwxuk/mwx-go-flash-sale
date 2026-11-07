package sale_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/payment"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

// fireN runs fn n times at the same instant (same start barrier as race)
// and returns every result in index order.
func fireN(n int, fn func(i int) (sale.Result, error)) ([]sale.Result, []error) {
	results := make([]sale.Result, n)
	errs := make([]error, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newSvc(t *testing.T, pool *pgxpool.Pool, ttl time.Duration, gw payment.Gateway) *sale.Service {
	t.Helper()
	strat, _ := sale.ParseStrategy("atomic")
	return sale.NewService(pool, strat, ttl, gw)
}

func createDrop(t *testing.T, svc *sale.Service, stock, limit int) int64 {
	t.Helper()
	d, err := svc.CreateDrop(context.Background(), sale.CreateDropInput{Name: t.Name(), TotalStock: stock, PerUserLimit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}

// C3: 50 identical requests (same user, same key) at the same instant → one
// reservation, and all 50 callers get the byte-identical response.
func TestIdempotentReserveConcurrentSameKey(t *testing.T) {
	for _, strategy := range sale.StrategyNames() {
		t.Run(strategy, func(t *testing.T) {
			pool := testdb.New(t, poolSize)
			svc, dropID := newDrop(t, pool, strategy, stock)

			results, errs := fireN(50, func(int) (sale.Result, error) {
				return svc.Reserve(context.Background(), dropID, "u1", 1, "same-key")
			})

			replayed := 0
			for i, err := range errs {
				if err != nil {
					t.Fatalf("request %d: %v", i, err)
				}
				if results[i].Status != 201 || !bytes.Equal(results[i].Body, results[0].Body) {
					t.Fatalf("request %d: status %d body %s, want identical to %s", i, results[i].Status, results[i].Body, results[0].Body)
				}
				if results[i].Replayed {
					replayed++
				}
			}
			if replayed != 49 {
				t.Errorf("replayed = %d, want 49 (exactly one original)", replayed)
			}
			if n := countRows(t, pool, `SELECT count(*) FROM reservations WHERE drop_id = $1`, dropID); n != 1 {
				t.Errorf("reservations = %d, want 1", n)
			}
			if rep := check(t, pool, dropID); !rep.OK() || rep.Stats.Remaining != stock-1 {
				t.Errorf("state %+v violations %+v", rep.Stats, rep.Violations)
			}
		})
	}
}

// C4: a key may only ever mean one request. Reusing it for a different
// quantity, a different drop, or another endpoint is a 422 with no effect.
func TestIdempotencyKeyReuseWithDifferentRequest(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	svc := newSvc(t, pool, time.Minute, nil)
	dropA := createDrop(t, svc, 10, 2)
	dropB := createDrop(t, svc, 10, 2)

	first, err := svc.Reserve(ctx, dropA, "u1", 1, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, dropA, "u1", 2, "k"); !errors.Is(err, sale.ErrIdempotencyMismatch) {
		t.Errorf("different quantity: want mismatch, got %v", err)
	}
	if _, err := svc.Reserve(ctx, dropB, "u1", 1, "k"); !errors.Is(err, sale.ErrIdempotencyMismatch) {
		t.Errorf("different drop: want mismatch, got %v", err)
	}
	if _, err := svc.Confirm(ctx, first.Reservation.ID, "u1", "k"); !errors.Is(err, sale.ErrIdempotencyMismatch) {
		t.Errorf("other endpoint: want mismatch, got %v", err)
	}
	// Keys are per user: another user's "k" is an unrelated request.
	if _, err := svc.Reserve(ctx, dropA, "u2", 1, "k"); err != nil {
		t.Errorf("same key, other user: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM reservations WHERE user_id = 'u1'`); n != 1 {
		t.Errorf("u1 reservations = %d, want 1 (mismatches must have no effect)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments`); n != 0 {
		t.Errorf("payments = %d, want 0", n)
	}
}

// Design choice (ADR-005): rejections are not stored, because they had no
// effect. A sold-out answer under key K does not stick to K: if stock comes
// back, retrying K can succeed.
func TestRejectionsAreNotStored(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	svc := newSvc(t, pool, time.Minute, payment.NewMock(payment.MockConfig{FailureRate: 1}))
	dropID := createDrop(t, svc, 1, 1)

	held, err := svc.Reserve(ctx, dropID, "winner", 1, "w")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, dropID, "late", 1, "K"); !errors.Is(err, sale.ErrSoldOut) {
		t.Fatalf("want sold out, got %v", err)
	}
	// The winner's payment is declined, which releases the unit.
	if res, err := svc.Confirm(ctx, held.Reservation.ID, "winner", "c"); err != nil || res.Status != 402 {
		t.Fatalf("declined confirm: %+v %v", res, err)
	}
	res, err := svc.Reserve(ctx, dropID, "late", 1, "K")
	if err != nil || res.Status != 201 || res.Replayed {
		t.Fatalf("retry of K after stock returned: %+v %v", res, err)
	}
}

// C5: a declined payment releases the hold and its stock exactly once, and a
// retry with the same key replays the 402.
func TestPaymentFailureReleasesHold(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	gw := payment.NewMock(payment.MockConfig{FailureRate: 1})
	svc := newSvc(t, pool, time.Minute, gw)
	dropID := createDrop(t, svc, 3, 1)

	held, err := svc.Reserve(ctx, dropID, "u1", 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Confirm(ctx, held.Reservation.ID, "u1", "c")
	if err != nil || res.Status != 402 || !bytes.Contains(res.Body, []byte("payment_failed")) {
		t.Fatalf("confirm: %+v %v", res, err)
	}
	again, err := svc.Confirm(ctx, held.Reservation.ID, "u1", "c")
	if err != nil || !again.Replayed || !bytes.Equal(again.Body, res.Body) {
		t.Fatalf("replay: %+v %v", again, err)
	}

	rep := check(t, pool, dropID)
	if !rep.OK() || rep.Stats.Remaining != 3 || rep.Stats.Held != 0 {
		t.Errorf("after decline: %+v %+v (stock must be fully back)", rep.Stats, rep.Violations)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments WHERE status = 'failed'`); n != 1 {
		t.Errorf("failed payments = %d, want 1", n)
	}
	if gw.TotalCharged() != 0 {
		t.Errorf("charged %d times for a declined payment", gw.TotalCharged())
	}
	// The user may try again with a fresh hold.
	if _, err := svc.Reserve(ctx, dropID, "u1", 1, "r2"); err != nil {
		t.Errorf("new hold after decline: %v", err)
	}
}

// C6: 20 confirms with the same key plus 20 with different keys, all at
// once, while the provider is slow. Exactly one payment row, one charge, and
// every caller sees the confirmed reservation.
func TestConcurrentConfirmChargesOnce(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 50)
	gw := payment.NewMock(payment.MockConfig{MinLatency: 50 * time.Millisecond, MaxLatency: 100 * time.Millisecond})
	svc := newSvc(t, pool, time.Minute, gw)
	dropID := createDrop(t, svc, 5, 1)
	held, err := svc.Reserve(ctx, dropID, "u1", 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	id := held.Reservation.ID

	results, errs := fireN(40, func(i int) (sale.Result, error) {
		key := "same"
		if i >= 20 {
			key = fmt.Sprintf("dup-%d", i) // double-click with a new key
		}
		return svc.Confirm(ctx, id, "u1", key)
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("confirm %d: %v", i, err)
		}
		if results[i].Status != 200 || results[i].Reservation.Status != "confirmed" {
			t.Fatalf("confirm %d: %+v", i, results[i])
		}
		if i < 20 && !bytes.Equal(results[i].Body, results[0].Body) {
			t.Fatalf("same-key response %d differs", i)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments WHERE reservation_id = $1`, id); n != 1 {
		t.Errorf("payment rows = %d, want 1", n)
	}
	if gw.Charges("res-"+itoa(id)) != 1 || gw.TotalCharged() != 1 {
		t.Errorf("charges = %d (total %d), want exactly 1", gw.Charges("res-"+itoa(id)), gw.TotalCharged())
	}
	if rep := check(t, pool, dropID); !rep.OK() || rep.Stats.Confirmed != 1 {
		t.Errorf("state %+v violations %+v", rep.Stats, rep.Violations)
	}
}

// C6 (second half): the client gives up mid-payment, after the provider has
// already charged. The payment stays pending; the retry with the same key
// finishes it without a second charge.
func TestConfirmRetryAfterTimeoutDoesNotDoubleCharge(t *testing.T) {
	pool := testdb.New(t, 4)
	gw := payment.NewMock(payment.MockConfig{MinLatency: 300 * time.Millisecond})
	svc := newSvc(t, pool, time.Minute, gw)
	dropID := createDrop(t, svc, 5, 1)
	held, err := svc.Reserve(context.Background(), dropID, "u1", 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	id := held.Reservation.ID

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := svc.Confirm(short, id, "u1", "c"); !errors.Is(err, sale.ErrPaymentPending) {
		t.Fatalf("timed-out confirm: want ErrPaymentPending, got %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments WHERE status = 'pending'`); n != 1 {
		t.Fatalf("pending payments = %d, want 1", n)
	}

	res, err := svc.Confirm(context.Background(), id, "u1", "c")
	if err != nil || res.Status != 200 || res.Replayed {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if gw.TotalCharged() != 1 || gw.Calls("res-"+itoa(id)) != 2 {
		t.Errorf("charged %d times over %d calls, want 1 charge over 2 calls", gw.TotalCharged(), gw.Calls("res-"+itoa(id)))
	}
	if rep := check(t, pool, dropID); !rep.OK() {
		t.Errorf("violations %+v", rep.Violations)
	}
}

// Concurrent confirms of a reservation whose payment is declined: the unit
// must come back exactly once, not once per caller.
func TestConcurrentDeclinedConfirmReleasesOnce(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 30)
	svc := newSvc(t, pool, time.Minute, payment.NewMock(payment.MockConfig{FailureRate: 1, MinLatency: 20 * time.Millisecond}))
	dropID := createDrop(t, svc, 2, 1)
	held, err := svc.Reserve(ctx, dropID, "u1", 1, "r")
	if err != nil {
		t.Fatal(err)
	}

	results, errs := fireN(20, func(i int) (sale.Result, error) {
		return svc.Confirm(ctx, held.Reservation.ID, "u1", "k-"+itoa(int64(i)))
	})
	for i := range errs {
		if errs[i] != nil || results[i].Status != 402 {
			t.Fatalf("confirm %d: %+v %v", i, results[i], errs[i])
		}
	}
	if rep := check(t, pool, dropID); !rep.OK() || rep.Stats.Remaining != 2 {
		t.Errorf("remaining %d (want 2: released exactly once), violations %+v", rep.Stats.Remaining, rep.Violations)
	}
}

// C8: confirming after the hold ran out is rejected and charges nothing.
func TestConfirmAfterExpiryIsRejected(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	gw := payment.NewMock(payment.MockConfig{})
	svc := newSvc(t, pool, 50*time.Millisecond, gw)
	dropID := createDrop(t, svc, 5, 1)
	held, err := svc.Reserve(ctx, dropID, "u1", 1, "r")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	if _, err := svc.Confirm(ctx, held.Reservation.ID, "u1", "c"); !errors.Is(err, sale.ErrHoldExpired) {
		t.Fatalf("want ErrHoldExpired, got %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payments`); n != 0 || gw.TotalCharged() != 0 {
		t.Errorf("payments=%d charged=%d, want 0/0", n, gw.TotalCharged())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
