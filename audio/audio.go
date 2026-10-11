// Package audio provides the audio plumbing callswarm needs to act as a caller:
// loading WAV files, converting them to telephony format (8 kHz μ-law), and
// cutting them into fixed-duration frames for real-time streaming.
package audio

const (
	// TelephonySampleRate is the sample rate of μ-law phone audio, in Hz.
	TelephonySampleRate = 8000
	// TelephonyFrameSamples is the number of samples in one 20 ms frame at 8 kHz.
	TelephonyFrameSamples = 160
)

// PCM is mono, 16-bit signed linear audio at a known sample rate.
type PCM struct {
	SampleRate int
	Samples    []int16
}
