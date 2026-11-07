package payment

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestChargeIsIdempotentPerKey(t *testing.T) {
	m := NewMock(MockConfig{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := m.Charge(context.Background(), "res-1", 1000); !ok || err != nil {
				t.Errorf("charge: %v %v", ok, err)
			}
		}()
	}
	wg.Wait()
	if m.Charges("res-1") != 1 || m.Calls("res-1") != 50 || m.TotalCharged() != 1 {
		t.Fatalf("charges=%d calls=%d total=%d, want 1/50/1", m.Charges("res-1"), m.Calls("res-1"), m.TotalCharged())
	}
}

func TestDeclineIsStickyAndNotCharged(t *testing.T) {
	m := NewMock(MockConfig{FailureRate: 1})
	for i := 0; i < 3; i++ {
		if ok, err := m.Charge(context.Background(), "k", 1); ok || err != nil {
			t.Fatalf("attempt %d: want decline, got %v %v", i, ok, err)
		}
	}
	if m.Charges("k") != 0 || m.TotalCharged() != 0 {
		t.Fatal("a decline must not count as a charge")
	}
}

// The caller can lose the answer even though the charge happened.
func TestTimeoutAfterChargeIsAmbiguous(t *testing.T) {
	m := NewMock(MockConfig{MinLatency: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m.Charge(ctx, "k", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if m.Charges("k") != 1 {
		t.Fatal("charge should already be recorded")
	}
	// Retrying with the same key returns the recorded outcome, no new charge.
	m.cfg.MinLatency = 0
	if ok, err := m.Charge(context.Background(), "k", 1); !ok || err != nil || m.TotalCharged() != 1 {
		t.Fatalf("retry: %v %v total=%d", ok, err, m.TotalCharged())
	}
}

func TestFailureRateRoughlyHonoured(t *testing.T) {
	m := NewMock(MockConfig{FailureRate: 0.3, Seed: 42})
	declined := 0
	for i := 0; i < 1000; i++ {
		if ok, _ := m.Charge(context.Background(), "key-"+strconv.Itoa(i), 1); !ok {
			declined++
		}
	}
	if declined < 250 || declined > 350 {
		t.Fatalf("declined %d of 1000 at rate 0.3", declined)
	}
}
