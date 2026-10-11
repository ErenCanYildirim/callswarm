// Package twilio implements the Twilio Media Streams WebSocket protocol, the
// format agent frameworks such as Pipecat and Jargo use to take phone calls.
//
// In a real call, Twilio connects to the agent and streams caller audio in;
// the agent streams its reply back. callswarm plays Twilio's side of that
// connection, so the agent under test needs no changes.
//
// Messages in each direction:
//
//	Twilio → agent: connected, start, media, dtmf, mark, stop
//	agent → Twilio: media, mark, clear
//
// See https://www.twilio.com/docs/voice/media-streams/websocket-messages.
package twilio

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Event names.
const (
	EventConnected = "connected"
	EventStart     = "start"
	EventMedia     = "media"
	EventDTMF      = "dtmf"
	EventMark      = "mark"
	EventStop      = "stop"
	EventClear     = "clear"
)

// Track names.
const (
	TrackInbound  = "inbound"
	TrackOutbound = "outbound"
)

// Media format of every Twilio stream.
const (
	Encoding   = "audio/x-mulaw"
	SampleRate = 8000
	Channels   = 1
)

// Message is one WebSocket text frame in either direction. Exactly one of the
// event-specific fields is set, matching Event.
type Message struct {
	Event          string `json:"event"`
	SequenceNumber Number `json:"sequenceNumber,omitempty"`
	StreamSid      string `json:"streamSid,omitempty"`

	// Only on "connected".
	Protocol string `json:"protocol,omitempty"`
	Version  string `json:"version,omitempty"`

	Start *Start `json:"start,omitempty"`
	Media *Media `json:"media,omitempty"`
	DTMF  *DTMF  `json:"dtmf,omitempty"`
	Mark  *Mark  `json:"mark,omitempty"`
	Stop  *Stop  `json:"stop,omitempty"`
}

// Start describes the stream; sent once, right after "connected".
type Start struct {
	AccountSid       string            `json:"accountSid"`
	StreamSid        string            `json:"streamSid"`
	CallSid          string            `json:"callSid"`
	Tracks           []string          `json:"tracks"`
	CustomParameters map[string]string `json:"customParameters"`
	MediaFormat      MediaFormat       `json:"mediaFormat"`
}

// MediaFormat is always μ-law, 8 kHz, mono on Twilio streams.
type MediaFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

// Media carries base64 μ-law audio. Track, Chunk and Timestamp are only set
// on audio sent by Twilio; agents send Payload alone.
type Media struct {
	Track     string `json:"track,omitempty"`
	Chunk     Number `json:"chunk,omitempty"`
	Timestamp Number `json:"timestamp,omitempty"` // ms since stream start
	Payload   string `json:"payload"`
}

// Audio decodes the base64 payload to raw μ-law bytes.
func (m *Media) Audio() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(m.Payload)
	if err != nil {
		return nil, fmt.Errorf("media payload: %w", err)
	}
	return b, nil
}

// DTMF is a keypad press.
type DTMF struct {
	Track string `json:"track"`
	Digit string `json:"digit"`
}

// Mark names a point in the agent's audio. The agent sends one after its
// media; Twilio echoes it back once that audio has played, or immediately
// when a clear discards the audio before it.
type Mark struct {
	Name string `json:"name"`
}

// Stop ends the stream.
type Stop struct {
	AccountSid string `json:"accountSid"`
	CallSid    string `json:"callSid"`
}

// Number is an integer that Twilio encodes as a JSON string ("42"). It also
// accepts a bare JSON number, since Twilio's docs describe some of these
// fields as numbers and implementations differ. It always encodes as a
// string, matching what Twilio sends.
type Number int64

// MarshalJSON encodes n as a quoted decimal string.
func (n Number) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(strconv.FormatInt(int64(n), 10))), nil
}

// UnmarshalJSON accepts "42" or 42.
func (n *Number) UnmarshalJSON(data []byte) error {
	s := string(bytes.TrimSpace(data))
	if s == "null" {
		return nil
	}
	if unq, err := strconv.Unquote(s); err == nil {
		s = unq
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid number %s", data)
	}
	*n = Number(v)
	return nil
}

// Encode serializes m to JSON.
func Encode(m Message) ([]byte, error) {
	return json.Marshal(m)
}

// Decode parses and validates a message. Unknown events are returned without
// error so callers can ignore them, as Twilio may add new ones.
func Decode(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, fmt.Errorf("decoding message: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ErrInvalid is wrapped by every validation error.
var ErrInvalid = errors.New("invalid message")

// Validate checks that the field required by the event is present.
func (m *Message) Validate() error {
	missing := func(field string) error {
		return fmt.Errorf("%w: %q event without %s", ErrInvalid, m.Event, field)
	}
	switch m.Event {
	case "":
		return fmt.Errorf("%w: missing event", ErrInvalid)
	case EventStart:
		if m.Start == nil {
			return missing("start")
		}
	case EventMedia:
		if m.Media == nil {
			return missing("media")
		}
		if _, err := m.Media.Audio(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	case EventDTMF:
		if m.DTMF == nil {
			return missing("dtmf")
		}
	case EventMark:
		if m.Mark == nil || m.Mark.Name == "" {
			return missing("mark name")
		}
	}
	return nil
}
