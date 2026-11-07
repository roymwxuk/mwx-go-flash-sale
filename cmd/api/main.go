// Command api runs the flash-sale HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/config"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/db"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/httpapi"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/payment"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sale"
	"github.com/roymwxuk/mwx-go-flash-sale/internal/sweeper"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run holds the real main body so that deferred cleanups execute before
// os.Exit (os.Exit skips defers).
func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// ctx is cancelled on SIGINT/SIGTERM, which starts graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	strategy, err := sale.ParseStrategy(cfg.Strategy)
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	// Mock provider with failure injection. Seeded from the clock: each run
	// declines a different set of reservations at the configured rate.
	payments := payment.NewMock(payment.MockConfig{
		MinLatency:  cfg.PaymentMinLatency,
		MaxLatency:  cfg.PaymentMaxLatency,
		FailureRate: cfg.PaymentFailureRate,
		Seed:        uint64(time.Now().UnixNano()),
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.New(pool, sale.NewService(pool, strategy, cfg.HoldTTL, payments), log, cfg.RequestTimeout).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The sweeper runs in every API instance; SKIP LOCKED makes N of them
	// safe (ADR-008). It gets its own cancel so shutdown can stop it after
	// the HTTP server has drained, and `sweeperDone` lets us wait for its
	// current batch to finish before the deferred pool.Close runs.
	sweepCtx, stopSweeper := context.WithCancel(context.Background())
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		sweeper.New(pool, sweeper.Config{
			Interval:  cfg.SweepInterval,
			BatchSize: cfg.SweepBatch,
			KeyTTL:    cfg.KeyTTL,
		}, log).Run(sweepCtx)
	}()
	defer func() {
		stopSweeper()
		<-sweeperDone
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "db_max_conns", cfg.DBMaxConns, "strategy", strategy.Name(),
			"payment_failure_rate", cfg.PaymentFailureRate, "sweep_interval", cfg.SweepInterval)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	// Deferred calls run in reverse order: stop the sweeper and wait for it,
	// then pool.Close. Nothing touches the pool after it is closed.
	return nil
}
