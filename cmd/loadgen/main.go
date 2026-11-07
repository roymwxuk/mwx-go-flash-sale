// Command loadgen starts the API with a chosen stock strategy and pool size,
// drives it over HTTP, verifies invariants directly in Postgres, and records
// the results. Scenarios follow docs/test-plan.md:
//
//	loadgen -scenario burst   -strategies naive,atomic -buyers 10000 -stock 100   # L1
//	loadgen -scenario ceiling -strategies atomic -rates 500,1000,2000,4000,8000   # L5
//
// Exit status is 1 if H1 fails for any strategy (a non-503 5xx, a hung
// request, or a transport error), or if any strategy other than "naive"
// violates an invariant. Naive is the intentionally broken baseline and is
// expected to oversell.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/db"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/loadgen"
)

type options struct {
	scenario      string
	apiBin        string
	databaseURL   string
	strategies    []string
	pool          int
	repeat        int
	serverTimeout time.Duration
	clientTimeout time.Duration
	resultsDir    string
	resultsMD     string
	pauseBetween  time.Duration

	// burst (L1)
	buyers, stock int
	dialConc      int
	dupRatio      float64
	retryRatio    float64
	confirm       bool

	// mock payment provider inside the API
	paymentFailureRate float64
	paymentMinLatency  time.Duration
	paymentMaxLatency  time.Duration

	// ceiling (L5)
	rates        []int
	stepDuration time.Duration
	p99Target    time.Duration
	maxErrorRate float64
	maxConns     int
	keepGoing    bool
}

func main() {
	var o options
	var strategies, rates string
	flag.StringVar(&o.scenario, "scenario", "burst", "burst (L1: everyone at once) or ceiling (L5: stepped constant rate)")
	flag.StringVar(&o.apiBin, "api-bin", "bin/api", "path to the API binary to start for each run")
	flag.StringVar(&o.databaseURL, "database-url", envOr("DATABASE_URL", "postgres://flashsale:flashsale@localhost:5432/flashsale?sslmode=disable"), "Postgres DSN (API and invariant checker)")
	flag.StringVar(&strategies, "strategies", "", "comma-separated stock strategies (default: naive,atomic for burst; atomic for ceiling)")
	flag.IntVar(&o.pool, "pool", 20, "API pgxpool MaxConns (DB_MAX_CONNS)")
	flag.IntVar(&o.repeat, "repeat", 1, "runs per strategy")
	flag.DurationVar(&o.serverTimeout, "server-timeout", 5*time.Second, "API REQUEST_TIMEOUT (pool wait included); exceeded → 503")
	flag.DurationVar(&o.clientTimeout, "client-timeout", 30*time.Second, "per-request HTTP timeout; a request slower than this +1s counts as hung (H1)")
	flag.StringVar(&o.resultsDir, "results-dir", "results", "directory for raw JSON results")
	flag.StringVar(&o.resultsMD, "results-md", "docs/results.md", "markdown file to append a summary to (empty: don't append)")
	flag.DurationVar(&o.pauseBetween, "pause", 3*time.Second, "pause between runs")

	flag.IntVar(&o.buyers, "buyers", 10000, "burst: number of simultaneous virtual buyers")
	flag.IntVar(&o.stock, "stock", 100, "burst: units in the drop")
	flag.IntVar(&o.dialConc, "dial-concurrency", 64, "burst: max simultaneous TCP connects during setup")
	flag.Float64Var(&o.dupRatio, "dup-ratio", 0, "burst: share of buyers who double-click (same key, own connection, within 50 ms)")
	flag.Float64Var(&o.retryRatio, "retry-ratio", 0, "burst: share of buyers who resend with the same key after their answer")
	flag.BoolVar(&o.confirm, "confirm", false, "burst: winners confirm (pay) their hold")
	flag.Float64Var(&o.paymentFailureRate, "payment-failure-rate", 0, "API mock payment: share of payments declined (holds released)")
	flag.DurationVar(&o.paymentMinLatency, "payment-min-latency", 100*time.Millisecond, "API mock payment: min latency")
	flag.DurationVar(&o.paymentMaxLatency, "payment-max-latency", 300*time.Millisecond, "API mock payment: max latency")

	flag.StringVar(&rates, "rates", "500,1000,2000,4000,8000", "ceiling: target req/s per step")
	flag.DurationVar(&o.stepDuration, "step", 20*time.Second, "ceiling: duration of each step")
	flag.DurationVar(&o.p99Target, "p99-target", 500*time.Millisecond, "ceiling: a step passes only if p99 is at or below this")
	flag.Float64Var(&o.maxErrorRate, "max-error-rate", 0.01, "ceiling: a step passes only if the error rate is below this")
	flag.IntVar(&o.maxConns, "max-conns", 2000, "ceiling: client connection cap (excess requests queue in the client, and that wait counts as latency)")
	flag.BoolVar(&o.keepGoing, "keep-going", false, "ceiling: run higher steps even after one fails")
	flag.Parse()

	switch {
	case strategies != "":
		o.strategies = strings.Split(strategies, ",")
	case o.scenario == "ceiling":
		o.strategies = []string{"atomic"}
	default:
		o.strategies = []string{"naive", "atomic"}
	}
	for _, r := range strings.Split(rates, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(r))
		if err != nil || n < 1 {
			log.Fatalf("bad -rates value %q", r)
		}
		o.rates = append(o.rates, n)
	}
	if o.scenario != "burst" && o.scenario != "ceiling" {
		log.Fatalf("unknown -scenario %q", o.scenario)
	}

	log.SetFlags(log.Ltime)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed, err := run(ctx, o)
	if err != nil {
		log.Fatal(err)
	}
	if failed {
		log.Println("FAIL: H1 failed, or invariant violations in a strategy that must be correct")
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) (failed bool, err error) {
	pool, err := db.NewPool(ctx, o.databaseURL, 4)
	if err != nil {
		return false, fmt.Errorf("connect to Postgres (did you run `make up`?): %w", err)
	}
	defer pool.Close()
	// Migrate here too, so the API's own start-up migration is a no-op and
	// does not count towards the first run.
	if err := db.Migrate(ctx, pool); err != nil {
		return false, err
	}
	var pgVersion string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion); err != nil {
		return false, err
	}

	rep := loadgen.Report{
		StartedAt: time.Now(),
		Scenario:  o.scenario,
		Commit:    loadgen.GitCommit(ctx),
		Command:   "loadgen " + strings.Join(os.Args[1:], " "),
		Machine:   loadgen.DetectMachine(ctx, pgVersion),
	}
	if len(os.Args) == 1 {
		rep.Command = "loadgen (all defaults)"
	}
	log.Printf("machine: %s · commit %s", rep.Machine, rep.Commit)

	for i, strategy := range o.strategies {
		for r := 1; r <= o.repeat; r++ {
			if i > 0 || r > 1 {
				time.Sleep(o.pauseBetween)
			}
			runID := fmt.Sprintf("%s-%s-%d", rep.StartedAt.Format("150405"), strategy, r)
			mustBeCorrect := strategy != "naive"

			if o.scenario == "burst" {
				res, err := runBurst(ctx, o, pool, strategy, runID)
				if err != nil {
					return false, fmt.Errorf("%s run %d: %w", strategy, r, err)
				}
				if !res.H1() || (mustBeCorrect && (res.Violations() > 0 || res.SampledOversell > 0 || res.KeyViolations > 0)) {
					failed = true
				}
				log.Printf("%-7s run %d: 201=%d oversold=%d violations=%d key_violations=%d 409=%d 503=%d errors=%d p50=%.1fms p95=%.1fms p99=%.1fms %.0f req/s peak_active=%d dup=%d retries=%d confirm(200=%d 402=%d 503=%d) H1=%v",
					strategy, r, res.Created, res.Oversold(), res.Violations(), res.KeyViolations, res.SoldOut, res.Unavailable,
					res.Errors()-res.Unavailable, res.Latency.P50, res.Latency.P95, res.Latency.P99,
					res.ThroughputRPS, res.PeakActive, res.Duplicates, res.Retries,
					res.Confirm.Confirmed, res.Confirm.Declined, res.Confirm.Unavailable, res.H1())
				rep.Runs = append(rep.Runs, res)
			} else {
				res, err := runCeiling(ctx, o, pool, strategy, runID)
				if err != nil {
					return false, fmt.Errorf("%s run %d: %w", strategy, r, err)
				}
				if !res.H1() || (mustBeCorrect && !res.Invariants.OK()) {
					failed = true
				}
				log.Printf("%-7s run %d: ceiling=%d req/s H1=%v violations=%d",
					strategy, r, res.CeilingRPS, res.H1(), len(res.Invariants.Violations))
				rep.Ceilings = append(rep.Ceilings, res)
			}
		}
	}

	path, err := loadgen.WriteJSON(o.resultsDir, rep)
	if err != nil {
		return failed, err
	}
	log.Printf("raw results: %s", path)
	if o.resultsMD != "" {
		if err := loadgen.AppendMarkdown(o.resultsMD, rep); err != nil {
			return failed, err
		}
		log.Printf("appended summary to %s", o.resultsMD)
	}
	fmt.Print(loadgen.Markdown(rep))
	return failed, nil
}

func runBurst(ctx context.Context, o options, pool *pgxpool.Pool, strategy, runID string) (loadgen.Result, error) {
	var res loadgen.Result
	err := withAPI(ctx, o, strategy, runID, func(baseURL string) error {
		var err error
		res, err = loadgen.Run(ctx, loadgen.Config{
			BaseURL:         baseURL,
			Buyers:          o.buyers,
			Stock:           o.stock,
			RunID:           runID,
			ClientTimeout:   o.clientTimeout,
			DialConcurrency: o.dialConc,
			Sampler:         activeSampler(pool),
			DupRatio:        o.dupRatio,
			RetryRatio:      o.retryRatio,
			Confirm:         o.confirm,
		})
		return err
	})
	res.Strategy, res.PoolSize = strategy, o.pool
	if err != nil {
		return res, err
	}
	rep, err := invariants.Check(ctx, pool, res.DropID)
	res.Invariants = &rep
	return res, err
}

func runCeiling(ctx context.Context, o options, pool *pgxpool.Pool, strategy, runID string) (loadgen.CeilingResult, error) {
	var res loadgen.CeilingResult
	err := withAPI(ctx, o, strategy, runID, func(baseURL string) error {
		var err error
		res, err = loadgen.RunCeiling(ctx, loadgen.CeilingConfig{
			BaseURL:       baseURL,
			RunID:         runID,
			Rates:         o.rates,
			StepDuration:  o.stepDuration,
			P99Target:     o.p99Target,
			MaxErrorRate:  o.maxErrorRate,
			StopOnFail:    !o.keepGoing,
			ClientTimeout: o.clientTimeout,
			MaxConns:      o.maxConns,
		})
		return err
	})
	res.Strategy, res.PoolSize = strategy, o.pool
	if err != nil {
		return res, err
	}
	rep, err := invariants.Check(ctx, pool, res.DropID)
	res.Invariants = &rep
	return res, err
}

// activeSampler reads held + confirmed for a drop. It uses loadgen's own
// small pool, not the API's, so sampling never steals the API's connections.
func activeSampler(pool *pgxpool.Pool) loadgen.ActiveSampler {
	return func(ctx context.Context, dropID int64) (int, error) {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(quantity), 0) FROM reservations
			WHERE drop_id = $1 AND status IN ('held', 'confirmed')`, dropID).Scan(&n)
		return n, err
	}
}

// withAPI starts a fresh API process for this strategy, calls fn with its
// base URL, and stops it. A fresh process per run means no warm caches or
// leftover connections leak from one strategy into the next.
func withAPI(ctx context.Context, o options, strategy, runID string, fn func(baseURL string) error) error {
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.resultsDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(filepath.Join(o.resultsDir, "api-"+runID+".log"))
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(o.apiBin)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+o.databaseURL,
		"HTTP_ADDR="+addr,
		"STRATEGY="+strategy,
		fmt.Sprintf("DB_MAX_CONNS=%d", o.pool),
		"REQUEST_TIMEOUT="+o.serverTimeout.String(),
		fmt.Sprintf("PAYMENT_FAILURE_RATE=%g", o.paymentFailureRate),
		"PAYMENT_MIN_LATENCY="+o.paymentMinLatency.String(),
		"PAYMENT_MAX_LATENCY="+o.paymentMaxLatency.String(),
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s (did you run `make build`?): %w", o.apiBin, err)
	}
	defer stopProcess(cmd)

	baseURL := "http://" + addr
	if err := waitHealthy(ctx, baseURL, 10*time.Second); err != nil {
		return fmt.Errorf("API did not become healthy (see %s): %w", logFile.Name(), err)
	}
	return fn(baseURL)
}

// stopProcess sends SIGINT (graceful shutdown) and kills after 10 s.
func stopProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("timeout")
}

// freeAddr asks the kernel for an unused loopback port.
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
