package audio

import (
	"fmt"

	resampler "github.com/tphakala/go-audio-resampler"
)

// Resampler is a streaming mono float32 sample-rate converter. It is stateful
// (the polyphase filter carries history across calls), so it is NOT safe for
// concurrent use and each pipeline owns its own instance. It is backed by
// tphakala/go-audio-resampler — a pure-Go libsoxr port — at QualityLow, the
// low-latency speech preset chosen in research-findings B5 for the 48 kHz
// downsample. Pure Go keeps the voice path CGo-free and Windows-buildable.
type Resampler struct {
	eng     *resampler.SimpleResamplerFloat32
	inRate  int
	outRate int
}

// NewResampler builds a streaming resampler from inRate to outRate (Hz, mono).
func NewResampler(inRate, outRate int) (*Resampler, error) {
	eng, err := resampler.NewEngineFloat32(float64(inRate), float64(outRate), resampler.QualityLow)
	if err != nil {
		return nil, fmt.Errorf("audio: new resampler %d->%d Hz: %w", inRate, outRate, err)
	}
	return &Resampler{eng: eng, inRate: inRate, outRate: outRate}, nil
}

// Process resamples one chunk and returns the freshly-allocated output samples
// (owned by the caller — safe to retain). Because the filter must fill before it
// emits, the per-call output length varies near the start of a stream and only
// converges to len(in)*outRate/inRate over many calls; callers must not assume a
// fixed ratio per call. A passthrough (inRate==outRate) still routes through the
// engine so behavior and latency stay uniform.
func (r *Resampler) Process(in []float32) ([]float32, error) {
	out, err := r.eng.Process(in)
	if err != nil {
		return nil, fmt.Errorf("audio: resample %d->%d Hz: %w", r.inRate, r.outRate, err)
	}
	return out, nil
}
