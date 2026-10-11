package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/twilio"
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

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// tone returns d of 1 kHz tone as μ-law, a whole number of frames.
func tone(d time.Duration, amp float64) []byte {
	n := int(d / FrameDuration * audio.TelephonyFrameSamples)
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(amp * math.Sin(2*math.Pi*1000*float64(i)/audio.TelephonySampleRate))
	}
	return audio.MulawEncode(pcm)
}

type eventLog struct {
	mu     sync.Mutex
	events []fakeagent.Event
}

func (l *eventLog) add(e fakeagent.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) has(kind fakeagent.EventKind) (fakeagent.Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == kind {
			return e, true
		}
	}
	return fakeagent.Event{}, false
}

func agentServer(t *testing.T, cfg fakeagent.Config) (*httptest.Server, *eventLog) {
	t.Helper()
	log := &eventLog{}
	cfg.OnEvent = log.add
	srv := httptest.NewServer(fakeagent.New(cfg))
	t.Cleanup(srv.Close)
	return srv, log
}

func dial(t *testing.T, url string, opts Options) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// collector drains a call's inbound messages in the background.
type collector struct {
	mu   sync.Mutex
	msgs []Inbound
	done chan struct{}
}

func collect(c *Conn) *collector {
	col := &collector{done: make(chan struct{})}
	go func() {
		defer close(col.done)
		for in := range c.Inbound() {
			col.mu.Lock()
			col.msgs = append(col.msgs, in)
			col.mu.Unlock()
		}
	}()
	return col
}

// waitFor returns messages up to and including the first that matches, or
// fails after timeout.
func (col *collector) waitFor(t *testing.T, match func(Inbound) bool, timeout time.Duration) []Inbound {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		col.mu.Lock()
		for i, in := range col.msgs {
			if match(in) {
				out := append([]Inbound(nil), col.msgs[:i+1]...)
				col.mu.Unlock()
				return out
			}
		}
		col.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no matching message within %v", timeout)
	return nil
}

func isKind(k Kind) func(Inbound) bool { return func(in Inbound) bool { return in.Kind == k } }

func audioOnly(msgs []Inbound) []Inbound {
	var out []Inbound
	for _, m := range msgs {
		if m.Kind == KindAudio {
			out = append(out, m)
		}
	}
	return out
}

func TestHandshakeAndClose(t *testing.T) {
	srv, log := agentServer(t, fakeagent.Config{})
	c := dial(t, wsURL(srv), Options{CallID: "hs"})
	col := collect(c)

	time.Sleep(100 * time.Millisecond)
	started, ok := log.has(fakeagent.EventStarted)
	if !ok || started.StreamSid != c.StreamSid() {
		t.Fatalf("agent did not accept the stream: %+v", started)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Errorf("Err() = %v, want ErrClosed", c.Err())
	}
	select {
	case <-col.done:
	case <-time.After(2 * time.Second):
		t.Fatal("inbound channel not closed after Close")
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := log.has(fakeagent.EventStopped); !ok {
		t.Error("agent never saw stop")
	}
	if err := c.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestPlayIsRealTime(t *testing.T) {
	srv, _ := agentServer(t, fakeagent.Config{})
	c := dial(t, wsURL(srv), Options{CallID: "pace"})
	collect(c)

	pb, err := c.Play(context.Background(), tone(time.Second, 8000))
	if err != nil {
		t.Fatal(err)
	}
	if pb.Frames != 50 {
		t.Errorf("frames = %d, want 50", pb.Frames)
	}
	within(t, "playback duration", pb.Ended.Sub(pb.Began), time.Second)

	s := c.Stats()
	t.Logf("pacing: %+v", s)
	if s.MeanLateness > 5*time.Millisecond {
		t.Errorf("mean lateness %v, want under 5ms", s.MeanLateness)
	}
}

// End-to-end calibration: the transport's own timestamps reproduce the
// fake agent's configured latency, and the echo is the caller's audio.
func TestFirstAudioMatchesAgentTiming(t *testing.T) {
	cfg := fakeagent.Config{ResponseDelay: 200 * time.Millisecond, EndpointSilence: 300 * time.Millisecond}
	srv, _ := agentServer(t, cfg)
	c := dial(t, wsURL(srv), Options{CallID: "ttfa"})
	col := collect(c)

	speech := tone(300*time.Millisecond, 8000)
	pb, err := c.Play(context.Background(), speech)
	if err != nil {
		t.Fatal(err)
	}
	// The agent can only detect the end of the turn because the
	// transport keeps streaming silence after the clip.
	msgs := audioOnly(col.waitFor(t, isKind(KindMark), 3*time.Second))
	if len(msgs) == 0 {
		t.Fatal("no agent audio")
	}
	within(t, "time to first audio", msgs[0].PlayAt.Sub(pb.Ended), cfg.ExpectedFirstAudio())

	var echo []byte
	for _, m := range msgs {
		echo = append(echo, m.Audio...)
	}
	if !bytes.Equal(echo, speech) {
		t.Errorf("echo (%d bytes) differs from caller audio (%d bytes)", len(echo), len(speech))
	}
	if c.Stats().SilenceSent == 0 {
		t.Error("no silence sent between clips")
	}
}

// Audio that arrives faster than real time plays back at real time.
func TestBurstAudioPlayAt(t *testing.T) {
	srv, _ := agentServer(t, fakeagent.Config{EndpointSilence: 200 * time.Millisecond, Burst: true})
	c := dial(t, wsURL(srv), Options{CallID: "burst"})
	col := collect(c)

	if _, err := c.Play(context.Background(), tone(400*time.Millisecond, 8000)); err != nil {
		t.Fatal(err)
	}
	msgs := audioOnly(col.waitFor(t, isKind(KindMark), 3*time.Second))
	if len(msgs) != 20 {
		t.Fatalf("got %d frames, want 20", len(msgs))
	}
	first, last := msgs[0], msgs[len(msgs)-1]
	if arrival := last.At.Sub(first.At); arrival > 50*time.Millisecond {
		t.Errorf("burst arrived over %v; test needs it fast", arrival)
	}
	if got := last.PlayAt.Sub(first.PlayAt); got != 380*time.Millisecond {
		t.Errorf("playout span = %v, want exactly 380ms", got)
	}
}

func TestBargeInClearArrives(t *testing.T) {
	cfg := fakeagent.Config{
		ResponseDelay:   100 * time.Millisecond,
		EndpointSilence: 300 * time.Millisecond,
		BargeIn:         true,
	}
	srv, _ := agentServer(t, cfg)
	c := dial(t, wsURL(srv), Options{CallID: "barge"})
	col := collect(c)

	if _, err := c.Play(context.Background(), tone(time.Second, 8000)); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, isKind(KindAudio), 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	pb, err := c.Play(context.Background(), tone(300*time.Millisecond, 6000))
	if err != nil {
		t.Fatal(err)
	}
	msgs := col.waitFor(t, isKind(KindClear), 2*time.Second)
	clear := msgs[len(msgs)-1]
	// Default 60 ms of speech to detect, no reaction delay.
	within(t, "interruption to clear", clear.At.Sub(pb.Began), 60*time.Millisecond)
}

// scripted runs a hand-written agent for checks the fake agent can't make.
func scripted(t *testing.T, script func(ctx context.Context, ws *websocket.Conn, in <-chan twilio.Message, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		in := make(chan twilio.Message, 1024)
		go func() {
			defer close(in)
			for {
				_, data, err := ws.Read(ctx)
				if err != nil {
					return
				}
				m, err := twilio.Decode(data)
				if err != nil {
					t.Errorf("caller sent invalid message: %v", err)
					return
				}
				in <- m
			}
		}()
		script(ctx, ws, in, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func agentSend(ctx context.Context, t *testing.T, ws *websocket.Conn, m twilio.Message) {
	data, _ := twilio.Encode(m)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Errorf("agent write: %v", err)
	}
}

// awaitEvent skips caller media until an event of the given kind arrives.
func awaitEvent(in <-chan twilio.Message, event string, timeout time.Duration) (twilio.Message, time.Time, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-in:
			if !ok {
				return m, time.Time{}, false
			}
			if m.Event == event {
				return m, time.Now(), true
			}
		case <-deadline:
			return twilio.Message{}, time.Time{}, false
		}
	}
}

func TestMarksEchoAfterPlayoutAndOnClear(t *testing.T) {
	type result struct {
		played, cleared time.Duration
		names           []string
	}
	res := make(chan result, 1)
	srv := scripted(t, func(ctx context.Context, ws *websocket.Conn, in <-chan twilio.Message, _ *http.Request) {
		start, _, ok := awaitEvent(in, twilio.EventStart, time.Second)
		if !ok {
			return
		}
		sid := start.StreamSid
		frame := bytes.Repeat([]byte{0x00}, audio.TelephonyFrameSamples)
		var r result

		// 200 ms of audio sent at once, then a mark: echoed after playout.
		for range 10 {
			agentSend(ctx, t, ws, twilio.AgentMedia(sid, frame))
		}
		agentSend(ctx, t, ws, twilio.AgentMark(sid, "m1"))
		sent := time.Now()
		m, at, ok := awaitEvent(in, twilio.EventMark, 2*time.Second)
		if !ok {
			return
		}
		r.played = at.Sub(sent)
		r.names = append(r.names, m.Mark.Name)

		// 500 ms of audio and a mark, then clear: echoed immediately.
		for range 25 {
			agentSend(ctx, t, ws, twilio.AgentMedia(sid, frame))
		}
		agentSend(ctx, t, ws, twilio.AgentMark(sid, "m2"))
		agentSend(ctx, t, ws, twilio.Clear(sid))
		sent = time.Now()
		m, at, ok = awaitEvent(in, twilio.EventMark, 2*time.Second)
		if !ok {
			return
		}
		r.cleared = at.Sub(sent)
		r.names = append(r.names, m.Mark.Name)
		res <- r
	})

	c := dial(t, wsURL(srv), Options{CallID: "marks"})
	collect(c)
	select {
	case r := <-res:
		if len(r.names) != 2 || r.names[0] != "m1" || r.names[1] != "m2" {
			t.Errorf("marks echoed = %v, want [m1 m2]", r.names)
		}
		within(t, "mark echo after playout", r.played, 200*time.Millisecond)
		if r.cleared > early {
			t.Errorf("mark echo after clear took %v, want immediate", r.cleared)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scripted agent did not finish")
	}
}

func TestSendsHeaderAndParameters(t *testing.T) {
	got := make(chan [2]string, 1)
	srv := scripted(t, func(_ context.Context, _ *websocket.Conn, in <-chan twilio.Message, r *http.Request) {
		start, _, ok := awaitEvent(in, twilio.EventStart, time.Second)
		if ok {
			got <- [2]string{r.Header.Get("Authorization"), start.Start.CustomParameters["agent"]}
		}
	})
	dial(t, wsURL(srv), Options{
		CallID:     "hdr",
		Header:     http.Header{"Authorization": {"Bearer secret"}},
		Parameters: map[string]string{"agent": "support"},
	})
	select {
	case g := <-got:
		if g[0] != "Bearer secret" || g[1] != "support" {
			t.Errorf("header, parameter = %q, %q", g[0], g[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no start received")
	}
}

func TestAgentHangUpEndsCall(t *testing.T) {
	srv := scripted(t, func(ctx context.Context, ws *websocket.Conn, in <-chan twilio.Message, _ *http.Request) {
		if _, _, ok := awaitEvent(in, twilio.EventStart, time.Second); ok {
			_ = ws.Close(websocket.StatusNormalClosure, "bye")
		}
	})
	c := dial(t, wsURL(srv), Options{CallID: "hangup"})
	collect(c)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Play(ctx, tone(time.Second, 8000))
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Play err = %v, want hang-up error", err)
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if !strings.Contains(c.Err().Error(), "hung up") {
		t.Errorf("Err() = %v", c.Err())
	}
	if err := c.Close(); err != nil {
		t.Errorf("close after hang-up: %v", err)
	}
}

func TestPlayCancellation(t *testing.T) {
	srv, _ := agentServer(t, fakeagent.Config{})
	c := dial(t, wsURL(srv), Options{CallID: "cancel"})
	collect(c)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	pb, err := c.Play(ctx, tone(time.Second, 8000))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if pb.Frames == 0 || pb.Frames >= 50 {
		t.Errorf("frames sent before cancel = %d", pb.Frames)
	}
	// The call is still usable.
	pb, err = c.Play(context.Background(), tone(100*time.Millisecond, 8000))
	if err != nil || pb.Frames != 5 {
		t.Errorf("play after cancel: %+v, %v", pb, err)
	}
}

func TestDialFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, "ws://127.0.0.1:1/nothing", Options{}); err == nil {
		t.Fatal("expected dial error")
	}
}

// Concurrent calls each hear only their own echo.
func TestConcurrentCalls(t *testing.T) {
	srv, _ := agentServer(t, fakeagent.Config{EndpointSilence: 100 * time.Millisecond, Burst: true})
	const calls = 25
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := Dial(ctx, wsURL(srv), Options{CallID: fmt.Sprint(i)})
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			defer func() { _ = c.Close() }()

			speech := tone(100*time.Millisecond, float64(1000+i*1000))
			if _, err := c.Play(ctx, speech); err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			var echo []byte
			for in := range c.Inbound() {
				if in.Kind == KindMark {
					break
				}
				echo = append(echo, in.Audio...)
			}
			if !bytes.Equal(echo, speech) {
				t.Errorf("call %d: heard %d bytes that are not its own echo", i, len(echo))
			}
		}()
	}
	wg.Wait()
}
