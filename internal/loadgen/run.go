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

	// Messy clients (test plan 2.4 / L6). Selection is deterministic per
	// buyer index, so a rerun picks the same buyers.
	DupRatio   float64 // share of buyers who double-click: same request, same key, 0–49 ms later, own connection
	RetryRatio float64 // share of buyers who resend with the same key after their answer (simulated timeout)
	Confirm    bool    // winners call POST /reservations/{id}/confirm (retried too, if selected for retry)
}

// Result is one burst run against one drop. Strategy and PoolSize are labels
// set by the caller, which knows how the server was started.
type Result struct {
	Strategy string `json:"strategy"`
	PoolSize int    `json:"pool_size"`
	Buyers   int    `json:"buyers"`
	Stock    int    `json:"stock"`
	DropID   int64  `json:"drop_id"`

	Outcomes       // first attempt of every buyer; the latency figures below use only these
	Retries    int `json:"retries"`    // same-key resends (reserve + confirm)
	Duplicates int `json:"duplicates"` // double-click requests
	// Extra covers the duplicate and retry reservation requests, so H1 also
	// applies to them.
	Extra   Outcomes     `json:"extra_requests"`
	Confirm ConfirmStats `json:"confirm"`
	// KeyViolations: one Idempotency-Key produced two *different* successful
	// responses (e.g. two reservations). Must be 0; part of I3.
	KeyViolations int `json:"key_violations"`

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

// H1 covers every request the run sent, not only first attempts.
func (r Result) H1() bool {
	return r.Outcomes.H1() && r.Extra.H1() && r.Confirm.H1()
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

	// Which buyers double-click, retry. Deterministic per index.
	dupOf := map[int]int{} // buyer index -> extra connection index
	for i := 0; i < cfg.Buyers; i++ {
		if pick(i, cfg.DupRatio, 1) {
			dupOf[i] = cfg.Buyers + len(dupOf)
		}
	}
	res.Duplicates = len(dupOf)

	// --- setup: one pre-dialed connection per buyer, plus one per duplicate ---
	setupStart := time.Now()
	clients, err := dialAll(ctx, u.Host, cfg.Buyers+len(dupOf), cfg.DialConcurrency, cfg.ClientTimeout)
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
	bodies := make([][]byte, cfg.Buyers)
	var extra, confirms []sample
	var dupResults []dupResult
	var mu sync.Mutex // guards extra, confirms and the counters below
	keyViolations, retries := 0, 0
	note := func(dst *[]sample, s sample) {
		mu.Lock()
		*dst = append(*dst, s)
		mu.Unlock()
	}
	// sameEffect: two successful answers to one key must be identical.
	sameEffect := func(a sample, aBody []byte, b sample, bBody []byte) {
		if a.err == nil && b.err == nil && isSuccess(a.status) && isSuccess(b.status) &&
			(a.status != b.status || !bytes.Equal(aBody, bBody)) {
			mu.Lock()
			keyViolations++
			mu.Unlock()
		}
	}

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	var released time.Time // written before close(start), read after: safe
	for i := 0; i < cfg.Buyers; i++ {
		user := fmt.Sprintf("lg-%s-%d", cfg.RunID, i)
		key := fmt.Sprintf("lg-%s-%d-k", cfg.RunID, i)
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			s, body := send(ctx, clients[i], reserveURL, user, key, released)
			samples[i], bodies[i] = s, body // each goroutine writes only its own index: no race

			if pick(i, cfg.RetryRatio, 2) {
				r, rBody := send(ctx, clients[i], reserveURL, user, key, time.Now())
				note(&extra, r)
				sameEffect(s, body, r, rBody)
				mu.Lock()
				retries++
				mu.Unlock()
			}
			if cfg.Confirm && s.err == nil && s.status == http.StatusCreated {
				var hold struct {
					ID int64 `json:"id"`
				}
				if json.Unmarshal(body, &hold) == nil {
					confirmURL := fmt.Sprintf("%s/reservations/%d/confirm", cfg.BaseURL, hold.ID)
					c, cBody := send(ctx, clients[i], confirmURL, user, key+"-confirm", time.Now())
					note(&confirms, c)
					if pick(i, cfg.RetryRatio, 3) {
						c2, c2Body := send(ctx, clients[i], confirmURL, user, key+"-confirm", time.Now())
						note(&confirms, c2)
						sameEffect(c, cBody, c2, c2Body)
						mu.Lock()
						retries++
						mu.Unlock()
					}
				}
			}
		}(i)

		if j, ok := dupOf[i]; ok {
			done.Add(1)
			go func(i, j int) {
				defer done.Done()
				<-start
				time.Sleep(time.Duration(i%50) * time.Millisecond) // "twice within 50 ms"
				d, dBody := send(ctx, clients[j], reserveURL, user, key, time.Now())
				note(&extra, d)
				// Compare once the original has finished (done.Wait below).
				mu.Lock()
				dupResults = append(dupResults, dupResult{i, d, dBody})
				mu.Unlock()
			}(i, j)
		}
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

	for _, d := range dupResults {
		sameEffect(samples[d.buyer], bodies[d.buyer], d.s, d.body)
	}
	res.KeyViolations = keyViolations
	res.Retries = retries

	// --- summarize ---
	hung := cfg.ClientTimeout + time.Second
	sum := summarize(samples, hung)
	res.Outcomes = sum.Outcomes
	res.ErrorSamples = sum.errorSamples
	res.PeakInFlight = sum.peakInFlight
	res.Extra = summarize(extra, hung).Outcomes
	res.Confirm = summarizeConfirms(confirms, hung)
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

type dupResult struct {
	buyer int
	s     sample
	body  []byte
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

// pick selects about ratio of all indexes, deterministically. salt makes the
// duplicate, retry and confirm-retry selections independent of each other.
func pick(i int, ratio float64, salt uint64) bool {
	if ratio <= 0 {
		return false
	}
	h := (uint64(i)+1)*0x9e3779b97f4a7c15 ^ salt*0xbf58476d1ce4e5b9
	h ^= h >> 31
	return float64(h%10000) < ratio*10000
}

// send POSTs with the buyer's headers and returns the sample and body. start
// is when the request was meant to go out (see sample).
func send(ctx context.Context, c *http.Client, url, user, key string, start time.Time) (sample, []byte) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("X-User-Id", user)
	req.Header.Set("Idempotency-Key", key)
	resp, err := c.Do(req)
	s := sample{err: err, start: start}
	var body []byte
	if err == nil {
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		s.status = resp.StatusCode
	}
	s.end = time.Now()
	return s, body
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
