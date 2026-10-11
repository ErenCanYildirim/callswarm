package audio

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// TestMulawEncodeMatchesReference checks every possible int16 input against
// CPython's audioop.lin2ulaw (see testdata/gen_fixtures.py).
func TestMulawEncodeMatchesReference(t *testing.T) {
	ref, err := os.ReadFile("testdata/mulaw_encode_ref.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(ref) != 65536 {
		t.Fatalf("reference length = %d", len(ref))
	}
	mismatches := 0
	for i := range 65536 {
		s := int16(i - 32768)
		if got := MulawEncodeSample(s); got != ref[i] {
			if mismatches < 10 {
				t.Errorf("encode(%d) = 0x%02X, want 0x%02X", s, got, ref[i])
			}
			mismatches++
		}
	}
	if mismatches > 0 {
		t.Errorf("%d mismatches in total", mismatches)
	}
}

// TestMulawDecodeMatchesReference checks all 256 codes against audioop.ulaw2lin.
func TestMulawDecodeMatchesReference(t *testing.T) {
	ref, err := os.ReadFile("testdata/mulaw_decode_ref.bin")
	if err != nil {
		t.Fatal(err)
	}
	for b := range 256 {
		want := int16(binary.LittleEndian.Uint16(ref[2*b:]))
		if got := MulawDecodeSample(byte(b)); got != want {
			t.Errorf("decode(0x%02X) = %d, want %d", b, got, want)
		}
	}
}

func TestMulawKnownValues(t *testing.T) {
	enc := map[int16]byte{
		0:             0xFF,
		-1:            0x7E, // -1 >> 2 is -1, so negatives never hit negative zero
		math.MaxInt16: 0x80,
		math.MinInt16: 0x00,
	}
	for s, want := range enc {
		if got := MulawEncodeSample(s); got != want {
			t.Errorf("encode(%d) = 0x%02X, want 0x%02X", s, got, want)
		}
	}
	dec := map[byte]int16{0x00: -32124, 0x80: 32124, 0xFF: 0, 0x7F: 0}
	for b, want := range dec {
		if got := MulawDecodeSample(b); got != want {
			t.Errorf("decode(0x%02X) = %d, want %d", b, got, want)
		}
	}
	if MulawEncodeSample(0) != MulawSilence {
		t.Error("MulawSilence does not match encode(0)")
	}
}

// Every code except negative zero (0x7F) must survive decode then encode.
func TestMulawCodeRoundTrip(t *testing.T) {
	for b := range 256 {
		if b == 0x7F {
			continue
		}
		if got := MulawEncodeSample(MulawDecodeSample(byte(b))); got != byte(b) {
			t.Errorf("encode(decode(0x%02X)) = 0x%02X", b, got)
		}
	}
}

func TestMulawSlices(t *testing.T) {
	in := []int16{0, 1000, -1000, 20000}
	enc := MulawEncode(in)
	dec := MulawDecode(enc)
	if len(enc) != len(in) || len(dec) != len(in) {
		t.Fatalf("lengths: enc %d dec %d", len(enc), len(dec))
	}
	for i := range in {
		if enc[i] != MulawEncodeSample(in[i]) || dec[i] != MulawDecodeSample(enc[i]) {
			t.Errorf("index %d differs from per-sample functions", i)
		}
	}
}

func BenchmarkMulawEncode(b *testing.B) {
	samples := make([]int16, TelephonyFrameSamples)
	for i := range samples {
		samples[i] = int16(i * 200)
	}
	b.ResetTimer()
	for b.Loop() {
		_ = MulawEncode(samples)
	}
}
