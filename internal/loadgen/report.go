package loadgen

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/roymwxuk/mwx-go-flash-sale/internal/invariants"
)

// Report is everything one loadgen invocation produced.
type Report struct {
	StartedAt time.Time       `json:"started_at"`
	Scenario  string          `json:"scenario"` // "burst" (L1) or "ceiling" (L5)
	Commit    string          `json:"commit"`   // git short sha, "-dirty" if uncommitted changes
	Command   string          `json:"command"`
	Machine   Machine         `json:"machine"`
	Runs      []Result        `json:"runs,omitempty"`
	Ceilings  []CeilingResult `json:"ceilings,omitempty"`
}

// GitCommit returns the short HEAD sha with "-dirty" when the work tree has
// uncommitted changes, so every result can be traced to exact code.
func GitCommit(ctx context.Context) string {
	sha := run(ctx, "git", "rev-parse", "--short", "HEAD")
	if sha == "" {
		return "unknown"
	}
	if run(ctx, "git", "status", "--porcelain", "--untracked-files=no") != "" {
		sha += "-dirty"
	}
	return sha
}

type Machine struct {
	CPU      string `json:"cpu"`
	Cores    int    `json:"cores"`
	RAM      string `json:"ram"`
	OS       string `json:"os"`
	Go       string `json:"go"`
	Postgres string `json:"postgres"`
	// DockerVM is the CPU/RAM visible to the Docker engine. On macOS Postgres
	// runs inside a VM that can be much smaller than the host.
	DockerVM string `json:"docker_vm,omitempty"`
}

func (m Machine) String() string {
	s := fmt.Sprintf("%s (%d cores), %s RAM, %s, %s, PostgreSQL %s", m.CPU, m.Cores, m.RAM, m.OS, m.Go, m.Postgres)
	if m.DockerVM != "" {
		s += fmt.Sprintf(" in Docker (engine VM: %s)", m.DockerVM)
	}
	return s
}

// DetectMachine collects the host spec. Every probe is best-effort: a missing
// tool yields "unknown" rather than an error, so loadgen runs anywhere.
func DetectMachine(ctx context.Context, postgresVersion string) Machine {
	m := Machine{
		CPU:      "unknown",
		Cores:    runtime.NumCPU(),
		RAM:      "unknown",
		OS:       runtime.GOOS + "/" + runtime.GOARCH,
		Go:       runtime.Version(),
		Postgres: postgresVersion,
	}
	switch runtime.GOOS {
	case "darwin":
		if v := run(ctx, "sysctl", "-n", "machdep.cpu.brand_string"); v != "" {
			m.CPU = v
		}
		if v, err := strconv.ParseInt(run(ctx, "sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			m.RAM = fmt.Sprintf("%d GB", v>>30)
		}
		if v := run(ctx, "sw_vers", "-productVersion"); v != "" {
			m.OS = "macOS " + v
		}
	case "linux":
		if v := grepField("/proc/cpuinfo", "model name"); v != "" {
			m.CPU = v
		}
		if v := grepField("/proc/meminfo", "MemTotal"); v != "" {
			kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			m.RAM = fmt.Sprintf("%d GB", kb>>20)
		}
		if v := grepField("/etc/os-release", "PRETTY_NAME"); v != "" {
			m.OS = strings.Trim(v, `"`)
		}
	}
	if v := run(ctx, "docker", "info", "--format", "{{.NCPU}} CPU / {{.MemTotal}}"); v != "" {
		var cpus int
		var mem int64
		if _, err := fmt.Sscanf(v, "%d CPU / %d", &cpus, &mem); err == nil {
			m.DockerVM = fmt.Sprintf("%d CPU / %.1f GB", cpus, float64(mem)/(1<<30))
		}
	}
	return m
}

func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// grepField returns the value after "key:" or "key=" on the first matching line.
func grepField(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, key) {
			if i := strings.IndexAny(line, ":="); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// WriteJSON stores the full report under dir and returns the file path.
func WriteJSON(dir string, rep Report) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "loadgen-"+rep.StartedAt.Format("20060102-150405")+".json")
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}

// Markdown renders the report as a section for docs/results.md, following
// the results template in docs/test-plan.md section 8.
func Markdown(rep Report) string {
	var b strings.Builder
	switch {
	case len(rep.Runs) > 0:
		first := rep.Runs[0]
		fmt.Fprintf(&b, "\n---\n\n## L1 burst: %s, commit %s: %d buyers vs %d units\n\n",
			rep.StartedAt.Format("2006-01-02 15:04"), rep.Commit, first.Buyers, first.Stock)
	case len(rep.Ceilings) > 0:
		fmt.Fprintf(&b, "\n---\n\n## L5 capacity ceiling: %s, commit %s\n\n",
			rep.StartedAt.Format("2006-01-02 15:04"), rep.Commit)
	default:
		return ""
	}
	fmt.Fprintf(&b, "**Command:** `%s`\n\n", rep.Command)
	fmt.Fprintf(&b, "**Machine:** %s. Load generator, API, and DB on one machine.\n\n", rep.Machine)

	if len(rep.Runs) > 0 {
		writeBurst(&b, rep.Runs)
	}
	for _, c := range rep.Ceilings {
		writeCeiling(&b, c)
	}
	return b.String()
}

func writeBurst(b *strings.Builder, runs []Result) {
	b.WriteString("| Strategy | Pool | 201 holds | Oversold (DB) | Violations | 409 sold out | 503 | Other errors | p50 ms | p95 ms | p99 ms | max ms | 201 p95 ms | Throughput req/s | Sell-out ms | H1 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
	for _, r := range runs {
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %.0f | %.1f | %s |\n",
			r.Strategy, r.PoolSize, r.Created, r.Oversold(), r.Violations(), r.SoldOut, r.Unavailable,
			r.Errors()-r.Unavailable, r.Latency.P50, r.Latency.P95, r.Latency.P99, r.Latency.Max,
			r.LatencyCreated.P95, r.ThroughputRPS, r.SellOutMs, okFail(r.H1()))
	}
	b.WriteString("\nPer run (test-plan template):\n")
	runNo := map[string]int{} // runs are numbered per strategy
	for _, r := range runs {
		runNo[r.Strategy]++
		fmt.Fprintf(b, "\n**%s, run %d** (pool=%d, drop %d)\n", r.Strategy, runNo[r.Strategy], r.PoolSize, r.DropID)
		fmt.Fprintf(b, "- Buyers: %d · Stock: %d · Requests: %d (first attempts %d, dup %d, retries %d, confirms %d) · Duration: %.2f s\n",
			r.Buyers, r.Stock, r.Requests+r.Extra.Requests+r.Confirm.Requests, r.Requests, r.Duplicates, r.Retries,
			r.Confirm.Requests, r.ElapsedMs/1000)
		fmt.Fprintf(b, "- Latency ms: p50 %.1f · p95 %.1f · p99 %.1f · max %.1f · 201 only: p50 %.1f / p95 %.1f / p99 %.1f · 409 only: p50 %.1f / p95 %.1f / p99 %.1f · Peak in-flight: %d\n",
			r.Latency.P50, r.Latency.P95, r.Latency.P99, r.Latency.Max,
			r.LatencyCreated.P50, r.LatencyCreated.P95, r.LatencyCreated.P99,
			r.LatencySoldOut.P50, r.LatencySoldOut.P95, r.LatencySoldOut.P99, r.PeakInFlight)
		fmt.Fprintf(b, "- Outcome (first attempts): held %d · 409 %d · 503 %d · other 5xx %d · transport errors %d · hung %d\n",
			r.Created, r.SoldOut, r.Unavailable, r.Server5xx, r.TransportErrors, r.Hung)
		if r.Extra.Requests > 0 {
			fmt.Fprintf(b, "- Duplicate/retry reservation requests: %d · 201 %d · 409 %d · 503 %d · other errors %d\n",
				r.Extra.Requests, r.Extra.Created, r.Extra.SoldOut, r.Extra.Unavailable, r.Extra.Errors()-r.Extra.Unavailable)
		}
		if r.Confirm.Requests > 0 {
			fmt.Fprintf(b, "- Confirm: %d requests · confirmed 200 %d · declined 402 %d (hold released) · 503 %d · other %d · latency ms p50 %.1f / p95 %.1f / p99 %.1f\n",
				r.Confirm.Requests, r.Confirm.Confirmed, r.Confirm.Declined, r.Confirm.Unavailable,
				r.Confirm.Server5xx+r.Confirm.OtherStatus+r.Confirm.TransportErrors+r.Confirm.Hung,
				r.Confirm.Latency.P50, r.Confirm.Latency.P95, r.Confirm.Latency.P99)
		}
		i1 := invStatus(r.Invariants, invariants.NoOversell)
		if r.SampledOversell > 0 {
			i1 = fmt.Sprintf("FAIL (oversold by %d mid-run)", r.SampledOversell)
		}
		fmt.Fprintf(b, "- Invariants: I1 %s, %d mid-run samples, peak active %d · I2 %s · I3 %s · I4 n/a here (no hold expires during a burst; see sweeper tests) · I5 %s · H1 %s\n",
			i1, r.Samples, r.PeakActive,
			invStatus(r.Invariants, invariants.PerUserLimit),
			i3Status(r),
			invStatus(r.Invariants, invariants.StockConservation),
			okFail(r.H1()))
		writeViolations(b, r.Invariants)
		for _, e := range r.ErrorSamples {
			fmt.Fprintf(b, "- Error sample: `%s`\n", e)
		}
	}
}

func writeCeiling(b *strings.Builder, c CeilingResult) {
	fmt.Fprintf(b, "**Strategy %s, pool %d, drop %d (stock %d, never sells out)**\n\n", c.Strategy, c.PoolSize, c.DropID, c.Stock)
	b.WriteString("| Target req/s | Sent req/s | Completed req/s | Requests | 201 | 503 | Other errors | Error rate | p50 ms | p95 ms | p99 ms | max ms | Peak in-flight | H1 | Pass |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|\n")
	for _, s := range c.Steps {
		fmt.Fprintf(b, "| %d | %.0f | %.0f | %d | %d | %d | %d | %.2f%% | %.1f | %.1f | %.1f | %.1f | %d | %s | %s |\n",
			s.TargetRPS, s.SentRPS, s.CompletedRPS, s.Requests, s.Created, s.Unavailable,
			s.Errors()-s.Unavailable+s.SoldOut, s.ErrorRate*100, s.Latency.P50, s.Latency.P95,
			s.Latency.P99, s.Latency.Max, s.PeakInFlight, okFail(s.H1()), passFail(s.Pass))
	}
	ceiling := "none (no step passed)"
	if c.CeilingRPS > 0 {
		ceiling = fmt.Sprintf("**%d req/s**", c.CeilingRPS)
	}
	fmt.Fprintf(b, "\nCeiling (last step with p99 within target and errors below the limit): %s · Invariants: I1 %s · I2 %s · I5 %s · H1 %s\n",
		ceiling,
		invStatus(c.Invariants, invariants.NoOversell),
		invStatus(c.Invariants, invariants.PerUserLimit),
		invStatus(c.Invariants, invariants.StockConservation),
		okFail(c.H1()))
	writeViolations(b, c.Invariants)
	for _, s := range c.Steps {
		for _, e := range s.ErrorSamples {
			fmt.Fprintf(b, "- Error sample (%d req/s): `%s`\n", s.TargetRPS, e)
		}
	}
	b.WriteString("\n")
}

// i3Status combines the DB checks with what only the client can see: one
// Idempotency-Key that produced two different successful responses.
func i3Status(r Result) string {
	st := invStatus(r.Invariants, invariants.NoDoubleCharge, invariants.PaymentConsistency)
	if r.KeyViolations > 0 {
		return fmt.Sprintf("FAIL (%d keys with two different effects; DB: %s)", r.KeyViolations, st)
	}
	return st
}

func writeViolations(b *strings.Builder, rep *invariants.Report) {
	if rep == nil {
		return
	}
	for _, v := range rep.Violations {
		fmt.Fprintf(b, "  - `%s`: %s\n", v.Invariant, v.Detail)
	}
}

// invStatus is "ok" or "FAIL (n)" for the named checker invariants.
func invStatus(rep *invariants.Report, names ...string) string {
	if rep == nil {
		return "not checked"
	}
	n := 0
	for _, v := range rep.Violations {
		for _, name := range names {
			if v.Invariant == name {
				n++
			}
		}
	}
	if n == 0 {
		return "ok"
	}
	return fmt.Sprintf("FAIL (%d)", n)
}

func okFail(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// AppendMarkdown appends the rendered section to path (e.g. docs/results.md).
func AppendMarkdown(path string, rep Report) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(Markdown(rep))
	return err
}
