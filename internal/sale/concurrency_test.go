package sale_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

const (
	buyers   = 1000
	stock    = 10
	poolSize = 50
)

// outcome counts how each racing buyer ended.
type outcome struct {
	ok, soldOut, perUser, other atomic.Int64
	firstOther                  atomic.Value // first unexpected error, for the failure message
}

// race releases n goroutines at the same instant against one drop.
// userOf(i) decides which user goroutine i buys as.
//
// The start barrier: every goroutine first signals `ready`, then blocks on
// `<-start`. Only when all n are parked do we close(start). A receive on a
// closed channel returns immediately for *every* waiter, so all n are
// released together instead of trickling in as they are spawned.
func race(t *testing.T, svc *sale.Service, dropID int64, n int, userOf func(i int) string) *outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var res outcome
	var ready, done sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			_, err := svc.Reserve(ctx, dropID, userOf(i), 1)
			switch {
			case err == nil:
				res.ok.Add(1)
			case errors.Is(err, sale.ErrSoldOut):
				res.soldOut.Add(1)
			case errors.Is(err, sale.ErrPerUserLimit):
				res.perUser.Add(1)
			default:
				res.other.Add(1)
				res.firstOther.CompareAndSwap(nil, err.Error())
			}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	if res.other.Load() > 0 {
		t.Errorf("%d unexpected errors, first: %v", res.other.Load(), res.firstOther.Load())
	}
	return &res
}

func newDrop(t *testing.T, pool *pgxpool.Pool, strategy string, total int) (*sale.Service, int64) {
	t.Helper()
	strat, err := sale.ParseStrategy(strategy)
	if err != nil {
		t.Fatal(err)
	}
	svc := sale.NewService(pool, strat, 5*time.Minute)
	d, err := svc.CreateDrop(context.Background(), sale.CreateDropInput{Name: t.Name(), TotalStock: total})
	if err != nil {
		t.Fatal(err)
	}
	return svc, d.ID
}

func check(t *testing.T, pool *pgxpool.Pool, dropID int64) invariants.Report {
	t.Helper()
	rep, err := invariants.Check(context.Background(), pool, dropID)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func distinctUsers(i int) string { return fmt.Sprintf("user-%d", i) }

// Strategy B must never oversell: exactly `stock` winners, everyone else
// gets a clean sold-out, and the DB passes every invariant.
func TestConcurrentAtomicNeverOversells(t *testing.T) {
	pool := testdb.New(t, poolSize)
	svc, dropID := newDrop(t, pool, "atomic", stock)

	res := race(t, svc, dropID, buyers, distinctUsers)

	if got := res.ok.Load(); got != stock {
		t.Errorf("successful holds = %d, want exactly %d", got, stock)
	}
	if got := res.soldOut.Load(); got != buyers-stock {
		t.Errorf("sold-out responses = %d, want %d", got, buyers-stock)
	}
	rep := check(t, pool, dropID)
	if !rep.OK() {
		t.Errorf("invariant violations: %+v", rep.Violations)
	}
	t.Logf("atomic: ok=%d sold_out=%d stats=%+v", res.ok.Load(), res.soldOut.Load(), rep.Stats)
}

// Strategy A is the intentionally broken baseline. This test asserts that it
// DOES oversell, which proves the race harness is strong enough to catch the
// bug. A lost update needs two transactions to interleave, so a single round
// could in principle come out clean; we allow a few rounds (flake-tolerant)
// and require at least one oversell.
func TestConcurrentNaiveOversells(t *testing.T) {
	pool := testdb.New(t, poolSize)
	const rounds = 5

	for round := 1; round <= rounds; round++ {
		svc, dropID := newDrop(t, pool, "naive", stock)
		res := race(t, svc, dropID, buyers, distinctUsers)
		rep := check(t, pool, dropID)
		t.Logf("naive round %d: ok=%d sold_out=%d oversold=%d stats=%+v",
			round, res.ok.Load(), res.soldOut.Load(), rep.Stats.Oversold(), rep.Stats)

		if rep.Stats.Oversold() > 0 {
			// The checker must flag it, or the referee itself is broken.
			if rep.OK() {
				t.Fatal("oversold but checker reported no violations")
			}
			return
		}
	}
	t.Fatalf("naive strategy did not oversell in %d rounds of %d buyers / %d units", rounds, buyers, stock)
}

// One user firing 50 identical requests at once: the partial UNIQUE index
// lets exactly one through, under BOTH strategies. The per-user limit lives in
// the schema, so even the broken strategy cannot violate it.
func TestConcurrentSameUserGetsOneHold(t *testing.T) {
	for _, strategy := range sale.StrategyNames() {
		t.Run(strategy, func(t *testing.T) {
			pool := testdb.New(t, poolSize)
			svc, dropID := newDrop(t, pool, strategy, stock)

			res := race(t, svc, dropID, 50, func(int) string { return "same-user" })

			if res.ok.Load() != 1 || res.perUser.Load() != 49 {
				t.Errorf("ok=%d per_user=%d, want 1 and 49", res.ok.Load(), res.perUser.Load())
			}
			if rep := check(t, pool, dropID); !rep.OK() {
				t.Errorf("invariant violations: %+v", rep.Violations)
			}
		})
	}
}

// Test plan C9: once stock is gone, a burst of 100 attempts all get a clean
// sold-out under both strategies, and remaining never goes below zero (the
// CHECK constraint would turn that into an error, which race() reports).
func TestConcurrentSoldOutIsConsistent(t *testing.T) {
	for _, strategy := range sale.StrategyNames() {
		t.Run(strategy, func(t *testing.T) {
			pool := testdb.New(t, poolSize)
			svc, dropID := newDrop(t, pool, strategy, 3)
			for i := 0; i < 3; i++ {
				if _, err := svc.Reserve(context.Background(), dropID, fmt.Sprintf("early-%d", i), 1); err != nil {
					t.Fatal(err)
				}
			}

			res := race(t, svc, dropID, 100, distinctUsers)

			if res.soldOut.Load() != 100 || res.ok.Load() != 0 {
				t.Errorf("ok=%d sold_out=%d, want 0 and 100", res.ok.Load(), res.soldOut.Load())
			}
			rep := check(t, pool, dropID)
			if !rep.OK() || rep.Stats.Remaining != 0 || rep.Stats.Held != 3 {
				t.Errorf("state %+v violations %+v", rep.Stats, rep.Violations)
			}
		})
	}
}
