// Package asr is Hina's local streaming speech recognition: a pure-Go port of
// the Nemotron 3.5 streaming ASR (0.6B) pipeline (research-findings B3). It turns
// 16 kHz mono mic audio into streaming partial transcripts and a final on turn
// commit, by running a cache-aware FastConformer encoder and an RNNT greedy
// decoder over the shared internal/onnx runtime, with a log-mel front-end and
// SentencePiece detokenizer implemented in Go (not in the graph).
//
// Decode-time context biasing boosts the configurable agent name so it
// transcribes reliably, and a session-layer wake-token strip removes a leading
// address before the request would reach the LLM. Everything here is CGo-free —
// it depends only on the onnx.Backend/Session abstraction, so the package builds
// and is unit-tested (with fake sessions + synthetic audio) in the default
// CGO_ENABLED=0 build. Real recognition needs the onnx-tagged build plus the
// downloaded Nemotron assets; without them the engine reports itself unavailable
// and NewStream returns ErrUnavailable.
//
// Turn boundaries (VAD / semantic VAD) are Phase 6: this package emits
// partials/finals for a speech segment the caller delimits via Stream.Finalize.
package asr

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// NativeSampleRate is the model's input rate: 16 kHz mono float32 (matches the
// rtc inbound ASR resample target).
const NativeSampleRate = sampleRate

// Asset filenames inside the Nemotron model directory. Exported so the asset
// manager pins exactly these. The encoder carries its weights in an external
// .data file alongside the graph.
const (
	FileEncoder     = "encoder.onnx"
	FileEncoderData = "encoder.onnx.data"
	FileDecoder     = "decoder_joint.onnx"
	FileTokenizer   = "tokenizer.model"
	FileConfig      = "config.json"
)

// ErrUnavailable is returned by NewStream when the engine has no usable runtime
// or model assets (the default build, or assets not installed).
var ErrUnavailable = errors.New("asr: local ASR unavailable (needs the onnx build + installed model assets)")

// Options selects per-request recognition parameters. Zero fields fall back to
// the engine defaults.
type Options struct {
	Language string // language tag (e.g. "en", "auto"); resolves to the prompt index
}

// Partial is an interim transcript emitted while the user is still speaking.
type Partial struct {
	Text string `json:"text"`
}

// Final is the committed transcript for a turn: the full text, plus the wake
// detection result and the request Body with any leading agent address stripped
// (Body == Text when the agent wasn't addressed). Truncated is set when the
// recognizer hit its per-segment resource cap (max audio duration or token count)
// and stopped processing further audio, so the transcript is incomplete.
type Final struct {
	Text         string `json:"text"`
	WakeDetected bool   `json:"wake_detected"`
	Body         string `json:"body"`
	Truncated    bool   `json:"truncated"`
}

// EventSink publishes engine lifecycle events (RuntimeModelLoaded/Unloaded/Error)
// for observability. Mirrors tts.EventSink. nil is allowed (no-op).
type EventSink interface {
	PublishEphemeral(events.Event)
}

// Status is the admin/doctor view of the ASR engine and its runtime.
type Status struct {
	Available   bool      `json:"available"`
	Loaded      bool      `json:"loaded"` // models currently resident (warm)
	Language    string    `json:"language"`
	Biasing     bool      `json:"biasing"` // agent-name biasing active
	Reason      string    `json:"reason,omitempty"`
	Runtime     onnx.Info `json:"runtime"`
	ColdLoadMs  int64     `json:"cold_load_ms"`
	LastChunkMs int64     `json:"last_chunk_ms"`
	ChunkCount  int64     `json:"chunk_count"`
	ErrorCount  int64     `json:"error_count"`
	LastError   string    `json:"last_error,omitempty"`
}

// Engine recognizes streaming speech. Implementations are safe for concurrent
// use; each NewStream is an independent per-session recognizer.
type Engine interface {
	// NewStream starts a recognition stream. onPartial (optional) is called with
	// each interim transcript as chunks decode. The caller feeds audio via
	// Stream.Write and ends the turn via Stream.Finalize.
	NewStream(ctx context.Context, opts Options, onPartial func(Partial)) (Stream, error)
	Available() bool
	Status() Status
	Close() error
}

// Stream is one per-session recognizer handle. Audio is fed via Write (blocking
// backpressure) or TryWrite (non-blocking, for real-time callers that must never
// stall); Finalize ends a turn and returns its committed transcript (resetting
// state for the next turn); Close stops the recognizer and releases its share of
// the model bundle. Implementations are safe for concurrent use. An interface
// (not a concrete type) so callers — and tests — can substitute a fake.
type Stream interface {
	Write(pcm []float32) error
	// TryWrite enqueues a frame without blocking, returning false if the input
	// buffer is full (the recognizer has fallen behind) or the stream is closed —
	// the frame is dropped. Real-time feeders use this so a slow recognizer can
	// never stall the producer (mirrors the lossy audio-out path under backpressure).
	TryWrite(pcm []float32) bool
	Finalize() (Final, error)
	Close() error
}

// AgentBias configures name biasing + wake-word stripping from the agent
// identity. ContextScore/DepthScaling default when non-positive.
type AgentBias struct {
	Name         string
	Aliases      []string
	ContextScore float64
	DepthScaling float64
}

// Config configures a Nemotron engine.
type Config struct {
	Backend  onnx.Backend  // shared ONNX runtime (may be the unavailable stub)
	ModelDir string        // dir with encoder.onnx(+.data), decoder_joint.onnx, tokenizer.model
	IdleTTL  time.Duration // unload models after this idle (0 = keep warm)
	Defaults Options       // default language
	Agent    AgentBias     // name biasing + wake stripping
	Sink     EventSink
	Log      *slog.Logger

	// EncoderPath is the checksum-verified path to encoder.onnx. The encoder is
	// loaded by PATH (not bytes) because it has an external weights file
	// (encoder.onnx.data) that ORT resolves relative to the model on disk; the
	// yalue binding has no in-memory external-data API. Verification of the graph
	// AND its .data file happens before the engine is built (cmd/hina verifies the
	// full manifest), and the asset root is owner-private — so the residual is a
	// same-user swap in the verify→open window, the documented SecureRoot residual.
	// Empty -> ModelDir/encoder.onnx (tests).
	EncoderPath string
	// ReadDecoder / ReadTokenizer (optional) return the VERIFIED bytes of the
	// self-contained decoder_joint.onnx and tokenizer.model, so those load from
	// exactly the checksum-verified bytes (no reopen window). nil -> path loading.
	ReadDecoder   func() ([]byte, error)
	ReadTokenizer func() ([]byte, error)
}

// models is the loaded Nemotron graph pair (one onnx.Lifecycle bundle).
type models struct {
	enc onnx.Session
	dec onnx.Session
}

func (m *models) Close() error {
	var errs []error
	for _, s := range []onnx.Session{m.enc, m.dec} {
		if s != nil {
			if err := s.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Nemotron is the streaming ASR Engine. It lazily loads the encoder+decoder
// bundle on first stream (shared, idle-unloaded via onnx.Lifecycle) and decodes
// each stream's audio independently. Safe for concurrent use.
type Nemotron struct {
	backend     onnx.Backend
	modelDir    string
	encoderPath string
	readDecoder func() ([]byte, error)

	tok         *Tokenizer
	front       *melFront
	bias        *BiasContext
	wake        *WakeMatcher
	blankID     int
	defaultLang int64
	langLabel   string
	sink        EventSink
	log         *slog.Logger

	available bool
	reason    string

	lc *onnx.Lifecycle[*models]

	coldLoadMs  atomic.Int64
	lastChunkMs atomic.Int64
	chunkCount  atomic.Int64
	errCount    atomic.Int64
	errMu       sync.Mutex
	lastErr     string
}

// NewRecognizer builds the engine. Like the TTS engine it never fails: a missing
// runtime or assets yield an engine that reports Available()==false with a
// Reason, so the server still starts and the gap is reported.
func NewRecognizer(cfg Config) *Nemotron {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	encPath := cfg.EncoderPath
	if encPath == "" {
		encPath = filepath.Join(cfg.ModelDir, FileEncoder)
	}
	n := &Nemotron{
		backend:     cfg.Backend,
		modelDir:    cfg.ModelDir,
		encoderPath: encPath,
		readDecoder: cfg.ReadDecoder,
		sink:        cfg.Sink,
		log:         cfg.Log,
		front:       newMelFront(),
		langLabel:   orAuto(cfg.Defaults.Language),
		defaultLang: resolveLang(cfg.Defaults.Language),
	}
	n.wake = NewWakeMatcher(cfg.Agent.Name, cfg.Agent.Aliases)

	if cfg.Backend == nil || !cfg.Backend.Info().Available {
		n.reason = "ONNX runtime unavailable"
		if cfg.Backend != nil {
			if r := cfg.Backend.Info().Reason; r != "" {
				n.reason = r
			}
		}
		return n
	}
	if reason := n.loadStaticAssets(cfg); reason != "" {
		n.reason = reason
		return n
	}
	n.available = true
	n.lc = onnx.NewLifecycle(cfg.IdleTTL, n.loadBundle, onnx.Hooks{
		OnLoad: func(d time.Duration) {
			n.coldLoadMs.Store(d.Milliseconds())
			n.emitRuntime(events.TypeRuntimeModelLoaded, map[string]any{"models": "nemotron", "load_ms": d.Milliseconds()})
		},
		OnUnload: func() {
			n.emitRuntime(events.TypeRuntimeModelUnloaded, map[string]any{"models": "nemotron"})
		},
		OnError: func(err error) {
			n.recordErr(err)
			n.emitRuntime(events.TypeRuntimeModelError, map[string]any{"models": "nemotron", "error": err.Error()})
		},
	})
	return n
}

// loadStaticAssets loads the tokenizer (verified bytes or path), confirms the
// model files exist, and builds the name-biasing trie. Returns "" on success or
// a human reason.
func (n *Nemotron) loadStaticAssets(cfg Config) string {
	for _, p := range []string{n.encoderPath, filepath.Join(n.modelDir, FileEncoderData), filepath.Join(n.modelDir, FileDecoder)} {
		if !fileExists(p) {
			return "missing model asset: " + filepath.Base(p)
		}
	}
	tok, err := n.loadTokenizer(cfg)
	if err != nil {
		return err.Error()
	}
	n.tok = tok
	n.blankID = tok.Size() // RNNT blank is one past the last piece
	n.bias = buildBias(tok, cfg.Agent)
	return ""
}

func (n *Nemotron) loadTokenizer(cfg Config) (*Tokenizer, error) {
	if cfg.ReadTokenizer != nil {
		data, err := cfg.ReadTokenizer()
		if err != nil {
			return nil, err
		}
		return TokenizerFromBytes(data)
	}
	return LoadTokenizer(filepath.Join(n.modelDir, FileTokenizer))
}

// buildBias compiles the agent name + aliases into a SentencePiece-token biasing
// trie. Each phrase is encoded with the tokenizer's Viterbi segmentation, so the
// trie matches the token sequence the model would emit for the name.
func buildBias(tok *Tokenizer, agent AgentBias) *BiasContext {
	var phrases [][]int
	for _, name := range append([]string{agent.Name}, agent.Aliases...) {
		if ids := tok.Encode(name); len(ids) > 0 {
			phrases = append(phrases, ids)
		}
	}
	return NewBiasContext(phrases, agent.ContextScore, agent.DepthScaling)
}

func (n *Nemotron) loadBundle(ctx context.Context) (*models, error) {
	return loadModels(ctx, n.backend, n.encoderPath, n.readDecoder, filepath.Join(n.modelDir, FileDecoder))
}

// loadModels opens the encoder (by path — it has external weights) and the
// decoder_joint (from verified bytes when available, else by path). On any
// failure it closes whatever opened so a partial load never leaks a session.
func loadModels(ctx context.Context, b onnx.Backend, encPath string, readDecoder func() ([]byte, error), decPath string) (*models, error) {
	m := &models{}
	enc, err := b.Open(encPath, encInputs, encOutputs)
	if err != nil {
		return nil, errors.New("asr: open encoder: " + err.Error())
	}
	m.enc = enc
	if err := ctx.Err(); err != nil {
		_ = m.Close()
		return nil, err
	}
	var dec onnx.Session
	if readDecoder != nil {
		data, rerr := readDecoder()
		if rerr != nil {
			_ = m.Close()
			return nil, rerr
		}
		dec, err = b.OpenBytes(data, decInputs, decOutputs)
	} else {
		dec, err = b.Open(decPath, decInputs, decOutputs)
	}
	if err != nil {
		_ = m.Close()
		return nil, errors.New("asr: open decoder_joint: " + err.Error())
	}
	m.dec = dec
	return m, nil
}

// Available reports whether recognition can run.
func (n *Nemotron) Available() bool { return n.available }

// Status snapshots the engine + runtime for the admin UI / doctor.
func (n *Nemotron) Status() Status {
	st := Status{
		Available:   n.available,
		Language:    n.langLabel,
		Biasing:     n.bias.Enabled(),
		Reason:      n.reason,
		ColdLoadMs:  n.coldLoadMs.Load(),
		LastChunkMs: n.lastChunkMs.Load(),
		ChunkCount:  n.chunkCount.Load(),
		ErrorCount:  n.errCount.Load(),
	}
	if n.backend != nil {
		st.Runtime = n.backend.Info()
	}
	if n.lc != nil {
		st.Loaded = n.lc.Loaded()
	}
	n.errMu.Lock()
	st.LastError = n.lastErr
	n.errMu.Unlock()
	return st
}

// Close releases the loaded model bundle (if any).
func (n *Nemotron) Close() error {
	if n.lc != nil {
		n.lc.Close()
	}
	return nil
}

// NewStream acquires the model bundle (cold-loading it if needed) and starts a
// per-session recognizer goroutine. The bundle stays pinned for the stream's
// lifetime (so it's never reloaded mid-call) and is released on Close. ctx bounds
// the stream's lifetime: cancelling it (e.g. the owning session's context on
// teardown) stops in-flight inference and releases the bundle, just like Close.
func (n *Nemotron) NewStream(ctx context.Context, opts Options, onPartial func(Partial)) (Stream, error) {
	if !n.available {
		return nil, ErrUnavailable
	}
	m, release, err := n.lc.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	lang := n.defaultLang
	if opts.Language != "" {
		lang = resolveLang(opts.Language)
	}
	proc := newStreamProc(m, n.tok, n.front, n.bias, n.blankID, lang)
	// Derive the stream's lifetime from ctx, so cancelling ctx (e.g. the rtc
	// session context on teardown) stops in-flight inference — including a flush
	// still running inside Finalize — and releases the bundle, rather than the
	// stream outliving its session on an independent context. Close cancels it too.
	sctx, cancel := context.WithCancel(ctx)
	s := &stream{
		eng:       n,
		proc:      proc,
		release:   release,
		onPartial: onPartial,
		in:        make(chan inMsg, 32),
		result:    make(chan finalResult),
		done:      make(chan struct{}),
		ctx:       sctx,
		cancel:    cancel,
	}
	go s.loop()
	return s, nil
}

func (n *Nemotron) buildFinal(text string) Final {
	detected, body := n.wake.Strip(text)
	return Final{Text: text, WakeDetected: detected, Body: body}
}

func (n *Nemotron) recordErr(err error) {
	n.errCount.Add(1)
	n.errMu.Lock()
	n.lastErr = err.Error()
	n.errMu.Unlock()
}

func (n *Nemotron) emitRuntime(typ string, payload any) {
	if n.sink == nil {
		return
	}
	e, err := events.New(events.SourceServer, typ, "", "", "", payload)
	if err != nil {
		n.log.Error("asr: build runtime event", "type", typ, "err", err)
		return
	}
	e.ServerTS = time.Now().UTC()
	n.sink.PublishEphemeral(e)
}

// inMsg is a Stream command: a buffer of audio, or a finalize request.
type inMsg struct {
	pcm []float32
	fin bool
}

// finalResult is what the loop sends back for a finalize request: the committed
// transcript, or a terminal decode error (so a model/ORT failure surfaces as an
// error rather than a falsely-successful empty/stale transcript).
type finalResult struct {
	final Final
	err   error
}

// stream is the concrete Stream: a per-session recognizer whose decoding runs on
// a single internal goroutine, so Write never blocks on inference longer than the
// bounded channel. Close stops the goroutine and releases the shared model
// bundle. Safe for concurrent Write/Finalize/Close.
type stream struct {
	eng       *Nemotron
	proc      *streamProc
	release   func()
	onPartial func(Partial)

	in     chan inMsg
	result chan finalResult
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	// fatalErr is set (loop goroutine only) when a non-cancel decode error makes
	// the current turn unrecoverable. Once set, further audio is dropped (no
	// memory growth, no reprocessing the failing chunk) and Finalize returns the
	// error instead of a false transcript; it clears on the next turn (reset).
	fatalErr error

	closeOnce sync.Once
}

// loop is the single decode goroutine: it feeds audio, emits partials, and on a
// finalize request flushes + returns the Final (or a terminal error) and resets
// for the next turn.
func (s *stream) loop() {
	defer close(s.done)
	defer s.release()
	for {
		select {
		case <-s.ctx.Done():
			return
		case msg := <-s.in:
			if msg.fin {
				s.finalize()
				continue
			}
			// A turn that hit a terminal decode error drops further audio until it
			// is finalized + reset, so a persistent model failure can't loop on the
			// same chunk and grow memory as new mic frames arrive.
			if s.fatalErr != nil {
				continue
			}
			start := time.Now()
			chunks, adv, err := s.proc.feed(s.ctx, msg.pcm)
			if err != nil {
				if s.ctx.Err() != nil {
					return // cancelled (Close) — stop quietly
				}
				s.eng.recordErr(err)
				s.fatalErr = err
				continue
			}
			if chunks > 0 {
				s.eng.chunkCount.Add(int64(chunks))
				s.eng.lastChunkMs.Store(time.Since(start).Milliseconds())
			}
			if adv && s.onPartial != nil {
				s.onPartial(Partial{Text: s.proc.transcript()})
			}
		}
	}
}

// finalize handles a fin message: flush the tail (unless the turn already failed),
// send the committed transcript or the terminal error, and reset for the next
// turn. Sending is ctx-guarded so Close can't deadlock it.
func (s *stream) finalize() {
	defer s.proc.reset()
	if s.fatalErr != nil {
		err := s.fatalErr
		s.fatalErr = nil
		s.send(finalResult{err: err})
		return
	}
	start := time.Now()
	chunks, _, err := s.proc.flush(s.ctx)
	if err != nil {
		if s.ctx.Err() != nil {
			return // cancelled (Close) — the Finalize caller observes ctx via its own select
		}
		s.eng.recordErr(err)
		s.send(finalResult{err: err})
		return
	}
	if chunks > 0 {
		s.eng.chunkCount.Add(int64(chunks))
		s.eng.lastChunkMs.Store(time.Since(start).Milliseconds())
	}
	final := s.eng.buildFinal(s.proc.transcript())
	// Flag the transcript incomplete if the recognizer hit its segment cap and
	// stopped consuming audio (read on the loop goroutine that owns proc — no race).
	final.Truncated = s.proc.Capped()
	s.send(finalResult{final: final})
}

func (s *stream) send(r finalResult) {
	select {
	case s.result <- r:
	case <-s.ctx.Done():
	}
}

// Write feeds 16 kHz mono PCM. The bytes are copied, so the caller may reuse the
// slice. It returns an error only if the stream is closed.
func (s *stream) Write(pcm []float32) error {
	if s.ctx.Err() != nil {
		return context.Canceled // closed: report rather than buffer audio nobody reads
	}
	if len(pcm) == 0 {
		return nil
	}
	buf := append([]float32(nil), pcm...)
	select {
	case s.in <- inMsg{pcm: buf}:
		return nil
	case <-s.ctx.Done():
		return context.Canceled
	}
}

// TryWrite enqueues pcm without blocking; it returns false (dropping the frame)
// if the stream is closed or the input buffer is full. The bytes are copied.
func (s *stream) TryWrite(pcm []float32) bool {
	if s.ctx.Err() != nil || len(pcm) == 0 {
		return false
	}
	buf := append([]float32(nil), pcm...)
	select {
	case s.in <- inMsg{pcm: buf}:
		return true
	default:
		return false // buffer full: recognizer is behind; drop the frame
	}
}

// Finalize ends the current turn: it flushes the trailing audio, returns the
// committed Final (with wake detection + stripped body), and resets the
// recognizer for the next turn. It returns a non-nil error if the turn hit a
// terminal decode failure (so the caller reports an error instead of a false
// transcript) or if the stream is closed.
func (s *stream) Finalize() (Final, error) {
	if s.ctx.Err() != nil {
		return Final{}, context.Canceled
	}
	select {
	case s.in <- inMsg{fin: true}:
	case <-s.ctx.Done():
		return Final{}, context.Canceled
	}
	select {
	case r := <-s.result:
		return r.final, r.err
	case <-s.ctx.Done():
		return Final{}, context.Canceled
	}
}

// Close stops the decode goroutine and releases the shared model bundle. It is
// idempotent and waits for the goroutine to finish (so the bundle release has
// happened on return).
func (s *stream) Close() error {
	s.closeOnce.Do(func() { s.cancel() })
	<-s.done
	return nil
}

func orAuto(s string) string {
	if s == "" {
		return "auto"
	}
	return s
}
