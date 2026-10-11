package audio

// G.711 μ-law codec, as used by Twilio Media Streams.
//
// The encoder follows the classic Sun/ITU-T G.191 reference implementation:
// the 16-bit sample is reduced to 14 bits with an arithmetic shift, biased,
// clipped, split into a 3-bit segment and 4-bit mantissa, and inverted. The
// arithmetic shift matters for negative samples near segment boundaries, and
// this matches CPython's audioop and common telephony stacks bit for bit.
// Decoding uses a precomputed 256-entry table.

const (
	mulawBias14 = 0x21 // 0x84 >> 2
	mulawClip14 = 8159
	mulawBias   = 0x84
)

// mulawSegEnd holds the upper bound of each 14-bit segment.
var mulawSegEnd = [8]int{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}

var mulawDecodeTable = func() [256]int16 {
	var t [256]int16
	for i := range 256 {
		u := ^byte(i)
		exponent := (u >> 4) & 0x07
		mantissa := u & 0x0F
		magnitude := ((int(mantissa) << 3) + mulawBias) << exponent
		magnitude -= mulawBias
		if u&0x80 != 0 {
			t[i] = int16(-magnitude)
		} else {
			t[i] = int16(magnitude)
		}
	}
	return t
}()

// MulawEncodeSample converts one 16-bit linear sample to a μ-law byte.
func MulawEncodeSample(s int16) byte {
	v := int(s) >> 2
	mask := byte(0xFF)
	if v < 0 {
		v = -v
		mask = 0x7F
	}
	if v > mulawClip14 {
		v = mulawClip14
	}
	v += mulawBias14

	seg := 0
	for seg < len(mulawSegEnd) && v > mulawSegEnd[seg] {
		seg++
	}
	if seg >= len(mulawSegEnd) {
		return 0x7F ^ mask
	}
	u := byte(seg<<4) | byte((v>>(seg+1))&0x0F)
	return u ^ mask
}

// MulawDecodeSample converts one μ-law byte to a 16-bit linear sample.
func MulawDecodeSample(b byte) int16 {
	return mulawDecodeTable[b]
}

// MulawEncode converts linear samples to μ-law bytes.
func MulawEncode(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = MulawEncodeSample(s)
	}
	return out
}

// MulawDecode converts μ-law bytes to linear samples.
func MulawDecode(data []byte) []int16 {
	out := make([]int16, len(data))
	for i, b := range data {
		out[i] = mulawDecodeTable[b]
	}
	return out
}
