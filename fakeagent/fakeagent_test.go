package fakeagent

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/twilio"
)

// Timing tolerances. Frames are 20 ms, so detection is quantized to that,
// and CI machines add scheduling jitter on top.
const (
	early = 30 * time.Millisecond
	late  = 100 * time.Millisecond
)

type received struct {
	msg twilio.Message
	at  time.Time
}

// call is a minimal Twilio-side client: it paces caller audio in real time
// and timestamps everything the agent sends back.
type call struct {
	t      *testing.T
	conn   *websocket.Conn
	caller *twilio.Caller
	in     chan received
	start  time.Time
	sent   int // frames sent so far
	ctx    context.Context
}

func newServer(t *testing.T, cfg Config) (*httptest.Server, *eventLog) {
	t.Helper()
	log := &eventLog{}
	cfg.OnEvent = log.add
	srv := httptest.NewServer(New(cfg))
	t.Cleanup(srv.Close)
	return srv, log
}

func dial(t *testing.T, srv *httptest.Server, id string) *call {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = conn.CloseNow() })

	c := &call{t: t, conn: conn, caller: twilio.NewCaller(id, nil), in: make(chan received, 4096), ctx: ctx}
	go func() {
		defer close(c.in)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			m, err := twilio.Decode(data)
			if err != nil {
				t.Errorf("agent sent invalid message %s: %v", data, err)
				return
			}
			c.in <- received{m, time.Now()}
		}
	}()
	c.send(c.caller.Connected())
	c.send(c.caller.Start())
	c.start = time.Now()
	return c
}

func (c *call) send(m twilio.Message) {
	data, err := twilio.Encode(m)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// play sends frames in real time, the way Twilio does: each frame goes out
// once its 20 ms of audio has been captured, at the end of its interval.
// It returns when the first and last frames were sent. The last send time is
// when the played audio ends; the first minus one frame is when it began.
func (c *call) play(frames [][]byte) (first, last time.Time) {
	for _, f := range frames {
		offset := time.Duration(c.sent) * frameDuration
		if d := time.Until(c.start.Add(offset + frameDuration)); d > 0 {
			time.Sleep(d)
		}
		last = time.Now()
		if first.IsZero() {
			first = last
		}
		c.send(c.caller.Media(f, offset))
		c.sent++
	}
	return first, last
}

func (c *call) hangUp() []received {
	c.send(c.caller.Stop())
	var all []received
	for r := range c.in {
		all = append(all, r)
	}
	return all
}

// tone returns d of a 1 kHz tone as μ-law frames.
func tone(d time.Duration, amp float64) [][]byte {
	n := int(d / frameDuration * audio.TelephonyFrameSamples)
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(amp * math.Sin(2*math.Pi*1000*float64(i)/audio.TelephonySampleRate))
	}
	return audio.Frames(audio.MulawEncode(pcm), audio.TelephonyFrameSamples)
}

func silence(d time.Duration) [][]byte {
	n := int(d / frameDuration)
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{audio.MulawSilence}, audio.TelephonyFrameSamples)
	}
	return frames
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) find(kind EventKind, response int) (Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == kind && e.Response == response {
			return e, true
		}
	}
	return Event{}, false
}

// mediaUntilMark returns the agent media received before the first mark.
func mediaUntilMark(msgs []received) (media []received, mark *received) {
	for i, r := range msgs {
		switch r.msg.Event {
		case twilio.EventMedia:
			media = append(media, r)
		case twilio.EventMark:
			return media, &msgs[i]
		}
	}
	return media, nil
}

func within(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-early || got > want+late {
		t.Errorf("%s = %v, want %v (-%v/+%v)", what, got, want, early, late)
	} else {
		t.Logf("%s = %v (configured %v)", what, got, want)
	}
}

func TestExpectedFirstAudio(t *testing.T) {
	if got := (Config{}).ExpectedFirstAudio(); got != 300*time.Millisecond {
		t.Errorf("default = %v, want 300ms", got)
	}
	cfg := Config{EndpointSilence: 200 * time.Millisecond, ResponseDelay: 150 * time.Millisecond}
	if got := cfg.ExpectedFirstAudio(); got != 350*time.Millisecond {
		t.Errorf("got %v, want 350ms", got)
	}
}

// The core calibration check: the agent answers when configured to, and
// echoes exactly what the caller said.
func TestFirstAudioLatencyAndEcho(t *testing.T) {
	cfg := Config{ResponseDelay: 200 * time.Millisecond, EndpointSilence: 300 * time.Millisecond}
	srv, log := newServer(t, cfg)
	c := dial(t, srv, "latency")

	speech := tone(300*time.Millisecond, 8000)
	_, speechEnd := c.play(speech)
	c.play(silence(1200 * time.Millisecond))
	msgs := c.hangUp()

	media, mark := mediaUntilMark(msgs)
	if len(media) == 0 {
		t.Fatal("agent never answered")
	}
	within(t, "time to first audio", media[0].at.Sub(speechEnd), cfg.ExpectedFirstAudio())

	if mark == nil || mark.msg.Mark.Name != "response-1" {
		t.Fatalf("missing response-1 mark, got %+v", mark)
	}
	if len(media) != len(speech) {
		t.Fatalf("echo has %d frames, want %d", len(media), len(speech))
	}
	for i, r := range media {
		got, _ := r.msg.Media.Audio()
		if !bytes.Equal(got, speech[i]) {
			t.Fatalf("echo frame %d differs from caller audio", i)
		}
	}

	// The agent's own record agrees with the configured delay.
	ended, ok1 := log.find(EventSpeechEnded, 0)
	started, ok2 := log.find(EventResponseStarted, 1)
	if !ok1 || !ok2 {
		t.Fatal("missing agent events")
	}
	within(t, "agent-side delay", started.At.Sub(ended.At), cfg.ResponseDelay)
}

// bargeInScenario: 1 s of caller speech, then an interruption 200 ms into
// the agent's reply. Returns when the interruption started and all messages.
func bargeInScenario(t *testing.T, cfg Config) (*eventLog, time.Time, []received) {
	t.Helper()
	srv, log := newServer(t, cfg)
	c := dial(t, srv, "bargein")

	c.play(tone(time.Second, 8000))
	// Reply starts ~400 ms after speech ends (300 endpoint + 100 delay).
	c.play(silence(600 * time.Millisecond))
	first, _ := c.play(tone(300*time.Millisecond, 6000))
	c.play(silence(1500 * time.Millisecond))
	return log, first.Add(-frameDuration), c.hangUp()
}

func TestBargeInStopsAudio(t *testing.T) {
	cfg := Config{
		ResponseDelay:   100 * time.Millisecond,
		EndpointSilence: 300 * time.Millisecond,
		BargeIn:         true,
		BargeInDelay:    100 * time.Millisecond,
	}
	log, interrupt, msgs := bargeInScenario(t, cfg)

	var clear *received
	for i, r := range msgs {
		if r.msg.Event == twilio.EventClear {
			clear = &msgs[i]
			break
		}
	}
	if clear == nil {
		t.Fatal("agent never sent clear")
	}
	// Default BargeInMinSpeech (60 ms) to detect, plus the reaction delay.
	within(t, "interrupt to clear", clear.at.Sub(interrupt), 60*time.Millisecond+cfg.BargeInDelay)

	// Nothing from the first response after the clear; the next audio is
	// the answer to the interruption, which comes much later.
	for _, r := range msgs {
		if r.msg.Event == twilio.EventMedia && r.at.After(clear.at) && r.at.Before(clear.at.Add(300*time.Millisecond)) {
			t.Fatalf("agent kept talking %v after clear", r.at.Sub(clear.at))
		}
	}
	if _, ok := log.find(EventResponseInterrupted, 1); !ok {
		t.Error("missing response_interrupted event for response 1")
	}
	if _, ok := log.find(EventResponseStarted, 2); !ok {
		t.Error("agent did not answer the interruption")
	}
}

func TestNoBargeInFinishesResponse(t *testing.T) {
	cfg := Config{ResponseDelay: 100 * time.Millisecond, EndpointSilence: 300 * time.Millisecond}
	_, _, msgs := bargeInScenario(t, cfg)

	for _, r := range msgs {
		if r.msg.Event == twilio.EventClear {
			t.Fatal("agent sent clear with barge-in disabled")
		}
	}
	media, mark := mediaUntilMark(msgs)
	if mark == nil {
		t.Fatal("first response never finished")
	}
	if want := len(tone(time.Second, 1)); len(media) != want {
		t.Errorf("first response had %d frames, want all %d", len(media), want)
	}
}

func TestBurstSendsAtOnce(t *testing.T) {
	cfg := Config{EndpointSilence: 200 * time.Millisecond, Burst: true}
	srv, _ := newServer(t, cfg)
	c := dial(t, srv, "burst")
	c.play(tone(400*time.Millisecond, 8000))
	c.play(silence(time.Second))
	media, mark := mediaUntilMark(c.hangUp())

	if len(media) != 20 {
		t.Fatalf("got %d frames, want 20", len(media))
	}
	if span := media[len(media)-1].at.Sub(media[0].at); span > 50*time.Millisecond {
		t.Errorf("burst took %v, want under 50ms", span)
	}
	// The mark still marks the end of the audio's real duration.
	if mark == nil || mark.at.Sub(media[0].at) < 380*time.Millisecond {
		t.Errorf("mark arrived too early: %+v", mark)
	}
}

func TestRejectsWrongMediaFormat(t *testing.T) {
	srv, _ := newServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()

	caller := twilio.NewCaller("bad", nil)
	start := caller.Start()
	start.Start.MediaFormat.SampleRate = 16000
	for _, m := range []twilio.Message{caller.Connected(), start} {
		data, _ := twilio.Encode(m)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = conn.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v (err %v), want policy violation", got, err)
	}
}

// Many calls at once each get their own audio back, with no cross-talk.
func TestConcurrentCallsAreIsolated(t *testing.T) {
	cfg := Config{EndpointSilence: 100 * time.Millisecond, Burst: true}
	srv, _ := newServer(t, cfg)

	const calls = 20
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dial(t, srv, fmt.Sprint(i))
			speech := tone(100*time.Millisecond, float64(1000+i*1000))
			c.play(speech)
			c.play(silence(500 * time.Millisecond))
			media, _ := mediaUntilMark(c.hangUp())
			if len(media) != len(speech) {
				t.Errorf("call %d: got %d frames, want %d", i, len(media), len(speech))
				return
			}
			for j, r := range media {
				got, _ := r.msg.Media.Audio()
				if !bytes.Equal(got, speech[j]) {
					t.Errorf("call %d: frame %d is not its own audio", i, j)
					return
				}
			}
		}()
	}
	wg.Wait()
}
