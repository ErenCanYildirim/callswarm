// Package vad detects speech in telephony audio.
//
// It is an energy-based detector: a 20 ms frame counts as speech when it is
// both louder than an absolute floor and a margin above the background noise
// level. The noise level is the quietest frame in a sliding window (minimum
// statistics): speech always dips between syllables, so the minimum tracks
// the background even while someone is talking. Raw speech frames are then
// grouped into segments: gaps shorter than MinSilence (the pauses between
// words) are bridged, and segments shorter than MinSpeech (clicks, pops) are
// dropped.
//
// Energy detection is reliable on clean recordings and synthesized speech,
// which is what callswarm deals with: pre-recorded caller clips and TTS
// output from the agent. It is not a speech classifier: loud, varying
// non-speech sound such as hold music is detected as speech, and a perfectly
// steady sound lasting longer than NoiseWindow (such as a test tone) becomes
// part of the background.
package vad

import (
	"math"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
)

// FrameDuration is the analysis frame length.
const FrameDuration = 20 * time.Millisecond

// silenceLevel is the level, in dBFS, reported for digital silence.
const silenceLevel = -96.0

// assumedNoise is the noise floor assumed before any audio has been heard:
// a quiet phone line. It fills the noise window at the start, so speech in
// the very first frames is detected, and real audio replaces it within one
// window.
const assumedNoise = -60.0

// Config tunes detection. Zero fields take the defaults noted on each.
type Config struct {
	// MinLevel is the quietest a speech frame can be, in dBFS. Default -45.
	MinLevel float64
	// Margin is how far above the background noise a speech frame must be,
	// in dB. Default 12.
	Margin float64
	// MinSpeech is the shortest stretch of speech kept. Default 60 ms.
	MinSpeech time.Duration
	// MinSilence is the shortest gap that splits speech into separate
	// segments. Default 250 ms.
	MinSilence time.Duration
	// NoiseWindow is how far back the noise floor looks for the quietest
	// frame. Default 2 s.
	NoiseWindow time.Duration
}

// WithDefaults returns c with zero fields set to their defaults.
func (c Config) WithDefaults() Config {
	if c.MinLevel == 0 {
		c.MinLevel = -45
	}
	if c.Margin == 0 {
		c.Margin = 12
	}
	if c.MinSpeech == 0 {
		c.MinSpeech = 60 * time.Millisecond
	}
	if c.MinSilence == 0 {
		c.MinSilence = 250 * time.Millisecond
	}
	if c.NoiseWindow == 0 {
		c.NoiseWindow = 2 * time.Second
	}
	return c
}

// Level returns the RMS level of samples in dBFS: 0 for a full-scale square
// wave, about -3 for a full-scale sine, and -96 for silence.
func Level(samples []int16) float64 {
	if len(samples) == 0 {
		return silenceLevel
	}
	// Remove any DC offset so it doesn't read as sound.
	var mean float64
	for _, s := range samples {
		mean += float64(s)
	}
	mean /= float64(len(samples))
	var sum float64
	for _, s := range samples {
		d := float64(s) - mean
		sum += d * d
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	if rms < 1 {
		return silenceLevel
	}
	return math.Max(20*math.Log10(rms/32768), silenceLevel)
}

// Classifier decides frame by frame whether audio is speech, tracking the
// background noise level as it goes. Use one Classifier per audio stream.
type Classifier struct {
	cfg    Config
	levels []float64 // ring buffer of recent frame levels
	next   int
}

// NewClassifier returns a classifier for one stream.
func NewClassifier(cfg Config) *Classifier {
	cfg = cfg.WithDefaults()
	levels := make([]float64, max(int(cfg.NoiseWindow/FrameDuration), 1))
	for i := range levels {
		levels[i] = assumedNoise
	}
	return &Classifier{cfg: cfg, levels: levels}
}

// Speech classifies one frame (ideally 20 ms) and updates the noise
// estimate.
func (c *Classifier) Speech(frame []int16) bool {
	level := Level(frame)
	c.levels[c.next] = level
	c.next = (c.next + 1) % len(c.levels)
	return level >= c.cfg.MinLevel && level >= c.Noise()+c.cfg.Margin
}

// Noise is the current background noise estimate in dBFS: the quietest
// frame in the window, including the latest.
func (c *Classifier) Noise() float64 {
	lo := c.levels[0]
	for _, l := range c.levels[1:] {
		lo = math.Min(lo, l)
	}
	return lo
}

// Frame is a classified stretch of audio on a timeline.
type Frame struct {
	Start  time.Time
	End    time.Time
	Speech bool
}

// Segment is a stretch of speech.
type Segment struct {
	Start time.Time
	End   time.Time
}

// Duration is the segment's length.
func (s Segment) Duration() time.Duration { return s.End.Sub(s.Start) }

// Merge groups speech frames into segments. Frames must be in time order.
// Gaps shorter than minSilence are bridged, then segments shorter than
// minSpeech are dropped.
func Merge(frames []Frame, minSpeech, minSilence time.Duration) []Segment {
	var segs []Segment
	for _, f := range frames {
		if !f.Speech || !f.End.After(f.Start) {
			continue
		}
		if n := len(segs); n > 0 && f.Start.Sub(segs[n-1].End) < minSilence {
			if f.End.After(segs[n-1].End) {
				segs[n-1].End = f.End
			}
			continue
		}
		segs = append(segs, Segment{Start: f.Start, End: f.End})
	}
	kept := segs[:0]
	for _, s := range segs {
		if s.Duration() >= minSpeech {
			kept = append(kept, s)
		}
	}
	return kept
}

// Span is a stretch of speech within a clip, as offsets from its start.
type Span struct {
	Start time.Duration
	End   time.Duration
}

// Detect finds the speech in a whole clip of 8 kHz audio.
func Detect(pcm []int16, cfg Config) []Span {
	cfg = cfg.WithDefaults()
	c := NewClassifier(cfg)
	var base time.Time
	var frames []Frame
	n := audio.TelephonyFrameSamples
	for off := 0; off < len(pcm); off += n {
		end := min(off+n, len(pcm))
		frames = append(frames, Frame{
			Start:  base.Add(sampleTime(off)),
			End:    base.Add(sampleTime(end)),
			Speech: c.Speech(pcm[off:end]),
		})
	}
	var spans []Span
	for _, s := range Merge(frames, cfg.MinSpeech, cfg.MinSilence) {
		spans = append(spans, Span{Start: s.Start.Sub(base), End: s.End.Sub(base)})
	}
	return spans
}

func sampleTime(samples int) time.Duration {
	return time.Duration(samples) * time.Second / audio.TelephonySampleRate
}
