package audio

import (
	"math"
	"testing"
)

func TestToneGeneratorAmplitudeBound(t *testing.T) {
	g := NewToneGenerator(OutputSampleRate, 440, 0.3)
	buf := make([]float32, 4800)
	g.Fill(buf)
	var peak float32
	for _, v := range buf {
		if a := float32(math.Abs(float64(v))); a > peak {
			peak = a
		}
	}
	if peak > 0.3001 {
		t.Fatalf("peak %v exceeds amplitude 0.3", peak)
	}
	if peak < 0.28 {
		t.Fatalf("peak %v far below amplitude 0.3 — tone not generated?", peak)
	}
}

// Phase must carry across Fill calls so two back-to-back frames are a single
// continuous sine — no discontinuity at the boundary.
func TestToneGeneratorPhaseContinuity(t *testing.T) {
	g := NewToneGenerator(OutputSampleRate, 440, 0.5)
	a := make([]float32, 480)
	b := make([]float32, 480)
	g.Fill(a)
	g.Fill(b)
	// Reference generator over the whole 960-sample span.
	ref := NewToneGenerator(OutputSampleRate, 440, 0.5)
	full := make([]float32, 960)
	ref.Fill(full)
	for i := 0; i < 480; i++ {
		if d := math.Abs(float64(b[i] - full[480+i])); d > 1e-5 {
			t.Fatalf("discontinuity at boundary sample %d: %v vs %v", i, b[i], full[480+i])
		}
	}
}

func TestToneGeneratorFrequencyByZeroCrossings(t *testing.T) {
	const freq = 500
	g := NewToneGenerator(OutputSampleRate, freq, 0.5)
	buf := make([]float32, OutputSampleRate) // 1 second
	g.Fill(buf)
	crossings := 0
	for i := 1; i < len(buf); i++ {
		if (buf[i-1] < 0 && buf[i] >= 0) || (buf[i-1] >= 0 && buf[i] < 0) {
			crossings++
		}
	}
	// ~2 zero crossings per cycle → ~2*freq in one second.
	if math.Abs(float64(crossings-2*freq)) > 4 {
		t.Fatalf("zero crossings=%d, want ~%d for %d Hz", crossings, 2*freq, freq)
	}
}
