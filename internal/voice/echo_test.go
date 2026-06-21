package voice

import (
	"math"
	"testing"
)

// tone returns n samples of a sine at the given peak amplitude.
func tone(n int, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*float64(i)/16))
	}
	return out
}

func TestEchoSuppressNotPlayingNeverSuppresses(t *testing.T) {
	e := NewEchoSuppressor(0, 0, 0)
	e.ObservePlayback(tone(512, 0.9)) // loud assistant audio observed
	if e.Suppress(tone(512, 0.001), false) {
		t.Fatal("must never suppress when the assistant is not playing")
	}
}

func TestEchoSuppressGatesQuietEcho(t *testing.T) {
	e := NewEchoSuppressor(0.6, 0.85, 1e-5)
	loud := tone(512, 0.8)
	e.ObservePlayback(loud) // assistant is loud
	// A quiet mic frame while playing (residual echo) is suppressed.
	if !e.Suppress(tone(512, 0.05), true) {
		t.Fatal("quiet mic energy during loud playback should be suppressed as echo")
	}
	// The user speaking clearly louder than the residual echo passes through.
	if e.Suppress(tone(512, 1.0), true) {
		t.Fatal("a mic frame louder than playback should not be suppressed")
	}
}

func TestEchoSuppressSilenceDuringPlayback(t *testing.T) {
	e := NewEchoSuppressor(0, 0, 0)
	e.ObservePlayback(tone(512, 0.5))
	if !e.Suppress(make([]float32, 512), true) {
		t.Fatal("near-silent frames during playback are never a barge-in")
	}
}

func TestEchoResetClearsGuard(t *testing.T) {
	e := NewEchoSuppressor(0.6, 0.85, 1e-5)
	e.ObservePlayback(tone(512, 0.9))
	e.Reset()
	// After reset, with no tracked playback energy, only sub-noise-floor is suppressed.
	if e.Suppress(tone(512, 0.05), true) {
		t.Fatal("after Reset there is no echo guard, so audible speech passes")
	}
}
