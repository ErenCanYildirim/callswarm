package metrics

import (
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/timeline"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func ms(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }

// event is a timeline entry for building test results by hand.
type event struct {
	at   int
	kind timeline.Kind
	step int
}

func result(kinds []scenario.Kind, events ...event) *scenario.Result {
	res := &scenario.Result{Scenario: "test", Timeline: timeline.New(t0)}
	for i, k := range kinds {
		res.Steps = append(res.Steps, scenario.StepResult{Index: i + 1, Kind: k})
	}
	for _, e := range events {
		res.Timeline.Add(ms(e.at), e.kind, e.step, "")
	}
	res.Timeline.Sort()
	return res
}

var (
	say       = scenario.KindSay
	interrupt = scenario.KindInterrupt
	pause     = scenario.KindPause
)

func TestFirstAudio(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say},
		event{0, timeline.CallerSpeechStarted, 1},
		event{500, timeline.CallerSpeechEnded, 1},
		event{950, timeline.AgentAudioStarted, 1},
	), Options{})
	if len(c.Turns) != 1 {
		t.Fatalf("turns = %+v", c.Turns)
	}
	tr := c.Turns[0]
	if !tr.Replied || tr.FirstAudio != 450*time.Millisecond || tr.Overlap() {
		t.Errorf("turn = %+v", tr)
	}
}

// A clip with two sentences: the turn ends after the second.
func TestFirstAudioFromLastSpeech(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say},
		event{0, timeline.CallerSpeechStarted, 1},
		event{500, timeline.CallerSpeechEnded, 1},
		event{900, timeline.CallerSpeechStarted, 1},
		event{1400, timeline.CallerSpeechEnded, 1},
		event{1800, timeline.AgentAudioStarted, 1},
	), Options{})
	if got := c.Turns[0].FirstAudio; got != 400*time.Millisecond {
		t.Errorf("first audio = %v, want 400ms", got)
	}
}

func TestAgentTalksOverCaller(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say},
		event{0, timeline.CallerSpeechStarted, 1},
		event{400, timeline.AgentAudioStarted, 1},
		event{500, timeline.CallerSpeechEnded, 1},
	), Options{})
	tr := c.Turns[0]
	if !tr.Overlap() || tr.FirstAudio != -100*time.Millisecond {
		t.Errorf("turn = %+v, want overlap of -100ms", tr)
	}
}

// Agent speech already under way when the caller starts belongs to an
// earlier turn; the reply is the next one.
func TestEarlierAgentSpeechIgnored(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say},
		event{-300, timeline.AgentAudioStarted, 0},
		event{0, timeline.CallerSpeechStarted, 1},
		event{500, timeline.CallerSpeechEnded, 1},
		event{900, timeline.AgentAudioStarted, 1},
	), Options{})
	if got := c.Turns[0].FirstAudio; got != 400*time.Millisecond {
		t.Errorf("first audio = %v, want 400ms", got)
	}
}

// Speech after the caller's next turn doesn't count as a reply to this one.
func TestNoReplyBeforeNextTurn(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say, pause, say},
		event{0, timeline.CallerSpeechStarted, 1},
		event{500, timeline.CallerSpeechEnded, 1},
		event{2000, timeline.CallerSpeechStarted, 3},
		event{2500, timeline.CallerSpeechEnded, 3},
		event{2900, timeline.AgentAudioStarted, 3},
	), Options{})
	if len(c.Turns) != 2 {
		t.Fatalf("turns = %+v", c.Turns)
	}
	if c.Turns[0].Replied {
		t.Errorf("turn 1 credited with turn 3's reply: %+v", c.Turns[0])
	}
	if !c.Turns[1].Replied || c.Turns[1].FirstAudio != 400*time.Millisecond {
		t.Errorf("turn 3 = %+v", c.Turns[1])
	}
}

func TestStepWithoutSpeechHasNoTurn(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say}), Options{})
	if len(c.Turns) != 0 {
		t.Errorf("turns = %+v", c.Turns)
	}
}

func TestInterruptionHandled(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say, interrupt},
		event{500, timeline.AgentAudioStarted, 1},
		event{1000, timeline.CallerSpeechStarted, 2},
		event{1210, timeline.AgentClear, 2},
		event{1210, timeline.AgentAudioStopped, 2},
		event{1400, timeline.CallerSpeechEnded, 2},
	), Options{})
	if len(c.Interruptions) != 1 {
		t.Fatalf("interruptions = %+v", c.Interruptions)
	}
	in := c.Interruptions[0]
	if in.StopLatency != 210*time.Millisecond || !in.Cleared || !in.Stopped {
		t.Errorf("interruption = %+v", in)
	}
}

func TestInterruptionIgnored(t *testing.T) {
	c := FromResult(result([]scenario.Kind{say, interrupt},
		event{500, timeline.AgentAudioStarted, 1},
		event{1000, timeline.CallerSpeechStarted, 2},
		event{1400, timeline.CallerSpeechEnded, 2},
		event{2200, timeline.AgentAudioStopped, 2}, // finished its reply
	), Options{})
	in := c.Interruptions[0]
	if in.StopLatency != 1200*time.Millisecond || in.Cleared || in.Stopped {
		t.Errorf("interruption = %+v", in)
	}
	// A looser threshold changes the verdict, not the measurement.
	in = FromResult(result([]scenario.Kind{say, interrupt},
		event{1000, timeline.CallerSpeechStarted, 2},
		event{2200, timeline.AgentAudioStopped, 2},
	), Options{StopWithin: 2 * time.Second}).Interruptions[0]
	if !in.Stopped || in.StopLatency != 1200*time.Millisecond {
		t.Errorf("with 2s threshold = %+v", in)
	}
}

func TestInterruptionNeverStopped(t *testing.T) {
	in := FromResult(result([]scenario.Kind{interrupt},
		event{0, timeline.AgentAudioStarted, 1},
		event{1000, timeline.CallerSpeechStarted, 1},
	), Options{}).Interruptions[0]
	if !in.AgentStopped.IsZero() || in.Stopped || in.StopLatency != 0 {
		t.Errorf("interruption = %+v", in)
	}
}

// A clear that comes after the agent already stopped didn't stop it.
func TestLateClearNotCredited(t *testing.T) {
	in := FromResult(result([]scenario.Kind{interrupt},
		event{1000, timeline.CallerSpeechStarted, 1},
		event{1500, timeline.AgentAudioStopped, 1},
		event{1800, timeline.AgentClear, 1},
	), Options{}).Interruptions[0]
	if in.Cleared {
		t.Errorf("late clear credited: %+v", in)
	}
}
