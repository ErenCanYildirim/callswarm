// Package livekit connects a simulated caller to a voice agent through a
// LiveKit room.
//
// Each call joins its own room as an ordinary participant, publishes the
// caller's audio as a microphone track and listens to the first audio track
// another participant publishes, which is taken to be the agent's. LiveKit
// Agents dispatched automatically to new rooms join on their own; an agent
// that registers with a name is dispatched explicitly with Options.AgentName.
//
// The agent joins after the caller, so Dial waits for it to answer: for its
// audio track to arrive and for it to subscribe to the caller's. Until then
// the caller would be talking to an empty room.
//
// Audio is Opus on the wire, encoded and decoded in pure Go. The caller's
// clips are 8 kHz telephone audio, as for every callswarm transport, and the
// agent's audio is decoded straight to 8 kHz for speech detection, so
// measurements work the same way as over Twilio. The caller's audio is
// therefore narrowband: fine for timing, slightly worse for an agent's
// speech recognition than a wideband microphone.
//
// WebRTC has no equivalent of Twilio's clear or marks. An agent that stops
// talking simply stops sending, and agent audio is taken to play as it
// arrives, queued behind audio still playing.
package livekit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	lkproto "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/ErenCanYildirim/callswarm/transport"
)

// inboundBuffer is how many agent frames may queue before reading pauses.
const inboundBuffer = 4096

// defaultAnswerTimeout is how long Dial waits for an agent by default. It
// allows for dispatch and a cold start.
const defaultAnswerTimeout = 30 * time.Second

// tokenValidity bounds how long a call's access token can be used to join.
const tokenValidity = time.Hour

// Options configures a call.
type Options struct {
	// URL is the LiveKit server, such as ws://localhost:7880 or
	// wss://project.livekit.cloud.
	URL string
	// APIKey and APISecret sign the caller's access token.
	APIKey    string
	APISecret string
	// Room is the room to create and join. Each call needs its own.
	Room string
	// Identity is the caller's participant identity. Default "callswarm".
	Identity string
	// AgentName, if set, dispatches the agent registered under that name to
	// the room. Leave it empty for agents dispatched automatically.
	AgentName string
	// AgentMetadata is passed to the dispatched agent's job.
	AgentMetadata string
	// Attributes are set on the caller's participant, where agents can read
	// them.
	Attributes map[string]string
	// AnswerTimeout bounds how long Dial waits for the agent to join, publish
	// its audio and subscribe to the caller's. Default 30s.
	AnswerTimeout time.Duration
}

// Call is one simulated call in a LiveKit room. It implements
// transport.Call.
type Call struct {
	opts   Options
	room   *lksdk.Room
	mic    *Mic
	sender *transport.Sender

	ctx    context.Context
	cancel context.CancelCauseFunc

	playout transport.Playout
	inbound chan transport.Inbound

	mu        sync.Mutex // guards agent and readers
	agent     string     // identity of the participant whose audio is heard
	ended     bool       // no more readers may start
	readers   sync.WaitGroup
	closeOnce sync.Once

	heard      chan struct{} // closed once the agent's audio track arrives
	subscribed chan struct{} // closed once someone subscribes to the caller
	subOnce    sync.Once
}

var _ transport.Call = (*Call)(nil)

// Dial joins the room, publishes the caller's microphone track, starts
// streaming silence on it, as a real participant's microphone would, and
// waits for the agent to answer.
func Dial(ctx context.Context, opts Options) (*Call, error) {
	if opts.Identity == "" {
		opts.Identity = "callswarm"
	}
	if opts.AnswerTimeout <= 0 {
		opts.AnswerTimeout = defaultAnswerTimeout
	}
	token, err := opts.token()
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithCancelCause(context.Background())
	c := &Call{
		opts:       opts,
		ctx:        callCtx,
		cancel:     cancel,
		inbound:    make(chan transport.Inbound, inboundBuffer),
		heard:      make(chan struct{}),
		subscribed: make(chan struct{}),
	}
	c.room = lksdk.NewRoom(c.callbacks())
	if err := c.room.JoinWithContextAndToken(ctx, opts.URL, token); err != nil {
		cancel(err)
		return nil, fmt.Errorf("joining room %s: %w", opts.Room, err)
	}

	c.mic, err = PublishMic(c.room, opts.Identity)
	if err != nil {
		c.room.Disconnect()
		cancel(err)
		return nil, err
	}
	c.sender = transport.NewSender(callCtx, cancel, func(frame []byte, _ time.Duration) error {
		return c.mic.Write(frame)
	})
	c.sender.Start(time.Now())

	go func() {
		<-callCtx.Done()
		c.mu.Lock()
		c.ended = true
		c.mu.Unlock()
		c.readers.Wait()
		close(c.inbound)
	}()

	if err := c.awaitAnswer(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// awaitAnswer waits until the agent's audio is heard and the agent is
// listening to the caller.
func (c *Call) awaitAnswer(ctx context.Context) error {
	timer := time.NewTimer(c.opts.AnswerTimeout)
	defer timer.Stop()
	for _, ch := range []chan struct{}{c.heard, c.subscribed} {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		case <-timer.C:
			return fmt.Errorf("livekit: no agent answered in room %s within %v", c.opts.Room, c.opts.AnswerTimeout)
		}
	}
	return nil
}

func (opts Options) token() (string, error) {
	switch {
	case opts.URL == "":
		return "", errors.New("livekit: URL is required")
	case opts.APIKey == "" || opts.APISecret == "":
		return "", errors.New("livekit: API key and secret are required")
	case opts.Room == "":
		return "", errors.New("livekit: room is required")
	}
	at := auth.NewAccessToken(opts.APIKey, opts.APISecret).
		SetIdentity(opts.Identity).
		SetName(opts.Identity).
		SetValidFor(tokenValidity).
		SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: opts.Room})
	if len(opts.Attributes) > 0 {
		at.SetAttributes(opts.Attributes)
	}
	if opts.AgentName != "" {
		at.SetAgents(&lkproto.RoomAgentDispatch{AgentName: opts.AgentName, Metadata: opts.AgentMetadata})
	}
	token, err := at.ToJWT()
	if err != nil {
		return "", fmt.Errorf("livekit: signing token: %w", err)
	}
	return token, nil
}

func (c *Call) callbacks() *lksdk.RoomCallback {
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.ended || c.agent != "" {
			return // only the first audio track is the agent
		}
		c.agent = rp.Identity()
		c.readers.Add(1)
		go c.readAgent(track)
		close(c.heard)
	}
	cb.OnLocalTrackSubscribed = func(*lksdk.LocalTrackPublication, *lksdk.LocalParticipant) {
		c.subOnce.Do(func() { close(c.subscribed) })
	}
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) {
		c.mu.Lock()
		agent := c.agent
		c.mu.Unlock()
		if rp.Identity() == agent {
			c.cancel(errors.New("agent left the room"))
		}
	}
	cb.OnDisconnectedWithReason = func(reason lksdk.DisconnectionReason) {
		c.cancel(fmt.Errorf("disconnected from LiveKit: %s", reason))
	}
	return cb
}

// readAgent passes the agent's audio to the scenario until the track ends.
func (c *Call) readAgent(track *webrtc.TrackRemote) {
	defer c.readers.Done()
	err := ReadAudio(track, func(at time.Time, mulaw []byte) bool {
		in := transport.Inbound{Kind: transport.KindAudio, At: at, Audio: mulaw, PlayAt: c.playout.Add(at, len(mulaw))}
		select {
		case c.inbound <- in:
			return true
		case <-c.ctx.Done():
			return false
		}
	})
	if err != nil {
		c.cancel(err)
	}
}

// Room is the room the call joined.
func (c *Call) Room() string { return c.opts.Room }

// Agent is the identity of the participant whose audio is being heard, or
// empty before any has published.
func (c *Call) Agent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agent
}

// Play queues μ-law caller audio and blocks until it has been sent in real
// time; see transport.Sender.Play.
func (c *Call) Play(ctx context.Context, mulaw []byte) (transport.Playback, error) {
	return c.sender.Play(ctx, mulaw)
}

// Inbound returns the agent's audio, closed when the call ends.
func (c *Call) Inbound() <-chan transport.Inbound { return c.inbound }

// Done is closed when the call has ended.
func (c *Call) Done() <-chan struct{} { return c.ctx.Done() }

// Err reports why the call ended: transport.ErrClosed after Close.
func (c *Call) Err() error { return context.Cause(c.ctx) }

// Stats reports how well the caller kept real-time pace.
func (c *Call) Stats() transport.Stats { return c.sender.Stats() }

// Close leaves the room. Calling it again is safe.
func (c *Call) Close() error {
	c.closeOnce.Do(func() {
		c.cancel(transport.ErrClosed)
		<-c.sender.Done()
		c.room.Disconnect()
	})
	return nil
}
