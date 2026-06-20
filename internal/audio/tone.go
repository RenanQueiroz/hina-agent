package audio

import "math"

// ToneGenerator produces a continuous sine wave as successive mono float32
// frames at a fixed sample rate. It keeps the phase across Fill calls so frames
// join seamlessly — no click at frame boundaries — which is what lets the
// outbound pacer stream an unbroken tone for the "generated tone plays cleanly"
// exit criterion. It is not safe for concurrent use.
type ToneGenerator struct {
	phase float64 // current phase in radians, kept in [0, 2π)
	inc   float64 // phase increment per sample
	amp   float64
}

// NewToneGenerator builds a generator for freqHz at the given sample rate and
// amplitude (0..1, clamped). A typical test tone is 440 Hz at amplitude 0.3.
func NewToneGenerator(sampleRate int, freqHz, amplitude float64) *ToneGenerator {
	if amplitude < 0 {
		amplitude = 0
	} else if amplitude > 1 {
		amplitude = 1
	}
	inc := 0.0
	if sampleRate > 0 {
		inc = 2 * math.Pi * freqHz / float64(sampleRate)
	}
	return &ToneGenerator{inc: inc, amp: amplitude}
}

// Fill writes the next len(dst) samples of the tone into dst, advancing phase.
func (t *ToneGenerator) Fill(dst []float32) {
	for i := range dst {
		dst[i] = float32(t.amp * math.Sin(t.phase))
		t.phase += t.inc
		if t.phase >= 2*math.Pi {
			t.phase -= 2 * math.Pi
		}
	}
}
