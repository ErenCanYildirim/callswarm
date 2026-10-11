package check

import (
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/summary"
)

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]Limit{
		"p95=800ms":  {P95, 800 * time.Millisecond},
		"P99 = 1.5s": {P99, 1500 * time.Millisecond},
		"max=2s":     {Max, 2 * time.Second},
		"mean=300ms": {Mean, 300 * time.Millisecond},
	} {
		got, err := ParseLimit(in)
		if err != nil || got != want {
			t.Errorf("ParseLimit(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"p95":         "stat=duration",
		"p42=1s":      "unknown statistic",
		"p95=fast":    "invalid duration",
		"p95=-1s":     "must be positive",
		"p95=0s":      "must be positive",
		"median=10ms": "p50",
	} {
		if _, err := ParseLimit(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseLimit(%q) error %v, want mention of %q", in, err, want)
		}
	}
}

func sample() summary.Summary {
	return summary.Summary{
		Calls: 10, Failed: 1, Health: summary.Degraded,
		Turns: 30, Replied: 29,
		FirstAudio: summary.Distribution{Count: 29, Mean: 520 * time.Millisecond, P50: 500 * time.Millisecond,
			P95: 700 * time.Millisecond, P99: 900 * time.Millisecond, Max: time.Second},
		Interruptions: summary.Interruptions{Count: 10, Stopped: 9,
			StopLatency: summary.Distribution{Count: 10, P90: 300 * time.Millisecond}},
	}
}

func byName(results []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestEvaluate(t *testing.T) {
	results := Evaluate(sample(), Criteria{
		MaxFailed:    0,
		FailOn:       summary.Unreliable,
		FirstAudio:   []Limit{{P95, 800 * time.Millisecond}, {P99, 800 * time.Millisecond}},
		StopLatency:  []Limit{{P90, 400 * time.Millisecond}},
		MinReplyRate: 0.95,
		MinStopRate:  0.95,
	})
	want := map[string]bool{
		"failed calls":          false, // 1 > 0
		"health":                true,  // degraded is not unreliable
		"reply rate":            true,  // 96.7%
		"first audio p95":       true,
		"first audio p99":       false,
		"interruptions stopped": false, // 90%
		"stop latency p90":      true,
	}
	got := byName(results)
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(results), len(want), results)
	}
	for name, pass := range want {
		if r, ok := got[name]; !ok || r.Passed != pass {
			t.Errorf("%s: %+v, want passed=%v", name, r, pass)
		}
	}
	if Failed(results) != 3 {
		t.Errorf("Failed = %d, want 3", Failed(results))
	}
	if r := got["reply rate"]; r.Actual != "96.7% (29 of 30)" || r.Limit != "at least 95%" {
		t.Errorf("reply rate formatting: %+v", r)
	}
	if r := got["first audio p99"]; r.Actual != "900ms (n=29)" || r.Limit != "at most 800ms" {
		t.Errorf("limit formatting: %+v", r)
	}
	// The order is fixed so reports diff cleanly.
	if results[0].Name != "failed calls" || results[len(results)-1].Name != "stop latency p90" {
		t.Errorf("order: %+v", results)
	}
}

func TestHealthLevels(t *testing.T) {
	for _, tc := range []struct {
		failOn summary.Health
		health summary.Health
		pass   bool
	}{
		{summary.Unreliable, summary.Healthy, true},
		{summary.Unreliable, summary.Degraded, true},
		{summary.Unreliable, summary.Unreliable, false},
		{summary.Degraded, summary.Healthy, true},
		{summary.Degraded, summary.Degraded, false},
		{summary.Degraded, summary.Unreliable, false},
	} {
		r := Evaluate(summary.Summary{Health: tc.health}, Criteria{MaxFailed: -1, FailOn: tc.failOn})
		if len(r) != 1 || r[0].Passed != tc.pass {
			t.Errorf("fail on %s, health %s: %+v, want passed=%v", tc.failOn, tc.health, r, tc.pass)
		}
	}
}

// A limit with nothing to measure fails: the run did not test it.
func TestNoDataFails(t *testing.T) {
	results := Evaluate(summary.Summary{Calls: 3}, Criteria{
		MaxFailed:    -1,
		FirstAudio:   []Limit{{P95, time.Second}},
		StopLatency:  []Limit{{Max, time.Second}},
		MinReplyRate: 1,
		MinStopRate:  1,
	})
	if len(results) != 4 || Failed(results) != 4 {
		t.Fatalf("want 4 failed results, got %+v", results)
	}
	for _, r := range results {
		if !strings.HasPrefix(r.Actual, "no ") {
			t.Errorf("%s: actual %q, want an explanation", r.Name, r.Actual)
		}
	}
}

func TestDisabledChecks(t *testing.T) {
	if r := Evaluate(sample(), Criteria{MaxFailed: -1}); len(r) != 0 {
		t.Errorf("no criteria produced results: %+v", r)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []Criteria{
		{FailOn: "healthy"},
		{MinReplyRate: 1.5},
		{MinStopRate: -0.1},
	} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if err := (Criteria{FailOn: summary.Degraded, MinReplyRate: 1}).Validate(); err != nil {
		t.Error(err)
	}
}
