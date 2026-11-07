// Package payment is a mock payment provider. It behaves like a real one in
// the ways that matter for double-charge safety:
//
//   - Charges are idempotent per key: the first call with a key decides the
//     outcome and charges at most once; later calls with the same key return
//     the recorded outcome without charging again.
//   - The outcome is recorded *before* the simulated network latency, so a
//     caller whose context ends mid-call gets an error although the charge
//     may already have happened. That is the ambiguous "did it charge?" case
//     every real integration has to handle, and the reason confirm retries
//     reuse the same key.
package payment

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Gateway charges money. approved=false means the provider declined; a
// non-nil error means the outcome is unknown to the caller.
type Gateway interface {
	Charge(ctx context.Context, key string, amountMinor int64) (approved bool, err error)
}

type MockConfig struct {
	MinLatency, MaxLatency time.Duration
	FailureRate            float64 // 0..1, share of new keys that are declined
	Seed                   uint64  // fixed seed makes failure injection reproducible
}

type Mock struct {
	cfg MockConfig

	mu       sync.Mutex
	rng      *rand.Rand
	outcomes map[string]bool // key -> approved
	calls    map[string]int  // key -> number of Charge calls
	charged  int             // approved charges actually taken
}

func NewMock(cfg MockConfig) *Mock {
	if cfg.MaxLatency < cfg.MinLatency {
		cfg.MaxLatency = cfg.MinLatency
	}
	return &Mock{
		cfg:      cfg,
		rng:      rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		outcomes: map[string]bool{},
		calls:    map[string]int{},
	}
}

func (m *Mock) Charge(ctx context.Context, key string, amountMinor int64) (bool, error) {
	m.mu.Lock()
	m.calls[key]++
	approved, seen := m.outcomes[key]
	if !seen {
		approved = m.rng.Float64() >= m.cfg.FailureRate
		m.outcomes[key] = approved
		if approved {
			m.charged++
		}
	}
	latency := m.cfg.MinLatency
	if span := m.cfg.MaxLatency - m.cfg.MinLatency; span > 0 {
		latency += time.Duration(m.rng.Int64N(int64(span)))
	}
	m.mu.Unlock()

	select {
	case <-time.After(latency):
		return approved, nil
	case <-ctx.Done():
		return false, ctx.Err() // outcome already recorded: caller must retry with the same key
	}
}

// Charges is how many times money was actually taken for key (0 or 1).
func (m *Mock) Charges(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outcomes[key] {
		return 1
	}
	return 0
}

// Calls is how many times Charge was called with key, including replays.
func (m *Mock) Calls(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[key]
}

// TotalCharged is the number of approved charges across all keys.
func (m *Mock) TotalCharged() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.charged
}
