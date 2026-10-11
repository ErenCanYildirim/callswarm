// Package report writes the results of a load run: a JSON document for
// machines and a plain-text summary for people.
//
// The JSON document has a schema version. Fields are only added within a
// version; anything renamed or removed bumps it. Durations are milliseconds
// with microsecond precision, and times within a call are offsets from the
// run's start, so documents diff cleanly between runs.
package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/ErenCanYildirim/callswarm/check"
	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/summary"
	"github.com/ErenCanYildirim/callswarm/timeline"
)

// SchemaVersion is the version of the JSON document format.
const SchemaVersion = 1

// Options controls what the JSON document includes.
type Options struct {
	// Timelines includes every call's event log. They are the best way to
	// investigate an odd number, at roughly 3 KB per call.
	Timelines bool
	// Checks are pass/fail results to include. Nil leaves them out.
	Checks []check.Result
}

// Document is the JSON report.
type Document struct {
	SchemaVersion int         `json:"schema_version"`
	StartedAt     time.Time   `json:"started_at"`
	Manifest      Manifest    `json:"manifest"`
	Summary       jsonSummary `json:"summary"`
	Checks        *jsonChecks `json:"checks,omitempty"`
	Calls         []jsonCall  `json:"calls"`
}

type jsonChecks struct {
	Passed  bool           `json:"passed"`
	Failed  int            `json:"failed"`
	Results []check.Result `json:"results"`
}

type jsonDist struct {
	Count  int     `json:"count"`
	MinMS  float64 `json:"min_ms"`
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P90MS  float64 `json:"p90_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

type jsonBucket struct {
	MinActive  int      `json:"min_active"`
	MaxActive  int      `json:"max_active"`
	FirstAudio jsonDist `json:"first_audio"`
}

type jsonInterruptions struct {
	Count       int      `json:"count"`
	Stopped     int      `json:"stopped"`
	Cleared     int      `json:"cleared"`
	StopLatency jsonDist `json:"stop_latency"`
}

type jsonPacing struct {
	FramesSent     int     `json:"frames_sent"`
	LateFrames     int     `json:"late_frames"`
	LateRatio      float64 `json:"late_ratio"`
	MeanLatenessMS float64 `json:"mean_lateness_ms"`
	MaxLatenessMS  float64 `json:"max_lateness_ms"`
}

type jsonSummary struct {
	Health        summary.Health    `json:"health"`
	Warnings      []string          `json:"warnings"`
	Calls         int               `json:"calls"`
	Failed        int               `json:"failed"`
	Cancelled     bool              `json:"cancelled"`
	MaxActive     int               `json:"max_active"`
	DurationMS    float64           `json:"duration_ms"`
	Turns         int               `json:"turns"`
	Replied       int               `json:"replied"`
	Overlaps      int               `json:"overlaps"`
	FirstAudio    jsonDist          `json:"first_audio"`
	ByConcurrency []jsonBucket      `json:"first_audio_by_concurrency"`
	Interruptions jsonInterruptions `json:"interruptions"`
	Pacing        jsonPacing        `json:"pacing"`
}

type jsonTurn struct {
	Step         int     `json:"step"`
	Replied      bool    `json:"replied"`
	FirstAudioMS float64 `json:"first_audio_ms,omitempty"`
	Overlap      bool    `json:"overlap,omitempty"`
}

type jsonInterruption struct {
	Step          int     `json:"step"`
	StopLatencyMS float64 `json:"stop_latency_ms,omitempty"`
	AgentStopped  bool    `json:"agent_stopped"`
	Stopped       bool    `json:"stopped_in_time"`
	Cleared       bool    `json:"cleared"`
}

type jsonCall struct {
	ID            int                `json:"id"`
	ActiveAtStart int                `json:"active_at_start"`
	StartMS       float64            `json:"start_ms"`
	DurationMS    float64            `json:"duration_ms"`
	Error         string             `json:"error,omitempty"`
	Turns         []jsonTurn         `json:"turns"`
	Interruptions []jsonInterruption `json:"interruptions"`
	Pacing        jsonPacing         `json:"pacing"`
	Timeline      *timeline.Timeline `json:"timeline,omitempty"`
}

// Build assembles the JSON document for a run.
func Build(rep *load.Report, s summary.Summary, m Manifest, opts Options) Document {
	doc := Document{
		SchemaVersion: SchemaVersion,
		StartedAt:     rep.Started.UTC(),
		Manifest:      m,
		Summary: jsonSummary{
			Health:        s.Health,
			Warnings:      nonNil(s.Warnings),
			Calls:         s.Calls,
			Failed:        s.Failed,
			Cancelled:     s.Cancelled,
			MaxActive:     s.MaxActive,
			DurationMS:    ms(s.Duration),
			Turns:         s.Turns,
			Replied:       s.Replied,
			Overlaps:      s.Overlaps,
			FirstAudio:    dist(s.FirstAudio),
			ByConcurrency: []jsonBucket{},
			Interruptions: jsonInterruptions{
				Count:       s.Interruptions.Count,
				Stopped:     s.Interruptions.Stopped,
				Cleared:     s.Interruptions.Cleared,
				StopLatency: dist(s.Interruptions.StopLatency),
			},
			Pacing: pacing(s.Pacing.FramesSent, s.Pacing.LateFrames, s.Pacing.LateRatio, s.Pacing.MeanLateness, s.Pacing.MaxLateness),
		},
		Calls: []jsonCall{},
	}
	if opts.Checks != nil {
		failed := check.Failed(opts.Checks)
		doc.Checks = &jsonChecks{Passed: failed == 0, Failed: failed, Results: opts.Checks}
	}
	for _, b := range s.ByConcurrency {
		doc.Summary.ByConcurrency = append(doc.Summary.ByConcurrency, jsonBucket{b.MinActive, b.MaxActive, dist(b.FirstAudio)})
	}

	for _, c := range rep.Calls {
		jc := jsonCall{
			ID:            c.ID,
			ActiveAtStart: c.ActiveAtStart,
			StartMS:       ms(c.Started.Sub(rep.Started)),
			DurationMS:    ms(c.Ended.Sub(c.Started)),
			Turns:         []jsonTurn{},
			Interruptions: []jsonInterruption{},
		}
		if c.Err != nil {
			jc.Error = c.Err.Error()
		}
		late := 0.0
		if c.Pacing.FramesSent > 0 {
			late = float64(c.Pacing.LateFrames) / float64(c.Pacing.FramesSent)
		}
		jc.Pacing = pacing(c.Pacing.FramesSent, c.Pacing.LateFrames, late, c.Pacing.MeanLateness, c.Pacing.MaxLateness)
		for _, t := range c.Metrics.Turns {
			jt := jsonTurn{Step: t.Step, Replied: t.Replied, Overlap: t.Overlap()}
			if t.Replied {
				jt.FirstAudioMS = ms(t.FirstAudio)
			}
			jc.Turns = append(jc.Turns, jt)
		}
		for _, in := range c.Metrics.Interruptions {
			ji := jsonInterruption{Step: in.Step, AgentStopped: !in.AgentStopped.IsZero(), Stopped: in.Stopped, Cleared: in.Cleared}
			if ji.AgentStopped {
				ji.StopLatencyMS = ms(in.StopLatency)
			}
			jc.Interruptions = append(jc.Interruptions, ji)
		}
		if opts.Timelines && c.Result != nil {
			jc.Timeline = c.Result.Timeline
		}
		doc.Calls = append(doc.Calls, jc)
	}
	return doc
}

// WriteJSON writes the document, indented.
func WriteJSON(w io.Writer, doc Document) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func dist(d summary.Distribution) jsonDist {
	return jsonDist{
		Count: d.Count, MinMS: ms(d.Min), MeanMS: ms(d.Mean),
		P50MS: ms(d.P50), P90MS: ms(d.P90), P95MS: ms(d.P95), P99MS: ms(d.P99), MaxMS: ms(d.Max),
	}
}

func pacing(sent, late int, ratio float64, mean, maxLate time.Duration) jsonPacing {
	return jsonPacing{FramesSent: sent, LateFrames: late, LateRatio: ratio, MeanLatenessMS: ms(mean), MaxLatenessMS: ms(maxLate)}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
