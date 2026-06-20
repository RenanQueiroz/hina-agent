package rtc

import (
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
)

func TestToneSourceFillsFullFrame(t *testing.T) {
	src := newToneSource()
	dst := make([]float32, audio.OutputFrameSamples)
	if n := src.Next(dst); n != len(dst) {
		t.Fatalf("tone Next returned %d, want %d", n, len(dst))
	}
	if src.Name() != ModeTone {
		t.Fatalf("name=%q", src.Name())
	}
	nonzero := false
	for _, v := range dst {
		if v != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Fatal("tone produced silence")
	}
}

func TestLoopbackSourceFIFOAndPadding(t *testing.T) {
	src := newLoopbackSource()
	src.Feed([]float32{1, 2, 3})

	dst := make([]float32, 5)
	n := src.Next(dst)
	if n != 3 {
		t.Fatalf("real samples=%d, want 3", n)
	}
	if dst[0] != 1 || dst[1] != 2 || dst[2] != 3 {
		t.Fatalf("FIFO order wrong: %v", dst[:3])
	}
	if dst[3] != 0 || dst[4] != 0 {
		t.Fatalf("tail not silence-padded: %v", dst[3:])
	}
	// Buffer is now drained.
	if n := src.Next(dst); n != 0 {
		t.Fatalf("expected empty after drain, got %d real samples", n)
	}
}

func TestLoopbackSourceDropsOldestOnOverflow(t *testing.T) {
	src := newLoopbackSource()
	// Feed 1.5x the cap; the oldest half should be dropped.
	over := loopbackMaxSamples + loopbackMaxSamples/2
	in := make([]float32, over)
	for i := range in {
		in[i] = float32(i)
	}
	src.Feed(in)

	src.mu.Lock()
	got := len(src.buf)
	first := src.buf[0]
	src.mu.Unlock()
	if got != loopbackMaxSamples {
		t.Fatalf("buffered %d, want capped at %d", got, loopbackMaxSamples)
	}
	// The retained front should be the newest window (oldest dropped).
	wantFirst := float32(over - loopbackMaxSamples)
	if first != wantFirst {
		t.Fatalf("after overflow front=%v, want %v (oldest dropped)", first, wantFirst)
	}
}
