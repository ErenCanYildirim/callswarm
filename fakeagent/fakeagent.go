// Package fakeagent is a voice agent with known, configurable behavior that
// speaks the Twilio Media Streams protocol. It needs no API keys.
//
// It listens for caller speech, waits for the caller to stop, pauses for a
// configured delay and then echoes the caller's words back. Because its
// timing is set by the test rather than by an STT, LLM and TTS stack, it is
// the ground truth for checking callswarm's own measurements: if the agent is
// configured to answer after 500 ms and callswarm reports 610 ms, the extra
// 110 ms is measurement error.
//
// Expected time to first audio, measured from the end of caller speech, is
// Config.EndpointSilence + Config.ResponseDelay, give or take one 20 ms frame.
package fakeagent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/twilio"
)

// frameDuration is the length of one μ-law frame from Twilio.
const frameDuration = 20 * time.Millisecond

// Config sets the agent's behavior. The zero value is usable; zero fields
// take the defaults noted on each field.
type Config struct {
	// ResponseDelay is how long the agent waits after detecting the end of
	// the caller's turn before it starts answering. Default 0.
	ResponseDelay time.Duration

	// EndpointSilence is how much silence after speech ends the caller's
	// turn. Counted in audio time, since Twilio streams continuously.
	// Default 300 ms.
	EndpointSilence time.Duration

	// SpeechThreshold is the RMS level (of 16-bit linear samples) above which
	// a frame counts as speech. Default 500.
	SpeechThreshold float64

	// BargeIn makes the agent stop talking when the caller speaks over it.
	// When false, the agent finishes every response regardless, which is
	// useful for checking that callswarm detects agents that ignore
	// interruptions.
	BargeIn bool

	// BargeInMinSpeech is how much caller speech during a response counts as
	// an interruption. Default 60 ms.
	BargeInMinSpeech time.Duration

	// BargeInDelay is the agent's reaction time between detecting an
	// interruption and stopping its audio. Default 0.
	BargeInDelay time.Duration

	// MaxResponse caps the length of an echoed response. Default 10 s.
	MaxResponse time.Duration

	// Burst sends each response as fast as possible instead of in real time.
	// Real agents do both; Twilio buffers whatever arrives early.
	Burst bool

	// OnEvent, if set, is called for each agent event. It is called from
	// multiple goroutines and must be safe for concurrent use.
	OnEvent func(Event)
}

// ExpectedFirstAudio is the time from the end of caller speech to the
// agent's first audio, give or take one frame.
func (c Config) ExpectedFirstAudio() time.Duration {
	c = c.withDefaults()
	return c.EndpointSilence + c.ResponseDelay
}

func (c Config) withDefaults() Config {
	if c.EndpointSilence <= 0 {
		c.EndpointSilence = 300 * time.Millisecond
	}
	if c.SpeechThreshold <= 0 {
		c.SpeechThreshold = 500
	}
	if c.BargeInMinSpeech <= 0 {
		c.BargeInMinSpeech = 60 * time.Millisecond
	}
	if c.MaxResponse <= 0 {
		c.MaxResponse = 10 * time.Second
	}
	return c
}

// EventKind identifies an agent event.
type EventKind string

// Agent events, in the order they typically occur.
const (
	EventStarted             EventKind = "started"              // start message accepted
	EventSpeechStarted       EventKind = "speech_started"       // caller began speaking
	EventSpeechEnded         EventKind = "speech_ended"         // end of turn detected
	EventResponseStarted     EventKind = "response_started"     // first response frame sent
	EventResponseFinished    EventKind = "response_finished"    // last frame and mark sent
	EventBargeInDetected     EventKind = "barge_in_detected"    // interruption recognized
	EventResponseInterrupted EventKind = "response_interrupted" // audio stopped, clear sent
	EventMarkReceived        EventKind = "mark_received"        // caller echoed a mark
	EventStopped             EventKind = "stopped"              // stream ended
)

// Event is something the agent did, with the wall-clock time it happened.
type Event struct {
	Kind      EventKind
	At        time.Time
	StreamSid string
	// Response numbers the agent's responses within a call, from 1. Set on
	// response and barge-in events.
	Response int
	// Detail carries the mark name on EventMarkReceived.
	Detail string
}

// Agent is an http.Handler that accepts Twilio Media Streams connections.
type Agent struct {
	cfg Config
}

// New returns an agent with the given behavior.
func New(cfg Config) *Agent {
	return &Agent{cfg: cfg.withDefaults()}
}

// ServeHTTP upgrades the request to a WebSocket and runs one call.
func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// Echoed responses can be large in burst mode.
	conn.SetReadLimit(1 << 20)
	s := &session{cfg: a.cfg, conn: conn}
	err = s.run(r.Context())
	// Close errors are ignored: the call is over either way, and the peer
	// may already have gone.
	switch {
	case err == nil:
		_ = conn.Close(websocket.StatusNormalClosure, "")
	case errors.Is(err, errBadStream):
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
	default:
		_ = conn.CloseNow()
	}
}

var errBadStream = errors.New("unsupported stream")

// utterance is one caller turn waiting to be answered.
type utterance struct {
	audio []byte    // μ-law, from the first speech frame to the last
	ended time.Time // when the end of turn was detected
}

type session struct {
	cfg       Config
	conn      *websocket.Conn
	streamSid string

	// Voice activity state, touched only by the read loop.
	inSpeech   bool
	speechRun  time.Duration
	silenceRun time.Duration
	current    []byte // the turn so far
	pending    []byte // silence since the last speech frame, kept if speech resumes

	// Response state, shared with the responder goroutine.
	mu          sync.Mutex
	responding  bool
	responseNum int
	cancelResp  context.CancelFunc
	bargedIn    bool
}

func (s *session) emit(kind EventKind, response int, detail string) {
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(Event{Kind: kind, At: time.Now(), StreamSid: s.streamSid, Response: response, Detail: detail})
	}
}

func (s *session) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	turns := make(chan utterance, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.respond(ctx, turns)
	}()
	defer wg.Wait()
	defer cancel()

	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		msg, err := twilio.Decode(data)
		if err != nil {
			return err
		}
		switch msg.Event {
		case twilio.EventStart:
			if err := checkFormat(msg.Start.MediaFormat); err != nil {
				return err
			}
			s.streamSid = msg.Start.StreamSid
			s.emit(EventStarted, 0, "")
		case twilio.EventMedia:
			frame, err := msg.Media.Audio()
			if err != nil {
				return err
			}
			if u, done := s.listen(frame); done {
				select {
				case turns <- u:
				default:
					// Too many unanswered turns; drop rather than block reading.
				}
			}
		case twilio.EventMark:
			s.emit(EventMarkReceived, 0, msg.Mark.Name)
		case twilio.EventStop:
			s.emit(EventStopped, 0, "")
			return nil
		}
	}
}

func checkFormat(f twilio.MediaFormat) error {
	if f.Encoding != twilio.Encoding || f.SampleRate != twilio.SampleRate || f.Channels != twilio.Channels {
		return fmt.Errorf("%w: %s %d Hz %d channel(s)", errBadStream, f.Encoding, f.SampleRate, f.Channels)
	}
	return nil
}

// listen runs simple energy-based voice activity detection on one frame. It
// returns a completed utterance when the caller's turn ends.
func (s *session) listen(frame []byte) (utterance, bool) {
	speech := rms(audio.MulawDecode(frame)) >= s.cfg.SpeechThreshold
	d := time.Duration(len(frame)) * time.Second / twilio.SampleRate

	if speech {
		s.speechRun += d
		s.silenceRun = 0
		if !s.inSpeech {
			s.inSpeech = true
			s.current = s.current[:0]
			s.pending = s.pending[:0]
			s.emit(EventSpeechStarted, 0, "")
		}
		s.checkBargeIn()
	} else {
		s.speechRun = 0
		if s.inSpeech {
			s.silenceRun += d
		}
	}

	if !s.inSpeech {
		return utterance{}, false
	}
	maxBytes := int(s.cfg.MaxResponse * twilio.SampleRate / time.Second)
	if speech && len(s.current) < maxBytes {
		// Keep any silence inside the utterance so the echo sounds natural,
		// but not the trailing silence that ends it.
		s.current = append(s.current, s.pending...)
		s.pending = s.pending[:0]
		s.current = append(s.current, frame...)
		if len(s.current) > maxBytes {
			s.current = s.current[:maxBytes]
		}
	} else if !speech {
		s.pending = append(s.pending, frame...)
	}

	if s.silenceRun >= s.cfg.EndpointSilence {
		s.inSpeech = false
		s.silenceRun = 0
		s.pending = s.pending[:0]
		u := utterance{audio: append([]byte(nil), s.current...), ended: time.Now()}
		s.emit(EventSpeechEnded, 0, "")
		return u, true
	}
	return utterance{}, false
}

// checkBargeIn stops the current response once the caller has spoken over
// it for long enough.
func (s *session) checkBargeIn() {
	if !s.cfg.BargeIn || s.speechRun < s.cfg.BargeInMinSpeech {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.responding || s.bargedIn {
		return
	}
	s.bargedIn = true
	s.emit(EventBargeInDetected, s.responseNum, "")
	cancel := s.cancelResp
	if s.cfg.BargeInDelay > 0 {
		time.AfterFunc(s.cfg.BargeInDelay, cancel)
	} else {
		cancel()
	}
}

// respond answers each completed turn in order.
func (s *session) respond(ctx context.Context, turns <-chan utterance) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-turns:
			if len(u.audio) == 0 {
				continue
			}
			// The delay runs from the end of the turn, not from when this
			// goroutine got to it, so a backlog doesn't inflate it.
			if !sleepUntil(ctx, u.ended.Add(s.cfg.ResponseDelay)) {
				return
			}
			if err := s.speak(ctx, u.audio); err != nil {
				return
			}
		}
	}
}

// speak streams one response, stopping early on barge-in.
func (s *session) speak(ctx context.Context, mulaw []byte) error {
	respCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	s.responseNum++
	n := s.responseNum
	s.responding = true
	s.bargedIn = false
	s.cancelResp = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.responding = false
		s.mu.Unlock()
	}()

	frames := audio.FramesPadded(mulaw, audio.TelephonyFrameSamples, audio.MulawSilence)
	start := time.Now()
	for i, f := range frames {
		if !s.cfg.Burst && !sleepUntil(respCtx, start.Add(time.Duration(i)*frameDuration)) {
			break
		}
		if respCtx.Err() != nil {
			break
		}
		if err := s.send(ctx, twilio.AgentMedia(s.streamSid, f)); err != nil {
			return err
		}
		if i == 0 {
			s.emit(EventResponseStarted, n, "")
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	// In burst mode every frame is already sent before a barge-in can land,
	// so wait out the response's real duration to give it the chance.
	if s.cfg.Burst {
		end := start.Add(time.Duration(len(frames)) * frameDuration)
		sleepUntil(respCtx, end)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	if respCtx.Err() != nil {
		// Interrupted: tell the caller to drop any audio still buffered.
		if err := s.send(ctx, twilio.Clear(s.streamSid)); err != nil {
			return err
		}
		s.emit(EventResponseInterrupted, n, "")
		return nil
	}
	if err := s.send(ctx, twilio.AgentMark(s.streamSid, fmt.Sprintf("response-%d", n))); err != nil {
		return err
	}
	s.emit(EventResponseFinished, n, "")
	return nil
}

func (s *session) send(ctx context.Context, m twilio.Message) error {
	data, err := twilio.Encode(m)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, data)
}

// sleepUntil waits until t or until ctx is done. It reports whether the
// wait completed.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func rms(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(samples)))
}
