package asr

import "math"

// fftPlan is a precomputed radix-2 Cooley–Tukey FFT for a fixed power-of-two
// size (n_fft=512 here). It is immutable after construction and so safe to share
// across the per-stream recognizers that all read the same model dims — the
// transform itself writes only into caller-provided scratch, never the plan.
//
// We roll our own rather than pull a dependency: the size is a fixed power of
// two, the control plane is CGo-free, and the only operation the front-end needs
// is a real-input forward FFT whose first n/2+1 power-spectrum bins feed the mel
// filterbank. Computation is in float64 for numerical headroom; the parakeet-rs
// reference uses a float32 realfft, and the difference is far below the int8
// model's quantization noise.
type fftPlan struct {
	n    int
	bits uint
	rev  []int     // bit-reversal permutation
	cos  []float64 // twiddle real parts, indexed by stage
	sin  []float64 // twiddle imag parts
}

// newFFTPlan builds a plan for transform size n, which must be a power of two.
func newFFTPlan(n int) *fftPlan {
	if n <= 0 || n&(n-1) != 0 {
		panic("asr: FFT size must be a positive power of two")
	}
	bits := uint(0)
	for (1 << bits) < n {
		bits++
	}
	rev := make([]int, n)
	for i := range rev {
		rev[i] = int(bitReverse(uint(i), bits))
	}
	// Twiddle factors for a decimation-in-time FFT: for each butterfly span the
	// roots of unity exp(-2πi·k/span). Precompute the full half-circle once.
	cos := make([]float64, n/2)
	sin := make([]float64, n/2)
	for k := 0; k < n/2; k++ {
		ang := -2 * math.Pi * float64(k) / float64(n)
		cos[k] = math.Cos(ang)
		sin[k] = math.Sin(ang)
	}
	return &fftPlan{n: n, bits: bits, rev: rev, cos: cos, sin: sin}
}

func bitReverse(x, bits uint) uint {
	var r uint
	for i := uint(0); i < bits; i++ {
		r = (r << 1) | (x & 1)
		x >>= 1
	}
	return r
}

// powerSpectrum computes |X[k]|^2 for k in [0, n/2] (the n/2+1 real-FFT bins)
// from a real input of length n. in is not modified; out must have length
// n/2+1. re/im are scratch buffers of length n owned by the caller (reused
// across frames to avoid per-frame allocation).
func (p *fftPlan) powerSpectrum(in []float64, out, re, im []float64) {
	n := p.n
	// Load input in bit-reversed order (decimation-in-time), zero imaginary part.
	for i := 0; i < n; i++ {
		re[i] = in[p.rev[i]]
		im[i] = 0
	}
	for span := 1; span < n; span <<= 1 {
		step := n / (span << 1) // twiddle index stride for this stage
		for start := 0; start < n; start += span << 1 {
			k := 0
			for j := start; j < start+span; j++ {
				wr := p.cos[k]
				wi := p.sin[k]
				l := j + span
				tr := wr*re[l] - wi*im[l]
				ti := wr*im[l] + wi*re[l]
				re[l] = re[j] - tr
				im[l] = im[j] - ti
				re[j] += tr
				im[j] += ti
				k += step
			}
		}
	}
	for k := 0; k <= n/2; k++ {
		out[k] = re[k]*re[k] + im[k]*im[k]
	}
}
