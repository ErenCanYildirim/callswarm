package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestLoadWAVMono(t *testing.T) {
	p, err := LoadWAV("testdata/tone_16k_mono.wav")
	if err != nil {
		t.Fatal(err)
	}
	if p.SampleRate != 16000 {
		t.Errorf("sample rate = %d, want 16000", p.SampleRate)
	}
	if len(p.Samples) != 8000 {
		t.Fatalf("len = %d, want 8000", len(p.Samples))
	}
	// Spot-check against the generator's formula.
	for _, n := range []int{0, 1, 9, 100, 7999} {
		want := int16(math.Round(10000 * math.Sin(2*math.Pi*440*float64(n)/16000)))
		if p.Samples[n] != want {
			t.Errorf("sample %d = %d, want %d", n, p.Samples[n], want)
		}
	}
}

func TestLoadWAVStereoDownmix(t *testing.T) {
	p, err := LoadWAV("testdata/stereo_44k.wav")
	if err != nil {
		t.Fatal(err)
	}
	if p.SampleRate != 44100 {
		t.Errorf("sample rate = %d, want 44100", p.SampleRate)
	}
	if len(p.Samples) != 100 {
		t.Fatalf("len = %d, want 100", len(p.Samples))
	}
	for i, s := range p.Samples {
		if s != -1000 {
			t.Fatalf("sample %d = %d, want -1000", i, s)
		}
	}
}

// buildWAV assembles a WAV file from raw chunks, for cases the fixtures don't cover.
func buildWAV(chunks ...[]byte) []byte {
	var body bytes.Buffer
	body.WriteString("WAVE")
	for _, c := range chunks {
		body.Write(c)
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

func chunk(id string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(payload)))
	b.Write(payload)
	if len(payload)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// fmtPayload builds a mono fmt chunk payload.
func fmtPayload(tag uint16, rate uint32, bits uint16) []byte {
	const channels uint16 = 1
	var b bytes.Buffer
	blockAlign := channels * bits / 8
	for _, v := range []any{tag, channels, rate, rate * uint32(blockAlign), blockAlign, bits} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

func samplesPayload(s ...int16) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, s)
	return b.Bytes()
}

func TestDecodeWAVSkipsUnknownOddChunks(t *testing.T) {
	data := buildWAV(
		chunk("LIST", []byte("abc")), // odd length, padded
		chunk("fmt ", fmtPayload(formatPCM, 8000, 16)),
		chunk("fact", []byte{1, 2, 3, 4}),
		chunk("data", samplesPayload(1, -2, 3)),
	)
	p, err := DecodeWAV(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []int16{1, -2, 3}
	if len(p.Samples) != len(want) {
		t.Fatalf("samples = %v, want %v", p.Samples, want)
	}
	for i := range want {
		if p.Samples[i] != want[i] {
			t.Fatalf("samples = %v, want %v", p.Samples, want)
		}
	}
}

func TestDecodeWAVExtensible(t *testing.T) {
	f := fmtPayload(formatExtensible, 48000, 16)
	var ext bytes.Buffer
	ext.Write(f)
	_ = binary.Write(&ext, binary.LittleEndian, uint16(22)) // cbSize
	_ = binary.Write(&ext, binary.LittleEndian, uint16(16)) // valid bits
	_ = binary.Write(&ext, binary.LittleEndian, uint32(4))  // channel mask
	ext.Write(pcmSubFormatGUID)

	p, err := DecodeWAV(bytes.NewReader(buildWAV(
		chunk("fmt ", ext.Bytes()),
		chunk("data", samplesPayload(42)),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if p.SampleRate != 48000 || len(p.Samples) != 1 || p.Samples[0] != 42 {
		t.Fatalf("got %+v", p)
	}
}

func TestDecodeWAVRejects(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		unsupported bool
	}{
		{"not riff", []byte("RIFX\x00\x00\x00\x00WAVE"), false},
		{"float", buildWAV(chunk("fmt ", fmtPayload(3, 8000, 32)), chunk("data", nil)), true},
		{"24-bit", buildWAV(chunk("fmt ", fmtPayload(formatPCM, 8000, 24)), chunk("data", nil)), true},
		{"no data", buildWAV(chunk("fmt ", fmtPayload(formatPCM, 8000, 16))), false},
		{"data before fmt", buildWAV(chunk("data", samplesPayload(1))), false},
		{"truncated header", []byte("RIFF"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeWAV(bytes.NewReader(tt.data))
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errors.Is(err, ErrUnsupportedWAV); got != tt.unsupported {
				t.Errorf("errors.Is(ErrUnsupportedWAV) = %v, want %v (err: %v)", got, tt.unsupported, err)
			}
		})
	}
}

func TestDecodeWAVTruncatedData(t *testing.T) {
	// Declared data size is larger than what follows; keep whole samples only.
	data := buildWAV(chunk("fmt ", fmtPayload(formatPCM, 8000, 16)))
	data = append(data, []byte("data")...)
	data = binary.LittleEndian.AppendUint32(data, 100)
	data = append(data, samplesPayload(7, 8)...)
	data = append(data, 0x01) // half a sample
	p, err := DecodeWAV(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Samples) != 2 || p.Samples[0] != 7 || p.Samples[1] != 8 {
		t.Fatalf("samples = %v, want [7 8]", p.Samples)
	}
}

func TestEncodeDecodeWAVRoundTrip(t *testing.T) {
	in := PCM{SampleRate: 8000, Samples: []int16{0, 1, -1, math.MaxInt16, math.MinInt16}}
	var buf bytes.Buffer
	if err := EncodeWAV(&buf, in); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 44+2*len(in.Samples) {
		t.Errorf("encoded length = %d", buf.Len())
	}
	out, err := DecodeWAV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.SampleRate != in.SampleRate || len(out.Samples) != len(in.Samples) {
		t.Fatalf("got %+v", out)
	}
	for i := range in.Samples {
		if out.Samples[i] != in.Samples[i] {
			t.Fatalf("sample %d = %d, want %d", i, out.Samples[i], in.Samples[i])
		}
	}
}
