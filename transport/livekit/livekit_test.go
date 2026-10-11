package livekit_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/fakeagent/livekitagent"
	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/transport/livekit"
)

// server is the LiveKit server the tests use, from CALLSWARM_LIVEKIT_URL,
// with the dev server's key and secret unless LIVEKIT_API_KEY and
// LIVEKIT_API_SECRET say otherwise. Start one with:
//
//	livekit-server --dev
func server(t *testing.T) livekitagent.Server {
	t.Helper()
	url := os.Getenv("CALLSWARM_LIVEKIT_URL")
	if url == "" {
		t.Skip("set CALLSWARM_LIVEKIT_URL to a LiveKit server, such as ws://localhost:7880, to run")
	}
	srv := livekitagent.Server{URL: url, APIKey: "devkey", APISecret: "secret"}
	if k := os.Getenv("LIVEKIT_API_KEY"); k != "" {
		srv.APIKey, srv.APISecret = k, os.Getenv("LIVEKIT_API_SECRET")
	}
	return srv
}

func writeScenario(t *testing.T, steps string) *scenario.Scenario {
	t.Helper()
	dir := t.TempDir()
	tone := func(name string, d time.Duration) {
		pcm := make([]int16, int(d.Seconds()*audio.TelephonySampleRate))
		for i := range pcm {
			pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/audio.TelephonySampleRate))
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := audio.EncodeWAV(f, audio.PCM{SampleRate: audio.TelephonySampleRate, Samples: pcm}); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	tone("short.wav", 600*time.Millisecond)
	tone("long.wav", 1800*time.Millisecond)
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte("reply_gap: 400ms\nsteps:\n"+steps), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// A call through LiveKit measures the fake agent's known timing, as a call
// over Twilio Media Streams does.
func TestCallMeasuresFakeAgent(t *testing.T) {
	srv := server(t)
	cfg := fakeagent.Config{EndpointSilence: 300 * time.Millisecond, ResponseDelay: 200 * time.Millisecond, BargeIn: true}
	room := fmt.Sprintf("callswarm-test-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	agentDone := make(chan error, 1)
	go func() { agentDone <- livekitagent.Join(ctx, srv, room, cfg) }()

	call, err := livekit.Dial(ctx, livekit.Options{URL: srv.URL, APIKey: srv.APIKey, APISecret: srv.APISecret, Room: room})
	if err != nil {
		t.Fatal(err)
	}
	sc := writeScenario(t, `
  - say: short.wav
  - say: short.wav
  - say: long.wav
    wait: none
  - interrupt: short.wav
    after: 400ms
`)
	res := scenario.Run(ctx, call, sc)
	stats := call.Stats()
	if err := call.Close(); err != nil {
		t.Error(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if call.Agent() != livekitagent.Identity {
		t.Errorf("heard %q, want the fake agent", call.Agent())
	}

	m := metrics.FromResult(res, metrics.Options{})
	want := cfg.ExpectedFirstAudio()
	for _, turn := range m.Turns {
		if !turn.Replied {
			t.Errorf("step %d: no reply", turn.Step)
			continue
		}
		t.Logf("step %d: first audio %v (fake agent answers after %v)", turn.Step, turn.FirstAudio, want)
		if d := turn.FirstAudio - want; d < -30*time.Millisecond || d > 80*time.Millisecond {
			t.Errorf("step %d: first audio %v, want %v", turn.Step, turn.FirstAudio, want)
		}
	}
	if len(m.Interruptions) != 1 {
		t.Fatalf("interruptions: %+v", m.Interruptions)
	}
	in := m.Interruptions[0]
	t.Logf("interruption: stopped after %v", in.StopLatency)
	// The fake agent stops after 60 ms of caller speech; WebRTC adds its
	// transit both ways.
	if !in.Stopped || in.StopLatency > 250*time.Millisecond {
		t.Errorf("interruption = %+v", in)
	}
	if stats.LateFrames > stats.FramesSent/100 {
		t.Errorf("caller pacing: %d of %d frames late", stats.LateFrames, stats.FramesSent)
	}

	select {
	case err := <-agentDone:
		if err != nil {
			t.Errorf("fake agent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("fake agent did not leave after the caller hung up")
	}
}

// Dial fails, rather than talking to an empty room, when no agent answers.
func TestDialWithoutAgent(t *testing.T) {
	srv := server(t)
	room := fmt.Sprintf("callswarm-empty-%d", time.Now().UnixNano())
	start := time.Now()
	_, err := livekit.Dial(context.Background(), livekit.Options{
		URL: srv.URL, APIKey: srv.APIKey, APISecret: srv.APISecret, Room: room,
		AnswerTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "no agent answered") {
		t.Fatalf("Dial: %v, want no agent answered", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Dial took %v", d)
	}
}
