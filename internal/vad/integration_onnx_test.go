//go:build onnx

package vad

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// TestSileroRealVAD is the committed end-to-end validation against the REAL Silero
// graph — it catches tensor-name/shape/dtype mismatches (notably the rank-0 `sr`
// scalar and the [2,1,128] LSTM state threading) that the fake-Model unit tests
// cannot. It runs when HINA_ASR_TEST_ASSETS points at an installed asset root (the
// shared layout `hina assets pull` produces: <root>/ort + <root>/vad/...) and skips
// otherwise. CI installs the pinned asset on Linux and sets the env var.
//
// It asserts the model RUNS and discriminates: pure silence yields no speech
// onset, while the real-speech fixture (followed by trailing silence) produces a
// detected segment (Start … End). False-start / interruption rate numbers come
// from the Phase 6 benchmark harness with recorded fixtures.
func TestSileroRealVAD(t *testing.T) {
	root := os.Getenv("HINA_ASR_TEST_ASSETS")
	if root == "" {
		t.Skip("HINA_ASR_TEST_ASSETS not set; skipping real Silero VAD (run `hina assets pull`)")
	}
	b, err := onnx.New(onnx.Config{LibDir: filepath.Join(root, "ort"), IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("onnx backend: %v", err)
	}
	defer b.Close()
	if !b.Info().Available {
		t.Fatalf("ONNX runtime unavailable: %s", b.Info().Reason)
	}
	e := NewEngine(Config{
		Backend: b,
		ReadModel: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("vad", "silero_vad.onnx"))
		},
	})
	if !e.Available() {
		t.Fatalf("engine unavailable: %s", e.Status().Reason)
	}
	defer e.Close()

	run := func(audio []float32) (starts, ends int) {
		s, err := e.NewStream(context.Background(), Params{})
		if err != nil {
			t.Fatalf("new stream: %v", err)
		}
		defer s.Close()
		for i := 0; i < len(audio); i += 320 { // 20 ms frames, the rtc mic cadence
			end := i + 320
			if end > len(audio) {
				end = len(audio)
			}
			evs, err := s.Write(audio[i:end])
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			for _, ev := range evs {
				switch ev.Kind {
				case EvStart:
					starts++
				case EvEnd, EvMax:
					ends++
				}
			}
		}
		return starts, ends
	}

	// 1.5 s of pure silence must not trigger a speech onset.
	silence := make([]float32, int(1.5*SampleRate))
	if starts, _ := run(silence); starts != 0 {
		t.Fatalf("silence produced %d speech onsets, want 0", starts)
	}

	// Real speech + 1 s trailing silence must produce a detected segment.
	speech := readWav16(t, filepath.Join("testdata", "speech.wav"))
	speech = append(speech, make([]float32, SampleRate)...) // trailing silence to end the turn
	starts, ends := run(speech)
	if starts == 0 {
		t.Fatal("real speech produced no speech onset — model/IO contract broken")
	}
	if ends == 0 {
		t.Fatal("real speech + trailing silence produced no turn end")
	}
	t.Logf("real Silero VAD: starts=%d ends=%d cold=%dms probes=%d",
		starts, ends, e.Status().ColdLoadMs, e.Status().ProbeCount)
}

func readWav16(t *testing.T, path string) []float32 {
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
