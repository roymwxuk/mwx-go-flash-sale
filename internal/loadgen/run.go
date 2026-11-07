// Package loadgen drives the API over real HTTP and summarizes the outcome.
// Two scenarios from docs/test-plan.md:
//   - Burst (L1): N buyers, each with an open connection, released at the
//     same instant against one drop. Maximum contention, by design.
//   - Ceiling (L5): constant request rate stepped upwards to find the
//     highest rate one instance sustains (see ceiling.go).
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
)

// ActiveSampler returns held + confirmed quantity for the drop, read straight
// from the DB. Burst calls it repeatedly *during* the run, so invariant I1
// ("at all times") is checked mid-flight, not only at the end.
type ActiveSampler func(ctx context.Context, dropID int64) (int, error)

type Config struct {
	BaseURL string // e.g. http://127.0.0.1:8080
	Buyers  int
	Stock   int
	RunID   string // makes user ids unique across runs

	// ClientTimeout bounds each buyer's request end to end. It should be
	// longer than the server's REQUEST_TIMEOUT so a server-side 503 is
	// observed as a 503, not as a client timeout.
	ClientTimeout time.Duration

	// DialConcurrency caps simultaneous TCP connects during setup so the
	// server's listen backlog (somaxconn, 128 on macOS) never overflows.
	DialConcurrency int

	Sampler     ActiveSampler // optional
	SampleEvery time.Duration // default 50 ms
}

// Result is one burst run against one drop. Strategy and PoolSize are labels
// set by the caller, which knows how the server was started.
type Result struct {
	Strategy string `json:"strategy"`
	PoolSize int    `json:"pool_size"`
	Buyers   int    `json:"buyers"`
	Stock    int    `json:"stock"`
	DropID   int64  `json:"drop_id"`

	Outcomes
	// Retries performed by the client or a retrying strategy. Always 0 until
	// retry simulation (milestone 4) / strategy D exist.
	Retries    int `json:"retries"`
	Duplicates int `json:"duplicates"`

	SetupMs        float64 `json:"setup_ms"` // time to open all connections
	ElapsedMs      float64 `json:"elapsed_ms"`
	ThroughputRPS  float64 `json:"throughput_rps"`
	SellOutMs      float64 `json:"sell_out_ms"` // release → last 201
	PeakInFlight   int     `json:"peak_in_flight"`
	Latency        Latency `json:"latency"`
	LatencyCreated Latency `json:"latency_201"`
	LatencySoldOut Latency `json:"latency_409"`

	// Mid-run I1 sampling.
	Samples         int `json:"active_samples"`
	PeakActive      int `json:"peak_active"`      // max held+confirmed seen
	SampledOversell int `json:"sampled_oversell"` // max(0, PeakActive-Stock)
	SampleErrors    int `json:"active_sample_errors"`

	ErrorSamples []string           `json:"error_samples,omitempty"`
	Invariants   *invariants.Report `json:"invariants,omitempty"`
}

// Oversold reads the DB-verified count; the HTTP 201 count is not trusted.
func (r Result) Oversold() int {
	if r.Invariants == nil {
		return 0
	}
	return r.Invariants.Stats.Oversold()
}

func (r Result) Violations() int {
	if r.Invariants == nil {
		return 0
	}
	return len(r.Invariants.Violations)
}

// Run creates a drop, opens one connection per buyer, releases every buyer at
// once, and waits for all responses.
func Run(ctx context.Context, cfg Config) (Result, error) {
	res := Result{Buyers: cfg.Buyers, Stock: cfg.Stock}

	dropID, err := createDrop(ctx, cfg.BaseURL, "loadgen-"+cfg.RunID, cfg.Stock)
	if err != nil {
		return res, err
	}
	res.DropID = dropID

	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return res, err
	}
	reserveURL := fmt.Sprintf("%s/drops/%d/reservations", cfg.BaseURL, dropID)

	// --- setup: one pre-dialed connection and client per buyer ---
	setupStart := time.Now()
	clients, err := dialAll(ctx, u.Host, cfg.Buyers, cfg.DialConcurrency, cfg.ClientTimeout)
	if err != nil {
		return res, err
	}
	defer func() {
		for _, c := range clients {
			c.CloseIdleConnections()
		}
	}()
	res.SetupMs = ms(time.Since(setupStart))

	// --- the burst ---
	samples := make([]sample, cfg.Buyers)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	var released time.Time // written before close(start), read after: safe
	for i := 0; i < cfg.Buyers; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			// Build the request before the barrier so only the send is timed.
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reserveURL, nil)
			req.Header.Set("X-User-Id", fmt.Sprintf("lg-%s-%d", cfg.RunID, i))
			req.Header.Set("Idempotency-Key", fmt.Sprintf("lg-%s-%d-k", cfg.RunID, i))
			ready.Done()
			<-start

			resp, err := clients[i].Do(req)
			s := sample{err: err, start: released}
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				s.status = resp.StatusCode
			}
			s.end = time.Now()
			samples[i] = s // each goroutine writes only its own index: no race
		}(i)
	}
	ready.Wait()

	stopSampling := make(chan struct{})
	samplingDone := make(chan struct{})
	go func() {
		defer close(samplingDone)
		sampleActive(ctx, cfg, dropID, &res, stopSampling)
	}()

	released = time.Now()
	close(start)
	done.Wait()
	close(stopSampling)
	<-samplingDone

	// --- summarize ---
	sum := summarize(samples, cfg.ClientTimeout+time.Second)
	res.Outcomes = sum.Outcomes
	res.ErrorSamples = sum.errorSamples
	res.PeakInFlight = sum.peakInFlight
	elapsed := sum.last.Sub(released)
	res.ElapsedMs = ms(elapsed)
	if elapsed > 0 {
		res.ThroughputRPS = float64(cfg.Buyers) / elapsed.Seconds()
	}
	if !sum.lastCreated.IsZero() {
		res.SellOutMs = ms(sum.lastCreated.Sub(released))
	}
	res.Latency = Summarize(sum.all)
	res.LatencyCreated = Summarize(sum.created)
	res.LatencySoldOut = Summarize(sum.soldOut)
	res.SampledOversell = max(0, res.PeakActive-cfg.Stock)
	return res, nil
}

// sampleActive polls held + confirmed until stop is closed, plus once more
// at the end so the final state is always included.
func sampleActive(ctx context.Context, cfg Config, dropID int64, res *Result, stop <-chan struct{}) {
	if cfg.Sampler == nil {
		return
	}
	every := cfg.SampleEvery
	if every <= 0 {
		every = 50 * time.Millisecond
	}
	take := func() {
		n, err := cfg.Sampler(ctx, dropID)
		if err != nil {
			res.SampleErrors++
			return
		}
		res.Samples++
		res.PeakActive = max(res.PeakActive, n)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			take()
			return
		case <-t.C:
			take()
		}
	}
}

// dialAll opens n TCP connections and wraps each in its own http.Client
// whose transport hands out exactly that connection.
//
// Why not one shared client with MaxConnsPerHost = n? A shared pool hands
// idle connections to whichever request asks first, so a warm-up cannot
// guarantee n distinct sockets, and any request without one dials during the
// burst. One client per buyer models what we care about: n separate users,
// each already connected, all sending at once.
func dialAll(ctx context.Context, host string, n, concurrency int, timeout time.Duration) ([]*http.Client, error) {
	if concurrency < 1 {
		concurrency = 64
	}
	clients := make([]*http.Client, n)
	sem := make(chan struct{}, concurrency)
	var firstErr atomic.Value
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			conn, err := dialNoLinger(ctx, host)
			if err != nil {
				firstErr.CompareAndSwap(nil, fmt.Errorf("dial %d/%d: %w", i, n, err))
				return
			}
			clients[i] = &http.Client{Timeout: timeout, Transport: singleConnTransport(conn)}
		}(i)
	}
	wg.Wait()
	if err, _ := firstErr.Load().(error); err != nil {
		for _, c := range clients {
			if c != nil {
				c.CloseIdleConnections()
			}
		}
		return nil, err
	}
	return clients, nil
}

// dialNoLinger opens a TCP connection with SO_LINGER=0: closing sends RST
// instead of FIN, so the client port skips TIME_WAIT (~30 s on macOS).
// Without it, 10,000 closed sockets exhaust the 16k ephemeral port range and
// the next run cannot dial. Fine for a load generator; never do this in a
// real client, since unsent data is discarded.
func dialNoLinger(ctx context.Context, host string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	return conn, nil
}

// singleConnTransport returns a Transport whose first dial returns the
// pre-opened conn and every later dial fails, so the request can never
// silently open a fresh connection mid-burst.
func singleConnTransport(conn net.Conn) *http.Transport {
	var used atomic.Bool
	return &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("loadgen: pre-dialed connection already used")
			}
			return conn, nil
		},
		MaxIdleConnsPerHost: 1,
		DisableCompression:  true,
	}
}

func createDrop(ctx context.Context, baseURL, name string, stock int) (int64, error) {
	body, _ := json.Marshal(map[string]any{"name": name, "total_stock": stock})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/drops", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create drop: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("create drop: HTTP %d: %s", resp.StatusCode, b)
	}
	var d struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return 0, err
	}
	return d.ID, nil
}
