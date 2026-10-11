// Package check turns a run summary into pass/fail results, so a run can
// gate a CI pipeline.
//
// A check without data fails rather than passes: a limit on p95 first audio
// when the agent never replied, or a required interruption stop rate when
// the scenario has no interruptions, means the run did not test what the
// limit asks about.
package check

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ErenCanYildirim/callswarm/summary"
)

// Stat names a statistic of a latency distribution.
type Stat string

// Statistics a limit can apply to.
const (
	Mean Stat = "mean"
	P50  Stat = "p50"
	P90  Stat = "p90"
	P95  Stat = "p95"
	P99  Stat = "p99"
	Max  Stat = "max"
)

var stats = []Stat{Mean, P50, P90, P95, P99, Max}

func (s Stat) of(d summary.Distribution) (time.Duration, bool) {
	switch s {
	case Mean:
		return d.Mean, true
	case P50:
		return d.P50, true
	case P90:
		return d.P90, true
	case P95:
		return d.P95, true
	case P99:
		return d.P99, true
	case Max:
		return d.Max, true
	}
	return 0, false
}

// Limit is an upper bound on one statistic of a latency distribution.
type Limit struct {
	Stat Stat
	Max  time.Duration
}

// ParseLimit parses "p95=800ms".
func ParseLimit(s string) (Limit, error) {
	name, value, ok := strings.Cut(s, "=")
	if !ok {
		return Limit{}, fmt.Errorf("want stat=duration such as p95=800ms, got %q", s)
	}
	st := Stat(strings.ToLower(strings.TrimSpace(name)))
	if _, ok := st.of(summary.Distribution{}); !ok {
		return Limit{}, fmt.Errorf("unknown statistic %q, want one of %s", name, statList())
	}
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return Limit{}, fmt.Errorf("%s: %w", name, err)
	}
	if d <= 0 {
		return Limit{}, fmt.Errorf("%s: limit must be positive, got %v", name, d)
	}
	return Limit{Stat: st, Max: d}, nil
}

func statList() string {
	names := make([]string, len(stats))
	for i, s := range stats {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// Criteria are the conditions a run must meet.
type Criteria struct {
	// MaxFailed is how many calls may fail. Negative disables the check.
	MaxFailed int
	// FailOn is the health level at which the run fails: summary.Degraded
	// or summary.Unreliable. Empty disables the check.
	FailOn summary.Health
	// FirstAudio limits time to first audio across all replied turns.
	FirstAudio []Limit
	// StopLatency limits how long the agent kept talking after being
	// interrupted.
	StopLatency []Limit
	// MinReplyRate is the share of turns the agent must answer, 0 to 1.
	// Zero disables the check.
	MinReplyRate float64
	// MinStopRate is the share of interruptions where the agent must stop
	// within the run's stop-within time, 0 to 1. Zero disables the check.
	MinStopRate float64
}

// Validate reports criteria that cannot be evaluated.
func (c Criteria) Validate() error {
	switch c.FailOn {
	case "", summary.Degraded, summary.Unreliable:
	default:
		return fmt.Errorf("fail-on must be %s or %s, got %q", summary.Degraded, summary.Unreliable, c.FailOn)
	}
	if c.MinReplyRate < 0 || c.MinReplyRate > 1 {
		return errors.New("minimum reply rate must be between 0 and 1")
	}
	if c.MinStopRate < 0 || c.MinStopRate > 1 {
		return errors.New("minimum stop rate must be between 0 and 1")
	}
	return nil
}

// Result is the outcome of one check.
type Result struct {
	Name   string `json:"name"`
	Limit  string `json:"limit"`
	Actual string `json:"actual"`
	Passed bool   `json:"passed"`
}

// Evaluate checks a summary against criteria, in a fixed order.
func Evaluate(s summary.Summary, c Criteria) []Result {
	var out []Result
	if c.MaxFailed >= 0 {
		out = append(out, Result{
			Name:   "failed calls",
			Limit:  fmt.Sprintf("at most %d", c.MaxFailed),
			Actual: fmt.Sprintf("%d of %d", s.Failed, s.Calls),
			Passed: s.Failed <= c.MaxFailed,
		})
	}
	if c.FailOn != "" {
		bad := s.Health == summary.Unreliable || (c.FailOn == summary.Degraded && s.Health == summary.Degraded)
		limit := "healthy or degraded"
		if c.FailOn == summary.Degraded {
			limit = "healthy"
		}
		out = append(out, Result{Name: "health", Limit: limit, Actual: string(s.Health), Passed: !bad})
	}
	if c.MinReplyRate > 0 {
		out = append(out, rate("reply rate", s.Replied, s.Turns, c.MinReplyRate, "turns"))
	}
	out = append(out, limits("first audio", s.FirstAudio, c.FirstAudio, "replies")...)
	if c.MinStopRate > 0 {
		out = append(out, rate("interruptions stopped", s.Interruptions.Stopped, s.Interruptions.Count, c.MinStopRate, "interruptions"))
	}
	out = append(out, limits("stop latency", s.Interruptions.StopLatency, c.StopLatency, "samples")...)
	return out
}

func rate(name string, n, total int, minRate float64, noun string) Result {
	r := Result{Name: name, Limit: "at least " + pct(minRate)}
	if total == 0 {
		r.Actual = "no " + noun
		return r
	}
	got := float64(n) / float64(total)
	r.Actual = fmt.Sprintf("%s (%d of %d)", pct(got), n, total)
	r.Passed = got >= minRate
	return r
}

func limits(metric string, d summary.Distribution, ls []Limit, noun string) []Result {
	var out []Result
	for _, l := range ls {
		r := Result{Name: metric + " " + string(l.Stat), Limit: "at most " + l.Max.String()}
		if v, _ := l.Stat.of(d); d.Count == 0 {
			r.Actual = "no " + noun
		} else {
			r.Actual = fmt.Sprintf("%v (n=%d)", v.Round(100*time.Microsecond), d.Count)
			r.Passed = v <= l.Max
		}
		out = append(out, r)
	}
	return out
}

func pct(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/10, 'f', -1, 64) + "%"
}

// Failed counts results that did not pass.
func Failed(results []Result) int {
	n := 0
	for _, r := range results {
		if !r.Passed {
			n++
		}
	}
	return n
}
