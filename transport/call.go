package transport

import (
	"context"
	"sync"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
)

// Call is one simulated call, whatever carries it. Conn is the Twilio Media
// Streams implementation; package transport/livekit has a LiveKit one.
//
// Audio on both sides is 8 kHz μ-law in 20 ms frames, whatever the wire
// format, so the scenario runner, speech detection and metrics are the same
// for every transport.
type Call interface {
	// Play queues caller audio and blocks until all of it has been sent in
	// real time. Concurrent calls play one after another.
	Play(ctx context.Context, mulaw []byte) (Playback, error)
	// Inbound returns the agent's audio and control messages in arrival
	// order, closed when the call ends. The consumer must keep draining it.
	Inbound() <-chan Inbound
	// Done is closed when the call has ended, by either side.
	Done() <-chan struct{}
	// Err reports why the call ended, or nil while it is active.
	Err() error
	// Stats reports how well the caller kept real-time pace.
	Stats() Stats
	// Close hangs up. Calling it again is safe.
	Close() error
}

var _ Call = (*Conn)(nil)

// Playout tracks when received agent audio plays, as a phone or a WebRTC
// client would hear it: audio that arrives while earlier audio is still
// playing waits its turn. It is safe for concurrent use.
type Playout struct {
	mu  sync.Mutex
	end time.Time
}

// Add records samples of 8 kHz audio arriving at at and returns when they
// start playing.
func (p *Playout) Add(at time.Time, samples int) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	playAt := at
	if p.end.After(at) {
		playAt = p.end
	}
	p.end = playAt.Add(time.Duration(samples) * time.Second / audio.TelephonySampleRate)
	return playAt
}

// Clear discards audio that has not played by at.
func (p *Playout) Clear(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.end.After(at) {
		p.end = at
	}
}

// End is when the audio added so far finishes playing.
func (p *Playout) End() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.end
}
