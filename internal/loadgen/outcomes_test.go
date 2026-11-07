package loadgen

import (
	"context"
	"errors"
	"testing"
	"time"
)

// timeoutErr satisfies net.Error with Timeout() == true, like a client timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestSummarizeClassifiesForH1(t *testing.T) {
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	samples := []sample{
		{status: 201, start: t0, end: at(10)},
		{status: 409, start: t0, end: at(5)},
		{status: 503, start: t0, end: at(2000)}, // allowed overload answer
		{status: 201, start: t0, end: at(3500)}, // slower than timeout+1s: hung
		{status: 500, start: t0, end: at(20)},
		{err: timeoutErr{}, start: t0, end: at(2000)},
		{err: errors.New("connection reset"), start: t0, end: at(1)},
	}
	s := summarize(samples, 2500*time.Millisecond)
	want := Outcomes{Requests: 7, Created: 2, SoldOut: 1, Unavailable: 1, Server5xx: 1, TransportErrors: 1, Hung: 2}
	if s.Outcomes != want {
		t.Fatalf("got %+v\nwant %+v", s.Outcomes, want)
	}
	if s.H1() {
		t.Fatal("H1 must fail with a 500, a hung request and a transport error")
	}
	if ok := (Outcomes{Created: 1, SoldOut: 5, Unavailable: 3}); !ok.H1() {
		t.Fatal("503s alone must not fail H1")
	}
}

func TestPeakInFlight(t *testing.T) {
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	samples := []sample{
		{start: at(0), end: at(10)},
		{start: at(5), end: at(15)},
		{start: at(6), end: at(7)},
		{start: at(10), end: at(20)}, // starts exactly when the first ends: not overlapping it
	}
	if got := peakInFlight(samples); got != 3 {
		t.Fatalf("peak = %d, want 3", got)
	}
}

func TestSampleActiveRecordsPeak(t *testing.T) {
	vals := []int{1, 7, 3}
	i := 0
	cfg := Config{SampleEvery: time.Millisecond, Sampler: func(context.Context, int64) (int, error) {
		v := vals[min(i, len(vals)-1)]
		i++
		return v, nil
	}}
	var res Result
	stop := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); close(stop) }()
	sampleActive(context.Background(), cfg, 1, &res, stop)
	if res.PeakActive != 7 || res.Samples < 3 {
		t.Fatalf("peak=%d samples=%d", res.PeakActive, res.Samples)
	}
}
