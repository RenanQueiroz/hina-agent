package asr

import "math"

// Log-mel front-end for the Nemotron streaming encoder, ported from NeMo's
// FilterbankFeatures spec (research-findings B3) and the parakeet-rs reference
// (src/audio.rs + nemotron.rs compute_mel_spectrogram). Faithful reproduction of
// this DSP is the phase's #1 risk: a small numeric mismatch silently raises WER
// far more than a decode bug, so every constant below is pinned to the spec and
// the validated reference.
//
// Pipeline (matching the reference exactly):
//  1. pre-emphasis y[i] = x[i] - 0.97·x[i-1] over the signal,
//  2. STFT: center zero-pad n_fft/2, a symmetric (periodic=False) Hann window of
//     win_length, power spectrum |X|^2 over the n_fft/2+1 bins,
//  3. project through a Slaney mel filterbank (n_mels triangles, Slaney-normed),
//  4. natural log with an additive guard ln(x + 2^-24),
//  5. NO per-feature normalization — this export feeds raw log-mel to the encoder
//     (config.json preprocessor.normalize == "NA"; reference comment "We dont
//     apply mel normalization unlike others").
const (
	sampleRate  = 16000
	nFFT        = 512
	winLength   = 400 // 25 ms
	hopLength   = 160 // 10 ms
	nMels       = 128
	preemphCoef = 0.97
	freqBins    = nFFT/2 + 1 // 257
	stftPad     = nFFT / 2   // center padding each side
)

// logZeroGuard is NeMo's log_zero_guard_type="add" value 2^-24, applied before
// the natural log so silence maps to a finite floor instead of -Inf.
var logZeroGuard = math.Exp2(-24)

// melFront is the immutable, reusable filterbank + window + FFT plan for the
// fixed preprocessor config. Built once per loaded model and shared read-only
// across per-stream recognizers; computeMel takes its scratch from the caller.
type melFront struct {
	plan    *fftPlan
	window  []float64   // symmetric Hann, length winLength
	melBank [][]float64 // [nMels][freqBins] Slaney triangles
}

func newMelFront() *melFront {
	return &melFront{
		plan:    newFFTPlan(nFFT),
		window:  hannWindow(winLength),
		melBank: melFilterbank(nFFT, nMels, sampleRate),
	}
}

// hannWindow is a symmetric Hann window (periodic=False / denominator N-1),
// matching NeMo's torch.hann_window(win_length, periodic=False) and the
// reference. A single-sample window is all-ones (avoids a divide-by-zero).
func hannWindow(n int) []float64 {
	w := make([]float64, n)
	if n == 1 {
		w[0] = 1
		return w
	}
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	return w
}

// Slaney mel scale (librosa default), reproduced from the reference.
const (
	melFSp       = 200.0 / 3.0
	melMinLogHz  = 1000.0
	melMinLogMel = melMinLogHz / melFSp
	melLogStep   = 0.06875177742094912
)

func hzToMel(hz float64) float64 {
	if hz < melMinLogHz {
		return hz / melFSp
	}
	return melMinLogMel + math.Log(hz/melMinLogHz)/melLogStep
}

func melToHz(mel float64) float64 {
	if mel < melMinLogMel {
		return mel * melFSp
	}
	return melMinLogHz * math.Exp((mel-melMinLogMel)*melLogStep)
}

// melFilterbank builds the [nMels][freqBins] Slaney-normalized triangular
// filterbank (librosa's mel_f / Slaney norm), matching create_mel_filterbank.
func melFilterbank(nfft, mels, sr int) [][]float64 {
	bins := nfft/2 + 1
	bank := make([][]float64, mels)
	for i := range bank {
		bank[i] = make([]float64, bins)
	}
	fmax := float64(sr) / 2.0
	melMin := hzToMel(0)
	melMax := hzToMel(fmax)

	melPoints := make([]float64, mels+2)
	for i := range melPoints {
		melPoints[i] = melToHz(melMin + (melMax-melMin)*float64(i)/float64(mels+1))
	}
	fftFreqs := make([]float64, bins)
	for i := range fftFreqs {
		fftFreqs[i] = float64(i) * float64(sr) / float64(nfft)
	}
	fdiff := make([]float64, mels+1)
	for i := range fdiff {
		fdiff[i] = melPoints[i+1] - melPoints[i]
	}
	for i := 0; i < mels; i++ {
		for k, freq := range fftFreqs {
			lower := (freq - melPoints[i]) / fdiff[i]
			upper := (melPoints[i+2] - freq) / fdiff[i+1]
			v := math.Min(lower, upper)
			if v < 0 {
				v = 0
			}
			bank[i][k] = v
		}
		// Slaney normalization: scale each filter so equal energy in Hz maps to
		// equal mel response (librosa norm="slaney").
		enorm := 2.0 / (melPoints[i+2] - melPoints[i])
		for k := range bank[i] {
			bank[i][k] *= enorm
		}
	}
	return bank
}

// preemphasize applies y[i] = x[i] - coef·x[i-1] into dst (len == len(src)).
// The first sample passes through (NeMo's preemphasis has no left history at the
// signal start). dst and src must not alias.
func preemphasize(dst, src []float64, coef float64) {
	if len(src) == 0 {
		return
	}
	dst[0] = src[0]
	for i := 1; i < len(src); i++ {
		dst[i] = src[i] - coef*src[i-1]
	}
}

// numMelFrames returns how many STFT/mel frames a signal of n samples yields
// after center padding: (n + 2·pad - n_fft)/hop + 1, clamped at 0.
func numMelFrames(n int) int {
	if n <= 0 {
		return 0 // no signal -> no frames (a padding-only frame carries nothing)
	}
	padded := n + 2*stftPad
	if padded < nFFT {
		return 0
	}
	return (padded-nFFT)/hopLength + 1
}

// computeMel computes the [nMels][frames] log-mel spectrogram of audio (16 kHz
// mono float32 in [-1,1]), WITHOUT normalization — exactly the reference's
// compute_mel_spectrogram. The result is laid out column-major-by-frame:
// out[m][t]. Returns nil for audio too short to yield a frame.
func (f *melFront) computeMel(audio []float32) [][]float32 {
	frames := numMelFrames(len(audio))
	if frames == 0 {
		return nil
	}
	// Match the reference order EXACTLY: pre-emphasize the raw signal first, THEN
	// center zero-pad by stftPad each side. (Padding first would apply the filter
	// across the signal→trailing-zero seam and perturb the final frame.) Frame 0
	// is centered on sample 0 (librosa center=True).
	raw := make([]float64, len(audio))
	for i, v := range audio {
		raw[i] = float64(v)
	}
	preRaw := make([]float64, len(audio))
	preemphasize(preRaw, raw, preemphCoef)
	pre := make([]float64, len(audio)+2*stftPad)
	copy(pre[stftPad:], preRaw)

	// Per-frame scratch reused across frames.
	frame := make([]float64, nFFT)
	power := make([]float64, freqBins)
	re := make([]float64, nFFT)
	im := make([]float64, nFFT)

	out := make([][]float32, nMels)
	for m := range out {
		out[m] = make([]float32, frames)
	}
	for t := 0; t < frames; t++ {
		start := t * hopLength
		// Window the first winLength samples into an n_fft buffer (rest zero).
		for i := 0; i < nFFT; i++ {
			if i < winLength {
				frame[i] = pre[start+i] * f.window[i]
			} else {
				frame[i] = 0
			}
		}
		f.plan.powerSpectrum(frame, power, re, im)
		// Mel projection + log guard.
		for m := 0; m < nMels; m++ {
			bank := f.melBank[m]
			var sum float64
			for k := 0; k < freqBins; k++ {
				sum += bank[k] * power[k]
			}
			out[m][t] = float32(math.Log(sum + logZeroGuard))
		}
	}
	return out
}
