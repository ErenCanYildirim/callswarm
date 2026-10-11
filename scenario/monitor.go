package scenario

import (
	"sync"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
	"github.com/ErenCanYildirim/callswarm/transport"
	"github.com/ErenCanYildirim/callswarm/vad"
)

// AgentFrame is up to 20 ms of agent audio as the caller heard it. Agent
// messages longer than 20 ms are split, so speech detection works at the
// same resolution whatever chunk size the agent sends.
type AgentFrame struct {
	// At is when the audio arrived.
	At time.Time
	// PlayAt is when it started playing.
	PlayAt time.Time
	// End is when it stopped playing: PlayAt plus its length, or earlier if
	// a clear cut it off.
	End time.Time
	// Speech reports whether voice activity detection found speech in it.
	Speech bool
	// Cleared reports whether a clear discarded the frame before it played.
	Cleared bool
	Audio   []byte
}

// Mark is a mark the agent placed after its audio.
type Mark struct {
	At   time.Time
	Name string
}

// Segment is a stretch of agent speech, as heard by the caller.
type Segment = vad.Segment

// AgentAudio is everything the agent sent during a run.
type AgentAudio struct {
	Frames []AgentFrame
	Clears []time.Time
	Marks  []Mark
}

// Speech groups the heard agent speech into segments, bridging gaps shorter
// than minSilence. A clear always ends a segment: speech after it is a new
// one, however soon it follows.
func (a AgentAudio) Speech(minSilence time.Duration) []Segment {
	var segs []Segment
	var frames []vad.Frame
	clears := a.Clears
	for _, f := range a.Frames {
		for len(clears) > 0 && !f.PlayAt.Before(clears[0]) {
			segs = append(segs, vad.Merge(frames, vadConfig.MinSpeech, minSilence)...)
			frames, clears = frames[:0], clears[1:]
		}
		frames = append(frames, vad.Frame{Start: f.PlayAt, End: f.End, Speech: f.Speech && !f.Cleared})
	}
	return append(segs, vad.Merge(frames, vadConfig.MinSpeech, minSilence)...)
}

// vadConfig is the voice activity detection used for caller clips and agent
// audio alike.
var vadConfig = vad.Config{}.WithDefaults()

// monitor consumes a call's inbound messages and answers questions about
// when the agent was speaking.
type monitor struct {
	mu    sync.Mutex
	agent AgentAudio
	vad   *vad.Classifier
}

func newMonitor(in <-chan transport.Inbound) *monitor {
	m := &monitor{vad: vad.NewClassifier(vadConfig)}
	go m.run(in)
	return m
}

func (m *monitor) run(in <-chan transport.Inbound) {
	for msg := range in {
		m.mu.Lock()
		switch msg.Kind {
		case transport.KindAudio:
			m.addAudio(msg)
		case transport.KindClear:
			m.agent.Clears = append(m.agent.Clears, msg.At)
			for i := range m.agent.Frames {
				f := &m.agent.Frames[i]
				if !f.PlayAt.Before(msg.At) {
					f.Cleared = true
				}
				if f.End.After(msg.At) {
					f.End = msg.At
				}
			}
		case transport.KindMark:
			m.agent.Marks = append(m.agent.Marks, Mark{At: msg.At, Name: msg.Mark})
		}
		m.mu.Unlock()
	}
}

// addAudio splits a message into 20 ms frames and classifies each.
func (m *monitor) addAudio(msg transport.Inbound) {
	n := audio.TelephonyFrameSamples
	for off := 0; off < len(msg.Audio); off += n {
		chunk := msg.Audio[off:min(off+n, len(msg.Audio))]
		start := msg.PlayAt.Add(sampleDuration(off))
		m.agent.Frames = append(m.agent.Frames, AgentFrame{
			At:     msg.At,
			PlayAt: start,
			End:    start.Add(sampleDuration(len(chunk))),
			Speech: m.vad.Speech(audio.MulawDecode(chunk)),
			Audio:  chunk,
		})
	}
}

func sampleDuration(samples int) time.Duration {
	return time.Duration(samples) * time.Second / audio.TelephonySampleRate
}

// segments groups heard speech, bridging gaps shorter than gap.
func (m *monitor) segments(gap time.Duration) []Segment {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agent.Speech(gap)
}

// active returns the first segment still playing at, or starting after, t.
func (m *monitor) active(t time.Time, gap time.Duration) (Segment, bool) {
	for _, s := range m.segments(gap) {
		if s.End.After(t) {
			return s, true
		}
	}
	return Segment{}, false
}

// segmentAt returns the segment that starts at start, as it stands now.
func (m *monitor) segmentAt(start time.Time, gap time.Duration) Segment {
	for _, s := range m.segments(gap) {
		if s.Start.Equal(start) {
			return s
		}
	}
	return Segment{Start: start, End: start}
}

// lastSpeechEnd is when the most recent heard speech ends.
func (m *monitor) lastSpeechEnd() time.Time {
	segs := m.segments(vadConfig.MinSilence)
	if len(segs) == 0 {
		return time.Time{}
	}
	return segs[len(segs)-1].End
}

func (m *monitor) snapshot() AgentAudio {
	m.mu.Lock()
	defer m.mu.Unlock()
	return AgentAudio{
		Frames: append([]AgentFrame(nil), m.agent.Frames...),
		Clears: append([]time.Time(nil), m.agent.Clears...),
		Marks:  append([]Mark(nil), m.agent.Marks...),
	}
}
