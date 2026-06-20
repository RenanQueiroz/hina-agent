package rtc

import (
	"sync"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
)

// Source produces successive 24 kHz mono float32 frames for the outbound pacer.
// Next fills dst with the next frame and returns the count of REAL (non-padded)
// samples; any remainder of dst is zero-filled silence so the pacer can keep a
// steady cadence even when a streaming source momentarily runs dry.
//
// Feeding is NOT part of this interface on purpose: each streaming source has its
// own producer (the loopback source is fed decoded mic PCM by the inbound decoder;
// the TTS source is fed synthesized PCM by runSpeak), and those inputs must never
// cross. Next must be safe to call concurrently with that source's own Feed.
type Source interface {
	Next(dst []float32) int
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

// ttsSource is a streaming FIFO fed by the TTS synthesizer (already resampled to
// 24 kHz). Unlike the loopback source it has a definite end: once End is called
// and the buffer drains, Done is closed so the session can stop the pacer and
// emit TTSCompleted. The buffer holds the whole utterance (synthesis outruns
// realtime playback); the reply is length-capped upstream so it can't grow
// without bound. Draining uses a read head index and compacts only occasionally
// (not on every 20 ms Next), so a long reply drains in amortized O(n) rather than
// O(n²). Safe for concurrent Feed (synth goroutine) / Next (pacer).
type ttsSource struct {
	mu         sync.Mutex
	buf        []float32
	head       int // read offset into buf (consumed prefix is buf[:head])
	ended      bool
	done       chan struct{}
	doneClosed bool
}

// ttsCompactThreshold is the consumed-prefix size past which Next drops the prefix
// (only when it is at least half the buffer), bounding amortized copy work.
const ttsCompactThreshold = 4096

func newTTSSource() *ttsSource { return &ttsSource{done: make(chan struct{})} }

func (t *ttsSource) Feed(samples []float32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return // no audio accepted after the utterance is closed
	}
	t.buf = append(t.buf, samples...)
}

func (t *ttsSource) Next(dst []float32) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := copy(dst, t.buf[t.head:])
	t.head += n
	for i := n; i < len(dst); i++ {
		dst[i] = 0 // pad with silence
	}
	// Reclaim the consumed prefix only occasionally (when it's large and at least
	// half the buffer), so steady draining doesn't memmove the whole tail each tick.
	if t.head >= ttsCompactThreshold && t.head*2 >= len(t.buf) {
		rem := copy(t.buf, t.buf[t.head:])
		t.buf = t.buf[:rem]
		t.head = 0
	}
	if t.ended && t.head >= len(t.buf) {
		t.closeDoneLocked()
	}
	return n
}

// End marks the utterance complete; once the buffer drains, Done fires. If the
// buffer is already empty, Done fires immediately.
func (t *ttsSource) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ended = true
	if t.head >= len(t.buf) {
		t.closeDoneLocked()
	}
}

func (t *ttsSource) closeDoneLocked() {
	if !t.doneClosed {
		t.doneClosed = true
		close(t.done)
	}
}

// Done is closed once the utterance has ended and all buffered audio has been
// drained by the pacer.
func (t *ttsSource) Done() <-chan struct{} { return t.done }

func (t *ttsSource) Name() string { return ModeTTS }
