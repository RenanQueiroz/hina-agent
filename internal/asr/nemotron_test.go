package asr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// --- fakes -----------------------------------------------------------------

// fakeEncoder returns a fixed 7-frame encoder output and echoes the caches, so
// the streaming chunk loop runs without a real model. runErr, when set, makes
// every Run fail (to exercise the terminal-error path).
type fakeEncoder struct {
	frames int
	runErr error
	gate   chan struct{} // if non-nil, Run blocks on it (stalls the recognizer loop)
}

func (e *fakeEncoder) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	if e.runErr != nil {
		return nil, e.runErr
	}
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f := e.frames
	return map[string]onnx.Tensor{
		"encoded":                     onnx.NewFloat32([]int64{1, hiddenDim, int64(f)}, make([]float32, hiddenDim*f)),
		"encoded_len":                 onnx.NewInt64([]int64{1}, []int64{int64(f)}),
		"cache_last_channel_next":     in["cache_last_channel"],
		"cache_last_time_next":        in["cache_last_time"],
		"cache_last_channel_len_next": in["cache_last_channel_len"],
	}, nil
}
func (e *fakeEncoder) Close() error { return nil }

// scriptedDecoder emits "▁hello ▁world" per turn: blank->hello, hello->world,
// world->blank. The token ids match the test tokenizer below.
type scriptedDecoder struct {
	helloID, worldID, blankID int
}

func (d *scriptedDecoder) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := int(in["targets"].Int32[0])
	logits := make([]float32, d.blankID+1)
	switch target {
	case d.blankID:
		logits[d.helloID] = 10
	case d.helloID:
		logits[d.worldID] = 10
	default:
		logits[d.blankID] = 10
	}
	return map[string]onnx.Tensor{
		"outputs":         onnx.NewFloat32([]int64{1, 1, 1, int64(len(logits))}, logits),
		"prednet_lengths": onnx.NewInt32([]int64{1}, []int32{1}),
		"output_states_1": in["input_states_1"],
		"output_states_2": in["input_states_2"],
	}, nil
}
func (d *scriptedDecoder) Close() error { return nil }

type fakeBackend struct {
	enc, dec onnx.Session
	avail    bool
}

func (b *fakeBackend) Info() onnx.Info { return onnx.Info{Available: b.avail, Version: "test"} }
func (b *fakeBackend) Close() error    { return nil }
func (b *fakeBackend) route(out []string) (onnx.Session, error) {
	for _, o := range out {
		if o == "encoded" {
			return b.enc, nil
		}
	}
	return b.dec, nil
}
func (b *fakeBackend) Open(_ string, _, out []string) (onnx.Session, error) { return b.route(out) }
func (b *fakeBackend) OpenBytes(_ []byte, _, out []string) (onnx.Session, error) {
	return b.route(out)
}

// testEngine builds a Nemotron engine over fakes with a synthetic tokenizer
// containing ▁hello / ▁world. agentName configures wake/biasing.
func testEngine(t *testing.T, agentName string) *Nemotron {
	return testEngineTTL(t, agentName, 0)
}

// testEngineTTL is testEngine with an explicit idle-unload TTL.
func testEngineTTL(t *testing.T, agentName string, idleTTL time.Duration) *Nemotron {
	return testEngineErr(t, agentName, idleTTL, nil)
}

// testEngineErr is testEngineTTL whose encoder fails every Run with encErr (to
// exercise the terminal-error path); encErr==nil gives a working engine.
func testEngineErr(t *testing.T, agentName string, idleTTL time.Duration, encErr error) *Nemotron {
	t.Helper()
	pieces := []string{"<unk>", spaceMarker, spaceMarker + "hello", spaceMarker + "world", "."}
	scores := []float32{-100, -1, -1, -1, -1}
	tokBytes := spModelBytes(pieces, scores)
	tk, err := TokenizerFromBytes(tokBytes)
	if err != nil {
		t.Fatal(err)
	}
	blank := tk.Size()

	dir := t.TempDir()
	for _, f := range []string{FileEncoder, FileEncoderData, FileDecoder} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend := &fakeBackend{
		enc:   &fakeEncoder{frames: 7, runErr: encErr},
		dec:   &scriptedDecoder{helloID: 2, worldID: 3, blankID: blank},
		avail: true,
	}
	return NewRecognizer(Config{
		Backend:       backend,
		ModelDir:      dir,
		IdleTTL:       idleTTL,
		Agent:         AgentBias{Name: agentName},
		ReadTokenizer: func() ([]byte, error) { return tokBytes, nil },
		ReadDecoder:   func() ([]byte, error) { return []byte("x"), nil },
	})
}

// --- tests -----------------------------------------------------------------

func TestEngineUnavailableWithoutRuntime(t *testing.T) {
	n := NewRecognizer(Config{Backend: &fakeBackend{avail: false}})
	if n.Available() {
		t.Fatal("engine must be unavailable without a runtime")
	}
	if _, err := n.NewStream(context.Background(), Options{}, nil); err != ErrUnavailable {
		t.Fatalf("NewStream err = %v, want ErrUnavailable", err)
	}
	if st := n.Status(); st.Available || st.Reason == "" {
		t.Fatalf("status = %+v, want unavailable with a reason", st)
	}
}

func TestStreamPartialsAndFinal(t *testing.T) {
	n := testEngine(t, "hello")
	if !n.Available() {
		t.Fatalf("engine unavailable: %s", n.Status().Reason)
	}
	defer n.Close()

	var partials []string
	s, err := n.NewStream(context.Background(), Options{}, func(p Partial) {
		partials = append(partials, p.Text)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// ~1 s of (silent) audio -> at least one full 56-frame chunk decodes.
	if err := s.Write(make([]float32, sampleRate)); err != nil {
		t.Fatal(err)
	}
	final, err := s.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if final.Text != "hello world" {
		t.Fatalf("final text = %q, want %q", final.Text, "hello world")
	}
	// Agent name "hello" -> wake detected, body stripped to "world".
	if !final.WakeDetected || final.Body != "world" {
		t.Fatalf("wake = (%v, %q), want (true, %q)", final.WakeDetected, final.Body, "world")
	}
	if len(partials) == 0 {
		t.Fatal("expected at least one partial during the turn")
	}
	if partials[len(partials)-1] != "hello world" {
		t.Fatalf("last partial = %q, want %q", partials[len(partials)-1], "hello world")
	}
	if st := n.Status(); st.ChunkCount == 0 {
		t.Fatal("status chunk count should be > 0 after a turn")
	}
}

func TestStreamResetsAcrossTurns(t *testing.T) {
	n := testEngine(t, "")
	defer n.Close()
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for turn := 0; turn < 2; turn++ {
		if err := s.Write(make([]float32, sampleRate)); err != nil {
			t.Fatal(err)
		}
		final, err := s.Finalize()
		if err != nil {
			t.Fatal(err)
		}
		// Each turn must decode fresh from a reset state (not accumulate).
		if final.Text != "hello world" {
			t.Fatalf("turn %d text = %q, want %q (state must reset)", turn, final.Text, "hello world")
		}
		if final.WakeDetected {
			t.Fatal("no agent name configured: wake must not trigger")
		}
	}
}

func TestStreamCloseAndIdleUnload(t *testing.T) {
	// A short idle TTL: after the only stream closes (releasing the last ref), the
	// shared bundle idle-unloads — proving lazy-load/idle-unload like TTS.
	n := testEngineTTL(t, "", 20*time.Millisecond)
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Write(make([]float32, sampleRate))
	if !n.lc.Loaded() {
		t.Fatal("bundle should be resident while the stream is active")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Write after Close fails cleanly rather than blocking/panicking.
	if err := s.Write([]float32{1}); err == nil {
		t.Fatal("Write after Close should error")
	}
	deadline := time.Now().Add(2 * time.Second)
	for n.lc.Loaded() {
		if time.Now().After(deadline) {
			t.Fatal("bundle should idle-unload after the stream closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = n.Close()
}

func TestBiasingEnabledWhenNameEncodes(t *testing.T) {
	n := testEngine(t, "hello") // "hello" encodes to ▁hello in the test vocab
	defer n.Close()
	if !n.Status().Biasing {
		t.Fatal("biasing should be enabled for an encodable agent name")
	}
	n2 := testEngine(t, "")
	defer n2.Close()
	if n2.Status().Biasing {
		t.Fatal("biasing should be disabled with no agent name")
	}
}

func TestStreamFeedErrorIsTerminal(t *testing.T) {
	boom := errors.New("encoder boom")
	n := testEngineErr(t, "", 0, boom)
	defer n.Close()
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// ~1 s of audio -> a full chunk -> the encoder errors -> the turn is terminal.
	if err := s.Write(make([]float32, sampleRate)); err != nil {
		t.Fatal(err)
	}
	// More audio after the failure must be dropped, not reprocessed/grown.
	_ = s.Write(make([]float32, sampleRate))
	final, ferr := s.Finalize()
	if ferr == nil {
		t.Fatalf("Finalize must return the decode error, got final=%+v", final)
	}
	if n.Status().ErrorCount == 0 {
		t.Fatal("error count should be > 0 after a decode failure")
	}
}

func TestStreamFlushErrorReturnsError(t *testing.T) {
	boom := errors.New("encoder boom")
	n := testEngineErr(t, "", 0, boom)
	defer n.Close()
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Less than a full chunk: feed runs no chunk, so the error first surfaces in
	// the flush at Finalize.
	if err := s.Write(make([]float32, sampleRate/4)); err != nil {
		t.Fatal(err)
	}
	if _, ferr := s.Finalize(); ferr == nil {
		t.Fatal("Finalize must surface a flush decode error")
	}
}

// A listening segment that is never stopped must not run inference or grow its
// transcript without bound: once the audio budget is hit, further chunks stop
// processing (the chunk count stops climbing) even as more audio is fed.
func TestStreamSegmentCapBoundsProcessing(t *testing.T) {
	// Shrink the audio budget so the test is fast; restore after.
	defer func(s, tk int) { maxSegmentSamples, maxSegmentTokens = s, tk }(maxSegmentSamples, maxSegmentTokens)
	maxSegmentSamples = 2 * sampleRate // ~2 s -> a few chunks

	n := testEngine(t, "")
	defer n.Close()
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Feed well past the budget (6 s) in 1 s increments, then Finalize — which
	// drains every queued write before returning, so the chunk count is settled
	// and the test is deterministic (no polling).
	for i := 0; i < 6; i++ {
		if err := s.Write(make([]float32, sampleRate)); err != nil {
			t.Fatal(err)
		}
	}
	final, err := s.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	// A capped segment must report its transcript as truncated (incomplete).
	if !final.Truncated {
		t.Fatal("a capped segment's Final must be marked Truncated")
	}
	cc := n.Status().ChunkCount
	// Some chunks decoded, but the count plateaued near the ~2 s budget (≈3-4
	// chunks) instead of climbing with the 6 s fed (≈10 chunks).
	if cc == 0 {
		t.Fatal("expected some chunks before the cap")
	}
	if cc > 6 {
		t.Fatalf("chunk count %d exceeded the segment budget; cap not enforced", cc)
	}
}

// When the recognizer stalls (a slow/blocked ORT run), TryWrite must drop frames
// (return false) instead of blocking, and Close must still return promptly — this
// is what keeps a busy recognizer from wedging the inbound loop or session
// teardown (the round-11 deadlock root).
func TestStreamTryWriteDropsAndCloseUnblocksWhenStalled(t *testing.T) {
	pieces := []string{"<unk>", spaceMarker, spaceMarker + "hello", spaceMarker + "world", "."}
	tokBytes := spModelBytes(pieces, []float32{-100, -1, -1, -1, -1})
	tk, _ := TokenizerFromBytes(tokBytes)
	dir := t.TempDir()
	for _, f := range []string{FileEncoder, FileEncoderData, FileDecoder} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate := make(chan struct{})
	backend := &fakeBackend{
		enc:   &fakeEncoder{frames: 7, gate: gate}, // blocks in Run -> stalls the loop
		dec:   &scriptedDecoder{helloID: 2, worldID: 3, blankID: tk.Size()},
		avail: true,
	}
	n := NewRecognizer(Config{
		Backend: backend, ModelDir: dir,
		ReadTokenizer: func() ([]byte, error) { return tokBytes, nil },
		ReadDecoder:   func() ([]byte, error) { return []byte("x"), nil },
	})
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	// One chunk's worth of audio starts the (blocked) encoder; the loop is now stuck.
	_ = s.Write(make([]float32, sampleRate))
	// Fill the input buffer; TryWrite must eventually report full (never block).
	full := false
	for i := 0; i < 1000; i++ {
		if !s.TryWrite(make([]float32, 320)) {
			full = true
			break
		}
	}
	if !full {
		t.Fatal("TryWrite never reported full despite a stalled recognizer (did it block?)")
	}
	// Close must return promptly even though the encoder Run is still blocked: the
	// stream context cancels the in-flight run.
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind the stalled recognizer")
	}
	close(gate) // release (the run already returned via ctx cancel)
}

func TestStreamContextCancelDoesNotBlock(t *testing.T) {
	n := testEngine(t, "")
	defer n.Close()
	s, err := n.NewStream(context.Background(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
}
