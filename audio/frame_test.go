package audio

import (
	"testing"
	"time"
)

func TestFrameSize(t *testing.T) {
	tests := []struct {
		rate int
		d    time.Duration
		want int
		ok   bool
	}{
		{8000, 20 * time.Millisecond, TelephonyFrameSamples, true},
		{16000, 20 * time.Millisecond, 320, true},
		{48000, 10 * time.Millisecond, 480, true},
		{44100, 20 * time.Millisecond, 882, true},
		{44100, 1 * time.Millisecond, 0, false}, // 44.1 samples
		{0, 20 * time.Millisecond, 0, false},
		{8000, 0, 0, false},
	}
	for _, tt := range tests {
		got, err := FrameSize(tt.rate, tt.d)
		if (err == nil) != tt.ok {
			t.Errorf("FrameSize(%d, %v) err = %v, want ok=%v", tt.rate, tt.d, err, tt.ok)
			continue
		}
		if got != tt.want {
			t.Errorf("FrameSize(%d, %v) = %d, want %d", tt.rate, tt.d, got, tt.want)
		}
	}
}

func TestFramesExact(t *testing.T) {
	data := make([]int16, 3*TelephonyFrameSamples)
	for i := range data {
		data[i] = int16(i)
	}
	frames := Frames(data, TelephonyFrameSamples)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		if len(f) != TelephonyFrameSamples {
			t.Fatalf("frame %d has %d samples", i, len(f))
		}
		if f[0] != int16(i*TelephonyFrameSamples) {
			t.Errorf("frame %d starts at %d", i, f[0])
		}
	}
}

func TestFramesPadsLastFrame(t *testing.T) {
	frames := Frames([]int16{1, 2, 3, 4, 5}, 2)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	last := frames[2]
	if len(last) != 2 || last[0] != 5 || last[1] != 0 {
		t.Errorf("last frame = %v, want [5 0]", last)
	}
}

func TestFramesPaddedMulawSilence(t *testing.T) {
	frames := FramesPadded([]byte{0x10, 0x20, 0x30}, 2, MulawSilence)
	last := frames[1]
	if last[0] != 0x30 || last[1] != MulawSilence {
		t.Errorf("last frame = %x, want [30 ff]", last)
	}
}

func TestFramesDoNotAliasInput(t *testing.T) {
	data := []int16{1, 2, 3, 4}
	frames := Frames(data, 2)
	frames[0][0] = 99
	if data[0] != 1 {
		t.Error("frame shares memory with input")
	}
}

func TestFramesEmptyAndInvalid(t *testing.T) {
	if Frames([]int16{}, 160) != nil {
		t.Error("empty input should give nil")
	}
	if Frames([]int16{1}, 0) != nil {
		t.Error("zero frame size should give nil")
	}
}

// End-to-end: a 16 kHz WAV becomes 20 ms μ-law frames ready for Twilio.
func TestPipelineWAVToTelephonyFrames(t *testing.T) {
	p, err := LoadWAV("testdata/tone_16k_mono.wav")
	if err != nil {
		t.Fatal(err)
	}
	p, err = Resample(p, TelephonySampleRate)
	if err != nil {
		t.Fatal(err)
	}
	size, err := FrameSize(p.SampleRate, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	frames := FramesPadded(MulawEncode(p.Samples), size, MulawSilence)
	// 0.5 s of audio in 20 ms frames.
	if len(frames) != 25 {
		t.Errorf("got %d frames, want 25", len(frames))
	}
	for i, f := range frames {
		if len(f) != 160 {
			t.Fatalf("frame %d has %d bytes, want 160", i, len(f))
		}
	}
}
