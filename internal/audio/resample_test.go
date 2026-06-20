package audio

import (
	"math"
	"testing"
)

// feed processes a long mono signal through the resampler in 20 ms-ish chunks
// and returns all output samples concatenated.
func feed(t *testing.T, r *Resampler, in []float32, chunk int) []float32 {
	t.Helper()
	var out []float32
	for i := 0; i < len(in); i += chunk {
		end := i + chunk
		if end > len(in) {
			end = len(in)
		}
		got, err := r.Process(in[i:end])
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		out = append(out, got...)
	}
	return out
}

func TestResamplerDownsampleRatio(t *testing.T) {
	r, err := NewResampler(InputSampleRate, ASRSampleRate) // 48k -> 16k
	if err != nil {
		t.Fatal(err)
	}
	// One second of 48 kHz input should yield ~16 k output samples (1/3).
	in := make([]float32, InputSampleRate)
	tone := NewToneGenerator(InputSampleRate, 440, 0.5)
	tone.Fill(in)
	out := feed(t, r, in, 960)
	want := ASRSampleRate
	if d := math.Abs(float64(len(out) - want)); d > float64(want)*0.02 {
		t.Fatalf("48k->16k of 1s produced %d samples, want ~%d", len(out), want)
	}
}

func TestResamplerOutputRatio24k(t *testing.T) {
	r, err := NewResampler(InputSampleRate, OutputSampleRate) // 48k -> 24k
	if err != nil {
		t.Fatal(err)
	}
	in := make([]float32, InputSampleRate)
	NewToneGenerator(InputSampleRate, 300, 0.5).Fill(in)
	out := feed(t, r, in, 960)
	want := OutputSampleRate
	if d := math.Abs(float64(len(out) - want)); d > float64(want)*0.02 {
		t.Fatalf("48k->24k of 1s produced %d samples, want ~%d", len(out), want)
	}
}

// A tone well below the output Nyquist must survive downsampling with most of
// its energy intact (the anti-alias filter shouldn't gut the passband).
func TestResamplerPreservesPassbandEnergy(t *testing.T) {
	r, err := NewResampler(InputSampleRate, ASRSampleRate)
	if err != nil {
		t.Fatal(err)
	}
	in := make([]float32, InputSampleRate)
	NewToneGenerator(InputSampleRate, 1000, 0.5).Fill(in) // 1 kHz << 8 kHz Nyquist
	out := feed(t, r, in, 960)
	if len(out) == 0 {
		t.Fatal("no output")
	}
	// Skip the filter warm-up at the head, then compare RMS to the input tone.
	const skip = 2000
	if len(out) <= skip {
		t.Fatalf("too few output samples: %d", len(out))
	}
	rms := func(s []float32) float64 {
		var sum float64
		for _, v := range s {
			sum += float64(v) * float64(v)
		}
		return math.Sqrt(sum / float64(len(s)))
	}
	got := rms(out[skip:])
	want := 0.5 / math.Sqrt2 // RMS of a 0.5-amplitude sine
	if got < want*0.7 || got > want*1.3 {
		t.Fatalf("passband RMS=%.4f, want ~%.4f (±30%%)", got, want)
	}
}

func TestResamplerRejectsBadRate(t *testing.T) {
	if _, err := NewResampler(0, 16000); err == nil {
		t.Fatal("expected error for zero input rate")
	}
}
