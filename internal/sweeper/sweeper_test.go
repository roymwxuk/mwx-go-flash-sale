package sweeper_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/payment"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sweeper"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newService(t *testing.T, pool *pgxpool.Pool, ttl time.Duration, gw payment.Gateway) *sale.Service {
	t.Helper()
	strat, _ := sale.ParseStrategy("atomic")
	return sale.NewService(pool, strat, ttl, gw)
}

func newDrop(t *testing.T, svc *sale.Service, stock int) int64 {
	t.Helper()
	d, err := svc.CreateDrop(context.Background(), sale.CreateDropInput{Name: t.Name(), TotalStock: stock})
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func hold(t *testing.T, svc *sale.Service, dropID int64, user string) int64 {
	t.Helper()
	r, err := svc.Reserve(context.Background(), dropID, user, 1, user+"-k")
	if err != nil {
		t.Fatal(err)
	}
	return r.Reservation.ID
}

func mustCheck(t *testing.T, pool *pgxpool.Pool, dropID int64) invariants.Report {
	t.Helper()
	rep, err := invariants.Check(context.Background(), pool, dropID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("invariant violations: %+v", rep.Violations)
	}
	return rep
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// C7: 200 expired holds, three sweepers started at the same instant with
// small batches so they constantly interleave. Every hold is released exactly
// once: the sweepers' counts add up to 200 (nobody double-counted), the stock
// comes back exactly, and the 50 live holds are untouched.
func TestConcurrentSweepersReleaseEachHoldOnce(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 20)
	shortSvc := newService(t, pool, 50*time.Millisecond, nil)
	longSvc := newService(t, pool, time.Hour, nil)
	dropID := newDrop(t, shortSvc, 300)

	for i := 0; i < 200; i++ {
		hold(t, shortSvc, dropID, fmt.Sprintf("exp-%d", i))
	}
	for i := 0; i < 50; i++ {
		hold(t, longSvc, dropID, fmt.Sprintf("live-%d", i))
	}
	time.Sleep(100 * time.Millisecond) // let the short holds expire

	const sweepers = 3
	released := make([]int, sweepers)
	units := make([]int, sweepers)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < sweepers; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			sw := sweeper.New(pool, sweeper.Config{BatchSize: 7}, quiet)
			ready.Done()
			<-start
			// SweepOnce can stop early when the other sweepers hold the
			// remaining rows (SKIP LOCKED), so loop until a pass finds nothing.
			for {
				st, err := sw.SweepOnce(ctx)
				if err != nil {
					t.Errorf("sweeper %d: %v", i, err)
					return
				}
				released[i] += st.Released
				units[i] += st.Units
				if st.Released == 0 {
					return
				}
			}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	total, totalUnits := 0, 0
	for i := range released {
		total += released[i]
		totalUnits += units[i]
	}
	t.Logf("released per sweeper: %v", released)
	if total != 200 || totalUnits != 200 {
		t.Fatalf("released %d holds / %d units across sweepers, want exactly 200 / 200", total, totalUnits)
	}
	if n := count(t, pool, `SELECT count(*) FROM reservations WHERE drop_id = $1 AND status = 'expired'`, dropID); n != 200 {
		t.Errorf("expired rows = %d, want 200", n)
	}
	rep := mustCheck(t, pool, dropID)
	if rep.Stats.Held != 50 || rep.Stats.Remaining != 250 {
		t.Errorf("held %d remaining %d, want 50 / 250", rep.Stats.Held, rep.Stats.Remaining)
	}
	// A second pass finds nothing: release cannot happen twice.
	if st, _ := sweeper.New(pool, sweeper.Config{}, quiet).SweepOnce(ctx); st.Released != 0 {
		t.Errorf("second pass released %d", st.Released)
	}
}

// A hold whose payment is pending is never expired by the sweeper, even long
// after its expiry: the money may already have moved. The confirm retry
// finishes it instead.
func TestSweeperSkipsPendingPayment(t *testing.T) {
	pool := testdb.New(t, 4)
	gw := payment.NewMock(payment.MockConfig{MinLatency: 300 * time.Millisecond})
	svc := newService(t, pool, time.Minute, gw)
	dropID := newDrop(t, svc, 5)
	id := hold(t, svc, dropID, "u1")

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := svc.Confirm(short, id, "u1", "c"); !errors.Is(err, sale.ErrPaymentPending) {
		t.Fatalf("want ErrPaymentPending, got %v", err)
	}
	// Pretend the payment window has long passed.
	if _, err := pool.Exec(context.Background(), `UPDATE reservations SET expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	st, err := sweeper.New(pool, sweeper.Config{}, quiet).SweepOnce(context.Background())
	if err != nil || st.Released != 0 {
		t.Fatalf("sweep: %+v %v; a pending payment must not be expired", st, err)
	}
	res, err := svc.Confirm(context.Background(), id, "u1", "c")
	if err != nil || res.Status != 200 {
		t.Fatalf("confirm retry: %+v %v", res, err)
	}
	mustCheck(t, pool, dropID)
}

// Confirm and sweeper race on holds that expire *while* buyers are paying.
// Three sweepers poll every 2 ms; 150 confirms start at random moments
// around each hold's expiry, with a slow provider. Whatever the interleaving,
// a buyer either pays and keeps the item or gets 410 and pays nothing:
// never "charged but expired" (payment_consistency) and never a lost unit.
func TestConfirmRacingSweeper(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 40)
	gw := payment.NewMock(payment.MockConfig{MinLatency: 20 * time.Millisecond, MaxLatency: 80 * time.Millisecond})
	// TTL comfortably exceeds the time to create all holds (~0.3 s under
	// -race), so every "60 ms before expiry" moment is still in the future
	// when the confirm goroutines start.
	const n, ttl = 150, 1500 * time.Millisecond
	svc := newService(t, pool, ttl, gw)
	dropID := newDrop(t, svc, n)

	ids := make([]int64, n)
	expires := make([]time.Time, n)
	for i := range ids {
		r, err := svc.Reserve(ctx, dropID, fmt.Sprintf("u%d", i), 1, fmt.Sprintf("u%d-k", i))
		if err != nil {
			t.Fatal(err)
		}
		ids[i], expires[i] = r.Reservation.ID, r.Reservation.ExpiresAt
	}
	// expires_at comes from the DB clock (a VM on macOS), while the confirms
	// below are scheduled with this process's clock. Measure the offset so
	// "around expiry" means the same moment on both clocks.
	skew := dbClockSkew(t, pool)
	t.Logf("db clock - host clock = %v", skew)

	sweepCtx, stopSweepers := context.WithCancel(ctx)
	var sweepWG sync.WaitGroup
	for i := 0; i < 3; i++ {
		sweepWG.Add(1)
		go func() {
			defer sweepWG.Done()
			sweeper.New(pool, sweeper.Config{Interval: 2 * time.Millisecond, BatchSize: 5}, quiet).Run(sweepCtx)
		}()
	}

	var mu sync.Mutex
	outcomes := map[string]int{}
	byTiming := map[string]int{}
	maxTook := map[string]int64{}
	var wg sync.WaitGroup
	rng := rand.New(rand.NewPCG(1, 2))
	for i, id := range ids {
		// Fire between 60 ms before and 60 ms after this hold's expiry.
		at := expires[i].Add(-skew - 60*time.Millisecond + time.Duration(rng.Int64N(int64(120*time.Millisecond))))
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			time.Sleep(time.Until(at))
			sentAt := time.Now()
			res, err := svc.Confirm(ctx, id, fmt.Sprintf("u%d", i), "c")
			tookMs := time.Since(sentAt).Milliseconds()
			key := ""
			switch {
			case err == nil:
				key = fmt.Sprint(res.Status)
			case errors.Is(err, sale.ErrHoldExpired):
				key = "410"
			default:
				key = "error: " + err.Error()
			}
			when := "after"
			if sentAt.Add(skew).Before(expires[i]) {
				when = "before"
			}
			mu.Lock()
			outcomes[key]++
			byTiming[when+" expiry → "+key]++
			if tookMs > maxTook[key] {
				maxTook[key] = tookMs
			}
			mu.Unlock()
		}(i, id)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond) // let the sweepers finish anything left
	stopSweepers()
	sweepWG.Wait()

	t.Logf("outcomes: %v; by timing: %v; slowest confirm ms: %v", outcomes, byTiming, maxTook)
	for k, v := range outcomes {
		if k != "200" && k != "410" {
			t.Errorf("%d confirms ended with %q", v, k)
		}
	}
	rep := mustCheck(t, pool, dropID) // includes payment_consistency
	if rep.Stats.Confirmed != outcomes["200"] || rep.Stats.Held != 0 {
		t.Errorf("confirmed %d (want %d), held %d (want 0)", rep.Stats.Confirmed, outcomes["200"], rep.Stats.Held)
	}
	if gw.TotalCharged() != rep.Stats.Confirmed {
		t.Errorf("charged %d times for %d confirmed reservations", gw.TotalCharged(), rep.Stats.Confirmed)
	}
	if outcomes["200"] == 0 || outcomes["410"] == 0 {
		t.Errorf("race not exercised: want both confirmed and expired outcomes, got %v", outcomes)
	}
}

// dbClockSkew estimates (DB clock - host clock), using the midpoint of the
// round trip and the fastest of several tries to cancel network delay.
func dbClockSkew(t *testing.T, pool *pgxpool.Pool) time.Duration {
	t.Helper()
	best := time.Duration(1<<62 - 1)
	var skew time.Duration
	for i := 0; i < 5; i++ {
		before := time.Now()
		var dbNow time.Time
		if err := pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
			t.Fatal(err)
		}
		rtt := time.Since(before)
		if rtt < best {
			best = rtt
			skew = dbNow.Sub(before.Add(rtt / 2))
		}
	}
	return skew
}

// Regression: one drop whose stock cannot be returned (here: corrupted so the
// refund would exceed total_stock, as after a naive-strategy oversell) must
// not stop the sweeper for every other drop.
func TestPoisonedDropDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	svc := newService(t, pool, 20*time.Millisecond, nil)
	bad := newDrop(t, svc, 5)
	good := newDrop(t, svc, 5)
	hold(t, svc, bad, "b1")
	hold(t, svc, good, "g1")
	hold(t, svc, good, "g2")
	// Corrupt the bad drop: its stock is already "full", so refunding the
	// expired hold would push remaining past total_stock.
	if _, err := pool.Exec(ctx, `UPDATE drops SET remaining = total_stock WHERE id = $1`, bad); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)

	st, err := sweeper.New(pool, sweeper.Config{}, quiet).SweepOnce(ctx)
	if err == nil || st.FailedDrops != 1 {
		t.Fatalf("want the bad drop reported (err %v, failed %d)", err, st.FailedDrops)
	}
	if st.Released != 2 {
		t.Fatalf("released %d, want the good drop's 2 holds released anyway", st.Released)
	}
	mustCheck(t, pool, good)
	if n := count(t, pool, `SELECT count(*) FROM reservations WHERE drop_id = $1 AND status = 'held'`, bad); n != 1 {
		t.Errorf("bad drop's hold must stay untouched (held=%d)", n)
	}
}

func TestSweeperDeletesOldIdempotencyKeys(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, 4)
	if _, err := pool.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, key, endpoint, request_hash, created_at) VALUES
		('u', 'old1', 'e', 'h', now() - interval '25 hours'),
		('u', 'old2', 'e', 'h', now() - interval '48 hours'),
		('u', 'new',  'e', 'h', now() - interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	st, err := sweeper.New(pool, sweeper.Config{KeyTTL: 24 * time.Hour, BatchSize: 1}, quiet).SweepOnce(ctx)
	if err != nil || st.KeysDeleted != 2 {
		t.Fatalf("deleted %d, %v; want 2", st.KeysDeleted, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM idempotency_keys WHERE key = 'new'`); n != 1 {
		t.Error("recent key must be kept")
	}
}

// Run must return promptly once its context is cancelled, so the API's
// graceful shutdown can wait for it before closing the pool.
func TestRunStopsOnCancel(t *testing.T) {
	pool := testdb.New(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sweeper.New(pool, sweeper.Config{Interval: time.Hour}, quiet).Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
