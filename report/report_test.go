package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/load"
	"github.com/ErenCanYildirim/callswarm/metrics"
	"github.com/ErenCanYildirim/callswarm/scenario"
	"github.com/ErenCanYildirim/callswarm/summary"
	"github.com/ErenCanYildirim/callswarm/timeline"
	"github.com/ErenCanYildirim/callswarm/transport"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://user:secret@agent.example.com/stream?token=abc#x": "wss://agent.example.com/stream",
		"ws://localhost:8080/twilio":                             "ws://localhost:8080/twilio",
		"ws://%zz":                                               "(invalid url)",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDigest(t *testing.T) {
	base := func() *scenario.Scenario {
		return &scenario.Scenario{ReplyGap: time.Second, Steps: []scenario.Step{
			{Kind: scenario.KindSay, Clip: "a.wav", Audio: []byte{1, 2, 3}, Wait: scenario.WaitReply},
			{Kind: scenario.KindPause, Pause: time.Second},
		}}
	}
	d := Digest(base())
	if !strings.HasPrefix(d, "sha256:") || Digest(base()) != d {
		t.Fatalf("digest not stable: %s", d)
	}
	renamed := base()
	renamed.Steps[0].Clip = "renamed.wav"
	if Digest(renamed) != d {
		t.Error("renaming a clip file changed the digest")
	}
	for name, change := range map[string]func(*scenario.Scenario){
		"audio":     func(s *scenario.Scenario) { s.Steps[0].Audio[1] = 9 },
		"pause":     func(s *scenario.Scenario) { s.Steps[1].Pause = 2 * time.Second },
		"wait":      func(s *scenario.Scenario) { s.Steps[0].Wait = scenario.WaitNone },
		"reply gap": func(s *scenario.Scenario) { s.ReplyGap = 2 * time.Second },
		"step added": func(s *scenario.Scenario) {
			s.Steps = append(s.Steps, scenario.Step{Kind: scenario.KindListen})
		},
	} {
		sc := base()
		change(sc)
		if Digest(sc) == d {
			t.Errorf("changing the %s left the digest unchanged", name)
		}
	}
}

func TestFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                       "0ms",
		600 * time.Microsecond:  "0.6ms",
		512 * time.Millisecond:  "512ms",
		-100 * time.Millisecond: "-100ms",
		1250 * time.Millisecond: "1.25s",
		2 * time.Second:         "2s",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%v) = %q, want %q", d, got, want)
		}
	}
	if got := human(45300 * time.Millisecond); got != "45.3s" {
		t.Errorf("human = %q", got)
	}
	if got := human(125 * time.Second); got != "2m05s" {
		t.Errorf("human = %q", got)
	}
}

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func dur(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

// sampleRun is a small, fixed run: four calls, one failed, one interruption.
func sampleRun() (*load.Report, summary.Summary, Manifest) {
	tl := timeline.New(t0)
	tl.Add(t0, timeline.RunStarted, 0, "demo")
	turn := func(step, firstMS int) metrics.Turn {
		return metrics.Turn{Step: step, Replied: true, CallerSpeechEnded: t0.Add(dur(1000)), FirstAudio: dur(firstMS)}
	}
	rep := &load.Report{Started: t0, Ended: t0.Add(dur(12300)), MaxActive: 4}
	for i, fa := range []int{480, 510, 530, 900} {
		c := load.Call{
			ID: i + 1, ActiveAtStart: i + 1,
			Started: t0.Add(dur(i * 100)), Ended: t0.Add(dur(10000 + i*100)),
			Metrics: metrics.Call{Turns: []metrics.Turn{turn(1, fa)}},
			Pacing:  transport.Stats{FramesSent: 500, LateFrames: 1, MeanLateness: 600 * time.Microsecond, MaxLateness: dur(12)},
			Result:  &scenario.Result{Timeline: tl},
		}
		rep.Calls = append(rep.Calls, c)
	}
	rep.Calls[1].Metrics.Interruptions = []metrics.Interruption{{
		Step: 2, AgentStopped: t0.Add(dur(5000)), StopLatency: dur(210), Stopped: true, Cleared: true,
	}}
	rep.Calls[3].Metrics.Turns = append(rep.Calls[3].Metrics.Turns, metrics.Turn{Step: 3})
	rep.Calls[3].Err = errors.New("step 3 (say): no agent speech within 10s")

	s := summary.Summarize(rep, summary.Thresholds{})
	m := Manifest{
		Version: "v0.1.0", GoVersion: "go1.24.7", OS: "linux", Arch: "amd64", CPUs: 2,
		URL: "ws://localhost:8080/twilio", Scenario: "demo", ScenarioDigest: "sha256:abc",
		Calls: 4, Concurrency: 4, StopWithinMS: 1000,
	}
	return rep, s, m
}

func TestTextGolden(t *testing.T) {
	_, s, m := sampleRun()
	var buf bytes.Buffer
	if err := WriteText(&buf, s, m); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "summary.txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	// Git on Windows may check the golden file out with CRLF endings.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("text summary changed (run with -update if intended):\n--- got ---\n%s\n--- want ---\n%s", buf.Bytes(), want)
	}
	for _, r := range buf.String() {
		if r > 127 {
			t.Fatalf("non-ASCII character %q in text output", r)
		}
	}
}

func TestTextWithoutReplies(t *testing.T) {
	var buf bytes.Buffer
	s := summary.Summary{Calls: 1, Turns: 2, Health: summary.Healthy}
	if err := WriteText(&buf, s, Manifest{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no replies to measure (2 turns played)") {
		t.Errorf("output:\n%s", buf.String())
	}
}

func TestJSONDocument(t *testing.T) {
	rep, s, m := sampleRun()
	for _, withTimelines := range []bool{false, true} {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, Build(rep, s, m, Options{Timelines: withTimelines})); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["schema_version"] != float64(1) {
			t.Errorf("schema_version = %v", doc["schema_version"])
		}
		sum := doc["summary"].(map[string]any)
		if fa := sum["first_audio"].(map[string]any); fa["p50_ms"] != float64(510) || fa["count"] != float64(4) {
			t.Errorf("first_audio = %v", fa)
		}
		calls := doc["calls"].([]any)
		if len(calls) != 4 {
			t.Fatalf("calls = %d", len(calls))
		}
		c4 := calls[3].(map[string]any)
		if !strings.Contains(c4["error"].(string), "no agent speech") || c4["start_ms"] != float64(300) {
			t.Errorf("call 4 = %v", c4)
		}
		if turns := c4["turns"].([]any); len(turns) != 2 || turns[1].(map[string]any)["replied"] != false {
			t.Errorf("call 4 turns = %v", turns)
		}
		if in := calls[1].(map[string]any)["interruptions"].([]any)[0].(map[string]any); in["stop_latency_ms"] != float64(210) || in["cleared"] != true {
			t.Errorf("interruption = %v", in)
		}
		// Empty lists are [] rather than null, so consumers can iterate.
		if _, ok := calls[0].(map[string]any)["interruptions"].([]any); !ok {
			t.Error("empty interruptions encoded as null")
		}
		_, hasTimeline := calls[0].(map[string]any)["timeline"]
		if hasTimeline != withTimelines {
			t.Errorf("timeline present = %v with Timelines = %v", hasTimeline, withTimelines)
		}
	}
}

// A real run produces a document that decodes back into the same shape.
func TestRealRunRoundTrip(t *testing.T) {
	srv := httptest.NewServer(fakeagent.New(fakeagent.Config{EndpointSilence: 200 * time.Millisecond, Burst: true}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	pcm := make([]int16, audio.TelephonySampleRate/5)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/audio.TelephonySampleRate))
	}
	f, _ := os.Create(filepath.Join(dir, "hi.wav"))
	if err := audio.EncodeWAV(f, audio.PCM{SampleRate: audio.TelephonySampleRate, Samples: pcm}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	path := filepath.Join(dir, "s.yaml")
	_ = os.WriteFile(path, []byte("name: round-trip\nreply_gap: 300ms\nsteps:\n  - say: hi.wav\n"), 0o644)
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := load.Plan{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "?key=secret", Scenario: sc, Calls: 5, Concurrency: 5}
	rep, err := load.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	s := summary.Summarize(rep, summary.Thresholds{})
	m := NewManifest("test", plan, summary.Thresholds{})

	var buf bytes.Buffer
	if err := WriteJSON(&buf, Build(rep, s, m, Options{Timelines: true})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Error("credentials from the URL leaked into the report")
	}
	var back Document
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Manifest.ScenarioDigest != Digest(sc) || back.Manifest.CPUs < 1 || back.Summary.Replied != 5 {
		t.Errorf("decoded manifest/summary = %+v / %+v", back.Manifest, back.Summary)
	}
	if tl := back.Calls[0].Timeline; tl == nil || len(tl.Events) == 0 {
		t.Error("timeline lost in the round trip")
	}

	var text bytes.Buffer
	if err := WriteText(&text, s, m); err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", text.String())
}
