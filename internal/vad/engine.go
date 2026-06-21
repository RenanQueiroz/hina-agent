package vad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// ErrUnavailable is returned by NewStream when the engine has no usable runtime or
// model (the default build, or the asset not installed).
var ErrUnavailable = errors.New("vad: local VAD unavailable (needs the onnx build + installed Silero model)")

// Silero graph I/O. The model takes a 512-sample window, the [2,1,128] LSTM state,
// and the int64 scalar sample rate; it returns the speech probability and the next
// state (research-findings B4).
const (
	inInput  = "input"
	inState  = "state"
	inSR     = "sr"
	outProb  = "output"
	outState = "stateN"
	stateLen = 2 * 1 * 128
)

var (
	sileroInputs  = []string{inInput, inState, inSR}
	sileroOutputs = []string{outProb, outState}
)

// EventSink publishes engine lifecycle events (RuntimeModelLoaded/Unloaded/Error)
// for observability. Mirrors tts/asr EventSink. nil is allowed (no-op).
type EventSink interface {
	PublishEphemeral(events.Event)
}

// Status is the admin/doctor view of the VAD engine and its runtime.
type Status struct {
	Available  bool      `json:"available"`
	Loaded     bool      `json:"loaded"` // model currently resident (warm)
	Reason     string    `json:"reason,omitempty"`
	Runtime    onnx.Info `json:"runtime"`
	ColdLoadMs int64     `json:"cold_load_ms"`
	ProbeCount int64     `json:"probe_count"`
	ErrorCount int64     `json:"error_count"`
	LastError  string    `json:"last_error,omitempty"`
}

// Config configures the VAD engine. The model loads from VERIFIED BYTES
// (ReadModel) when available — it is small and self-contained (no external data) —
// else from ModelPath (tests). Params are the default tunables; per-stream
// overrides are passed to NewStream.
type Config struct {
	Backend   onnx.Backend
	ModelPath string                 // path to silero_vad.onnx (used when ReadModel is nil)
	ReadModel func() ([]byte, error) // returns the verified model bytes (preferred)
	IdleTTL   time.Duration          // unload the model after this idle (0 = keep warm)
	Params    Params                 // default VAD tunables
	Sink      EventSink
	Log       *slog.Logger
}

// Engine loads the Silero model lazily (shared, idle-unloaded via onnx.Lifecycle)
// and hands each live session its own VAD Stream. Like the TTS/ASR engines it
// never fails to construct: a missing runtime/model yields Available()==false with
// a Reason. Safe for concurrent use.
type Engine struct {
	backend   onnx.Backend
	modelPath string
	readModel func() ([]byte, error)
	params    Params
	sink      EventSink
	log       *slog.Logger

	available bool
	reason    string
	lc        *onnx.Lifecycle[onnx.Session]

	coldLoadMs atomic.Int64
	probeCount atomic.Int64
	errCount   atomic.Int64
	errMu      sync.Mutex
	lastErr    string
}

// NewEngine builds the engine. It reports Available()==false (with a Reason)
// rather than erroring when the runtime or model is absent, so the server still
// starts and the gap is surfaced by doctor / the admin UI.
func NewEngine(cfg Config) *Engine {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	e := &Engine{
		backend:   cfg.Backend,
		modelPath: cfg.ModelPath,
		readModel: cfg.ReadModel,
		params:    cfg.Params.normalize(),
		sink:      cfg.Sink,
		log:       cfg.Log,
	}
	if cfg.Backend == nil || !cfg.Backend.Info().Available {
		e.reason = "ONNX runtime unavailable"
		if cfg.Backend != nil {
			if r := cfg.Backend.Info().Reason; r != "" {
				e.reason = r
			}
		}
		return e
	}
	if cfg.ReadModel == nil && cfg.ModelPath == "" {
		e.reason = "no Silero model configured"
		return e
	}
	e.available = true
	e.lc = onnx.NewLifecycle(cfg.IdleTTL, e.loadModel, onnx.Hooks{
		OnLoad: func(d time.Duration) {
			e.coldLoadMs.Store(d.Milliseconds())
			e.emitRuntime(events.TypeRuntimeModelLoaded, map[string]any{"models": "silero-vad", "load_ms": d.Milliseconds()})
		},
		OnUnload: func() {
			e.emitRuntime(events.TypeRuntimeModelUnloaded, map[string]any{"models": "silero-vad"})
		},
		OnError: func(err error) {
			e.recordErr(err)
			e.emitRuntime(events.TypeRuntimeModelError, map[string]any{"models": "silero-vad", "error": err.Error()})
		},
	})
	return e
}

// loadModel opens the Silero graph (from verified bytes when available, else by
// path) as one shared idle-unloadable session.
func (e *Engine) loadModel(_ context.Context) (onnx.Session, error) {
	if e.readModel != nil {
		data, err := e.readModel()
		if err != nil {
			return nil, err
		}
		return e.backend.OpenBytes(data, sileroInputs, sileroOutputs)
	}
	return e.backend.Open(e.modelPath, sileroInputs, sileroOutputs)
}

// Available reports whether VAD can run.
func (e *Engine) Available() bool { return e.available }

// DefaultParams returns the engine's normalized default tunables.
func (e *Engine) DefaultParams() Params { return e.params }

// Status snapshots the engine + runtime for the admin UI / doctor.
func (e *Engine) Status() Status {
	st := Status{
		Available:  e.available,
		Reason:     e.reason,
		ColdLoadMs: e.coldLoadMs.Load(),
		ProbeCount: e.probeCount.Load(),
		ErrorCount: e.errCount.Load(),
	}
	if e.backend != nil {
		st.Runtime = e.backend.Info()
	}
	if e.lc != nil {
		st.Loaded = e.lc.Loaded()
	}
	e.errMu.Lock()
	st.LastError = e.lastErr
	e.errMu.Unlock()
	return st
}

// Close releases the loaded model (if any).
func (e *Engine) Close() error {
	if e.lc != nil {
		e.lc.Close()
	}
	return nil
}

// NewStream acquires the model (cold-loading it if needed) and returns a VAD
// Stream with params merged over the engine defaults (zero fields fall back). ctx
// bounds the model probes; the bundle stays pinned until Stream.Close. p may be
// the zero Params to use the engine defaults.
func (e *Engine) NewStream(ctx context.Context, p Params) (*Stream, error) {
	if !e.available {
		return nil, ErrUnavailable
	}
	sess, release, err := e.lc.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	m := &silero{eng: e, ctx: ctx, session: sess, release: release, state: make([]float32, stateLen)}
	return NewStream(m, e.mergeParams(p)), nil
}

// mergeParams overlays non-zero fields of p on the engine defaults.
func (e *Engine) mergeParams(p Params) Params {
	out := e.params
	if p.Threshold > 0 && p.Threshold <= 1 {
		out.Threshold = p.Threshold
	}
	if p.MinSilence > 0 {
		out.MinSilence = p.MinSilence
	}
	if p.PreSpeech > 0 {
		out.PreSpeech = p.PreSpeech
	}
	if p.MinSpeech > 0 {
		out.MinSpeech = p.MinSpeech
	}
	if p.MaxDuration > 0 {
		out.MaxDuration = p.MaxDuration
	}
	return out
}

func (e *Engine) recordErr(err error) {
	e.errCount.Add(1)
	e.errMu.Lock()
	e.lastErr = err.Error()
	e.errMu.Unlock()
}

func (e *Engine) emitRuntime(typ string, payload any) {
	if e.sink == nil {
		return
	}
	ev, err := events.New(events.SourceServer, typ, "", "", "", payload)
	if err != nil {
		e.log.Error("vad: build runtime event", "type", typ, "err", err)
		return
	}
	ev.ServerTS = time.Now().UTC()
	e.sink.PublishEphemeral(ev)
}

// silero is the per-stream Model: it runs the shared Silero session over the
// stream's own LSTM state. Probe is single-flight (one stream goroutine); the
// shared session serializes Run internally, so concurrent streams are safe.
type silero struct {
	eng     *Engine
	ctx     context.Context
	session onnx.Session
	release func()
	state   []float32 // [2,1,128] LSTM carry, owned by this stream
	once    sync.Once
}

// Probe runs one window through the Silero graph and returns its speech
// probability, threading the LSTM state forward.
func (m *silero) Probe(window []float32) (float32, error) {
	if len(window) != WindowSize {
		return 0, fmt.Errorf("vad: window has %d samples, want %d", len(window), WindowSize)
	}
	out, err := m.session.Run(m.ctx, map[string]onnx.Tensor{
		inInput: onnx.NewFloat32([]int64{1, WindowSize}, window),
		inState: onnx.NewFloat32([]int64{2, 1, 128}, m.state),
		inSR:    onnx.NewInt64([]int64{1}, []int64{SampleRate}), // [1] (the ORT build rejects a rank-0 scalar)
	})
	if err != nil {
		m.eng.recordErr(err)
		return 0, err
	}
	prob, ok := out[outProb]
	if !ok || len(prob.Float32) < 1 {
		return 0, errors.New("vad: missing probability output")
	}
	if st, ok := out[outState]; ok && len(st.Float32) == stateLen {
		copy(m.state, st.Float32)
	}
	m.eng.probeCount.Add(1)
	return prob.Float32[0], nil
}

// Reset zeros the LSTM carry for a fresh stream/segment.
func (m *silero) Reset() {
	for i := range m.state {
		m.state[i] = 0
	}
}

// Close releases this stream's share of the shared model bundle (idempotent).
func (m *silero) Close() error {
	m.once.Do(func() {
		if m.release != nil {
			m.release()
		}
	})
	return nil
}
