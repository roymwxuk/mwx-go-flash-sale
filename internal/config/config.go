// Package config loads runtime settings from environment variables.
// Only the standard library is used; every setting has a safe local default.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL string
	HTTPAddr    string

	// DBMaxConns is the pgxpool MaxConns. It is deliberately much smaller than
	// the number of concurrent buyers; see the pool-sizing ADR (milestone 5).
	DBMaxConns int32

	// RequestTimeout bounds every request, including the time spent waiting
	// for a pool connection, so pool exhaustion becomes a 503 instead of a hang.
	RequestTimeout time.Duration

	ShutdownTimeout time.Duration

	// Strategy names the StockStrategy (validated by sale.ParseStrategy).
	Strategy string

	// HoldTTL is how long a reservation holds stock before it expires.
	HoldTTL time.Duration

	// Mock payment provider (failure injection).
	PaymentMinLatency  time.Duration
	PaymentMaxLatency  time.Duration
	PaymentFailureRate float64
}

const defaultDatabaseURL = "postgres://flashsale:flashsale@localhost:5432/flashsale?sslmode=disable"

// Load reads the config from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

// load takes a getenv function so tests can supply a fake environment
// without mutating the real one.
func load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:     stringOr(getenv("DATABASE_URL"), defaultDatabaseURL),
		HTTPAddr:        stringOr(getenv("HTTP_ADDR"), ":8080"),
		DBMaxConns:      20,
		RequestTimeout:  5 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		Strategy:        stringOr(getenv("STRATEGY"), "atomic"),
		HoldTTL:         5 * time.Minute,

		PaymentMinLatency: 100 * time.Millisecond,
		PaymentMaxLatency: 300 * time.Millisecond,
	}

	if v := getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		c.DBMaxConns = int32(n)
	}

	var err error
	if c.RequestTimeout, err = durationOr(getenv, "REQUEST_TIMEOUT", c.RequestTimeout); err != nil {
		return Config{}, err
	}
	if c.ShutdownTimeout, err = durationOr(getenv, "SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if c.HoldTTL, err = durationOr(getenv, "HOLD_TTL", c.HoldTTL); err != nil {
		return Config{}, err
	}
	if c.PaymentMinLatency, err = nonNegDurationOr(getenv, "PAYMENT_MIN_LATENCY", c.PaymentMinLatency); err != nil {
		return Config{}, err
	}
	if c.PaymentMaxLatency, err = nonNegDurationOr(getenv, "PAYMENT_MAX_LATENCY", c.PaymentMaxLatency); err != nil {
		return Config{}, err
	}
	if c.PaymentMaxLatency < c.PaymentMinLatency {
		return Config{}, fmt.Errorf("PAYMENT_MAX_LATENCY must be >= PAYMENT_MIN_LATENCY")
	}
	if v := getenv("PAYMENT_FAILURE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return Config{}, fmt.Errorf("PAYMENT_FAILURE_RATE must be between 0 and 1, got %q", v)
		}
		c.PaymentFailureRate = f
	}
	return c, nil
}

func stringOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func durationOr(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 5s, got %q", key, v)
	}
	return d, nil
}

// nonNegDurationOr is like durationOr but allows 0 (e.g. no payment latency).
func nonNegDurationOr(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration >= 0 like 100ms, got %q", key, v)
	}
	return d, nil
}
