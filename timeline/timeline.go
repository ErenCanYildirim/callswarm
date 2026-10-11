// Package timeline is the timestamped event log of one simulated call: what
// the caller did, when the caller and the agent were speaking, and what the
// agent sent. Metrics are computed from it, and it is the first thing to read
// when a number looks wrong.
package timeline

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Kind is the type of an event.
type Kind string

// Event kinds.
const (
	RunStarted Kind = "run_started"
	RunEnded   Kind = "run_ended"
	RunFailed  Kind = "run_failed"

	StepStarted Kind = "step_started"
	StepEnded   Kind = "step_ended"

	// Caller audio is the clip as sent; caller speech is the part of it
	// that is speech, found by voice activity detection. They differ by the
	// silence recorded before and after the words.
	CallerAudioStarted  Kind = "caller_audio_started"
	CallerAudioEnded    Kind = "caller_audio_ended"
	CallerSpeechStarted Kind = "caller_speech_started"
	CallerSpeechEnded   Kind = "caller_speech_ended"

	// Agent audio events mark agent speech as the caller heard it.
	AgentAudioStarted Kind = "agent_audio_started"
	AgentAudioStopped Kind = "agent_audio_stopped"

	AgentClear Kind = "agent_clear"
	AgentMark  Kind = "agent_mark"
)

// order breaks ties between events at the same instant, so cause comes
// before effect (a step starts before its audio does).
var order = map[Kind]int{
	RunStarted: 0, StepStarted: 1,
	CallerAudioStarted: 2, CallerSpeechStarted: 3, CallerSpeechEnded: 4, CallerAudioEnded: 5,
	AgentAudioStarted: 6, AgentClear: 7, AgentAudioStopped: 8, AgentMark: 9,
	StepEnded: 10, RunFailed: 11, RunEnded: 12,
}

// Event is one thing that happened during a call.
type Event struct {
	At   time.Time
	Kind Kind
	// Step is the scenario step the event belongs to, from 1, or 0 for
	// events outside any step.
	Step int
	// Detail is free-form context, such as a clip path or mark name.
	Detail string
}

// Timeline is a call's events in time order.
type Timeline struct {
	// Start is the reference time for offsets.
	Start  time.Time
	Events []Event
}

// New returns an empty timeline whose offsets count from start.
func New(start time.Time) *Timeline {
	return &Timeline{Start: start}
}

// Add records an event. Events may be added in any order; Sort orders them.
func (t *Timeline) Add(at time.Time, kind Kind, step int, detail string) {
	t.Events = append(t.Events, Event{At: at, Kind: kind, Step: step, Detail: detail})
}

// Sort puts events in time order.
func (t *Timeline) Sort() {
	sort.SliceStable(t.Events, func(i, j int) bool {
		a, b := t.Events[i], t.Events[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return order[a.Kind] < order[b.Kind]
	})
}

// Offset is the time from the timeline's start to e.
func (t *Timeline) Offset(e Event) time.Duration { return e.At.Sub(t.Start) }

// Filter returns the events of the given kind, in order. A step of 0
// matches any step.
func (t *Timeline) Filter(kind Kind, step int) []Event {
	var out []Event
	for _, e := range t.Events {
		if e.Kind == kind && (step == 0 || e.Step == step) {
			out = append(out, e)
		}
	}
	return out
}

// First returns the first event of the given kind and step (0 for any).
func (t *Timeline) First(kind Kind, step int) (Event, bool) {
	for _, e := range t.Events {
		if e.Kind == kind && (step == 0 || e.Step == step) {
			return e, true
		}
	}
	return Event{}, false
}

// Last returns the last event of the given kind and step (0 for any).
func (t *Timeline) Last(kind Kind, step int) (Event, bool) {
	for i := len(t.Events) - 1; i >= 0; i-- {
		if e := t.Events[i]; e.Kind == kind && (step == 0 || e.Step == step) {
			return e, true
		}
	}
	return Event{}, false
}

// WriteText writes the timeline in a readable form, one event per line:
//
//	+0.000s  step 1  step_started           say clips/hello.wav
//	+0.020s  step 1  caller_audio_started
func (t *Timeline) WriteText(w io.Writer) error {
	for _, e := range t.Events {
		step := "      "
		if e.Step > 0 {
			step = fmt.Sprintf("step %-2d", e.Step)
		}
		line := fmt.Sprintf("%+9.3fs  %s  %-22s %s", t.Offset(e).Seconds(), step, e.Kind, e.Detail)
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	return nil
}

// String is the text form.
func (t *Timeline) String() string {
	var b strings.Builder
	_ = t.WriteText(&b)
	return b.String()
}

// jsonTimeline is the JSON shape: offsets in milliseconds from the start,
// which is what tools plotting a timeline want.
type jsonTimeline struct {
	Start  time.Time   `json:"start"`
	Events []jsonEvent `json:"events"`
}

type jsonEvent struct {
	OffsetMS float64 `json:"offset_ms"`
	Kind     Kind    `json:"kind"`
	Step     int     `json:"step,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

// MarshalJSON encodes events with millisecond offsets from Start.
func (t *Timeline) MarshalJSON() ([]byte, error) {
	j := jsonTimeline{Start: t.Start, Events: make([]jsonEvent, len(t.Events))}
	for i, e := range t.Events {
		j.Events[i] = jsonEvent{
			OffsetMS: float64(t.Offset(e).Microseconds()) / 1000,
			Kind:     e.Kind,
			Step:     e.Step,
			Detail:   e.Detail,
		}
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes the format written by MarshalJSON, to microsecond
// precision.
func (t *Timeline) UnmarshalJSON(data []byte) error {
	var j jsonTimeline
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	t.Start = j.Start
	t.Events = make([]Event, len(j.Events))
	for i, e := range j.Events {
		off := time.Duration(e.OffsetMS * float64(time.Millisecond)).Round(time.Microsecond)
		t.Events[i] = Event{At: j.Start.Add(off), Kind: e.Kind, Step: e.Step, Detail: e.Detail}
	}
	return nil
}
