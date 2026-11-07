package loadgen_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/httpapi"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/loadgen"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/payment"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/testdb"
)

func TestSummarizeNearestRank(t *testing.T) {
	var s []time.Duration
	for i := 100; i >= 1; i-- { // 1..100 ms, deliberately unsorted
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	got := loadgen.Summarize(s)
	want := loadgen.Latency{P50: 50, P95: 95, P99: 99, Max: 100, Mean: 50.5}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if s[0] != 100*time.Millisecond {
		t.Fatal("Summarize must not reorder the caller's slice")
	}
	if (loadgen.Summarize(nil) != loadgen.Latency{}) {
		t.Fatal("empty input should give zero latency")
	}
}

func TestMarkdownIncludesRunsAndViolations(t *testing.T) {
	rep := loadgen.Report{
		StartedAt: time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC),
		Command:   "loadgen -buyers 10",
		Machine:   loadgen.Machine{CPU: "TestCPU", Cores: 8, RAM: "16 GB", OS: "testOS", Go: "go1.x", Postgres: "16"},
		Runs: []loadgen.Result{{
			Strategy: "naive", PoolSize: 20, Buyers: 10, Stock: 2, Outcomes: loadgen.Outcomes{Created: 5},
			Invariants: &invariants.Report{
				Stats:      invariants.Stats{TotalStock: 2, Held: 5},
				Violations: []invariants.Violation{{Invariant: "no_oversell", Detail: "x"}},
			},
		}},
	}
	md := loadgen.Markdown(rep)
	for _, want := range []string{"10 buyers vs 2 units", "TestCPU (8 cores)", "| naive | 20 | 5 | 3 | 1 |", "`no_oversell`: x"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

// End to end through real HTTP and real Postgres, at a size that runs in CI.
func TestRunAgainstRealServer(t *testing.T) {
	pool := testdb.New(t, 20)
	strat, _ := sale.ParseStrategy("atomic")
	svc := sale.NewService(pool, strat, time.Minute, nil)
	srv := httptest.NewServer(httpapi.New(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Second).Handler())
	defer srv.Close()

	const buyers, stock = 300, 7
	res, err := loadgen.Run(context.Background(), loadgen.Config{
		BaseURL: srv.URL, Buyers: buyers, Stock: stock, RunID: "t",
		ClientTimeout: 30 * time.Second, DialConcurrency: 32,
		Sampler: func(ctx context.Context, dropID int64) (int, error) {
			var n int
			err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(quantity),0) FROM reservations
				WHERE drop_id = $1 AND status IN ('held','confirmed')`, dropID).Scan(&n)
			return n, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != stock || res.SoldOut != buyers-stock || !res.H1() || res.Errors() != 0 {
		t.Fatalf("unexpected outcome: %+v", res)
	}
	if res.Latency.P50 <= 0 || res.LatencyCreated.P50 <= 0 || res.ThroughputRPS <= 0 || res.SellOutMs <= 0 {
		t.Fatalf("metrics not populated: %+v", res)
	}
	// The final sample always runs after the burst, so the peak is the stock.
	if res.Samples < 1 || res.PeakActive != stock || res.SampledOversell != 0 {
		t.Fatalf("mid-run sampling: samples=%d peak=%d", res.Samples, res.PeakActive)
	}
	if res.PeakInFlight < 1 || res.PeakInFlight > buyers {
		t.Fatalf("peak in-flight = %d", res.PeakInFlight)
	}
	rep, err := invariants.Check(context.Background(), pool, res.DropID)
	if err != nil || !rep.OK() {
		t.Fatalf("invariants: %+v, %v", rep, err)
	}
}

// L5 at a tiny scale: two low-rate steps that any machine passes.
func TestRunCeilingAgainstRealServer(t *testing.T) {
	pool := testdb.New(t, 10)
	strat, _ := sale.ParseStrategy("atomic")
	svc := sale.NewService(pool, strat, time.Minute, nil)
	srv := httptest.NewServer(httpapi.New(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second).Handler())
	defer srv.Close()

	res, err := loadgen.RunCeiling(context.Background(), loadgen.CeilingConfig{
		BaseURL: srv.URL, RunID: "c", Rates: []int{50, 100}, StepDuration: time.Second,
		P99Target: 2 * time.Second, MaxErrorRate: 0.01, StopOnFail: true,
		ClientTimeout: 10 * time.Second, MaxConns: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.CeilingRPS != 100 || !res.H1() {
		t.Fatalf("unexpected ceiling result: %+v", res)
	}
	for _, s := range res.Steps {
		// Open-loop schedule: exactly rate × duration requests, all 201.
		if s.Requests != s.TargetRPS || s.Created != s.Requests {
			t.Errorf("step %d: requests=%d created=%d", s.TargetRPS, s.Requests, s.Created)
		}
	}
	rep, err := invariants.Check(context.Background(), pool, res.DropID)
	if err != nil || !rep.OK() {
		t.Fatalf("invariants: %+v, %v", rep, err)
	}
	if rep.Stats.Held != 150 {
		t.Fatalf("held = %d, want 150", rep.Stats.Held)
	}
}

// Messy clients end to end: double clicks, same-key retries, winners paying
// with 30% declined. No key may produce two different effects, H1 holds for
// every request, and the DB passes every invariant.
func TestRunMessyClients(t *testing.T) {
	pool := testdb.New(t, 20)
	strat, _ := sale.ParseStrategy("atomic")
	gw := payment.NewMock(payment.MockConfig{FailureRate: 0.3, Seed: 7, MaxLatency: 20 * time.Millisecond})
	svc := sale.NewService(pool, strat, time.Minute, gw)
	srv := httptest.NewServer(httpapi.New(pool, svc, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Second).Handler())
	defer srv.Close()

	const buyers, stock = 300, 20
	res, err := loadgen.Run(context.Background(), loadgen.Config{
		BaseURL: srv.URL, Buyers: buyers, Stock: stock, RunID: "m",
		ClientTimeout: 30 * time.Second, DialConcurrency: 32,
		DupRatio: 0.2, RetryRatio: 0.3, Confirm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicates == 0 || res.Retries == 0 || res.Confirm.Requests < stock {
		t.Fatalf("messy traffic not generated: dup=%d retries=%d confirms=%d", res.Duplicates, res.Retries, res.Confirm.Requests)
	}
	if res.KeyViolations != 0 || !res.H1() || res.Created != stock {
		t.Fatalf("key_violations=%d H1=%v created=%d: %+v", res.KeyViolations, res.H1(), res.Created, res)
	}
	if res.Confirm.Declined == 0 || res.Confirm.Confirmed == 0 {
		t.Fatalf("want both confirmed and declined payments: %+v", res.Confirm)
	}
	rep, err := invariants.Check(context.Background(), pool, res.DropID)
	if err != nil || !rep.OK() {
		t.Fatalf("invariants: %+v, %v", rep, err)
	}
	// Each winner paid at most once, whatever the retries.
	if gw.TotalCharged() != rep.Stats.Confirmed {
		t.Fatalf("provider charged %d times for %d confirmed reservations", gw.TotalCharged(), rep.Stats.Confirmed)
	}
}
