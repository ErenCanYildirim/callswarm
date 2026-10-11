package transport

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
)

// SendFunc sends one 20 ms frame of μ-law caller audio. Offset is the frame's
// position in the call's audio stream.
type SendFunc func(frame []byte, offset time.Duration) error

// Sender streams caller audio in real time, as a microphone would: one 20 ms
// frame per interval for the life of the call, clip audio when some is
// queued and silence otherwise. Frame n is sent at start + (n+1)*20ms, once
// its audio would have been captured. Transports supply the SendFunc that
// puts a frame on their wire.
type Sender struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	send   SendFunc

	queueMu    sync.Mutex
	queue      []*clip
	senderDone bool // set once the sender has stopped; no more clips play

	statsMu       sync.Mutex
	stats         Stats
	totalLateness time.Duration

	done chan struct{}
}

type clip struct {
	frames [][]byte
	queued time.Time
	next   int
	pb     Playback
	done   chan struct{}
	err    error
}

// NewSender returns a sender for a call. It stops when ctx is done, and
// cancels the call through cancel if a send fails.
func NewSender(ctx context.Context, cancel context.CancelCauseFunc, send SendFunc) *Sender {
	return &Sender{ctx: ctx, cancel: cancel, send: send, done: make(chan struct{})}
}

// Start begins streaming, with frame 0 covering the 20 ms after start.
func (s *Sender) Start(start time.Time) { go s.loop(start) }

// Done is closed once the sender has stopped.
func (s *Sender) Done() <-chan struct{} { return s.done }

// Stats reports pacing so far.
func (s *Sender) Stats() Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	st := s.stats
	if n := st.FramesSent; n > 0 {
		st.MeanLateness = s.totalLateness / time.Duration(n)
	}
	return st
}

// Play queues μ-law audio and blocks until all of it has been sent in real
// time. A partial last frame is padded with silence. Concurrent Play calls
// play one after another, in the order they were made.
//
// The audio starts with the first frame whose 20 ms window begins after the
// call to Play, so Playback.Began is never earlier than the call, and at
// most one frame later.
func (s *Sender) Play(ctx context.Context, mulaw []byte) (Playback, error) {
	cl := &clip{
		frames: audio.FramesPadded(mulaw, audio.TelephonyFrameSamples, audio.MulawSilence),
		queued: time.Now(),
		done:   make(chan struct{}),
	}
	if len(cl.frames) == 0 {
		return Playback{}, nil
	}
	s.queueMu.Lock()
	if s.senderDone {
		s.queueMu.Unlock()
		return Playback{}, context.Cause(s.ctx)
	}
	s.queue = append(s.queue, cl)
	s.queueMu.Unlock()

	select {
	case <-cl.done:
		return cl.pb, cl.err
	case <-ctx.Done():
		s.dropClip(cl)
		<-cl.done
		return cl.pb, ctx.Err()
	case <-s.ctx.Done():
		<-cl.done
		return cl.pb, cl.err
	}
}

// dropClip stops a clip; frames already sent stay sent.
func (s *Sender) dropClip(target *clip) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for i, cl := range s.queue {
		if cl == target {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			cl.err = context.Canceled
			close(cl.done)
			return
		}
	}
}

func (s *Sender) loop(start time.Time) {
	defer close(s.done)
	defer s.failQueue()
	silence := make([]byte, audio.TelephonyFrameSamples)
	for i := range silence {
		silence[i] = audio.MulawSilence
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C
	for n := 0; ; n++ {
		due := start.Add(time.Duration(n+1) * FrameDuration)
		timer.Reset(time.Until(due))
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			return
		}

		frame, cl := s.nextFrame(due.Add(-FrameDuration))
		isSilence := cl == nil
		if isSilence {
			frame = silence
		}
		now := time.Now()
		if err := s.send(frame, time.Duration(n)*FrameDuration); err != nil {
			s.cancel(fmt.Errorf("sending audio: %w", err))
			return
		}
		s.recordSend(now.Sub(due), isSilence)
		if cl != nil {
			s.advance(cl, now)
		}
	}
}

// nextFrame returns the next frame of the clip at the head of the queue for
// the frame window starting at windowStart. A clip doesn't start in a window
// that began before it was queued: that frame's audio would predate Play.
func (s *Sender) nextFrame(windowStart time.Time) ([]byte, *clip) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if len(s.queue) == 0 {
		return nil, nil
	}
	cl := s.queue[0]
	if cl.next == 0 && windowStart.Before(cl.queued) {
		return nil, nil
	}
	return cl.frames[cl.next], cl
}

// advance records a sent frame and completes the clip after its last frame.
func (s *Sender) advance(cl *clip, sentAt time.Time) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if len(s.queue) == 0 || s.queue[0] != cl {
		return // dropped while sending
	}
	if cl.next == 0 {
		cl.pb.Began = sentAt.Add(-FrameDuration)
	}
	cl.next++
	cl.pb.Frames = cl.next
	cl.pb.Ended = sentAt
	if cl.next == len(cl.frames) {
		s.queue = s.queue[1:]
		close(cl.done)
	}
}

func (s *Sender) failQueue() {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.senderDone = true
	for _, cl := range s.queue {
		cl.err = context.Cause(s.ctx)
		close(cl.done)
	}
	s.queue = nil
}

func (s *Sender) recordSend(lateness time.Duration, isSilence bool) {
	lateness = max(lateness, 0)
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	s.stats.FramesSent++
	if isSilence {
		s.stats.SilenceSent++
	}
	s.totalLateness += lateness
	s.stats.MaxLateness = max(s.stats.MaxLateness, lateness)
	if lateness > 5*time.Millisecond {
		s.stats.LateFrames++
	}
}
