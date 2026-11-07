package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.DBMaxConns != 20 || c.RequestTimeout != 5*time.Second ||
		c.Strategy != "atomic" || c.HoldTTL != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := load(env(map[string]string{
		"DATABASE_URL":         "postgres://x",
		"HTTP_ADDR":            ":9999",
		"DB_MAX_CONNS":         "50",
		"REQUEST_TIMEOUT":      "750ms",
		"STRATEGY":             "naive",
		"HOLD_TTL":             "30s",
		"PAYMENT_MIN_LATENCY":  "0s",
		"PAYMENT_MAX_LATENCY":  "0s",
		"PAYMENT_FAILURE_RATE": "0.25",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://x" || c.HTTPAddr != ":9999" || c.DBMaxConns != 50 || c.RequestTimeout != 750*time.Millisecond ||
		c.Strategy != "naive" || c.HoldTTL != 30*time.Second ||
		c.PaymentMaxLatency != 0 || c.PaymentFailureRate != 0.25 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"zero conns":       {"DB_MAX_CONNS": "0"},
		"non-numeric":      {"DB_MAX_CONNS": "lots"},
		"bad duration":     {"REQUEST_TIMEOUT": "5"},
		"failure rate > 1": {"PAYMENT_FAILURE_RATE": "1.5"},
		"max < min":        {"PAYMENT_MIN_LATENCY": "300ms", "PAYMENT_MAX_LATENCY": "100ms"},
		"negative timeout": {"SHUTDOWN_TIMEOUT": "-1s"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(env(e)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
