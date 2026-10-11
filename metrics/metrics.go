// Package metrics computes how a voice agent behaved during a scripted call,
// from the call's timeline.
//
// Two measurements are made:
//
// Time to first audio, for every turn where the caller speaks (say steps):
// from the end of the caller's speech to the start of the agent's next
// speech, both as heard on the line. Speech boundaries come from voice
// activity detection, not from clip or message boundaries, so silence at the
// end of a recording or a burst of silent frames from the agent doesn't
// distort it. It is negative when the agent started talking before the
// caller finished.
//
// Interruption handling, for every interrupt step: from the moment the
// caller's interrupting speech starts to the moment the agent's speech stops,
// and whether the agent sent a clear to drop its buffered audio. An agent
// that ignores the interruption keeps talking until its reply is done, so a
// stop that takes longer than Options.StopWithin counts as not handled.
package metrics

import (
	"time"

	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/timeline"
)

// DefaultStopWithin is how quickly an agent must stop talking after being
// interrupted for the interruption to count as handled.
const DefaultStopWithin = time.Second

// Options tunes how metrics are judged.
type Options struct {
	// StopWithin is the longest an agent may keep talking after the caller
	// interrupts and still count as having stopped. Default 1 s.
	StopWithin time.Duration
}

// Turn is the agent's response to one thing the caller said.
type Turn struct {
	// Step is the say step, from 1.
	Step int
	// CallerSpeechEnded is when the caller's words ended.
	CallerSpeechEnded time.Time
	// Replied reports whether the agent started speaking before the
	// caller's next speech (or the end of the run).
	Replied bool
	// AgentAudioStarted is when the agent's reply was first heard.
	AgentAudioStarted time.Time
	// FirstAudio is AgentAudioStarted minus CallerSpeechEnded. Negative
	// when the agent started before the caller finished.
	FirstAudio time.Duration
}

// Overlap reports whether the agent started talking before the caller
// finished.
func (t Turn) Overlap() bool { return t.Replied && t.FirstAudio < 0 }

// Interruption is how the agent reacted to being talked over.
type Interruption struct {
	// Step is the interrupt step, from 1.
	Step int
	// CallerSpeechStarted is when the caller's interrupting words began.
	CallerSpeechStarted time.Time
	// AgentStopped is when the interrupted agent speech ended. Zero if it
	// never ended during the run.
	AgentStopped time.Time
	// StopLatency is AgentStopped minus CallerSpeechStarted: how long the
	// two talked over each other.
	StopLatency time.Duration
	// Cleared reports whether the agent sent a clear between the
	// interruption and the end of its speech, or just after it (see
	// clearGrace).
	Cleared bool
	// Stopped reports whether the agent stopped within Options.StopWithin.
	Stopped bool
}

// Call is the metrics for one call.
type Call struct {
	Scenario      string
	Turns         []Turn
	Interruptions []Interruption
	// Err is the run's error, if it failed. Metrics cover the steps that
	// ran.
	Err error
}

// FromResult computes the metrics for a finished run.
func FromResult(res *scenario.Result, opts Options) Call {
	if opts.StopWithin <= 0 {
		opts.StopWithin = DefaultStopWithin
	}
	c := Call{Scenario: res.Scenario, Err: res.Err}
	tl := res.Timeline
	for _, sr := range res.Steps {
		switch sr.Kind {
		case scenario.KindSay:
			if t, ok := turn(tl, sr.Index); ok {
				c.Turns = append(c.Turns, t)
			}
		case scenario.KindInterrupt:
			if in, ok := interruption(tl, sr.Index, opts); ok {
				c.Interruptions = append(c.Interruptions, in)
			}
		}
	}
	return c
}

// turn measures the reply to a say step. A step whose clip was never sent
// has no turn.
func turn(tl *timeline.Timeline, step int) (Turn, bool) {
	start, ok := tl.First(timeline.CallerSpeechStarted, step)
	if !ok {
		return Turn{}, false
	}
	end, _ := tl.Last(timeline.CallerSpeechEnded, step)
	t := Turn{Step: step, CallerSpeechEnded: end.At}

	// The reply is the first agent speech after the caller started talking
	// and before the caller's next turn. Agent speech that began earlier
	// belongs to a previous turn.
	until := nextTurn(tl, step)
	for _, e := range tl.Filter(timeline.AgentAudioStarted, 0) {
		if e.At.After(start.At) && (until.IsZero() || e.At.Before(until)) {
			t.Replied = true
			t.AgentAudioStarted = e.At
			t.FirstAudio = e.At.Sub(end.At)
			break
		}
	}
	return t, true
}

// clearGrace is how long after the agent's audio stops a clear still counts
// as part of stopping. An agent that streams in real time has almost nothing
// queued, so when it stops sending, playback can run dry a few milliseconds
// before its clear message arrives behind the last audio.
const clearGrace = 100 * time.Millisecond

// interruption measures the reaction to an interrupt step.
func interruption(tl *timeline.Timeline, step int, opts Options) (Interruption, bool) {
	start, ok := tl.First(timeline.CallerSpeechStarted, step)
	if !ok {
		return Interruption{}, false
	}
	in := Interruption{Step: step, CallerSpeechStarted: start.At}

	// The interrupted speech is the agent speech in progress when the
	// caller started; it ends at the first stop after that.
	for _, e := range tl.Filter(timeline.AgentAudioStopped, 0) {
		if e.At.After(start.At) {
			in.AgentStopped = e.At
			in.StopLatency = e.At.Sub(start.At)
			in.Stopped = in.StopLatency <= opts.StopWithin
			break
		}
	}
	for _, e := range tl.Filter(timeline.AgentClear, 0) {
		if !e.At.Before(start.At) && (in.AgentStopped.IsZero() || !e.At.After(in.AgentStopped.Add(clearGrace))) {
			in.Cleared = true
			break
		}
	}
	return in, true
}

// nextTurn is when the caller next starts speaking in a later step, or
// zero. Pauses between sentences of the same clip don't end the turn.
func nextTurn(tl *timeline.Timeline, step int) time.Time {
	for _, e := range tl.Filter(timeline.CallerSpeechStarted, 0) {
		if e.Step > step {
			return e.At
		}
	}
	return time.Time{}
}
