package metrics

// Calibration: run real calls against the fake agent with known timing and
// check that the metrics recover it. If these drift, the measurements can't
// be trusted on real agents either.

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/timeline"
	"github.com/ErenCanYildirim/callswarm/transport"
)

// Tolerances: audio moves in 20 ms frames, and test machines add jitter.
const (
	early = 30 * time.Millisecond
	late  = 100 * time.Millisecond
)

func within(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-early || got > want+late {
		t.Errorf("%s = %v, want %v (-%v/+%v)", what, got, want, early, late)
	} else {
		t.Logf("%s: configured %v, measured %v (error %+v)", what, want, got.Round(time.Millisecond), (got - want).Round(time.Millisecond))
	}
}

// writeClip writes a 16 kHz WAV: optional silence, tone, optional silence.
func writeClip(t *testing.T, path string, lead, speech, tail time.Duration) {
	t.Helper()
	const rate = 16000
	n := func(d time.Duration) int { return int(d.Seconds() * rate) }
	samples := make([]int16, n(lead)+n(speech)+n(tail))
	for i := range n(speech) {
		samples[n(lead)+i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/rate))
	}
	f, err := os.Create(path)
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

// run plays a scenario against a fake agent and returns its metrics and the
// raw result.
func run(t *testing.T, cfg fakeagent.Config, yaml string) (Call, *scenario.Result) {
	t.Helper()
	dir := t.TempDir()
	// Recorded-style clips, with silence around the words.
	writeClip(t, filepath.Join(dir, "question.wav"), 200*time.Millisecond, 600*time.Millisecond, 400*time.Millisecond)
	writeClip(t, filepath.Join(dir, "long.wav"), 0, 2*time.Second, 0)
	writeClip(t, filepath.Join(dir, "stop.wav"), 0, 400*time.Millisecond, 0)
	path := filepath.Join(dir, "s.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(fakeagent.New(cfg))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Options{CallID: "cal"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	res := scenario.Run(ctx, conn, sc)
	if res.Err != nil {
		t.Fatalf("run failed: %v\n%s", res.Err, res.Timeline)
	}
	return FromResult(res, Options{}), res
}

func TestCalibrationFirstAudio(t *testing.T) {
	const twoTurns = `
reply_gap: 400ms
steps:
  - say: question.wav
  - say: question.wav
`
	for _, delay := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond} {
		t.Run(fmt.Sprint(delay), func(t *testing.T) {
			t.Parallel()
			cfg := fakeagent.Config{ResponseDelay: delay, EndpointSilence: 300 * time.Millisecond}
			c, _ := run(t, cfg, twoTurns)
			if len(c.Turns) != 2 {
				t.Fatalf("turns = %+v", c.Turns)
			}
			for _, tr := range c.Turns {
				if !tr.Replied {
					t.Fatalf("step %d: no reply", tr.Step)
				}
				within(t, fmt.Sprintf("step %d first audio", tr.Step), tr.FirstAudio, cfg.ExpectedFirstAudio())
			}
		})
	}
}

func TestCalibrationInterruption(t *testing.T) {
	const interruptReply = `
reply_gap: 400ms
steps:
  - say: long.wav
    wait: none
  - interrupt: stop.wav
    after: 300ms
`
	// The fake agent needs 60 ms of speech to notice, then waits its
	// configured reaction time.
	for _, reaction := range []time.Duration{0, 150 * time.Millisecond, 400 * time.Millisecond} {
		t.Run(fmt.Sprint(reaction), func(t *testing.T) {
			t.Parallel()
			cfg := fakeagent.Config{EndpointSilence: 300 * time.Millisecond, BargeIn: true, BargeInDelay: reaction}
			c, _ := run(t, cfg, interruptReply)
			if len(c.Interruptions) != 1 {
				t.Fatalf("interruptions = %+v", c.Interruptions)
			}
			in := c.Interruptions[0]
			within(t, "stop latency", in.StopLatency, 60*time.Millisecond+reaction)
			if !in.Cleared || !in.Stopped {
				t.Errorf("interruption = %+v, want cleared and stopped", in)
			}
		})
	}

	t.Run("ignored", func(t *testing.T) {
		t.Parallel()
		cfg := fakeagent.Config{EndpointSilence: 300 * time.Millisecond}
		c, res := run(t, cfg, interruptReply)
		in := c.Interruptions[0]
		// The agent finishes its 2 s reply regardless, so it talks over the
		// caller for whatever was left of the reply when the interruption
		// began (about 1.7 s; the exact offset lands on a frame boundary).
		agentStart, _ := res.Timeline.First(timeline.AgentAudioStarted, 0)
		remaining := 2*time.Second - in.CallerSpeechStarted.Sub(agentStart.At)
		within(t, "stop latency", in.StopLatency, remaining)
		if in.Cleared || in.Stopped {
			t.Errorf("interruption = %+v, want not cleared and not stopped", in)
		}
	})
}
