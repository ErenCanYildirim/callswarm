package audio

import (
	"fmt"
	"time"
)

// FrameSize returns the number of samples in a frame of duration d at the
// given sample rate. It returns an error unless d covers a whole number of
// samples, since fractional frames would make stream pacing drift.
func FrameSize(sampleRate int, d time.Duration) (int, error) {
	if sampleRate <= 0 {
		return 0, fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid frame duration %v", d)
	}
	num := int64(sampleRate) * int64(d)
	if num%int64(time.Second) != 0 {
		return 0, fmt.Errorf("%v at %d Hz is not a whole number of samples", d, sampleRate)
	}
	return int(num / int64(time.Second)), nil
}

// Frames splits data into consecutive frames of frameSize elements. The last
// frame is padded with zero values to full length, so every frame has the
// same duration. Frames share no memory with data.
//
// For μ-law bytes, note that silence is 0xFF, not 0; use FramesPadded with
// pad 0xFF for encoded audio.
func Frames[T any](data []T, frameSize int) [][]T {
	var zero T
	return FramesPadded(data, frameSize, zero)
}

// FramesPadded is Frames with an explicit padding value for the last frame.
func FramesPadded[T any](data []T, frameSize int, pad T) [][]T {
	if frameSize <= 0 || len(data) == 0 {
		return nil
	}
	n := (len(data) + frameSize - 1) / frameSize
	frames := make([][]T, n)
	for i := range n {
		f := make([]T, frameSize)
		copied := copy(f, data[i*frameSize:])
		for j := copied; j < frameSize; j++ {
			f[j] = pad
		}
		frames[i] = f
	}
	return frames
}

// MulawSilence is the μ-law encoding of a zero sample.
const MulawSilence byte = 0xFF
