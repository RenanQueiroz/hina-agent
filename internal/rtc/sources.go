package rtc

import (
	"sync"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
)

// Source produces successive 24 kHz mono float32 frames for the outbound pacer.
// Next fills dst with the next frame and returns the count of REAL (non-padded)
// samples; any remainder of dst is zero-filled silence so the pacer can keep a
// steady cadence even when a streaming source momentarily runs dry. Feed lets a
// streaming source accept decoded mic PCM (no-op for generators). Implementations
// must be safe for concurrent Next/Feed (the pacer and the inbound decoder run
// on different goroutines).
type Source interface {
	Next(dst []float32) int
	Feed(samples []float32)
	Name() string
}

// toneSource is an endless sine generator; it always fills the whole frame.
type toneSource struct {
	gen *audio.ToneGenerator
}

func newToneSource() *toneSource {
	return &toneSource{gen: audio.NewToneGenerator(audio.OutputSampleRate, toneFrequencyHz, toneAmplitude)}
}

func (t *toneSource) Next(dst []float32) int {
	t.gen.Fill(dst)
	return len(dst)
}

func (t *toneSource) Feed([]float32) {}

func (t *toneSource) Name() string { return ModeTone }

// loopbackSource is a bounded FIFO of decoded mic PCM (already resampled to
// 24 kHz). The inbound decoder Feeds it; the pacer drains it. The buffer is
// capped at loopbackMaxSamples so a fast producer / paused consumer can't grow
// memory without bound — excess oldest audio is dropped, which keeps echo
// latency bounded (a jitter-buffer overflow, not a leak).
type loopbackSource struct {
	mu  sync.Mutex
	buf []float32
}

// loopbackMaxSamples caps buffered loopback audio at 1 second (24 kHz mono).
const loopbackMaxSamples = audio.OutputSampleRate

func newLoopbackSource() *loopbackSource { return &loopbackSource{} }

func (l *loopbackSource) Feed(samples []float32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, samples...)
	if len(l.buf) > loopbackMaxSamples {
		drop := len(l.buf) - loopbackMaxSamples
		rem := copy(l.buf, l.buf[drop:]) // compact, dropping oldest
		l.buf = l.buf[:rem]
	}
}

func (l *loopbackSource) Next(dst []float32) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := copy(dst, l.buf)
	rem := copy(l.buf, l.buf[n:]) // compact remaining to the front
	l.buf = l.buf[:rem]
	for i := n; i < len(dst); i++ {
		dst[i] = 0 // pad the rest with silence
	}
	return n
}

func (l *loopbackSource) Name() string { return ModeLoopback }
