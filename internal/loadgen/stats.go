package loadgen

import (
	"sort"
	"time"
)

// Latency is a summary of request durations, in milliseconds.
type Latency struct {
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
}

// Summarize computes exact percentiles by sorting every sample. With at most
// tens of thousands of samples per run, sorting is cheap and avoids the
// bucket-boundary error of a histogram.
func Summarize(samples []time.Duration) Latency {
	if len(samples) == 0 {
		return Latency{}
	}
	s := append([]time.Duration(nil), samples...) // don't reorder the caller's slice
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })

	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return Latency{
		P50:  ms(percentile(s, 50)),
		P95:  ms(percentile(s, 95)),
		P99:  ms(percentile(s, 99)),
		Max:  ms(s[len(s)-1]),
		Mean: ms(sum / time.Duration(len(s))),
	}
}

// percentile uses the nearest-rank method on a sorted slice: the smallest
// sample such that at least p% of samples are <= it.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
