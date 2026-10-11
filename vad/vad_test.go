package vad

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/ErenCanYildirim/callswarm/audio"
)

const rate = audio.TelephonySampleRate

func samples(d time.Duration) int { return int(d * rate / time.Second) }

// tone appends d of a 1 kHz tone at the given dBFS level.
func tone(pcm []int16, d time.Duration, dbfs float64) []int16 {
	amp := math.Pow(10, dbfs/20) * 32768 * math.Sqrt2 // RMS → peak
	for i := range samples(d) {
		pcm = append(pcm, int16(amp*math.Sin(2*math.Pi*1000*float64(i)/rate)))
	}
	return pcm
}

// noise appends d of white noise at roughly the given dBFS level.
func noise(pcm []int16, d time.Duration, dbfs float64, r *rand.Rand) []int16 {
	sigma := math.Pow(10, dbfs/20) * 32768
	for range samples(d) {
		pcm = append(pcm, int16(r.NormFloat64()*sigma))
	}
	return pcm
}

func silence(pcm []int16, d time.Duration) []int16 {
	return append(pcm, make([]int16, samples(d))...)
}

func near(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if d := got - want; d < -FrameDuration || d > FrameDuration {
		t.Errorf("%s = %v, want %v ± one frame", what, got, want)
	}
}

func TestLevel(t *testing.T) {
	if got := Level(make([]int16, 160)); got != silenceLevel {
		t.Errorf("silence = %.1f dBFS", got)
	}
	if got := Level(tone(nil, 20*time.Millisecond, -20)); math.Abs(got+20) > 0.5 {
		t.Errorf("-20 dBFS tone measured %.1f", got)
	}
	// A constant offset is not sound.
	dc := make([]int16, 160)
	for i := range dc {
		dc[i] = 5000
	}
	if got := Level(dc); got != silenceLevel {
		t.Errorf("DC offset measured %.1f dBFS", got)
	}
}

func TestDetectFindsSpeechInsideSilence(t *testing.T) {
	var pcm []int16
	pcm = silence(pcm, 300*time.Millisecond)
	pcm = tone(pcm, 500*time.Millisecond, -20)
	pcm = silence(pcm, 400*time.Millisecond)

	spans := Detect(pcm, Config{})
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %v", len(spans), spans)
	}
	near(t, "start", spans[0].Start, 300*time.Millisecond)
	near(t, "end", spans[0].End, 800*time.Millisecond)
}

// Recordings have room noise; speech must stand out from it, and the noise
// itself must not count.
func TestDetectOverBackgroundNoise(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var pcm []int16
	pcm = noise(pcm, 500*time.Millisecond, -50, r)
	pcm = tone(pcm, 400*time.Millisecond, -20)
	pcm = noise(pcm, 600*time.Millisecond, -50, r)
	pcm = tone(pcm, 300*time.Millisecond, -25)
	pcm = noise(pcm, 500*time.Millisecond, -50, r)

	spans := Detect(pcm, Config{})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	near(t, "first start", spans[0].Start, 500*time.Millisecond)
	near(t, "first end", spans[0].End, 900*time.Millisecond)
	near(t, "second start", spans[1].Start, 1500*time.Millisecond)
	near(t, "second end", spans[1].End, 1800*time.Millisecond)
}

// Steady loud background (such as ambience under an agent) counts as speech
// only until it fills the noise window; then it is background, and speech
// over it is still found.
func TestNoiseFloorAdapts(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var pcm []int16
	pcm = silence(pcm, 200*time.Millisecond)
	pcm = noise(pcm, 3*time.Second, -40, r) // well above MinLevel
	pcm = tone(pcm, 400*time.Millisecond, -10)

	c := NewClassifier(Config{})
	var tail []bool
	for off := 0; off+160 <= len(pcm); off += 160 {
		s := c.Speech(pcm[off : off+160])
		if off >= samples(2500*time.Millisecond) && off < samples(3200*time.Millisecond) {
			tail = append(tail, s)
		}
	}
	for i, s := range tail {
		if s {
			t.Fatalf("noise still classified as speech %v into it", 2300*time.Millisecond+time.Duration(i)*FrameDuration)
		}
	}
	spans := Detect(pcm, Config{})
	last := spans[len(spans)-1]
	near(t, "speech after noise", last.Start, 3200*time.Millisecond)
}

func TestShortClickIsDropped(t *testing.T) {
	var pcm []int16
	pcm = silence(pcm, 200*time.Millisecond)
	pcm = tone(pcm, 20*time.Millisecond, -10) // one frame
	pcm = silence(pcm, 200*time.Millisecond)
	if spans := Detect(pcm, Config{}); len(spans) != 0 {
		t.Errorf("click detected as speech: %v", spans)
	}
}

func TestPausesBetweenWordsAreBridged(t *testing.T) {
	var pcm []int16
	pcm = tone(pcm, 300*time.Millisecond, -20)
	pcm = silence(pcm, 150*time.Millisecond) // within a sentence
	pcm = tone(pcm, 300*time.Millisecond, -20)
	pcm = silence(pcm, 500*time.Millisecond) // between sentences
	pcm = tone(pcm, 300*time.Millisecond, -20)

	spans := Detect(pcm, Config{})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	near(t, "first end", spans[0].End, 750*time.Millisecond)
	near(t, "second start", spans[1].Start, 1250*time.Millisecond)

	// A shorter MinSilence splits at the word gap too.
	if n := len(Detect(pcm, Config{MinSilence: 100 * time.Millisecond})); n != 3 {
		t.Errorf("with 100ms MinSilence got %d spans, want 3", n)
	}
}

func TestQuietSoundBelowFloorIgnored(t *testing.T) {
	var pcm []int16
	pcm = silence(pcm, 200*time.Millisecond)
	pcm = tone(pcm, 500*time.Millisecond, -55) // above noise, below -45 floor
	if spans := Detect(pcm, Config{}); len(spans) != 0 {
		t.Errorf("sound below MinLevel detected: %v", spans)
	}
}

func TestMergeOnTimeline(t *testing.T) {
	t0 := time.Unix(100, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	frames := []Frame{
		{at(0), at(20), true},
		{at(20), at(40), true},
		{at(40), at(60), true},
		{at(100), at(120), true}, // 40 ms gap: bridged
		{at(500), at(520), true}, // alone: too short
		{at(900), at(900), true}, // zero length: ignored
	}
	segs := Merge(frames, 60*time.Millisecond, 100*time.Millisecond)
	if len(segs) != 1 || !segs[0].Start.Equal(at(0)) || !segs[0].End.Equal(at(120)) {
		t.Errorf("segments = %v", segs)
	}
}

// Agent replies start with speech in the very first frame.
func TestSpeechAtStreamStart(t *testing.T) {
	pcm := tone(nil, 400*time.Millisecond, -20)
	spans := Detect(pcm, Config{})
	if len(spans) != 1 {
		t.Fatalf("got %v, want one span", spans)
	}
	near(t, "start", spans[0].Start, 0)
	near(t, "end", spans[0].End, 400*time.Millisecond)
}

func TestDetectEmpty(t *testing.T) {
	if spans := Detect(nil, Config{}); spans != nil {
		t.Errorf("got %v", spans)
	}
}
