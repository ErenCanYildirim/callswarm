package summary

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/transport"
)

func msList(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}

func TestPercentileNearestRank(t *testing.T) {
	v := msList(10, 20, 30, 40, 50, 60, 70, 80, 90, 100)
	for _, tc := range []struct {
		p    float64
		want int
	}{{0, 10}, {10, 10}, {11, 20}, {50, 50}, {90, 90}, {99, 100}, {100, 100}} {
		if got := Percentile(v, tc.p); got != time.Duration(tc.want)*time.Millisecond {
			t.Errorf("p%v = %v, want %dms", tc.p, got, tc.want)
		}
	}
	if Percentile(nil, 50) != 0 {
		t.Error("empty input should give 0")
	}
}

func TestDistribute(t *testing.T) {
	in := msList(300, 100, 200, 400) // unsorted on purpose
	d := Distribute(in)
	want := Distribution{
		Count: 4, Min: 100 * time.Millisecond, Mean: 250 * time.Millisecond,
		P50: 200 * time.Millisecond, P90: 400 * time.Millisecond, P95: 400 * time.Millisecond,
		P99: 400 * time.Millisecond, Max: 400 * time.Millisecond,
	}
	if d != want {
		t.Errorf("got %+v\nwant %+v", d, want)
	}
	if in[0] != 300*time.Millisecond {
		t.Error("input was modified")
	}
	if (Distribute(nil) != Distribution{}) {
		t.Error("empty input should give a zero distribution")
	}
}

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// call builds a finished call with the given turns (speech end, first audio).
func call(id, startMS, endMS int, frames, late int, turns ...[2]int) load.Call {
	c := load.Call{
		ID: id, Started: at(startMS), Ended: at(endMS),
		Pacing: transport.Stats{FramesSent: frames, LateFrames: late, MeanLateness: time.Millisecond},
	}
	for _, tr := range turns {
		c.Metrics.Turns = append(c.Metrics.Turns, metrics.Turn{
			Replied: true, CallerSpeechEnded: at(tr[0]), FirstAudio: time.Duration(tr[1]) * time.Millisecond,
		})
	}
	return c
}

func TestSummarizeCountsAndConcurrency(t *testing.T) {
	rep := &load.Report{
		Started: at(0), Ended: at(10000), MaxActive: 4,
		Calls: []load.Call{
			// One call alone at first, then four overlapping.
			call(1, 0, 10000, 500, 0, [2]int{1000, 400}, [2]int{8000, 600}),
			call(2, 5000, 10000, 250, 0, [2]int{6000, 700}),
			call(3, 5000, 10000, 250, 0, [2]int{6000, 800}),
			call(4, 5000, 10000, 250, 0, [2]int{6000, -100}),
		},
	}
	rep.Calls[3].Metrics.Turns = append(rep.Calls[3].Metrics.Turns, metrics.Turn{}) // never replied
	rep.Calls[1].Err = errors.New("boom")

	s := Summarize(rep, Thresholds{})
	if s.Calls != 4 || s.Failed != 1 || s.Turns != 6 || s.Replied != 5 || s.Overlaps != 1 {
		t.Errorf("counts: calls=%d failed=%d turns=%d replied=%d overlaps=%d", s.Calls, s.Failed, s.Turns, s.Replied, s.Overlaps)
	}
	if s.FirstAudio.Count != 5 || s.FirstAudio.Max != 800*time.Millisecond {
		t.Errorf("first audio = %+v", s.FirstAudio)
	}
	// The turn at 1 s had one call active; the rest had four.
	if len(s.ByConcurrency) != 2 {
		t.Fatalf("buckets = %+v", s.ByConcurrency)
	}
	low, high := s.ByConcurrency[0], s.ByConcurrency[1]
	if low.MinActive != 1 || low.MaxActive != 1 || low.FirstAudio.Count != 1 || low.FirstAudio.Max != 400*time.Millisecond {
		t.Errorf("low bucket = %+v", low)
	}
	if high.MinActive != 4 || high.FirstAudio.Count != 4 {
		t.Errorf("high bucket = %+v", high)
	}
}

func TestPacingIsFrameWeighted(t *testing.T) {
	rep := &load.Report{Calls: []load.Call{
		{Pacing: transport.Stats{FramesSent: 900, LateFrames: 0, MeanLateness: time.Millisecond, MaxLateness: 3 * time.Millisecond}},
		{Pacing: transport.Stats{FramesSent: 100, LateFrames: 50, MeanLateness: 11 * time.Millisecond, MaxLateness: 40 * time.Millisecond}},
	}}
	p := Summarize(rep, Thresholds{}).Pacing
	if p.FramesSent != 1000 || p.LateFrames != 50 || p.LateRatio != 0.05 {
		t.Errorf("pacing = %+v", p)
	}
	if p.MeanLateness != 2*time.Millisecond || p.MaxLateness != 40*time.Millisecond {
		t.Errorf("mean %v (want 2ms), max %v (want 40ms)", p.MeanLateness, p.MaxLateness)
	}
}

func TestHealthVerdicts(t *testing.T) {
	turns := make([][2]int, 200) // enough samples not to warn about count
	for tc, want := range map[[2]int]Health{
		{1000, 0}:   Healthy,
		{1000, 10}:  Healthy, // 1.0% is not over the 1% line
		{1000, 11}:  Degraded,
		{1000, 50}:  Degraded, // 5.0% is not over the 5% line
		{1000, 51}:  Unreliable,
		{1000, 400}: Unreliable,
	} {
		rep := &load.Report{Calls: []load.Call{call(1, 0, 1000, tc[0], tc[1], turns...)}}
		s := Summarize(rep, Thresholds{})
		if s.Health != want {
			t.Errorf("%d/%d late: health %s, want %s", tc[1], tc[0], s.Health, want)
		}
		if want != Healthy && (len(s.Warnings) == 0 || !strings.Contains(s.Warnings[0], "late")) {
			t.Errorf("%d/%d late: warnings %q should explain the lateness", tc[1], tc[0], s.Warnings)
		}
		if want == Healthy && len(s.Warnings) != 0 {
			t.Errorf("%d/%d late: unexpected warnings %q", tc[1], tc[0], s.Warnings)
		}
	}
	// Custom thresholds change the verdict.
	rep := &load.Report{Calls: []load.Call{call(1, 0, 1000, 1000, 30, turns...)}}
	if h := Summarize(rep, Thresholds{WarnLateRatio: 0.05, UnreliableLateRatio: 0.2}).Health; h != Healthy {
		t.Errorf("3%% late with 5%% warn threshold: %s", h)
	}
}

func TestWarnings(t *testing.T) {
	rep := &load.Report{Cancelled: true, Calls: []load.Call{call(1, 0, 1000, 100, 0, [2]int{500, 400})}}
	rep.Calls[0].Metrics.Turns = append(rep.Calls[0].Metrics.Turns, metrics.Turn{})
	rep.Calls[0].Err = errors.New("x")
	w := strings.Join(Summarize(rep, Thresholds{}).Warnings, "\n")
	for _, want := range []string{"only 1 first-audio samples", "1 of 1 calls failed", "cancelled", "never replied in 1 of 2 turns"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings missing %q:\n%s", want, w)
		}
	}
}

func TestInterruptionSummary(t *testing.T) {
	c := load.Call{Metrics: metrics.Call{Interruptions: []metrics.Interruption{
		{Stopped: true, Cleared: true, AgentStopped: at(100), StopLatency: 200 * time.Millisecond},
		{Stopped: false, AgentStopped: at(200), StopLatency: 1500 * time.Millisecond},
		{Stopped: false}, // never stopped
	}}}
	in := Summarize(&load.Report{Calls: []load.Call{c}}, Thresholds{}).Interruptions
	if in.Count != 3 || in.Stopped != 1 || in.Cleared != 1 || in.StopLatency.Count != 2 || in.StopLatency.Max != 1500*time.Millisecond {
		t.Errorf("interruptions = %+v", in)
	}
}

// A light real run is healthy and accurate.
func TestLightLoadIsHealthyAndAccurate(t *testing.T) {
	s := Summarize(runLoad(t, 20), Thresholds{})
	if s.Failed != 0 || s.Replied != 40 {
		t.Fatalf("failed=%d replied=%d, want 0 and 40", s.Failed, s.Replied)
	}
	if s.Health != Healthy {
		t.Errorf("health %s at light load: %q", s.Health, s.Warnings)
	}
	want := agentCfg.ExpectedFirstAudio()
	if err := s.FirstAudio.P50 - want; err < -30*time.Millisecond || err > 30*time.Millisecond {
		t.Errorf("p50 first audio %v, want %v", s.FirstAudio.P50, want)
	}
	t.Logf("p50 %v p99 %v, late frames %.2f%%", s.FirstAudio.P50, s.FirstAudio.P99, s.Pacing.LateRatio*100)
}
