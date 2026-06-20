package tts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// --- fake ONNX backend/session that mimics the four Supertonic graphs ---

type recorder struct {
	mu          sync.Mutex
	order       []string  // model invoked, by primary output name
	steps       []float32 // current_step values seen by the vector estimator
	totalStep   float32   // total_step seen by the vector estimator
	noisyShape  []int64   // noisy_latent shape fed to the vector estimator
	latentShape []int64   // latent shape fed to the vocoder
}

func (r *recorder) add(name string) {
	r.mu.Lock()
	r.order = append(r.order, name)
	r.mu.Unlock()
}

type fakeBackend struct {
	rec      *recorder
	duration float32
	wavLen   int
}

func (b fakeBackend) Info() onnx.Info {
	return onnx.Info{Available: true, Version: "fake", Provider: "CPU"}
}
func (b fakeBackend) Close() error { return nil }
func (b fakeBackend) Open(_ string, in, out []string) (onnx.Session, error) {
	return &fakeSession{out: out, rec: b.rec, duration: b.duration, wavLen: b.wavLen}, nil
}
func (b fakeBackend) OpenBytes(_ []byte, in, out []string) (onnx.Session, error) {
	return b.Open("", in, out)
}

type fakeSession struct {
	out      []string
	rec      *recorder
	duration float32
	wavLen   int
	block    chan struct{} // if non-nil, Run blocks here until closed or ctx is cancelled
}

func (s *fakeSession) Close() error { return nil }

func (s *fakeSession) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.block != nil {
		// Emulate a slow in-flight ORT call that the real backend would Terminate.
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	name := s.out[0]
	s.rec.add(name)
	switch name {
	case "duration":
		return map[string]onnx.Tensor{"duration": onnx.NewFloat32([]int64{1}, []float32{s.duration})}, nil
	case "text_emb":
		l := in["text_ids"].Shape[1]
		return map[string]onnx.Tensor{"text_emb": onnx.NewFloat32([]int64{1, 4, l}, make([]float32, 4*l))}, nil
	case "denoised_latent":
		nl := in["noisy_latent"]
		s.rec.mu.Lock()
		s.rec.steps = append(s.rec.steps, in["current_step"].Float32[0])
		s.rec.totalStep = in["total_step"].Float32[0]
		s.rec.noisyShape = nl.Shape
		s.rec.mu.Unlock()
		return map[string]onnx.Tensor{"denoised_latent": onnx.NewFloat32(nl.Shape, make([]float32, len(nl.Float32)))}, nil
	case "wav_tts":
		s.rec.mu.Lock()
		s.rec.latentShape = in["latent"].Shape
		s.rec.mu.Unlock()
		return map[string]onnx.Tensor{"wav_tts": onnx.NewFloat32([]int64{1, int64(s.wavLen)}, make([]float32, s.wavLen))}, nil
	}
	return nil, errors.New("unexpected model: " + name)
}

func testParams() params {
	return params{SampleRate: 100, BaseChunkSize: 2, ChunkCompress: 1, LatentDimBase: 2}
}

func testVoice() *Voice {
	return &Voice{
		ID: "M1", StyleTTL: []float32{0.1, 0.2, 0.3, 0.4}, TTLDims: []int64{1, 2, 2},
		StyleDP: []float32{0.5, 0.6, 0.7, 0.8}, DPDims: []int64{1, 2, 2},
	}
}

// TestInferSentencePipeline asserts the four graphs are driven in the right order
// with the right tensor shapes and step schedule (the ODE lives in the graph; the
// Go loop only feeds 0-based current_step + constant total_step), and that the
// waveform is trimmed to the predicted duration.
func TestInferSentencePipeline(t *testing.T) {
	rec := &recorder{}
	b := fakeBackend{rec: rec, duration: 0.1, wavLen: 50}
	m := &models{
		dp:  mustOpen(t, b, dpOutputs),
		te:  mustOpen(t, b, teOutputs),
		ve:  mustOpen(t, b, veOutputs),
		voc: mustOpen(t, b, vocOutputs),
	}
	p := testParams() // sr=100, chunkSize=2, latentDim=2
	ids := []int64{1, 2, 3, 4, 5}
	var noiseN int
	noise := func(n int) []float32 { noiseN = n; return make([]float32, n) }

	pcm, _, err := inferSentence(context.Background(), m, p, testVoice(), ids, 3, 1.0, 0, noise)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}

	// duration 0.1s * 100 Hz = 10 samples; latentLen = ceil(10/2) = 5; latentDim 2.
	if noiseN != 2*5 {
		t.Fatalf("noise count = %d, want 10 (latentDim*latentLen)", noiseN)
	}
	if len(pcm) != 10 {
		t.Fatalf("pcm len = %d, want 10 (trimmed to duration)", len(pcm))
	}
	wantOrder := []string{"duration", "text_emb", "denoised_latent", "denoised_latent", "denoised_latent", "wav_tts"}
	if got := rec.order; !eqStr(got, wantOrder) {
		t.Fatalf("call order = %v, want %v", got, wantOrder)
	}
	if !eqF32(rec.steps, []float32{0, 1, 2}) {
		t.Fatalf("current_step schedule = %v, want [0 1 2]", rec.steps)
	}
	if rec.totalStep != 3 {
		t.Fatalf("total_step = %v, want 3", rec.totalStep)
	}
	if !eqI64(rec.noisyShape, []int64{1, 2, 5}) {
		t.Fatalf("noisy_latent shape = %v, want [1 2 5]", rec.noisyShape)
	}
	if !eqI64(rec.latentShape, []int64{1, 2, 5}) {
		t.Fatalf("vocoder latent shape = %v, want [1 2 5]", rec.latentShape)
	}
}

// A cancelled context stops the pipeline before any graph call, so a superseded
// or barge-in'd utterance can't keep running the full sentence.
func TestInferSentenceCancellation(t *testing.T) {
	rec := &recorder{}
	b := fakeBackend{rec: rec, duration: 0.1, wavLen: 50}
	m := &models{
		dp:  mustOpen(t, b, dpOutputs),
		te:  mustOpen(t, b, teOutputs),
		ve:  mustOpen(t, b, veOutputs),
		voc: mustOpen(t, b, vocOutputs),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := inferSentence(ctx, m, testParams(), testVoice(), []int64{1, 2, 3}, 8, 1.0, 0,
		func(n int) []float32 { return make([]float32, n) })
	if err == nil {
		t.Fatal("cancelled context should stop inference with an error")
	}
	if len(rec.order) != 0 {
		t.Fatalf("no graph should run under a pre-cancelled context, ran %v", rec.order)
	}
}

// A cancellation that lands WHILE a graph call is in flight (not just between
// calls) unwinds promptly, and the later graphs never run — mirroring the real
// backend Terminating an in-flight ORT call on barge-in.
func TestInferSentenceInFlightCancellation(t *testing.T) {
	rec := &recorder{}
	m := &models{
		dp:  &fakeSession{out: dpOutputs, rec: rec, duration: 0.1, wavLen: 50},
		te:  &fakeSession{out: teOutputs, rec: rec, duration: 0.1, wavLen: 50},
		ve:  &fakeSession{out: veOutputs, rec: rec, block: make(chan struct{})}, // blocks in-flight
		voc: &fakeSession{out: vocOutputs, rec: rec, wavLen: 50},
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := inferSentence(ctx, m, testParams(), testVoice(), []int64{1, 2, 3}, 8, 1.0, 0,
			func(n int) []float32 { return make([]float32, n) })
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond) // let dp+te run and ve block
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inferSentence did not unwind on in-flight cancellation")
	}
	rec.mu.Lock()
	order := append([]string(nil), rec.order...)
	rec.mu.Unlock()
	for _, n := range order {
		if n == "wav_tts" {
			t.Fatalf("vocoder ran despite mid-flight cancellation; order=%v", order)
		}
	}
}

// A graph whose verified-read fails (tampered/corrupt) is never opened: the bytes
// are read+verified together, so the cold load fails via the stream error rather
// than feeding unverified bytes to ORT. (Config/tokenizer read succeeds so the
// engine is available; the failure is the lazy graph load.)
func TestSynthesizerReverifiesBeforeColdLoad(t *testing.T) {
	rec := &recorder{}
	s := NewSynthesizer(Config{
		Backend: fakeBackend{rec: rec, duration: 0.1, wavLen: 50},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
		ReadAsset: func(file string) ([]byte, error) {
			if file == FileConfig || file == FileIndexer {
				return os.ReadFile(filepath.Join("testdata", "onnx", file))
			}
			return nil, errors.New("tampered model") // the graph files fail verification
		},
	})
	if !s.Available() {
		t.Fatalf("engine unavailable: %s", s.Status().Reason)
	}
	stream, err := s.Synthesize(context.Background(), "Hello.", Options{Speed: 1.0})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Segments {
	}
	if stream.Err() == nil || !strings.Contains(stream.Err().Error(), "tampered model") {
		t.Fatalf("expected a verification failure, got %v", stream.Err())
	}
	if len(rec.order) != 0 {
		t.Fatal("no graph should have been run after a failed verified-read")
	}
}

// A voice whose verified-read fails is rejected synchronously (never decoded).
func TestSynthesizerReverifiesVoice(t *testing.T) {
	s := NewSynthesizer(Config{
		Backend: fakeBackend{rec: &recorder{}, duration: 0.1, wavLen: 50},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
		ReadVoiceAsset: func(string) ([]byte, error) { return nil, errors.New("tampered voice") },
	})
	if _, err := s.Synthesize(context.Background(), "Hello.", Options{}); err == nil ||
		!strings.Contains(err.Error(), "tampered voice") {
		t.Fatalf("expected a voice verification failure, got %v", err)
	}
}

func TestVoiceAllowlist(t *testing.T) {
	rec := &recorder{}
	s := NewSynthesizer(Config{
		Backend: fakeBackend{rec: rec, duration: 0.1, wavLen: 50},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
	})
	// A traversal-style voice id is rejected before it can become a path.
	if _, err := s.voice("../../../etc/passwd"); err == nil {
		t.Fatal("traversal voice id must be rejected")
	}
	if _, err := s.voice("Z9"); err == nil {
		t.Fatal("unknown voice id must be rejected")
	}
	// A configured traversal default falls back to the preset default.
	s2 := NewSynthesizer(Config{
		Backend: fakeBackend{rec: rec, duration: 0.1, wavLen: 50},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
		Defaults: Options{Voice: "../../secret"},
	})
	if got := s2.Status().Voice; got != defaultVoice {
		t.Fatalf("invalid default voice = %q, want fallback %q", got, defaultVoice)
	}
}

func TestSynthesizerStreaming(t *testing.T) {
	rec := &recorder{}
	s := NewSynthesizer(Config{
		Backend:  fakeBackend{rec: rec, duration: 0.1, wavLen: 50},
		OnnxDir:  "testdata/onnx",
		VoiceDir: "testdata/voice_styles",
	})
	if !s.Available() {
		t.Fatalf("engine unavailable: %s", s.Status().Reason)
	}
	// Speed 1.0 keeps the trimmed length deterministic (0.1s * 100 Hz = 10).
	stream, err := s.Synthesize(context.Background(), "Hello world. Bye now.", Options{Speed: 1.0})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if stream.SampleRate() != 100 {
		t.Fatalf("sample rate = %d, want 100", stream.SampleRate())
	}
	var segs []Segment
	for seg := range stream.Segments {
		segs = append(segs, seg)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2 (one per sentence)", len(segs))
	}
	for i, seg := range segs {
		if seg.Index != i || len(seg.PCM) != 10 || seg.SampleRate != 100 {
			t.Fatalf("segment %d = %+v", i, seg)
		}
	}
	st := s.Status()
	if st.SynthCount != 2 || !st.Loaded {
		t.Fatalf("status after synth = %+v", st)
	}
	// A fully-rendered reply is not truncated.
	if stream.Truncated() {
		t.Fatal("a complete reply must not be marked truncated")
	}
}

// loadCountBackend counts Open calls (and uses no-op sessions) to test loadModels.
type loadCountBackend struct{ opens int }

func (b *loadCountBackend) Info() onnx.Info { return onnx.Info{Available: true} }
func (b *loadCountBackend) Close() error    { return nil }
func (b *loadCountBackend) Open(string, []string, []string) (onnx.Session, error) {
	b.opens++
	return noopSession{}, nil
}
func (b *loadCountBackend) OpenBytes([]byte, []string, []string) (onnx.Session, error) {
	b.opens++
	return noopSession{}, nil
}

type noopSession struct{}

func (noopSession) Run(context.Context, map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	return nil, nil
}
func (noopSession) Close() error { return nil }

// A cold model load honors cancellation: a request cancelled before the load
// opens no graphs (and a normal load opens all four).
func TestLoadModelsCancellation(t *testing.T) {
	b := &loadCountBackend{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadModels(ctx, b, "dir", nil); err == nil {
		t.Fatal("expected cancellation error")
	}
	if b.opens != 0 {
		t.Fatalf("opened %d graphs under a pre-cancelled context, want 0", b.opens)
	}

	b2 := &loadCountBackend{}
	if _, err := loadModels(context.Background(), b2, "dir", nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	if b2.opens != 4 {
		t.Fatalf("opened %d graphs, want 4", b2.opens)
	}
}

// unavailBackend is an explicitly-unavailable runtime (independent of the build
// tag / ambient ORT library), so the unavailable-engine test is deterministic in
// both the default and onnx builds.
type unavailBackend struct{}

func (unavailBackend) Info() onnx.Info {
	return onnx.Info{Available: false, Reason: "test: runtime off"}
}
func (unavailBackend) Close() error { return nil }
func (unavailBackend) Open(string, []string, []string) (onnx.Session, error) {
	return nil, onnx.ErrUnavailable
}
func (unavailBackend) OpenBytes([]byte, []string, []string) (onnx.Session, error) {
	return nil, onnx.ErrUnavailable
}

// A short-but-many-sentences payload (under the byte cap) is rejected
// synchronously rather than spawning thousands of pipeline runs.
func TestSynthesizeTooManySegments(t *testing.T) {
	s := NewSynthesizer(Config{
		Backend: fakeBackend{rec: &recorder{}, duration: 0.1, wavLen: 50},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
	})
	text := strings.Repeat("a. ", maxSegments+10)
	if _, err := s.Synthesize(context.Background(), text, Options{Speed: 1.0}); !errors.Is(err, ErrTooManySegments) {
		t.Fatalf("err = %v, want ErrTooManySegments", err)
	}
}

// The per-request audio budget is strict: even with several long sentences the
// total emitted samples never exceed maxTotalOutputSeconds (no full-sentence
// overshoot past the cap).
func TestSynthesizerTotalOutputCap(t *testing.T) {
	// Each fake sentence predicts a very long duration and the fake vocoder returns
	// plenty of samples, so the per-sentence + per-request caps are what bound it.
	s := NewSynthesizer(Config{
		Backend: fakeBackend{rec: &recorder{}, duration: 1000, wavLen: 1_000_000},
		OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles",
	})
	stream, err := s.Synthesize(context.Background(), "one. two. three. four. five.", Options{Speed: 1.0})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for seg := range stream.Segments {
		total += len(seg.PCM)
	}
	maxTotal := maxTotalOutputSeconds * 100 // fixture SampleRate = 100
	if total > maxTotal {
		t.Fatalf("total %d exceeds the per-request cap %d", total, maxTotal)
	}
	if total != maxTotal {
		t.Fatalf("expected the cap reached exactly, total=%d cap=%d", total, maxTotal)
	}
	// A cap-shortened reply is flagged truncated so the client knows the spoken
	// text was incomplete (even though no error is set — the audio is valid).
	if !stream.Truncated() {
		t.Fatal("a cap-shortened reply must be marked truncated")
	}
	if stream.Err() != nil {
		t.Fatalf("truncation is not an error, got %v", stream.Err())
	}
}

func TestSynthesizerUnavailable(t *testing.T) {
	// An unavailable backend -> the engine is unavailable.
	s := NewSynthesizer(Config{Backend: unavailBackend{}, OnnxDir: "testdata/onnx", VoiceDir: "testdata/voice_styles"})
	if s.Available() {
		t.Fatal("engine should be unavailable with an unavailable backend")
	}
	if s.Status().Reason == "" {
		t.Fatal("unavailable engine should report a reason")
	}
	if _, err := s.Synthesize(context.Background(), "hi", Options{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("synthesize err = %v, want ErrUnavailable", err)
	}
}

func TestSynthesizerMissingAssets(t *testing.T) {
	// Available runtime but no model files on disk -> unavailable with a reason.
	s := NewSynthesizer(Config{
		Backend:  fakeBackend{rec: &recorder{}},
		OnnxDir:  t.TempDir(),
		VoiceDir: t.TempDir(),
	})
	if s.Available() {
		t.Fatal("engine should be unavailable when assets are missing")
	}
	if s.Status().Reason == "" {
		t.Fatal("missing-assets engine should report a reason")
	}
}

// A model load emits RuntimeModelLoaded on the sink.
func TestSynthesizerRuntimeEvents(t *testing.T) {
	sink := &captureSink{}
	s := NewSynthesizer(Config{
		Backend:  fakeBackend{rec: &recorder{}, duration: 0.1, wavLen: 50},
		OnnxDir:  "testdata/onnx",
		VoiceDir: "testdata/voice_styles",
		Sink:     sink,
	})
	stream, err := s.Synthesize(context.Background(), "Hi.", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Segments {
	}
	if !sink.has(events.TypeRuntimeModelLoaded) {
		t.Fatalf("expected RuntimeModelLoaded, got %v", sink.types())
	}
}

// --- test helpers ---

type captureSink struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *captureSink) PublishEphemeral(e events.Event) {
	c.mu.Lock()
	c.evs = append(c.evs, e)
	c.mu.Unlock()
}
func (c *captureSink) has(typ string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}
func (c *captureSink) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.evs))
	for i, e := range c.evs {
		out[i] = e.Type
	}
	return out
}

func mustOpen(t *testing.T, b onnx.Backend, out []string) onnx.Session {
	t.Helper()
	s, err := b.Open("x", []string{"in"}, out)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eqStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func eqF32(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func eqI64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
