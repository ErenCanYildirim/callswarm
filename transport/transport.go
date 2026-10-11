// Package transport connects a simulated caller to a voice agent.
//
// Conn plays Twilio's side of a Media Streams WebSocket call: it performs the
// handshake, streams caller audio in real time, and receives the agent's
// audio. It also emulates what Twilio does with that audio — buffering it,
// playing it out in real time, echoing marks when playback reaches them and
// discarding the buffer on clear — so agents that depend on that behavior
// work unchanged, and so callers can tell when agent audio is actually heard
// rather than merely received.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ErenCanYildirim/callswarm/twilio"
)

// FrameDuration is the length of one caller audio frame.
const FrameDuration = 20 * time.Millisecond

// writeTimeout bounds each WebSocket write. Writes use their own context
// rather than the call's: the WebSocket library closes the connection when a
// write's context is cancelled mid-write, which would break Close.
const writeTimeout = 5 * time.Second

// inboundBuffer is how many agent messages may queue before the read loop
// waits for the consumer.
const inboundBuffer = 4096

// Options configures a connection.
type Options struct {
	// CallID distinguishes concurrent calls in stream and call SIDs.
	CallID string
	// Parameters are sent as the start message's customParameters, which
	// some agents use to route or configure the call.
	Parameters map[string]string
	// Header is added to the WebSocket upgrade request, for example to
	// carry credentials.
	Header http.Header
	// CloseTimeout bounds how long Close spends sending stop. Default 5 s.
	CloseTimeout time.Duration
}

// Kind is the type of an inbound agent message.
type Kind int

// Inbound message kinds.
const (
	KindAudio Kind = iota // a frame of agent audio
	KindMark              // the agent placed a mark after its audio
	KindClear             // the agent discarded its buffered audio
)

func (k Kind) String() string {
	switch k {
	case KindAudio:
		return "audio"
	case KindMark:
		return "mark"
	case KindClear:
		return "clear"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Inbound is one message from the agent.
type Inbound struct {
	Kind Kind
	// At is when the message arrived.
	At time.Time
	// Audio is μ-law agent audio, for KindAudio.
	Audio []byte
	// PlayAt is when a caller on a real phone would start hearing this
	// audio: arrival time if nothing is buffered, otherwise the moment the
	// audio ahead of it finishes. Agents that send faster than real time
	// make PlayAt later than At. Set for KindAudio.
	PlayAt time.Time
	// Mark is the mark name, for KindMark.
	Mark string
}

// Playback describes caller audio after it has been sent.
type Playback struct {
	// Began is when the audio started, in real time. Frames are sent once
	// captured, so this is one frame before the first send.
	Began time.Time
	// Ended is when the last frame was sent, which is when the audio ended.
	Ended time.Time
	// Frames is the number of 20 ms frames sent.
	Frames int
}

// Stats describes how well the sender kept real-time pace. Large lateness
// means the load generator itself was struggling, which would distort
// latency measurements.
type Stats struct {
	FramesSent   int           // total frames, including silence fill
	SilenceSent  int           // frames of silence sent while idle
	MaxLateness  time.Duration // worst delay of a frame past its schedule
	MeanLateness time.Duration
	LateFrames   int // frames sent more than 5 ms past schedule
}

// Conn is one simulated call over Twilio Media Streams.
type Conn struct {
	opts   Options
	ws     *websocket.Conn
	caller *twilio.Caller

	ctx    context.Context // cancelled when the call ends for any reason
	cancel context.CancelCauseFunc

	writeMu sync.Mutex // guards caller and writes, so sequence numbers go out in order
	sender  *Sender
	playout Playout

	marksMu   sync.Mutex
	marks     []*pendingMark
	inbound   chan Inbound
	readDone  chan struct{}
	closeOnce sync.Once
}

type pendingMark struct {
	name  string
	timer *time.Timer
}

// ErrClosed is the cause when the call was ended by Close.
var ErrClosed = errors.New("call closed")

// Dial connects to a Twilio Media Streams agent at url (ws:// or wss://) and
// performs the handshake. The call streams silence until audio is played,
// as a real phone line would.
func Dial(ctx context.Context, url string, opts Options) (*Conn, error) {
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 5 * time.Second
	}
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: opts.Header})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	ws.SetReadLimit(1 << 20)

	callCtx, cancel := context.WithCancelCause(context.Background())
	c := &Conn{
		opts:     opts,
		ws:       ws,
		caller:   twilio.NewCaller(opts.CallID, opts.Parameters),
		ctx:      callCtx,
		cancel:   cancel,
		inbound:  make(chan Inbound, inboundBuffer),
		readDone: make(chan struct{}),
	}
	c.sender = NewSender(callCtx, cancel, func(frame []byte, offset time.Duration) error {
		return c.writeNow(func(tc *twilio.Caller) twilio.Message { return tc.Media(frame, offset) })
	})
	for _, build := range []func(*twilio.Caller) twilio.Message{(*twilio.Caller).Connected, (*twilio.Caller).Start} {
		if err := c.write(ctx, build); err != nil {
			_ = ws.CloseNow()
			cancel(err)
			return nil, fmt.Errorf("handshake: %w", err)
		}
	}
	go c.readLoop()
	c.sender.Start(time.Now())
	return c, nil
}

// StreamSid identifies this call in the agent's logs.
func (c *Conn) StreamSid() string { return c.caller.StreamSid }

// Inbound returns the agent's messages in arrival order. It is closed when
// the call ends. The consumer must keep draining it; if it fills up, reading
// from the agent pauses.
func (c *Conn) Inbound() <-chan Inbound { return c.inbound }

// Done is closed when the call has ended, by either side.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Err reports why the call ended: ErrClosed after Close, or the error that
// ended it. It is nil while the call is active.
func (c *Conn) Err() error { return context.Cause(c.ctx) }

// Stats reports sender pacing so far.
func (c *Conn) Stats() Stats { return c.sender.Stats() }

// Play queues μ-law audio and blocks until all of it has been sent in real
// time; see Sender.Play.
func (c *Conn) Play(ctx context.Context, mulaw []byte) (Playback, error) {
	return c.sender.Play(ctx, mulaw)
}

// Close ends the call the way Twilio does: it sends stop and closes the
// WebSocket. It returns an error only if the stop message could not be
// sent. Calling it again, or after the agent hung up, is safe.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		alreadyEnded := c.ctx.Err() != nil
		c.cancel(ErrClosed)
		<-c.sender.Done()
		c.clearMarks(false)

		if !alreadyEnded {
			ctx, cancel := context.WithTimeout(context.Background(), c.opts.CloseTimeout)
			defer cancel()
			if werr := c.write(ctx, (*twilio.Caller).Stop); werr != nil {
				err = fmt.Errorf("sending stop: %w", werr)
			}
			// The agent may close first in response to stop; either way the
			// call is over, so close-handshake errors are not reported.
			_ = c.ws.Close(websocket.StatusNormalClosure, "")
		}
		_ = c.ws.CloseNow()
		<-c.readDone
	})
	return err
}

// write builds a message and sends it. Building happens under the lock
// because the caller assigns sequence numbers.
func (c *Conn) write(ctx context.Context, build func(*twilio.Caller) twilio.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := twilio.Encode(build(c.caller))
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// writeNow writes with a fresh timeout, independent of the call's context.
func (c *Conn) writeNow(build func(*twilio.Caller) twilio.Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return c.write(ctx, build)
}

// readLoop receives agent messages until the connection ends.
func (c *Conn) readLoop() {
	defer close(c.readDone)
	defer close(c.inbound)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			if c.ctx.Err() == nil {
				if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
					c.cancel(errors.New("agent hung up"))
				} else {
					c.cancel(fmt.Errorf("receiving: %w", err))
				}
			}
			return
		}
		at := time.Now()
		msg, err := twilio.Decode(data)
		if err != nil {
			c.cancel(fmt.Errorf("agent sent invalid message: %w", err))
			_ = c.ws.CloseNow()
			return
		}
		var in Inbound
		switch msg.Event {
		case twilio.EventMedia:
			pcm, err := msg.Media.Audio()
			if err != nil {
				continue // Decode already validated the payload
			}
			in = Inbound{Kind: KindAudio, At: at, Audio: pcm, PlayAt: c.playout.Add(at, len(pcm))}
		case twilio.EventMark:
			c.addMark(at, msg.Mark.Name)
			in = Inbound{Kind: KindMark, At: at, Mark: msg.Mark.Name}
		case twilio.EventClear:
			c.clearBuffer(at)
			in = Inbound{Kind: KindClear, At: at}
		default:
			continue
		}
		select {
		case c.inbound <- in:
		case <-c.ctx.Done():
			// The call has ended and nobody may be reading; drop the rest.
		}
	}
}

// addMark echoes the mark once the audio buffered before it has played.
func (c *Conn) addMark(at time.Time, name string) {
	c.marksMu.Lock()
	defer c.marksMu.Unlock()
	wait := c.playout.End().Sub(at)
	if wait <= 0 {
		go c.echoMark(name)
		return
	}
	pm := &pendingMark{name: name}
	pm.timer = time.AfterFunc(wait, func() {
		if c.removeMark(pm) {
			c.echoMark(name)
		}
	})
	c.marks = append(c.marks, pm)
}

func (c *Conn) removeMark(pm *pendingMark) bool {
	c.marksMu.Lock()
	defer c.marksMu.Unlock()
	for i, m := range c.marks {
		if m == pm {
			c.marks = append(c.marks[:i], c.marks[i+1:]...)
			return true
		}
	}
	return false
}

// clearBuffer discards unplayed agent audio and, like Twilio, immediately
// echoes the marks that were waiting on it.
func (c *Conn) clearBuffer(at time.Time) {
	c.playout.Clear(at)
	c.clearMarks(true)
}

func (c *Conn) clearMarks(echo bool) {
	c.marksMu.Lock()
	marks := c.marks
	c.marks = nil
	c.marksMu.Unlock()
	for _, m := range marks {
		if m.timer.Stop() && echo {
			c.echoMark(m.name)
		}
	}
}

func (c *Conn) echoMark(name string) {
	if c.ctx.Err() != nil {
		return
	}
	markPlayed := func(tc *twilio.Caller) twilio.Message { return tc.MarkPlayed(name) }
	if err := c.writeNow(markPlayed); err != nil && c.ctx.Err() == nil {
		c.cancel(fmt.Errorf("echoing mark: %w", err))
	}
}
