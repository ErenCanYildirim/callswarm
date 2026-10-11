package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	formatPCM        = 0x0001
	formatExtensible = 0xFFFE
)

// pcmSubFormatGUID is KSDATAFORMAT_SUBTYPE_PCM, the sub-format GUID that marks
// a WAVE_FORMAT_EXTENSIBLE file as integer PCM.
var pcmSubFormatGUID = []byte{
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// ErrUnsupportedWAV is returned for WAV files callswarm does not decode,
// such as compressed, floating-point or non-16-bit audio.
var ErrUnsupportedWAV = errors.New("unsupported WAV format")

// LoadWAV reads a WAV file from disk. See DecodeWAV for supported formats.
func LoadWAV(path string) (PCM, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PCM{}, err
	}
	pcm, err := DecodeWAV(bytes.NewReader(data))
	if err != nil {
		return PCM{}, fmt.Errorf("%s: %w", path, err)
	}
	return pcm, nil
}

// DecodeWAV decodes a RIFF/WAVE stream containing 16-bit integer PCM.
// Both the plain PCM format tag and WAVE_FORMAT_EXTENSIBLE with a PCM
// sub-format are accepted. Multi-channel audio is downmixed to mono by
// averaging channels. Unknown chunks (LIST, fact, cue, ...) are skipped.
func DecodeWAV(r io.Reader) (PCM, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return PCM{}, fmt.Errorf("reading RIFF header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return PCM{}, errors.New("not a RIFF/WAVE file")
	}

	var (
		haveFmt       bool
		channels      int
		sampleRate    int
		bitsPerSample int
	)

	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return PCM{}, errors.New("no data chunk found")
			}
			return PCM{}, fmt.Errorf("reading chunk header: %w", err)
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		// Chunks are padded to an even number of bytes.
		pad := size & 1

		switch id {
		case "fmt ":
			if size < 16 {
				return PCM{}, fmt.Errorf("fmt chunk too short: %d bytes", size)
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return PCM{}, fmt.Errorf("reading fmt chunk: %w", err)
			}
			tag := binary.LittleEndian.Uint16(buf[0:2])
			channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			bitsPerSample = int(binary.LittleEndian.Uint16(buf[14:16]))

			switch tag {
			case formatPCM:
			case formatExtensible:
				// cbSize(2) validBits(2) channelMask(4) subFormat(16)
				if size < 40 {
					return PCM{}, fmt.Errorf("extensible fmt chunk too short: %d bytes", size)
				}
				if !bytes.Equal(buf[24:40], pcmSubFormatGUID) {
					return PCM{}, fmt.Errorf("%w: extensible sub-format is not integer PCM", ErrUnsupportedWAV)
				}
			default:
				return PCM{}, fmt.Errorf("%w: format tag 0x%04X", ErrUnsupportedWAV, tag)
			}
			if bitsPerSample != 16 {
				return PCM{}, fmt.Errorf("%w: %d bits per sample (need 16)", ErrUnsupportedWAV, bitsPerSample)
			}
			if channels < 1 {
				return PCM{}, fmt.Errorf("invalid channel count %d", channels)
			}
			if sampleRate < 1 {
				return PCM{}, fmt.Errorf("invalid sample rate %d", sampleRate)
			}
			haveFmt = true
			if pad == 1 {
				if _, err := io.CopyN(io.Discard, r, 1); err != nil {
					return PCM{}, fmt.Errorf("skipping fmt padding: %w", err)
				}
			}

		case "data":
			if !haveFmt {
				return PCM{}, errors.New("data chunk before fmt chunk")
			}
			raw := make([]byte, size)
			n, err := io.ReadFull(r, raw)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
				return PCM{}, fmt.Errorf("reading data chunk: %w", err)
			}
			// Tolerate truncated files (some recorders write a wrong size),
			// keeping only whole frames.
			raw = raw[:n]
			return PCM{
				SampleRate: sampleRate,
				Samples:    downmix(raw, channels),
			}, nil

		default:
			if _, err := io.CopyN(io.Discard, r, size+pad); err != nil {
				return PCM{}, fmt.Errorf("skipping %q chunk: %w", id, err)
			}
		}
	}
}

// downmix converts interleaved little-endian 16-bit samples to mono.
func downmix(raw []byte, channels int) []int16 {
	frameBytes := 2 * channels
	frames := len(raw) / frameBytes
	out := make([]int16, frames)
	for i := range frames {
		base := i * frameBytes
		if channels == 1 {
			out[i] = int16(binary.LittleEndian.Uint16(raw[base:]))
			continue
		}
		var sum int
		for c := range channels {
			sum += int(int16(binary.LittleEndian.Uint16(raw[base+2*c:])))
		}
		out[i] = int16(sum / channels)
	}
	return out
}

// EncodeWAV writes mono 16-bit PCM as a canonical 44-byte-header WAV file.
// It is used to save caller and agent recordings for inspection.
func EncodeWAV(w io.Writer, p PCM) error {
	dataLen := len(p.Samples) * 2
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataLen))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], formatPCM)
	binary.LittleEndian.PutUint16(hdr[22:24], 1)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(p.SampleRate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(p.SampleRate*2))
	binary.LittleEndian.PutUint16(hdr[32:34], 2)
	binary.LittleEndian.PutUint16(hdr[34:36], 16)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataLen))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	body := make([]byte, dataLen)
	for i, s := range p.Samples {
		binary.LittleEndian.PutUint16(body[2*i:], uint16(s))
	}
	_, err := w.Write(body)
	return err
}
