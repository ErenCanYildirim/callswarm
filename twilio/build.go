package twilio

import (
	"encoding/base64"
	"time"
)

// Caller builds the messages Twilio sends for one stream, keeping the
// sequence number, media chunk counter and stream identifiers consistent.
// It is not safe for concurrent use; each simulated call owns one.
type Caller struct {
	StreamSid  string
	CallSid    string
	AccountSid string
	Parameters map[string]string

	seq   int64
	chunk int64
}

// NewCaller returns a Caller with fake but well-formed identifiers derived
// from id, so logs from concurrent calls are easy to tell apart.
func NewCaller(id string, params map[string]string) *Caller {
	return &Caller{
		StreamSid:  "MZ" + id,
		CallSid:    "CA" + id,
		AccountSid: "ACcallswarm",
		Parameters: params,
	}
}

func (c *Caller) next() Number {
	c.seq++
	return Number(c.seq)
}

// Connected is the first message on every stream. Twilio sends it without
// a sequence number.
func (c *Caller) Connected() Message {
	return Message{Event: EventConnected, Protocol: "Call", Version: "1.0.0"}
}

// Start announces the stream and its audio format.
func (c *Caller) Start() Message {
	params := c.Parameters
	if params == nil {
		params = map[string]string{}
	}
	// Twilio counts "connected" as sequence number 1.
	if c.seq == 0 {
		c.seq = 1
	}
	return Message{
		Event:          EventStart,
		SequenceNumber: c.next(),
		StreamSid:      c.StreamSid,
		Start: &Start{
			AccountSid:       c.AccountSid,
			StreamSid:        c.StreamSid,
			CallSid:          c.CallSid,
			Tracks:           []string{TrackInbound},
			CustomParameters: params,
			MediaFormat:      MediaFormat{Encoding: Encoding, SampleRate: SampleRate, Channels: Channels},
		},
	}
}

// Media wraps one frame of caller μ-law audio. offset is the frame's position
// in the stream, which becomes the millisecond timestamp.
func (c *Caller) Media(mulaw []byte, offset time.Duration) Message {
	c.chunk++
	return Message{
		Event:          EventMedia,
		SequenceNumber: c.next(),
		StreamSid:      c.StreamSid,
		Media: &Media{
			Track:     TrackInbound,
			Chunk:     Number(c.chunk),
			Timestamp: Number(offset.Milliseconds()),
			Payload:   base64.StdEncoding.EncodeToString(mulaw),
		},
	}
}

// DTMF reports a keypad press by the caller.
func (c *Caller) DTMF(digit string) Message {
	return Message{
		Event:          EventDTMF,
		SequenceNumber: c.next(),
		StreamSid:      c.StreamSid,
		DTMF:           &DTMF{Track: TrackInbound, Digit: digit},
	}
}

// MarkPlayed echoes an agent mark back, signalling that the audio before it
// has finished playing (or was discarded by a clear).
func (c *Caller) MarkPlayed(name string) Message {
	return Message{
		Event:          EventMark,
		SequenceNumber: c.next(),
		StreamSid:      c.StreamSid,
		Mark:           &Mark{Name: name},
	}
}

// Stop ends the stream.
func (c *Caller) Stop() Message {
	return Message{
		Event:          EventStop,
		SequenceNumber: c.next(),
		StreamSid:      c.StreamSid,
		Stop:           &Stop{AccountSid: c.AccountSid, CallSid: c.CallSid},
	}
}

// AgentMedia is audio sent by the agent. Agents send the payload alone.
func AgentMedia(streamSid string, mulaw []byte) Message {
	return Message{
		Event:     EventMedia,
		StreamSid: streamSid,
		Media:     &Media{Payload: base64.StdEncoding.EncodeToString(mulaw)},
	}
}

// AgentMark asks to be told when the agent audio sent so far has played.
func AgentMark(streamSid, name string) Message {
	return Message{Event: EventMark, StreamSid: streamSid, Mark: &Mark{Name: name}}
}

// Clear discards the agent's buffered audio, which is how agents stop
// talking when the caller interrupts.
func Clear(streamSid string) Message {
	return Message{Event: EventClear, StreamSid: streamSid}
}
