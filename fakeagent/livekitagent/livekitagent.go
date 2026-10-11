// Package livekitagent runs the fake agent in LiveKit rooms, with the same
// known timing as fakeagent.Agent has over Twilio Media Streams, so
// callswarm's LiveKit measurements can be checked against a known answer.
//
// It hears the first other participant's microphone, answers by echoing it
// back on its own microphone track, and stops talking when interrupted.
// LiveKit has no clear or marks, so those parts of fakeagent's behavior are
// skipped.
package livekitagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/auth"
	lkproto "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/fakeagent"
	"github.com/ErenCanYildirim/callswarm/transport/livekit"
)

// pollInterval is how often Serve looks for new rooms.
const pollInterval = 200 * time.Millisecond

// Server is how to reach a LiveKit server.
type Server struct {
	// URL is the LiveKit server, such as ws://localhost:7880.
	URL       string
	APIKey    string
	APISecret string
}

// Identity is the participant identity the agent joins with.
const Identity = "fake-agent"

// Join joins room and answers the caller until ctx is done, the caller
// leaves or the agent is disconnected.
func Join(ctx context.Context, srv Server, room string, cfg fakeagent.Config) error {
	token, err := auth.NewAccessToken(srv.APIKey, srv.APISecret).
		SetIdentity(Identity).
		SetName(Identity).
		SetKind(lkproto.ParticipantInfo_AGENT).
		SetValidFor(time.Hour).
		SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).
		ToJWT()
	if err != nil {
		return fmt.Errorf("signing token: %w", err)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// The session exists before joining, so a caller already in the room is
	// heard from the start. It can't answer before the microphone is
	// published, but it never needs to: a reply comes only after the caller
	// has spoken and paused.
	out := &micOutput{}
	session := fakeagent.NewSession(cfg, room, out)
	var (
		mu      sync.Mutex
		caller  string
		hearing sync.WaitGroup
	)
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		mu.Lock()
		defer mu.Unlock()
		if track.Kind() != webrtc.RTPCodecTypeAudio || caller != "" {
			return
		}
		caller = rp.Identity()
		hearing.Add(1)
		go func() {
			defer hearing.Done()
			hear(ctx, track, session)
		}()
	}
	cb.OnParticipantDisconnected = func(rp *lksdk.RemoteParticipant) {
		mu.Lock()
		defer mu.Unlock()
		if rp.Identity() == caller {
			cancel(errors.New("caller left"))
		}
	}
	cb.OnDisconnected = func() { cancel(errors.New("disconnected")) }

	r := lksdk.NewRoom(cb)
	if err := r.JoinWithContextAndToken(ctx, srv.URL, token); err != nil {
		return fmt.Errorf("joining room %s: %w", room, err)
	}
	defer r.Disconnect()
	mic, err := livekit.PublishMic(r, Identity)
	if err != nil {
		return err
	}
	out.mic.Store(mic)

	session.Run(ctx)
	r.Disconnect()
	hearing.Wait()
	return nil
}

// hear feeds the caller's audio to the session in 20 ms frames.
func hear(ctx context.Context, track *webrtc.TrackRemote, s *fakeagent.Session) {
	var pending []byte
	_ = livekit.ReadAudio(track, func(_ time.Time, mulaw []byte) bool {
		pending = append(pending, mulaw...)
		for len(pending) >= audio.TelephonyFrameSamples {
			s.Hear(pending[:audio.TelephonyFrameSamples])
			pending = pending[audio.TelephonyFrameSamples:]
		}
		return ctx.Err() == nil
	})
}

// micOutput sends the session's audio on the agent's microphone, once it is
// published. LiveKit has no clear or marks.
type micOutput struct{ mic atomic.Pointer[livekit.Mic] }

func (o *micOutput) Audio(_ context.Context, frame []byte) error {
	m := o.mic.Load()
	if m == nil {
		return errors.New("microphone not published yet")
	}
	return m.Write(frame)
}

func (*micOutput) Clear(context.Context) error        { return nil }
func (*micOutput) Mark(context.Context, string) error { return nil }

// Serve joins every new room whose name starts with prefix, until ctx is
// done. It finds rooms by polling the server, so the agent arrives a moment
// after the caller, as a dispatched agent does.
func Serve(ctx context.Context, srv Server, prefix string, cfg fakeagent.Config, logf func(format string, args ...any)) error {
	rooms := lksdk.NewRoomServiceClient(srv.URL, srv.APIKey, srv.APISecret)
	joined := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		resp, err := rooms.ListRooms(ctx, &lkproto.ListRoomsRequest{})
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("listing rooms: %w", err)
		}
		for _, room := range resp.GetRooms() {
			name := room.GetName()
			if !strings.HasPrefix(name, prefix) || joined[name] {
				continue
			}
			joined[name] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := Join(ctx, srv, name, cfg); err != nil && logf != nil {
					logf("room %s: %v", name, err)
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
