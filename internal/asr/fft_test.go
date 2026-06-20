package asr

import (
	"math"
	"testing"
)

// naivePower is an O(n^2) reference DFT power spectrum to validate the radix-2
// FFT against. Only used in tests.
func naivePower(in []float64) []float64 {
	n := len(in)
	out := make([]float64, n/2+1)
	for k := 0; k <= n/2; k++ {
		var re, im float64
		for t := 0; t < n; t++ {
			ang := -2 * math.Pi * float64(k) * float64(t) / float64(n)
			re += in[t] * math.Cos(ang)
			im += in[t] * math.Sin(ang)
		}
		out[k] = re*re + im*im
	}
	return out
}

func TestFFTMatchesNaiveDFT(t *testing.T) {
	n := 512
	p := newFFTPlan(n)
	in := make([]float64, n)
	// A deterministic pseudo-random-ish signal (no rand: keep the test stable).
	for i := range in {
		in[i] = math.Sin(0.3*float64(i)) + 0.5*math.Cos(0.017*float64(i*i%97))
	}
	got := make([]float64, n/2+1)
	re := make([]float64, n)
	im := make([]float64, n)
	p.powerSpectrum(in, got, re, im)
	want := naivePower(in)
	for k := range want {
		// Power can be large; compare with a relative+absolute tolerance.
		diff := math.Abs(got[k] - want[k])
		tol := 1e-6 * (1 + math.Abs(want[k]))
		if diff > tol {
			t.Fatalf("bin %d: got %.6g want %.6g (diff %.3g)", k, got[k], want[k], diff)
		}
	}
}

func TestFFTSineConcentratesAtBin(t *testing.T) {
	// A 1 kHz sine at 16 kHz lands in bin 1000*512/16000 = 32.
	n := 512
	p := newFFTPlan(n)
	in := make([]float64, n)
	for i := range in {
		in[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / 16000)
	}
	out := make([]float64, n/2+1)
	re := make([]float64, n)
	im := make([]float64, n)
	p.powerSpectrum(in, out, re, im)
	maxBin, maxVal := 0, 0.0
	for k, v := range out {
		if v > maxVal {
			maxVal, maxBin = v, k
		}
	}
	if maxBin != 32 {
		t.Fatalf("peak bin = %d, want 32", maxBin)
	}
}

func TestFFTPanicsOnNonPowerOfTwo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-power-of-two size")
		}
	}()
	newFFTPlan(500)
}
