package livekit

import (
	"fmt"
	"sync"
	"time"

	lkproto "github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/tphakala/go-opus/opus"

	"github.com/ErenCanYildirim/callswarm/audio"
)

// encodeBitrate is the Opus bitrate for published audio. Narrowband speech
// needs little; this is generous for an 8 kHz source.
const encodeBitrate = 32000

// Mic is a published microphone track carrying 8 kHz μ-law audio as Opus.
type Mic struct {
	mu     sync.Mutex // the encoder is stateful and not safe for concurrent use
	track  *webrtc.TrackLocalStaticSample
	enc    *opus.Encoder
	packet []byte
}

// PublishMic publishes a microphone track in room. Agents listen for the
// microphone source.
func PublishMic(room *lksdk.Room, name string) (*Mic, error) {
	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: audio.TelephonySampleRate,
		Channels:   1,
		Bitrate:    encodeBitrate,
	})
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		name+"-mic", name)
	if err != nil {
		return nil, err
	}
	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   "microphone",
		Source: lkproto.TrackSource_MICROPHONE,
	}); err != nil {
		return nil, fmt.Errorf("publishing microphone: %w", err)
	}
	return &Mic{track: track, enc: enc, packet: make([]byte, 1500)}, nil
}

// Write sends one 20 ms frame of μ-law audio. Pacing is the caller's job.
func (m *Mic) Write(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.enc.Encode(audio.MulawDecode(frame), m.packet)
	if err != nil {
		return fmt.Errorf("opus: %w", err)
	}
	return m.track.WriteSample(media.Sample{Data: m.packet[:n], Duration: frameDuration})
}

const frameDuration = 20 * time.Millisecond

// ReadAudio decodes a remote Opus track to 8 kHz μ-law and passes each
// packet's audio to fn with its arrival time, until the track ends or fn
// returns false. Malformed packets are skipped, as lost audio.
func ReadAudio(track *webrtc.TrackRemote, fn func(at time.Time, mulaw []byte) bool) error {
	dec, err := opus.NewDecoder(audio.TelephonySampleRate, 1)
	if err != nil {
		return fmt.Errorf("opus decoder: %w", err)
	}
	// The longest Opus packet is 120 ms.
	pcm := make([]int16, audio.TelephonySampleRate*120/1000)
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return nil // the track ended or the room was left
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		at := time.Now()
		n, err := dec.Decode(pkt.Payload, pcm)
		if err != nil {
			continue
		}
		if !fn(at, audio.MulawEncode(pcm[:n])) {
			return nil
		}
	}
}
