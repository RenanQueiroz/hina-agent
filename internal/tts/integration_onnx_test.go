//go:build onnx

package tts

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// TestSupertonicRealSynthesis is the committed end-to-end validation of the
// Supertonic pipeline against the REAL model graphs — it catches tensor-name,
// shape, dtype, or step-schedule mismatches that the fake-session unit tests
// cannot. It runs when HINA_TTS_TEST_ASSETS points at an installed asset root
// (layout produced by `hina assets pull`: <root>/ort + <root>/supertonic/...) and
// skips otherwise, so the onnx-tagged build still passes on a host without the
// ~400 MB models. CI installs the pinned assets and sets the env var, so this
// actually exercises the real pipeline there.
func TestSupertonicRealSynthesis(t *testing.T) {
	root := os.Getenv("HINA_TTS_TEST_ASSETS")
	if root == "" {
		t.Skip("HINA_TTS_TEST_ASSETS not set; skipping real Supertonic synthesis (run `hina assets pull` and point it at the asset root)")
	}
	b, err := onnx.New(onnx.Config{LibDir: filepath.Join(root, "ort"), IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("onnx backend: %v", err)
	}
	defer b.Close()
	if !b.Info().Available {
		t.Fatalf("ONNX runtime unavailable: %s", b.Info().Reason)
	}
	// Load graphs/config + voices from CHECKSUM-VERIFIED bytes via onnx.OpenBytes —
	// the production path (cmd/hina wires these the same way). This exercises the
	// WithONNXData load of the real graphs against ORT, not just path opens.
	s := NewSynthesizer(Config{
		Backend:  b,
		OnnxDir:  filepath.Join(root, "supertonic", "onnx"),
		VoiceDir: filepath.Join(root, "supertonic", "voice_styles"),
		ReadAsset: func(file string) ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("supertonic", "onnx", file))
		},
		ReadVoiceAsset: func(id string) ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("supertonic", "voice_styles", id+".json"))
		},
	})
	if !s.Available() {
		t.Fatalf("engine unavailable: %s", s.Status().Reason)
	}

	// Two sentences -> streamed sentence-by-sentence; a default-voice synthesis.
	stream, err := s.Synthesize(context.Background(), "Phase four synthesizes real speech. Streaming works.", Options{Voice: "M1", Lang: "en"})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	total, nseg := 0, 0
	for seg := range stream.Segments {
		nseg++
		if seg.SampleRate != NativeSampleRate {
			t.Fatalf("segment sample rate = %d, want %d", seg.SampleRate, NativeSampleRate)
		}
		for _, v := range seg.PCM {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("non-finite sample in segment %d", seg.Index)
			}
		}
		total += len(seg.PCM)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if nseg < 2 {
		t.Fatalf("got %d segments, want >= 2 (one per sentence)", nseg)
	}
	if total < stream.SampleRate()/2 {
		t.Fatalf("implausibly short audio: %d samples (%.3fs)", total, float64(total)/float64(stream.SampleRate()))
	}
	t.Logf("real synthesis: %d segments, %.2fs at %d Hz, cold load %d ms",
		nseg, float64(total)/float64(stream.SampleRate()), stream.SampleRate(), s.Status().ColdLoadMillis)

	// A second preset voice validates voice-vector loading beyond the default.
	stream2, err := s.Synthesize(context.Background(), "A different voice.", Options{Voice: "F3"})
	if err != nil {
		t.Fatalf("synthesize F3: %v", err)
	}
	for range stream2.Segments {
	}
	if err := stream2.Err(); err != nil {
		t.Fatalf("stream F3: %v", err)
	}
}
