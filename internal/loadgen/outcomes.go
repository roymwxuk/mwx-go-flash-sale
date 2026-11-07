package loadgen

import (
	"errors"
	"net"
	"net/http"
	"sort"
	"time"
)

// sample is one finished request. start is when the request was *meant* to
// be sent (the barrier release, or its slot in a constant-rate schedule), so
// time spent queueing inside the client counts as latency. Measuring from the
// actual send would hide that queueing ("coordinated omission").
type sample struct {
	status int
	err    error
	start  time.Time
	end    time.Time
}

// Outcomes classifies responses. Test plan section 3: 409 is a successful
// answer (sold out), 503 is the only allowed 5xx, and nothing may hang.
type Outcomes struct {
	Requests        int `json:"requests"`
	Created         int `json:"created_201"`
	SoldOut         int `json:"sold_out_409"`
	Unavailable     int `json:"unavailable_503"`
	Server5xx       int `json:"server_5xx_other"` // 5xx other than 503: H1 violation
	OtherStatus     int `json:"other_status"`     // unexpected 2xx/4xx
	TransportErrors int `json:"transport_errors"` // reset, refused, ...: H1 violation
	// Hung: the client gave up (timeout) or the response took longer than
	// client timeout + 1 s. H1 violation.
	Hung int `json:"hung"`
}

// H1 is the test plan's "no 5xx except an explicit 503, no hung requests".
// Transport errors also fail it: a request that never got an answer is not
// "handled", whatever the reason.
func (o Outcomes) H1() bool {
	return o.Server5xx == 0 && o.Hung == 0 && o.TransportErrors == 0
}

// Errors is everything that is neither a 201 nor a 409.
func (o Outcomes) Errors() int {
	return o.Unavailable + o.Server5xx + o.OtherStatus + o.TransportErrors + o.Hung
}

type summary struct {
	Outcomes
	all, created, soldOut []time.Duration
	first, last           time.Time // earliest start, latest end
	lastCreated           time.Time
	peakInFlight          int
	errorSamples          []string
}

func summarize(samples []sample, hungAfter time.Duration) summary {
	var s summary
	s.Requests = len(samples)
	for _, x := range samples {
		if s.first.IsZero() || x.start.Before(s.first) {
			s.first = x.start
		}
		if x.end.After(s.last) {
			s.last = x.end
		}
		d := x.end.Sub(x.start)
		if x.err != nil {
			var ne net.Error
			if errors.As(x.err, &ne) && ne.Timeout() {
				s.Hung++
			} else {
				s.TransportErrors++
			}
			if len(s.errorSamples) < 3 {
				s.errorSamples = append(s.errorSamples, x.err.Error())
			}
			continue
		}
		s.all = append(s.all, d)
		if hungAfter > 0 && d > hungAfter {
			s.Hung++
		}
		switch {
		case x.status == http.StatusCreated:
			s.Created++
			s.created = append(s.created, d)
			if x.end.After(s.lastCreated) {
				s.lastCreated = x.end
			}
		case x.status == http.StatusConflict:
			s.SoldOut++
			s.soldOut = append(s.soldOut, d)
		case x.status == http.StatusServiceUnavailable:
			s.Unavailable++
		case x.status >= 500:
			s.Server5xx++
		default:
			s.OtherStatus++
		}
	}
	s.peakInFlight = peakInFlight(samples)
	return s
}

// peakInFlight is the maximum number of requests open at the same moment,
// found by sweeping +1 at each start and -1 at each end in time order.
func peakInFlight(samples []sample) int {
	type event struct {
		at    time.Time
		delta int
	}
	ev := make([]event, 0, 2*len(samples))
	for _, x := range samples {
		ev = append(ev, event{x.start, +1}, event{x.end, -1})
	}
	// At equal times, process ends first so back-to-back requests don't
	// count as overlapping.
	sort.Slice(ev, func(i, j int) bool {
		if ev[i].at.Equal(ev[j].at) {
			return ev[i].delta < ev[j].delta
		}
		return ev[i].at.Before(ev[j].at)
	})
	cur, peak := 0, 0
	for _, e := range ev {
		cur += e.delta
		peak = max(peak, cur)
	}
	return peak
}

// ConfirmStats summarises POST /reservations/{id}/confirm calls. 200 is a
// confirmed purchase and 402 a declined payment (hold released); both are
// handled answers. 503 is allowed (payment outcome unknown, retry).
type ConfirmStats struct {
	Requests        int     `json:"requests"`
	Confirmed       int     `json:"confirmed_200"`
	Declined        int     `json:"declined_402"`
	Unavailable     int     `json:"unavailable_503"`
	Server5xx       int     `json:"server_5xx_other"`
	OtherStatus     int     `json:"other_status"`
	TransportErrors int     `json:"transport_errors"`
	Hung            int     `json:"hung"`
	Latency         Latency `json:"latency"`
}

func (c ConfirmStats) H1() bool {
	return c.Server5xx == 0 && c.Hung == 0 && c.TransportErrors == 0
}

func summarizeConfirms(samples []sample, hungAfter time.Duration) ConfirmStats {
	var c ConfirmStats
	var lat []time.Duration
	for _, x := range samples {
		c.Requests++
		if x.err != nil {
			var ne net.Error
			if errors.As(x.err, &ne) && ne.Timeout() {
				c.Hung++
			} else {
				c.TransportErrors++
			}
			continue
		}
		d := x.end.Sub(x.start)
		lat = append(lat, d)
		if hungAfter > 0 && d > hungAfter {
			c.Hung++
		}
		switch {
		case x.status == http.StatusOK:
			c.Confirmed++
		case x.status == http.StatusPaymentRequired:
			c.Declined++
		case x.status == http.StatusServiceUnavailable:
			c.Unavailable++
		case x.status >= 500:
			c.Server5xx++
		default:
			c.OtherStatus++
		}
	}
	c.Latency = Summarize(lat)
	return c
}
