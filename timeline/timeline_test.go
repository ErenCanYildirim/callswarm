package timeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func ms(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }

func sample() *Timeline {
	tl := New(t0)
	// Added out of order on purpose.
	tl.Add(ms(950), AgentAudioStarted, 1, "")
	tl.Add(ms(0), RunStarted, 0, "")
	tl.Add(ms(20), CallerAudioStarted, 1, "")
	tl.Add(ms(0), StepStarted, 1, "say hello.wav")
	tl.Add(ms(500), CallerSpeechEnded, 1, "")
	tl.Add(ms(120), CallerSpeechStarted, 1, "")
	tl.Add(ms(600), CallerAudioEnded, 1, "")
	tl.Add(ms(1400), AgentAudioStopped, 1, "")
	tl.Add(ms(1400), AgentMark, 1, "response-1")
	tl.Sort()
	return tl
}

func TestSortOrdersByTimeThenCause(t *testing.T) {
	tl := sample()
	var kinds []Kind
	for _, e := range tl.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []Kind{
		RunStarted, StepStarted, CallerAudioStarted, CallerSpeechStarted,
		CallerSpeechEnded, CallerAudioEnded, AgentAudioStarted, AgentAudioStopped, AgentMark,
	}
	if len(kinds) != len(want) {
		t.Fatalf("got %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("order = %v, want %v", kinds, want)
		}
	}
}

func TestQueries(t *testing.T) {
	tl := sample()
	e, ok := tl.First(CallerSpeechEnded, 1)
	if !ok || tl.Offset(e) != 500*time.Millisecond {
		t.Errorf("First = %+v, %v", e, ok)
	}
	if _, ok := tl.First(CallerSpeechEnded, 2); ok {
		t.Error("found an event for a step that has none")
	}
	if n := len(tl.Filter(AgentAudioStarted, 0)); n != 1 {
		t.Errorf("Filter any step = %d events", n)
	}
	tl.Add(ms(2000), AgentAudioStarted, 2, "")
	if e, _ := tl.Last(AgentAudioStarted, 0); e.Step != 2 {
		t.Errorf("Last = %+v", e)
	}
}

func TestText(t *testing.T) {
	text := sample().String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d lines:\n%s", len(lines), text)
	}
	if want := "   +0.000s          run_started"; lines[0] != want {
		t.Errorf("line 1 = %q, want %q", lines[0], want)
	}
	if want := "   +0.000s  step 1   step_started           say hello.wav"; lines[1] != want {
		t.Errorf("line 2 = %q, want %q", lines[1], want)
	}
	if !strings.HasPrefix(lines[6], "   +0.950s  step 1   agent_audio_started") {
		t.Errorf("line 7 = %q", lines[6])
	}
}

func TestJSONRoundTrip(t *testing.T) {
	tl := sample()
	tl.Add(ms(1401).Add(234*time.Microsecond), AgentClear, 1, "")
	data, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"offset_ms":1401.234`) {
		t.Errorf("offsets not in milliseconds: %s", data)
	}
	if strings.Contains(string(data), `"step":0`) {
		t.Errorf("zero step should be omitted: %s", data)
	}
	var back Timeline
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Start.Equal(tl.Start) || len(back.Events) != len(tl.Events) {
		t.Fatalf("round trip lost data: %+v", back)
	}
	for i := range tl.Events {
		a, b := tl.Events[i], back.Events[i]
		if !a.At.Equal(b.At) || a.Kind != b.Kind || a.Step != b.Step || a.Detail != b.Detail {
			t.Errorf("event %d: %+v != %+v", i, a, b)
		}
	}
}
