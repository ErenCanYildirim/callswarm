package audio

import (
	"math"
	"testing"
)

// sine returns n samples of a tone at amplitude 10000.
func sine(rate int, freq float64, n int) []int16 {
	const amp = 10000
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(math.Round(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))))
	}
	return out
}

// rms of the middle of a signal, skipping edges where the filter is truncated.
func midRMS(s []int16) float64 {
	trim := len(s) / 10
	s = s[trim : len(s)-trim]
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestResampleLength(t *testing.T) {
	tests := []struct {
		from, to, n, want int
	}{
		{16000, 8000, 16000, 8000},
		{48000, 8000, 48000, 8000},
		{44100, 8000, 44100, 8000},
		{8000, 16000, 800, 1600},
		{44100, 8000, 441, 80},
	}
	for _, tt := range tests {
		out, err := Resample(PCM{SampleRate: tt.from, Samples: make([]int16, tt.n)}, tt.to)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Samples) != tt.want || out.SampleRate != tt.to {
			t.Errorf("%d→%d n=%d: got len %d rate %d, want len %d", tt.from, tt.to, tt.n, len(out.Samples), out.SampleRate, tt.want)
		}
	}
}

func TestResamplePreservesDC(t *testing.T) {
	in := make([]int16, 4800)
	for i := range in {
		in[i] = 1234
	}
	out, err := Resample(PCM{SampleRate: 48000, Samples: in}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range out.Samples {
		if s != 1234 {
			t.Fatalf("sample %d = %d, want 1234", i, s)
		}
	}
}

// A 1 kHz tone is inside the telephone band and must pass through intact.
func TestResamplePassband(t *testing.T) {
	for _, from := range []int{16000, 44100, 48000} {
		in := sine(from, 1000, from/2)
		out, err := Resample(PCM{SampleRate: from, Samples: in}, 8000)
		if err != nil {
			t.Fatal(err)
		}
		inRMS, outRMS := midRMS(in), midRMS(out.Samples)
		if ratio := outRMS / inRMS; ratio < 0.98 || ratio > 1.02 {
			t.Errorf("%d Hz → 8 kHz: 1 kHz tone gain = %.3f, want ~1", from, ratio)
		}
	}
}

// A 6 kHz tone is above the 4 kHz Nyquist limit at 8 kHz. Naive decimation
// would alias it to 2 kHz at full strength; the filter must remove it.
func TestResampleRejectsAliasing(t *testing.T) {
	in := sine(48000, 6000, 24000)
	out, err := Resample(PCM{SampleRate: 48000, Samples: in}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	attenuation := 20 * math.Log10(midRMS(out.Samples)/midRMS(in))
	if attenuation > -60 {
		t.Errorf("6 kHz tone attenuated by %.1f dB, want at least 60 dB", -attenuation)
	}
}

func TestResampleUpsamplePassband(t *testing.T) {
	in := sine(8000, 1000, 4000)
	out, err := Resample(PCM{SampleRate: 8000, Samples: in}, 16000)
	if err != nil {
		t.Fatal(err)
	}
	want := sine(16000, 1000, 8000)
	// Compare in the middle, allowing a small error from interpolation.
	var maxErr int
	for i := 800; i < 7200; i++ {
		d := int(out.Samples[i]) - int(want[i])
		if d < 0 {
			d = -d
		}
		maxErr = max(maxErr, d)
	}
	if maxErr > 100 {
		t.Errorf("max error vs ideal 16 kHz sine = %d, want ≤ 100", maxErr)
	}
}

func TestResampleSameRateCopies(t *testing.T) {
	in := []int16{1, 2, 3}
	out, err := Resample(PCM{SampleRate: 8000, Samples: in}, 8000)
	if err != nil {
		t.Fatal(err)
	}
	out.Samples[0] = 99
	if in[0] != 1 {
		t.Error("same-rate resample aliased the input slice")
	}
}

func TestResampleInvalidRates(t *testing.T) {
	if _, err := Resample(PCM{SampleRate: 0}, 8000); err == nil {
		t.Error("expected error for zero source rate")
	}
	if _, err := Resample(PCM{SampleRate: 8000}, -1); err == nil {
		t.Error("expected error for negative target rate")
	}
}

func TestResampleFixtureToTelephony(t *testing.T) {
	p, err := LoadWAV("testdata/tone_16k_mono.wav")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Resample(p, TelephonySampleRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Samples) != 4000 {
		t.Fatalf("len = %d, want 4000", len(out.Samples))
	}
	if ratio := midRMS(out.Samples) / midRMS(p.Samples); ratio < 0.98 || ratio > 1.02 {
		t.Errorf("440 Hz tone gain = %.3f, want ~1", ratio)
	}
}
