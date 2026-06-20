package rtc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
)

// newDSPSession builds a Session with no peer connection, sufficient to unit-test
// the inbound DSP and outbound buffering in isolation. start is backdated so the
// throttled meta emits on the first call.
func newDSPSession() (*Session, *fakeSink) {
	sink := &fakeSink{}
	s := &Session{
		id:             "rtc_test",
		userID:         "u",
		conversationID: "c",
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		sink:           sink,
		start:          time.Now().Add(-time.Second),
		metrics:        newMetrics(),
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.in = newInbound(s)
	s.out = newOutbound(s)
	return s, sink
}

func TestProcessPCM48DownsamplesAndFeedsLoopback(t *testing.T) {
	s, sink := newDSPSession()
	s.out.source = newLoopbackSource() // loopback active so feedLoopback lands

	// One second of 48 kHz mono audio, in 20 ms chunks like real RTP.
	in := make([]float32, audio.InputSampleRate)
	audio.NewToneGenerator(audio.InputSampleRate, 440, 0.5).Fill(in)
	const chunk = audio.InputSampleRate / 1000 * 20 // 960 samples = 20 ms
	for i := 0; i < len(in); i += chunk {
		end := i + chunk
		if end > len(in) {
			end = len(in)
		}
		s.in.processPCM48(in[i:end])
	}

	// ~16 k samples should have flowed to the ASR meter (capture cursor).
	if d := abs64(s.in.captureSamples - audio.ASRSampleRate); d > audio.ASRSampleRate/50 {
		t.Fatalf("captureSamples=%d, want ~%d", s.in.captureSamples, audio.ASRSampleRate)
	}

	// ~24 k samples should be buffered in the loopback source.
	lb := s.out.source.(*loopbackSource)
	lb.mu.Lock()
	got := len(lb.buf)
	lb.mu.Unlock()
	// The bounded buffer caps at 1 s (24 k); 1 s of input fills it near the cap.
	if got < loopbackMaxSamples*9/10 {
		t.Fatalf("loopback buffered %d samples, want close to %d", got, loopbackMaxSamples)
	}

	// At least one AudioInputFrame meta event should have been emitted.
	if !contains(sink.types(), events.TypeAudioInputFrame) {
		t.Fatalf("no AudioInputFrame meta emitted; got %v", sink.types())
	}
	if s.metrics.snapshot().CaptureMs == 0 {
		t.Fatal("capture cursor (ms) not recorded in metrics")
	}
}

func TestProcessPCM48WithoutSourceDoesNotPanic(t *testing.T) {
	s, _ := newDSPSession() // no outbound source set
	in := make([]float32, 960)
	audio.NewToneGenerator(audio.InputSampleRate, 440, 0.5).Fill(in)
	s.in.processPCM48(in) // feedLoopback no-ops when source is nil
	if s.in.captureSamples == 0 {
		t.Fatal("expected capture samples even with no loopback source")
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
