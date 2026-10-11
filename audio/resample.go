package audio

import (
	"fmt"
	"math"
)

// resampleHalfWidth is the number of input-rate zero crossings on each side of
// the windowed-sinc kernel. Larger values give a sharper anti-aliasing filter
// at higher cost. 16 is ample for preparing caller audio offline.
const resampleHalfWidth = 16

// resampleRolloff places the filter cutoff slightly below the Nyquist
// frequency of the lower rate, leaving room for the transition band.
const resampleRolloff = 0.9

// Resample converts p to the target sample rate using band-limited
// (windowed-sinc) interpolation. When downsampling, content above the new
// Nyquist frequency is filtered out rather than aliased into the speech band.
//
// It is intended for preparing audio files ahead of a run, not for the
// real-time path, and favors accuracy over speed.
func Resample(p PCM, targetRate int) (PCM, error) {
	if p.SampleRate <= 0 {
		return PCM{}, fmt.Errorf("invalid source sample rate %d", p.SampleRate)
	}
	if targetRate <= 0 {
		return PCM{}, fmt.Errorf("invalid target sample rate %d", targetRate)
	}
	if p.SampleRate == targetRate {
		out := make([]int16, len(p.Samples))
		copy(out, p.Samples)
		return PCM{SampleRate: targetRate, Samples: out}, nil
	}

	in := p.Samples
	ratio := float64(targetRate) / float64(p.SampleRate)
	outLen := int(math.Round(float64(len(in)) * ratio))

	// Cutoff as a fraction of the input sample rate's Nyquist frequency.
	cutoff := resampleRolloff * math.Min(1, ratio)
	// Kernel half-width measured in input samples.
	halfWidth := float64(resampleHalfWidth) / cutoff

	out := make([]int16, outLen)
	for i := range outLen {
		center := float64(i) / ratio
		lo := int(math.Ceil(center - halfWidth))
		hi := int(math.Floor(center + halfWidth))
		if lo < 0 {
			lo = 0
		}
		if hi > len(in)-1 {
			hi = len(in) - 1
		}

		var acc, norm float64
		for j := lo; j <= hi; j++ {
			x := float64(j) - center
			w := cutoff * sinc(cutoff*x) * blackman(x/halfWidth)
			acc += w * float64(in[j])
			norm += w
		}
		// Normalizing by the summed weights keeps DC gain at exactly 1,
		// including near the edges where the kernel is truncated.
		if norm != 0 {
			acc /= norm
		}
		out[i] = clamp16(acc)
	}
	return PCM{SampleRate: targetRate, Samples: out}, nil
}

// sinc is the normalized sinc function sin(πx)/(πx).
func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// blackman is a Blackman window over t in [-1, 1], zero outside.
func blackman(t float64) float64 {
	if t <= -1 || t >= 1 {
		return 0
	}
	a := math.Pi * (t + 1) // maps [-1,1] to [0, 2π]
	return 0.42 - 0.5*math.Cos(a) + 0.08*math.Cos(2*a)
}

func clamp16(v float64) int16 {
	v = math.Round(v)
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(v)
}
