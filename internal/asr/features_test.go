package asr

import (
	"math"
	"testing"
)

func TestHannWindow(t *testing.T) {
	w := hannWindow(400)
	if len(w) != 400 {
		t.Fatalf("len = %d, want 400", len(w))
	}
	// Symmetric Hann: endpoints ~0, center ~1, mirror symmetry.
	if math.Abs(w[0]) > 1e-9 || math.Abs(w[399]) > 1e-9 {
		t.Fatalf("endpoints not ~0: %g, %g", w[0], w[399])
	}
	for i := 0; i < 200; i++ {
		if math.Abs(w[i]-w[399-i]) > 1e-12 {
			t.Fatalf("window not symmetric at %d", i)
		}
	}
	// Single-sample window degenerates to all-ones (no divide-by-zero).
	if got := hannWindow(1); len(got) != 1 || got[0] != 1 {
		t.Fatalf("hann(1) = %v, want [1]", got)
	}
}

func TestMelFilterbankShapeAndNorm(t *testing.T) {
	bank := melFilterbank(nFFT, nMels, sampleRate)
	if len(bank) != nMels {
		t.Fatalf("rows = %d, want %d", len(bank), nMels)
	}
	for m := range bank {
		if len(bank[m]) != freqBins {
			t.Fatalf("row %d cols = %d, want %d", m, len(bank[m]), freqBins)
		}
		var sum float64
		for _, v := range bank[m] {
			if v < 0 {
				t.Fatalf("row %d has a negative weight %g (triangles are non-negative)", m, v)
			}
			sum += v
		}
		if sum <= 0 {
			t.Fatalf("row %d is all-zero (no passband)", m)
		}
	}
}

func TestPreemphasis(t *testing.T) {
	src := []float64{1, 2, 3, 4}
	dst := make([]float64, len(src))
	preemphasize(dst, src, 0.97)
	want := []float64{1, 2 - 0.97*1, 3 - 0.97*2, 4 - 0.97*3}
	for i := range want {
		if math.Abs(dst[i]-want[i]) > 1e-12 {
			t.Fatalf("dst[%d] = %g, want %g", i, dst[i], want[i])
		}
	}
	// Empty input is a no-op (no panic).
	preemphasize(nil, nil, 0.97)
}

func TestNumMelFrames(t *testing.T) {
	// 1 s @16 kHz: (16000 + 512 - 512)/160 + 1 = 101 frames.
	if got := numMelFrames(16000); got != 101 {
		t.Fatalf("numMelFrames(16000) = %d, want 101", got)
	}
	// Too short to produce any frame (< n_fft after padding -> 0).
	if got := numMelFrames(1); got != 1 {
		// 1 + 512 - 512 = ... padded=1+512=513 >= 512 -> (513-512)/160+1 = 1.
		t.Fatalf("numMelFrames(1) = %d, want 1", got)
	}
	if got := numMelFrames(0); got != 0 {
		t.Fatalf("numMelFrames(0) = %d, want 0", got)
	}
}

func TestComputeMelShapeAndSilenceFloor(t *testing.T) {
	f := newMelFront()
	// 0.5 s of silence -> every bin is the log floor ln(2^-24), no normalization.
	audio := make([]float32, 8000)
	mel := f.computeMel(audio)
	if len(mel) != nMels {
		t.Fatalf("mel rows = %d, want %d", len(mel), nMels)
	}
	wantFrames := numMelFrames(len(audio))
	floor := float32(math.Log(logZeroGuard))
	for m := range mel {
		if len(mel[m]) != wantFrames {
			t.Fatalf("row %d frames = %d, want %d", m, len(mel[m]), wantFrames)
		}
		for t2, v := range mel[m] {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("non-finite mel at [%d,%d]", m, t2)
			}
			// Silence (after padding) must sit at the additive log floor — proof we
			// did NOT apply per-feature mean/var normalization (that would zero-center
			// it instead).
			if math.Abs(float64(v-floor)) > 1e-3 {
				t.Fatalf("silence mel[%d,%d] = %g, want floor %g (normalization must be OFF)", m, t2, v, floor)
			}
		}
	}
}

func TestComputeMelSineEnergyConcentrates(t *testing.T) {
	f := newMelFront()
	// 1 kHz tone -> energy should concentrate in the mel band covering 1 kHz, not
	// the very low or very high mel bins.
	audio := make([]float32, 16000)
	for i := range audio {
		audio[i] = float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/sampleRate))
	}
	mel := f.computeMel(audio)
	// Use an interior frame to avoid edge effects.
	frame := len(mel[0]) / 2
	peak, peakBin := float32(math.Inf(-1)), 0
	for m := range mel {
		if mel[m][frame] > peak {
			peak, peakBin = mel[m][frame], m
		}
	}
	// 1 kHz maps to roughly mel bin ~40 of 128 on this scale; assert it's in the
	// lower-middle, never the extremes.
	if peakBin < 20 || peakBin > 70 {
		t.Fatalf("1 kHz peak mel bin = %d, expected ~20..70", peakBin)
	}
}

func TestComputeMelTooShort(t *testing.T) {
	f := newMelFront()
	if mel := f.computeMel(nil); mel != nil {
		t.Fatalf("empty audio should yield nil mel, got %d rows", len(mel))
	}
}
