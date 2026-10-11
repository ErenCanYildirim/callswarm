package scenario

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/transport"
)

const (
	early = 30 * time.Millisecond
	late  = 100 * time.Millisecond
)

func within(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-early || got > want+late {
		t.Errorf("%s = %v, want %v (-%v/+%v)", what, got, want, early, late)
	} else {
		t.Logf("%s = %v (want %v)", what, got, want)
	}
}

// writeTone writes a 1 kHz tone as a 16 kHz WAV, so loading exercises
// resampling as well.
func writeTone(t *testing.T, dir, name string, d time.Duration) {
	t.Helper()
	const rate = 16000
	samples := make([]int16, int(d.Seconds()*rate))
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/rate))
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := audio.EncodeWAV(f, audio.PCM{SampleRate: rate, Samples: samples}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// fixture writes clips and a scenario file into a temp dir and loads it.
func fixture(t *testing.T, yaml string) *Scenario {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "clips"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTone(t, filepath.Join(dir, "clips"), "short.wav", 400*time.Millisecond)
	writeTone(t, filepath.Join(dir, "clips"), "long.wav", 1500*time.Millisecond)
	writeTone(t, filepath.Join(dir, "clips"), "tiny.wav", 100*time.Millisecond)
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestParseFullScenario(t *testing.T) {
	sc := fixture(t, `
name: everything
reply_timeout: 5s
reply_gap: 400ms
steps:
  - listen
  - say: clips/short.wav
  - pause: 1s
  - say: clips/long.wav
    wait: none
  - interrupt: clips/short.wav
    after: 800ms
  - say: clips/tiny.wav
    wait: 2s
  - interrupt: clips/tiny.wav
    after: 0s
    wait: reply
`)
	if sc.Name != "everything" || sc.ReplyTimeout != 5*time.Second || sc.ReplyGap != 400*time.Millisecond {
		t.Errorf("settings = %+v", sc)
	}
	if sc.MaxReply != DefaultMaxReply {
		t.Errorf("max reply default = %v", sc.MaxReply)
	}
	want := []struct {
		kind Kind
		wait WaitMode
	}{
		{KindListen, ""}, {KindSay, WaitReply}, {KindPause, ""}, {KindSay, WaitNone},
		{KindInterrupt, WaitQuiet}, {KindSay, WaitFixed}, {KindInterrupt, WaitReply},
	}
	if len(sc.Steps) != len(want) {
		t.Fatalf("got %d steps, want %d", len(sc.Steps), len(want))
	}
	for i, w := range want {
		if st := sc.Steps[i]; st.Kind != w.kind || st.Wait != w.wait {
			t.Errorf("step %d = %s/%s, want %s/%s", i+1, st.Kind, st.Wait, w.kind, w.wait)
		}
	}
	if sc.Steps[2].Pause != time.Second || sc.Steps[4].After != 800*time.Millisecond || sc.Steps[5].WaitFor != 2*time.Second {
		t.Error("durations not parsed")
	}
	// 400 ms at 8 kHz, converted from the 16 kHz file.
	if n := len(sc.Steps[1].Audio); n != 3200 {
		t.Errorf("short clip = %d bytes, want 3200", n)
	}
}

func TestParseRejects(t *testing.T) {
	dir := t.TempDir()
	writeTone(t, dir, "a.wav", 100*time.Millisecond)
	tests := map[string]struct{ yaml, want string }{
		"no steps":           {"name: x\n", "no steps"},
		"unknown field":      {"steps:\n  - say: a.wav\n    volume: 3\n", "volume"},
		"unknown top":        {"stepz: []\n", "stepz"},
		"bare unknown":       {"steps:\n  - talk\n", "unknown step"},
		"two actions":        {"steps:\n  - say: a.wav\n    pause: 1s\n", "exactly one"},
		"no action":          {"steps:\n  - wait: none\n", "exactly one"},
		"bad pause":          {"steps:\n  - pause: soon\n", "pause"},
		"negative pause":     {"steps:\n  - pause: -1s\n", "pause"},
		"interrupt no after": {"steps:\n  - interrupt: a.wav\n", "needs after"},
		"after on say":       {"steps:\n  - say: a.wav\n    after: 1s\n", "only valid on interrupt"},
		"wait on pause":      {"steps:\n  - pause: 1s\n    wait: none\n", "only valid on say"},
		"bad wait":           {"steps:\n  - say: a.wav\n    wait: forever\n", "wait"},
		"missing clip":       {"steps:\n  - say: nope.wav\n", "nope.wav"},
		"not wav":            {"steps:\n  - say: a.mp3\n", "only .wav"},
		"bad setting":        {"reply_gap: fast\nsteps:\n  - pause: 1s\n", "reply_gap"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), dir)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestStepNumberInErrors(t *testing.T) {
	dir := t.TempDir()
	writeTone(t, dir, "a.wav", 100*time.Millisecond)
	_, err := Parse([]byte("steps:\n  - say: a.wav\n  - pause: x\n"), dir)
	if err == nil || !strings.Contains(err.Error(), "step 2") {
		t.Errorf("error = %v, want it to name step 2", err)
	}
}

func startCall(t *testing.T, cfg fakeagent.Config) *transport.Conn {
	t.Helper()
	srv := httptest.NewServer(fakeagent.New(cfg))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Options{CallID: t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A full conversation against the fake agent: a question and its reply, a
// pause, then the caller cuts into a long reply.
func TestRunConversation(t *testing.T) {
	cfg := fakeagent.Config{
		ResponseDelay:   150 * time.Millisecond,
		EndpointSilence: 300 * time.Millisecond,
		BargeIn:         true,
	}
	conn := startCall(t, cfg)
	sc := fixture(t, `
name: conversation
reply_gap: 400ms
steps:
  - say: clips/short.wav
  - pause: 200ms
  - say: clips/long.wav
    wait: none
  - interrupt: clips/short.wav
    after: 300ms
`)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := Run(ctx, conn, sc)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Steps) != 4 {
		t.Fatalf("ran %d steps, want 4", len(res.Steps))
	}

	say := res.Steps[0]
	if say.Reply == nil {
		t.Fatal("no reply recorded for step 1")
	}
	within(t, "reply delay", say.Reply.Start.Sub(say.Caller.Ended), cfg.ExpectedFirstAudio())
	within(t, "reply length (echo of 400ms)", say.Reply.End.Sub(say.Reply.Start), 400*time.Millisecond)

	pause := res.Steps[1]
	within(t, "pause", pause.Ended.Sub(pause.Started), 200*time.Millisecond)

	if res.Steps[2].Reply != nil {
		t.Error("say with wait: none should not record a reply")
	}

	in := res.Steps[3]
	if in.Reply == nil {
		t.Fatal("interrupt did not record the speech it cut into")
	}
	within(t, "interruption offset", in.Caller.Began.Sub(in.Reply.Start), 300*time.Millisecond)
	// The agent stops 60 ms into the interruption (its barge-in detection),
	// and the clear cuts off the rest of the 1.5 s echo.
	within(t, "agent stopped after", in.Reply.End.Sub(in.Caller.Began), 60*time.Millisecond)
	if len(res.Agent.Clears) != 1 {
		t.Errorf("clears = %d, want 1", len(res.Agent.Clears))
	}
	if len(res.Agent.Frames) == 0 || len(res.Agent.Marks) == 0 {
		t.Error("agent audio and marks not recorded")
	}
}

func TestListenTimesOutWithoutGreeting(t *testing.T) {
	conn := startCall(t, fakeagent.Config{})
	sc := fixture(t, "reply_timeout: 300ms\nsteps:\n  - listen\n  - say: clips/tiny.wav\n")
	res := Run(context.Background(), conn, sc)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no agent speech within 300ms") {
		t.Fatalf("err = %v", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "step 1 (listen)") {
		t.Errorf("error should name the step: %v", res.Err)
	}
	if len(res.Steps) != 1 {
		t.Errorf("ran %d steps after failure, want stop at 1", len(res.Steps))
	}
}

func TestInterruptAfterAgentFinishedFails(t *testing.T) {
	conn := startCall(t, fakeagent.Config{EndpointSilence: 200 * time.Millisecond})
	sc := fixture(t, `
reply_gap: 300ms
steps:
  - say: clips/tiny.wav
    wait: none
  - interrupt: clips/tiny.wav
    after: 2s
`)
	res := Run(context.Background(), conn, sc)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "stopped speaking") {
		t.Fatalf("err = %v, want agent stopped speaking", res.Err)
	}
}

func TestRunStopsWhenCallEnds(t *testing.T) {
	conn := startCall(t, fakeagent.Config{})
	sc := fixture(t, "steps:\n  - pause: 5s\n")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
	}()
	start := time.Now()
	res := Run(context.Background(), conn, sc)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "call ended") {
		t.Fatalf("err = %v, want call ended", res.Err)
	}
	if time.Since(start) > time.Second {
		t.Error("run kept going after the call ended")
	}
}
