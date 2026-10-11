package scenario

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/timeline"
	"github.com/ErenCanYildirim/callswarm/transport"
	"github.com/ErenCanYildirim/callswarm/twilio"
	"github.com/coder/websocket"
)

// part is a piece of a test clip: tone or silence.
type part struct {
	d    time.Duration
	tone bool
}

// writeClip writes a 16 kHz WAV made of tone and silence parts, like a real
// recording with silence around the words.
func writeClip(t *testing.T, path string, parts ...part) {
	t.Helper()
	const rate = 16000
	var samples []int16
	for _, p := range parts {
		for i := range int(p.d.Seconds() * rate) {
			var v int16
			if p.tone {
				v = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/rate))
			}
			samples = append(samples, v)
		}
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

// stepOneAt returns when the first event of kind happened in step 1.
func stepOneAt(t *testing.T, tl *timeline.Timeline, kind timeline.Kind) time.Time {
	t.Helper()
	e, ok := tl.First(kind, 1)
	if !ok {
		t.Fatalf("no %s event for step 1 in:\n%s", kind, tl)
	}
	return e.At
}

func near(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if d := got - want; d < -20*time.Millisecond || d > 20*time.Millisecond {
		t.Errorf("%s = %v, want %v ± 20ms", what, got, want)
	}
}

func TestTimelineSeparatesSpeechFromClip(t *testing.T) {
	dir := t.TempDir()
	// 200 ms lead-in, 500 ms of speech, 500 ms tail: a typical recording.
	writeClip(t, filepath.Join(dir, "padded.wav"),
		part{200 * time.Millisecond, false}, part{500 * time.Millisecond, true}, part{500 * time.Millisecond, false})
	writeClip(t, filepath.Join(dir, "long.wav"), part{1500 * time.Millisecond, true})
	writeClip(t, filepath.Join(dir, "stop.wav"), part{400 * time.Millisecond, true})
	path := filepath.Join(dir, "s.yaml")
	if err := os.WriteFile(path, []byte(`
name: timeline
reply_gap: 400ms
steps:
  - say: padded.wav
  - say: long.wav
    wait: none
  - interrupt: stop.wav
    after: 300ms
`), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if sp := sc.Steps[0].Speech; len(sp) != 1 {
		t.Fatalf("padded clip speech = %v, want one span", sp)
	}

	cfg := fakeagent.Config{ResponseDelay: 150 * time.Millisecond, EndpointSilence: 300 * time.Millisecond, BargeIn: true}
	conn := startCall(t, cfg)
	res := Run(context.Background(), conn, sc)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	tl := res.Timeline
	t.Logf("timeline:\n%s", tl)

	audioStart := stepOneAt(t, tl, timeline.CallerAudioStarted)
	audioEnd := stepOneAt(t, tl, timeline.CallerAudioEnded)
	speechStart := stepOneAt(t, tl, timeline.CallerSpeechStarted)
	speechEnd := stepOneAt(t, tl, timeline.CallerSpeechEnded)
	near(t, "lead-in silence", speechStart.Sub(audioStart), 200*time.Millisecond)
	near(t, "trailing silence", audioEnd.Sub(speechEnd), 500*time.Millisecond)

	// The agent answers relative to the end of speech, so that is the
	// reference that recovers its configured latency. Measuring from the end
	// of the file would be off by the 500 ms tail.
	agentStart := stepOneAt(t, tl, timeline.AgentAudioStarted)
	within(t, "agent start after caller speech", agentStart.Sub(speechEnd), cfg.ExpectedFirstAudio())
	t.Logf("measured from end of clip instead: %v", agentStart.Sub(audioEnd).Round(time.Millisecond))

	// The interruption cut the agent off with a clear.
	if _, ok := tl.First(timeline.AgentClear, 3); !ok {
		t.Errorf("no clear during the interrupt step:\n%s", tl)
	}
	var cut bool
	for _, e := range tl.Filter(timeline.AgentAudioStopped, 0) {
		if e.Detail == "cut off by clear" {
			cut = true
		}
	}
	if !cut {
		t.Error("no agent_audio_stopped marked as cut off by clear")
	}

	// Structure: bracketed by run events, sorted, and serializable.
	if first := tl.Events[0]; first.Kind != timeline.RunStarted {
		t.Errorf("first event = %s", first.Kind)
	}
	// Each step's events fall within the step.
	for _, sr := range res.Steps {
		for _, e := range tl.Events {
			if e.Step == sr.Index && strings.HasPrefix(string(e.Kind), "caller_") &&
				(e.At.Before(sr.Started) || e.At.After(sr.Ended)) {
				t.Errorf("step %d: %s at %v is outside the step", sr.Index, e.Kind, tl.Offset(e))
			}
		}
	}
	if last := tl.Events[len(tl.Events)-1]; last.Kind != timeline.RunEnded {
		t.Errorf("last event = %s", last.Kind)
	}
	for i := 1; i < len(tl.Events); i++ {
		if tl.Events[i].At.Before(tl.Events[i-1].At) {
			t.Fatalf("events out of order at %d", i)
		}
	}
	if _, err := json.Marshal(tl); err != nil {
		t.Errorf("timeline JSON: %v", err)
	}
}

func TestTimelineRecordsFailure(t *testing.T) {
	conn := startCall(t, fakeagent.Config{})
	sc := fixture(t, "reply_timeout: 200ms\nsteps:\n  - listen\n")
	res := Run(context.Background(), conn, sc)
	e, ok := res.Timeline.First(timeline.RunFailed, 0)
	if !ok || !strings.Contains(e.Detail, "no agent speech") {
		t.Errorf("run_failed = %+v, %v", e, ok)
	}
	if e, _ := res.Timeline.First(timeline.StepEnded, 1); !strings.Contains(e.Detail, "no agent speech") {
		t.Errorf("step_ended detail = %q", e.Detail)
	}
}

func TestSilentClipRejected(t *testing.T) {
	dir := t.TempDir()
	writeClip(t, filepath.Join(dir, "quiet.wav"), part{time.Second, false})
	_, err := Parse([]byte("steps:\n  - say: quiet.wav\n"), dir)
	if err == nil || !strings.Contains(err.Error(), "no speech detected") {
		t.Errorf("err = %v", err)
	}
}

// Agents send audio in chunks of any size; detection runs per 20 ms either
// way.
func TestMonitorSplitsLargeChunks(t *testing.T) {
	in := make(chan transport.Inbound, 1)
	m := newMonitor(in)
	at := time.Now()
	in <- transport.Inbound{Kind: transport.KindAudio, At: at, PlayAt: at, Audio: make([]byte, 800)} // 100 ms
	close(in)
	deadline := time.Now().Add(time.Second)
	for len(m.snapshot().Frames) < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	frames := m.snapshot().Frames
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
	for i, f := range frames {
		if want := at.Add(time.Duration(i) * 20 * time.Millisecond); !f.PlayAt.Equal(want) || f.End.Sub(f.PlayAt) != 20*time.Millisecond {
			t.Errorf("frame %d plays %v..%v", i, f.PlayAt.Sub(at), f.End.Sub(at))
		}
	}
}

// A clear ends agent speech for good: a reply that starts soon after it is a
// separate segment, even if the gap is shorter than a pause between words.
func TestClearSplitsAgentSpeech(t *testing.T) {
	at := time.Now()
	ms := func(n int) time.Time { return at.Add(time.Duration(n) * time.Millisecond) }
	var a AgentAudio
	for i := 0; i < 15; i++ { // 0-300 ms, cut off by the clear at 200 ms
		f := AgentFrame{PlayAt: ms(i * 20), End: ms(i*20 + 20), Speech: true}
		if i >= 10 {
			f.Cleared = true
		}
		if f.End.After(ms(200)) {
			f.End = ms(200)
		}
		a.Frames = append(a.Frames, f)
	}
	for i := 0; i < 10; i++ { // new reply 100 ms after the clear
		a.Frames = append(a.Frames, AgentFrame{PlayAt: ms(300 + i*20), End: ms(320 + i*20), Speech: true})
	}
	a.Clears = []time.Time{ms(200)}

	segs := a.Speech(vadConfig.MinSilence)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2: %v", len(segs), segs)
	}
	if !segs[0].End.Equal(ms(200)) || !segs[1].Start.Equal(ms(300)) {
		t.Errorf("segments = %v..%v, %v..%v",
			segs[0].Start.Sub(at), segs[0].End.Sub(at), segs[1].Start.Sub(at), segs[1].End.Sub(at))
	}
	// Without the clear, the 100 ms gap would be bridged.
	a.Clears = nil
	if n := len(a.Speech(vadConfig.MinSilence)); n != 1 {
		t.Errorf("without a clear got %d segments, want 1", n)
	}
}

// Agents on real networks deliver audio with gaps. A brief gap in the agent's
// speech at the moment of a planned interruption is a pause, not the end of
// its speech, and the interruption must still happen.
func TestInterruptThroughDeliveryGap(t *testing.T) {
	tone := make([]int16, audio.TelephonySampleRate) // 1 s
	for i := range tone {
		tone[i] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/audio.TelephonySampleRate))
	}
	speech := audio.Frames(audio.MulawEncode(tone), audio.TelephonyFrameSamples)

	// The agent speaks 200 ms, goes quiet for 140 ms (covering the 300 ms
	// interruption point), then continues.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx := r.Context()
		go func() { // drain caller audio
			for {
				if _, _, err := ws.Read(ctx); err != nil {
					return
				}
			}
		}()
		send := func(frames [][]byte) {
			for _, f := range frames {
				data, _ := twilio.Encode(twilio.AgentMedia("MZgap", f))
				if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
		send(speech[:10])
		time.Sleep(340 * time.Millisecond)
		send(speech[10:])
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), transport.Options{CallID: "gap"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	sc := fixture(t, "reply_gap: 400ms\nsteps:\n  - interrupt: clips/tiny.wav\n    after: 300ms\n    wait: none\n")
	res := Run(ctx, conn, sc)
	if res.Err != nil {
		t.Fatalf("interrupt failed during a 140 ms delivery gap: %v\n%s", res.Err, res.Timeline)
	}
}

// Agents send nothing between replies, and the caller hears silence then.
// The noise floor has to count that silence: if it only saw the audio the
// agent sent, a long, steady reply would become the background, and the
// next reply would not be detected as speech at all.
func TestMonitorCountsSilenceBetweenReplies(t *testing.T) {
	tone := func(d time.Duration) []byte {
		pcm := make([]int16, int(d.Seconds()*audio.TelephonySampleRate))
		for i := range pcm {
			pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/audio.TelephonySampleRate))
		}
		return audio.MulawEncode(pcm)
	}
	in := make(chan transport.Inbound, 2)
	m := newMonitor(in)
	at := time.Now()
	in <- transport.Inbound{Kind: transport.KindAudio, At: at, PlayAt: at, Audio: tone(3 * time.Second)}
	second := at.Add(5 * time.Second) // two seconds of nothing after the first reply
	in <- transport.Inbound{Kind: transport.KindAudio, At: second, PlayAt: second, Audio: tone(time.Second)}
	close(in)

	deadline := time.Now().Add(time.Second)
	for len(m.snapshot().Frames) < 200 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segs := m.segments(vadConfig.MinSilence)
	if len(segs) != 2 || !segs[1].Start.Equal(second) {
		for _, s := range segs {
			t.Logf("segment %v..%v", s.Start.Sub(at), s.End.Sub(at))
		}
		t.Fatalf("got %d segments, want the second reply detected at 5s", len(segs))
	}
}
