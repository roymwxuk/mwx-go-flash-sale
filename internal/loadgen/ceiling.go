package loadgen

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
)

// CeilingConfig drives test-plan scenario L5: constant request rates, stepped
// upwards, against a drop with enough stock that it never sells out. Every
// request is therefore a real hot-row write, the most expensive path.
type CeilingConfig struct {
	BaseURL      string
	RunID        string
	Rates        []int         // target req/s per step, e.g. 500,1000,2000,4000,8000
	StepDuration time.Duration // e.g. 20 s
	// Pass criteria per step (test plan: p99 <= 500 ms and errors < 1%).
	P99Target    time.Duration
	MaxErrorRate float64
	StopOnFail   bool // skip higher steps once one fails

	ClientTimeout time.Duration
	MaxConns      int // client connection cap; extra requests queue in the client
}

type Step struct {
	TargetRPS int `json:"target_rps"`
	Outcomes
	SentRPS      float64  `json:"sent_rps"`      // requests scheduled / step duration
	CompletedRPS float64  `json:"completed_rps"` // responses / (first send → last response)
	ErrorRate    float64  `json:"error_rate"`
	PeakInFlight int      `json:"peak_in_flight"`
	Latency      Latency  `json:"latency"`
	Pass         bool     `json:"pass"`
	ErrorSamples []string `json:"error_samples,omitempty"`
}

type CeilingResult struct {
	Strategy   string             `json:"strategy"`
	PoolSize   int                `json:"pool_size"`
	DropID     int64              `json:"drop_id"`
	Stock      int                `json:"stock"`
	Steps      []Step             `json:"steps"`
	CeilingRPS int                `json:"ceiling_rps"` // highest passing step, 0 if none
	Invariants *invariants.Report `json:"invariants,omitempty"`
}

// H1 must hold at every step, including the failing ones: overload may give
// 503s and high latency, but never other 5xx or hung requests.
func (c CeilingResult) H1() bool {
	for _, s := range c.Steps {
		if !s.H1() {
			return false
		}
	}
	return true
}

// RunCeiling runs every step against one drop and reports the ceiling.
func RunCeiling(ctx context.Context, cfg CeilingConfig) (CeilingResult, error) {
	var res CeilingResult
	// Stock covers every request that could possibly be sent, plus slack, so
	// no step ever measures the cheap sold-out path.
	for _, r := range cfg.Rates {
		res.Stock += int(float64(r)*cfg.StepDuration.Seconds()) + 1
	}
	res.Stock += 1000

	dropID, err := createDrop(ctx, cfg.BaseURL, "ceiling-"+cfg.RunID, res.Stock)
	if err != nil {
		return res, err
	}
	res.DropID = dropID

	// Unlike the burst, a constant rate is what a shared keep-alive pool is
	// good at: connections are reused, and MaxConnsPerHost bounds sockets so
	// an overloaded step cannot exhaust ephemeral ports.
	sem := make(chan struct{}, 64) // limit concurrent dials (listen backlog)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			defer func() { <-sem }()
			return dialNoLinger(ctx, addr)
		},
		MaxConnsPerHost:     cfg.MaxConns,
		MaxIdleConns:        cfg.MaxConns,
		MaxIdleConnsPerHost: cfg.MaxConns,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: cfg.ClientTimeout}
	url := fmt.Sprintf("%s/drops/%d/reservations", cfg.BaseURL, dropID)

	for i, rate := range cfg.Rates {
		step := runStep(ctx, client, url, fmt.Sprintf("%s-s%d", cfg.RunID, i), rate, cfg)
		res.Steps = append(res.Steps, step)
		if step.Pass {
			res.CeilingRPS = rate
		} else if cfg.StopOnFail {
			break
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	return res, nil
}

// runStep sends `rate` requests per second for StepDuration, open-loop: the
// schedule does not slow down when the server does. Requests go out in 10 ms
// slots; a fractional per-slot count is carried over so the average is exact.
func runStep(ctx context.Context, client *http.Client, url, stepID string, rate int, cfg CeilingConfig) Step {
	const slot = 10 * time.Millisecond
	perSlot := float64(rate) * slot.Seconds()
	slots := int(cfg.StepDuration / slot)

	var mu sync.Mutex
	samples := make([]sample, 0, int(float64(rate)*cfg.StepDuration.Seconds())+1)
	var wg sync.WaitGroup

	begin := time.Now()
	carry, n := 0.0, 0
	for k := 0; k < slots && ctx.Err() == nil; k++ {
		due := begin.Add(time.Duration(k) * slot)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		carry += perSlot
		count := int(carry)
		carry -= float64(count)
		for j := 0; j < count; j++ {
			id := n
			n++
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
				req.Header.Set("X-User-Id", fmt.Sprintf("cl-%s-%d", stepID, id))
				req.Header.Set("Idempotency-Key", fmt.Sprintf("cl-%s-%d-k", stepID, id))
				resp, err := client.Do(req)
				s := sample{err: err, start: due} // latency from the scheduled time
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					s.status = resp.StatusCode
				}
				s.end = time.Now()
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}()
		}
	}
	wg.Wait() // finish this step's stragglers before the next step starts

	sum := summarize(samples, cfg.ClientTimeout+time.Second)
	st := Step{
		TargetRPS:    rate,
		Outcomes:     sum.Outcomes,
		SentRPS:      float64(n) / cfg.StepDuration.Seconds(),
		PeakInFlight: sum.peakInFlight,
		Latency:      Summarize(sum.all),
		ErrorSamples: sum.errorSamples,
	}
	if span := sum.last.Sub(sum.first); span > 0 {
		st.CompletedRPS = float64(sum.Requests) / span.Seconds()
	}
	if sum.Requests > 0 {
		// In this scenario a 409 can only mean something went wrong (stock
		// never runs out, every user is new), so only 201 counts as success.
		st.ErrorRate = float64(sum.Requests-sum.Created) / float64(sum.Requests)
	}
	st.Pass = st.Requests > 0 && st.ErrorRate < cfg.MaxErrorRate &&
		st.Latency.P99 <= ms(cfg.P99Target) && st.H1()
	return st
}
