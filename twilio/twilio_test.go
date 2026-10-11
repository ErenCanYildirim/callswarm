package twilio

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Messages in the shape Twilio documents, as an agent would receive them.
var twilioSamples = map[string]string{
	"connected": `{"event":"connected","protocol":"Call","version":"1.0.0"}`,
	"start": `{"event":"start","sequenceNumber":"2","start":{"accountSid":"AC1","streamSid":"MZ1",
		"callSid":"CA1","tracks":["inbound"],"customParameters":{"Department":"billing"},
		"mediaFormat":{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1}},"streamSid":"MZ1"}`,
	"media": `{"event":"media","sequenceNumber":"3","media":{"track":"inbound","chunk":"1",
		"timestamp":"5","payload":"//8="},"streamSid":"MZ1"}`,
	"dtmf": `{"event":"dtmf","streamSid":"MZ1","sequenceNumber":"5","dtmf":{"track":"inbound","digit":"4"}}`,
	"mark": `{"event":"mark","sequenceNumber":"4","streamSid":"MZ1","mark":{"name":"reply-1"}}`,
	"stop": `{"event":"stop","sequenceNumber":"6","stop":{"accountSid":"AC1","callSid":"CA1"},"streamSid":"MZ1"}`,
}

func TestDecodeTwilioSamples(t *testing.T) {
	for name, raw := range twilioSamples {
		t.Run(name, func(t *testing.T) {
			m, err := Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if m.Event != name {
				t.Fatalf("event = %q, want %q", m.Event, name)
			}
		})
	}

	m, _ := Decode([]byte(twilioSamples["start"]))
	if m.SequenceNumber != 2 || m.Start.MediaFormat.SampleRate != 8000 ||
		m.Start.CustomParameters["Department"] != "billing" {
		t.Errorf("start fields not decoded: %+v", m.Start)
	}

	m, _ = Decode([]byte(twilioSamples["media"]))
	audio, err := m.Media.Audio()
	if err != nil {
		t.Fatal(err)
	}
	if m.Media.Chunk != 1 || m.Media.Timestamp != 5 || !bytes.Equal(audio, []byte{0xFF, 0xFF}) {
		t.Errorf("media fields not decoded: %+v %x", m.Media, audio)
	}
}

// Every message survives encode then decode unchanged.
func TestRoundTrip(t *testing.T) {
	c := NewCaller("1", map[string]string{"k": "v"})
	msgs := []Message{
		c.Connected(),
		c.Start(),
		c.Media([]byte{1, 2, 3}, 20*time.Millisecond),
		c.DTMF("7"),
		c.MarkPlayed("reply-1"),
		c.Stop(),
		AgentMedia("MZ1", []byte{0xFF, 0x7F}),
		AgentMark("MZ1", "reply-1"),
		Clear("MZ1"),
	}
	for _, in := range msgs {
		t.Run(in.Event, func(t *testing.T) {
			data, err := Encode(in)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Decode(data)
			if err != nil {
				t.Fatalf("decode %s: %v", data, err)
			}
			if !reflect.DeepEqual(in, out) {
				t.Errorf("round trip changed message:\n in: %+v\nout: %+v", in, out)
			}
		})
	}
}

func TestCallerSequencing(t *testing.T) {
	c := NewCaller("42", nil)
	if c.Connected().SequenceNumber != 0 {
		t.Error("connected should have no sequence number")
	}
	if got := c.Start().SequenceNumber; got != 2 {
		t.Errorf("start sequence = %d, want 2", got)
	}
	m1 := c.Media(nil, 0)
	m2 := c.Media(nil, 20*time.Millisecond)
	if m1.SequenceNumber != 3 || m2.SequenceNumber != 4 {
		t.Errorf("media sequences = %d, %d, want 3, 4", m1.SequenceNumber, m2.SequenceNumber)
	}
	if m1.Media.Chunk != 1 || m2.Media.Chunk != 2 {
		t.Errorf("chunks = %d, %d, want 1, 2", m1.Media.Chunk, m2.Media.Chunk)
	}
	if m2.Media.Timestamp != 20 {
		t.Errorf("timestamp = %d, want 20", m2.Media.Timestamp)
	}
	if c.Stop().SequenceNumber != 5 {
		t.Error("stop should continue the sequence")
	}
	if c.StreamSid != "MZ42" || c.CallSid != "CA42" {
		t.Errorf("ids = %s %s", c.StreamSid, c.CallSid)
	}
}

// Twilio sends numbers as strings; agents may compare them as such.
func TestEncodeUsesStringNumbers(t *testing.T) {
	c := NewCaller("1", nil)
	c.Start()
	data, err := Encode(c.Media([]byte{1}, 40*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["sequenceNumber"].(string); !ok {
		t.Errorf("sequenceNumber not a string: %s", data)
	}
	media := raw["media"].(map[string]any)
	for _, k := range []string{"chunk", "timestamp"} {
		if _, ok := media[k].(string); !ok {
			t.Errorf("%s not a string: %s", k, data)
		}
	}
}

// Agent messages carry no sequence number and no inbound-only media fields.
func TestAgentMessagesAreMinimal(t *testing.T) {
	data, _ := Encode(AgentMedia("MZ1", []byte{1}))
	want := `{"event":"media","streamSid":"MZ1","media":{"payload":"AQ=="}}`
	if string(data) != want {
		t.Errorf("agent media = %s, want %s", data, want)
	}
	data, _ = Encode(Clear("MZ1"))
	if string(data) != `{"event":"clear","streamSid":"MZ1"}` {
		t.Errorf("clear = %s", data)
	}
}

func TestNumberAcceptsStringOrNumber(t *testing.T) {
	for _, in := range []string{`"17"`, `17`, ` "17" `} {
		var n Number
		if err := json.Unmarshal([]byte(in), &n); err != nil || n != 17 {
			t.Errorf("unmarshal %s = %d, %v", in, n, err)
		}
	}
	for _, in := range []string{`"x"`, `1.5`, `"1e3"`, `true`} {
		var n Number
		if err := json.Unmarshal([]byte(in), &n); err == nil {
			t.Errorf("unmarshal %s: expected error", in)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	bad := map[string]string{
		"not json":       `{`,
		"no event":       `{"streamSid":"MZ1"}`,
		"start no body":  `{"event":"start"}`,
		"media no body":  `{"event":"media"}`,
		"media bad b64":  `{"event":"media","media":{"payload":"***"}}`,
		"mark no name":   `{"event":"mark","mark":{"name":""}}`,
		"dtmf no body":   `{"event":"dtmf"}`,
		"bad seq number": `{"event":"stop","sequenceNumber":"abc"}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	_, err := Decode([]byte(`{"event":"media"}`))
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("validation error should wrap ErrInvalid: %v", err)
	}
}

func TestDecodeIgnoresUnknownEvents(t *testing.T) {
	m, err := Decode([]byte(`{"event":"future","streamSid":"MZ1","extra":{}}`))
	if err != nil || m.Event != "future" {
		t.Errorf("got %+v, %v", m, err)
	}
}
