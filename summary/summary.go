// Package summary condenses a load run into distributions and a verdict on
// whether the numbers can be trusted.
//
// Percentiles use the nearest-rank method: p99 of 50 samples is the 50th
// smallest, the maximum. With few samples the high percentiles are just the
// largest values, so each distribution carries its sample count.
//
// The trust verdict comes from the caller's own pacing. Every call streams a
// frame every 20 ms; when the machine running callswarm is overloaded, frames
// go out late, and the same scheduling delays hit the timestamps on the
// agent's audio, inflating measured latency. A run where too many frames were
// late is flagged, because its tail latency describes the test machine as
// much as the agent.
package summary

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/transport"
)

// Distribution describes a set of durations.
type Distribution struct {
	Count int
	Min   time.Duration
	Mean  time.Duration
	P50   time.Duration
	P90   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

// Distribute computes a distribution. The input is not modified.
func Distribute(values []time.Duration) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	v := slices.Clone(values)
	slices.Sort(v)
	var sum time.Duration
	for _, x := range v {
		sum += x
	}
	return Distribution{
		Count: len(v),
		Min:   v[0],
		Mean:  sum / time.Duration(len(v)),
		P50:   Percentile(v, 50),
		P90:   Percentile(v, 90),
		P95:   Percentile(v, 95),
		P99:   Percentile(v, 99),
		Max:   v[len(v)-1],
	}
}

// Percentile returns the nearest-rank p-th percentile of sorted values: the
// smallest value with at least p percent of values at or below it.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// Thresholds decide when pacing problems make a run untrustworthy.
type Thresholds struct {
	// WarnLateRatio is the share of late frames (more than 5 ms past
	// schedule) above which tail percentiles are flagged as possibly
	// inflated. Default 0.01.
	WarnLateRatio float64
	// UnreliableLateRatio is the share of late frames above which the run's
	// latency numbers are flagged as unreliable. Default 0.05.
	UnreliableLateRatio float64
	// MinSamples is the sample count below which p99 is flagged as not
	// meaningful. Default 100.
	MinSamples int
}

// DefaultThresholds were calibrated by running increasing numbers of calls
// against the fake agent (true first audio 500 ms) on a 2-core machine with
// the agent in the same process, and comparing measured to true latency:
//
//	late frames   p50 error   p99 error   verdict
//	   ≤ 0.5%       ≤ 1 ms     ≤ 15 ms    healthy
//	   2-3.5%         2 ms     42-47 ms   degraded
//	    7.4%          4 ms      112 ms    unreliable
//	     14%         14 ms      399 ms    unreliable
//	     34%        172 ms      768 ms    unreliable
//
// The share of late frames tracks measurement error closely, so it is a
// machine-independent signal; the absolute call counts are not.
var DefaultThresholds = Thresholds{WarnLateRatio: 0.01, UnreliableLateRatio: 0.05, MinSamples: 100}

// WithDefaults returns t with zero fields set to the defaults.
func (t Thresholds) WithDefaults() Thresholds {
	if t.WarnLateRatio == 0 {
		t.WarnLateRatio = DefaultThresholds.WarnLateRatio
	}
	if t.UnreliableLateRatio == 0 {
		t.UnreliableLateRatio = DefaultThresholds.UnreliableLateRatio
	}
	if t.MinSamples == 0 {
		t.MinSamples = DefaultThresholds.MinSamples
	}
	return t
}

// Pacing is the caller's real-time pacing across all calls.
type Pacing struct {
	FramesSent   int
	LateFrames   int
	LateRatio    float64
	MeanLateness time.Duration
	MaxLateness  time.Duration
}

// Interruptions summarizes interrupt steps across calls.
type Interruptions struct {
	Count   int
	Stopped int // stopped within the threshold
	Cleared int // sent a clear
	// StopLatency covers interruptions where the agent's speech ended
	// during the call, whether or not within the threshold.
	StopLatency Distribution
}

// Bucket is first-audio latency for turns that happened while a given range
// of calls was active.
type Bucket struct {
	MinActive  int
	MaxActive  int
	FirstAudio Distribution
}

// Health is the verdict on whether the numbers can be trusted.
type Health string

// Health levels.
const (
	Healthy    Health = "healthy"
	Degraded   Health = "degraded"   // tail percentiles may be inflated
	Unreliable Health = "unreliable" // latency reflects the test machine
)

// Summary is a load run condensed.
type Summary struct {
	Calls     int
	Failed    int
	Cancelled bool
	MaxActive int
	Duration  time.Duration

	// Turns counts say steps that were played; Replied counts those the
	// agent answered, and Overlaps those where it started before the
	// caller finished.
	Turns    int
	Replied  int
	Overlaps int
	// FirstAudio covers replied turns.
	FirstAudio Distribution
	// ByConcurrency splits FirstAudio by how many calls were active when
	// the caller finished speaking, to show how latency changes with load.
	ByConcurrency []Bucket

	Interruptions Interruptions
	Pacing        Pacing

	Health   Health
	Warnings []string
}

// sample is one replied turn with the load at the time.
type sample struct {
	firstAudio time.Duration
	active     int
}

// Summarize condenses a report.
func Summarize(rep *load.Report, th Thresholds) Summary {
	th = th.WithDefaults()
	s := Summary{
		Calls:     len(rep.Calls),
		Failed:    rep.Failed(),
		Cancelled: rep.Cancelled,
		MaxActive: rep.MaxActive,
		Duration:  rep.Ended.Sub(rep.Started),
	}

	var samples []sample
	var firstAudio, stopLatency []time.Duration
	var pacing []transport.Stats
	for _, c := range rep.Calls {
		pacing = append(pacing, c.Pacing)
		for _, t := range c.Metrics.Turns {
			s.Turns++
			if !t.Replied {
				continue
			}
			s.Replied++
			if t.Overlap() {
				s.Overlaps++
			}
			firstAudio = append(firstAudio, t.FirstAudio)
			samples = append(samples, sample{t.FirstAudio, activeAt(rep.Calls, t.CallerSpeechEnded)})
		}
		for _, in := range c.Metrics.Interruptions {
			s.Interruptions.Count++
			if in.Stopped {
				s.Interruptions.Stopped++
			}
			if in.Cleared {
				s.Interruptions.Cleared++
			}
			if !in.AgentStopped.IsZero() {
				stopLatency = append(stopLatency, in.StopLatency)
			}
		}
	}
	s.FirstAudio = Distribute(firstAudio)
	s.ByConcurrency = buckets(samples, s.MaxActive)
	s.Interruptions.StopLatency = Distribute(stopLatency)
	s.Pacing = combinePacing(pacing)
	s.Health, s.Warnings = judge(s, th)
	return s
}

// activeAt counts calls in progress at t.
func activeAt(calls []load.Call, t time.Time) int {
	n := 0
	for _, c := range calls {
		if !c.Started.After(t) && c.Ended.After(t) {
			n++
		}
	}
	return n
}

// buckets splits samples into up to four equal ranges of concurrency.
func buckets(samples []sample, maxActive int) []Bucket {
	if len(samples) == 0 || maxActive < 1 {
		return nil
	}
	n := min(4, maxActive)
	var out []Bucket
	for i := range n {
		lo := 1 + i*maxActive/n
		hi := (i + 1) * maxActive / n
		var vals []time.Duration
		for _, sm := range samples {
			if sm.active >= lo && sm.active <= hi {
				vals = append(vals, sm.firstAudio)
			}
		}
		if len(vals) > 0 {
			out = append(out, Bucket{MinActive: lo, MaxActive: hi, FirstAudio: Distribute(vals)})
		}
	}
	return out
}

func combinePacing(stats []transport.Stats) Pacing {
	var p Pacing
	var total time.Duration
	for _, s := range stats {
		p.FramesSent += s.FramesSent
		p.LateFrames += s.LateFrames
		total += s.MeanLateness * time.Duration(s.FramesSent)
		p.MaxLateness = max(p.MaxLateness, s.MaxLateness)
	}
	if p.FramesSent > 0 {
		p.LateRatio = float64(p.LateFrames) / float64(p.FramesSent)
		p.MeanLateness = total / time.Duration(p.FramesSent)
	}
	return p
}

// judge decides how far the numbers can be trusted and why.
func judge(s Summary, th Thresholds) (Health, []string) {
	health := Healthy
	var warnings []string
	worsen := func(h Health) {
		if h == Unreliable || (h == Degraded && health == Healthy) {
			health = h
		}
	}

	switch r := s.Pacing.LateRatio; {
	case r > th.UnreliableLateRatio:
		worsen(Unreliable)
		warnings = append(warnings, fmt.Sprintf(
			"%.1f%% of caller audio frames went out late (mean %v, worst %v): this machine was overloaded, "+
				"so measured latency includes its own delays. Reduce concurrency or use a bigger machine.",
			r*100, s.Pacing.MeanLateness.Round(10*time.Microsecond), s.Pacing.MaxLateness.Round(time.Millisecond)))
	case r > th.WarnLateRatio:
		worsen(Degraded)
		warnings = append(warnings, fmt.Sprintf(
			"%.1f%% of caller audio frames went out late (worst %v): tail percentiles may be inflated by this machine.",
			r*100, s.Pacing.MaxLateness.Round(time.Millisecond)))
	}
	if s.FirstAudio.Count > 0 && s.FirstAudio.Count < th.MinSamples {
		warnings = append(warnings, fmt.Sprintf(
			"only %d first-audio samples: p99 is close to the maximum and not a stable estimate.", s.FirstAudio.Count))
	}
	if s.Failed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d of %d calls failed; their completed steps are included.", s.Failed, s.Calls))
	}
	if s.Cancelled {
		warnings = append(warnings, "the run was cancelled before it finished.")
	}
	if noReply := s.Turns - s.Replied; noReply > 0 {
		warnings = append(warnings, fmt.Sprintf("the agent never replied in %d of %d turns.", noReply, s.Turns))
	}
	return health, warnings
}
