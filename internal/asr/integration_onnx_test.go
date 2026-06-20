//go:build onnx

package asr

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// TestNemotronRealRecognition is the committed end-to-end validation against the
// REAL Nemotron graphs — it catches tensor-name, shape, dtype, and cache-threading
// mismatches the fake-session unit tests cannot. It runs when HINA_ASR_TEST_ASSETS
// points at an installed asset root (the layout `hina assets pull` produces:
// <root>/ort + <root>/nemotron/...) and skips otherwise, so the onnx-tagged build
// still passes on a host without the ~680 MB models. CI installs the pinned
// assets on Linux and sets the env var, exercising the real pipeline there.
//
// It feeds a synthetic tone, not speech, so it asserts the pipeline RUNS (encoder
// + RNNT decode produce a finite, well-formed result through the real graphs) and
// the cache resets cleanly across turns — not word accuracy. Real-speech WER and
// the name-biasing substitution-rate drop are validated by the Phase 6 harness
// against recorded fixtures.
func TestNemotronRealRecognition(t *testing.T) {
	root := os.Getenv("HINA_ASR_TEST_ASSETS")
	if root == "" {
		t.Skip("HINA_ASR_TEST_ASSETS not set; skipping real Nemotron recognition (run `hina assets pull` and point it at the asset root)")
	}
	b, err := onnx.New(onnx.Config{LibDir: filepath.Join(root, "ort"), IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("onnx backend: %v", err)
	}
	defer b.Close()
	if !b.Info().Available {
		t.Fatalf("ONNX runtime unavailable: %s", b.Info().Reason)
	}
	n := NewRecognizer(Config{
		Backend:     b,
		ModelDir:    assets.ASRDir(root),
		EncoderPath: assets.ASREncoderPath(root), // loaded by path (external .data)
		ReadDecoder: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "decoder_joint.onnx"))
		},
		ReadTokenizer: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "tokenizer.model"))
		},
		Agent: AgentBias{Name: "Hina", Aliases: []string{"Nina"}},
	})
	if !n.Available() {
		t.Fatalf("engine unavailable: %s", n.Status().Reason)
	}
	defer n.Close()
	if !n.Status().Biasing {
		t.Fatal("expected name biasing to be enabled for 'Hina'")
	}

	transcribe := func() Final {
		t.Helper()
		s, err := n.NewStream(context.Background(), Options{Language: "en"}, func(Partial) {})
		if err != nil {
			t.Fatalf("new stream: %v", err)
		}
		defer s.Close()
		// ~2 s of a faint 220 Hz tone in 20 ms frames (320 samples @16 kHz), the
		// cadence the rtc mic pipeline delivers.
		frame := make([]float32, 320)
		phase := 0.0
		for i := 0; i < 100; i++ {
			for j := range frame {
				frame[j] = float32(0.05 * math.Sin(phase))
				phase += 2 * math.Pi * 220 / float64(sampleRate)
			}
			if err := s.Write(frame); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		final, err := s.Finalize()
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		return final
	}

	first := transcribe()
	for _, r := range first.Text {
		if r == 0xFFFD {
			t.Fatalf("transcript has replacement chars (bad detokenize): %q", first.Text)
		}
	}
	if n.Status().ChunkCount == 0 {
		t.Fatal("no encoder chunks decoded — the streaming loop never ran")
	}
	t.Logf("real ASR: text=%q wake=%v body=%q cold=%dms chunks=%d",
		first.Text, first.WakeDetected, first.Body, n.Status().ColdLoadMs, n.Status().ChunkCount)

	// A second turn on the same engine must not carry state from the first
	// (per-stream cache + decoder reset across turns).
	_ = transcribe()
}

// TestNemotronRealSpeechTranscript guards the highest-risk paths against the REAL
// model with REAL speech: a small committed 16 kHz fixture ("please turn on the
// light") is decoded and the transcript is checked for stable, high-confidence
// words. A numeric log-mel/normalization bug, a tokenizer/detokenizer break, or a
// decode/cache regression would yield an empty or garbage transcript and fail
// this — failures a tone-only test cannot catch. The synthetic voice is robotic,
// so this asserts content (not exact WER); the word-accuracy/biasing benchmark is
// the Phase 6 harness with recorded speech.
func TestNemotronRealSpeechTranscript(t *testing.T) {
	root := os.Getenv("HINA_ASR_TEST_ASSETS")
	if root == "" {
		t.Skip("HINA_ASR_TEST_ASSETS not set; skipping real Nemotron speech transcript")
	}
	audio := readTestWav16(t, filepath.Join("testdata", "speech.wav"))

	b, err := onnx.New(onnx.Config{LibDir: filepath.Join(root, "ort"), IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("onnx backend: %v", err)
	}
	defer b.Close()
	n := NewRecognizer(Config{
		Backend: b, ModelDir: assets.ASRDir(root), EncoderPath: assets.ASREncoderPath(root),
		ReadDecoder: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "decoder_joint.onnx"))
		},
		ReadTokenizer: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "tokenizer.model"))
		},
	})
	if !n.Available() {
		t.Fatalf("engine unavailable: %s", n.Status().Reason)
	}
	defer n.Close()

	s, err := n.NewStream(context.Background(), Options{Language: "en"}, nil)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	defer s.Close()
	for i := 0; i < len(audio); i += 320 { // 20 ms frames, like the rtc mic pipeline
		end := i + 320
		if end > len(audio) {
			end = len(audio)
		}
		if err := s.Write(audio[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	final, err := s.Finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	t.Logf("real speech transcript: %q", final.Text)

	got := strings.ToLower(final.Text)
	// Stable, high-confidence words the model recognizes for this fixture. A broken
	// front-end/decoder produces empty/garbage and misses these.
	for _, want := range []string{"please", "turn", "light"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript %q missing the expected word %q (front-end/decoder regression?)", final.Text, want)
		}
	}
}

// readTestWav16 loads a canonical 16-bit PCM WAV (44-byte header) as float32 in
// [-1,1]. Test-only; the fixture is known-good mono 16 kHz.
func readTestWav16(t *testing.T, path string) []float32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) <= 44 {
		t.Fatalf("%s too small to be a WAV", path)
	}
	pcm := b[44:]
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / 32768
	}
	return out
}
