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
