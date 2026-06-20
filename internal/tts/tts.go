// Package tts is Hina's local text-to-speech engine: a pure-Go port of the
// official Supertonic 3 ONNX pipeline (research-findings B2). It turns assistant
// text into 44.1 kHz mono speech, streaming sentence-by-sentence, by running four
// ONNX graphs (duration predictor → text encoder → flow-matching vector estimator
// → vocoder) through the shared internal/onnx runtime. No phonemizer and no local
// HTTP TTS hop: text preprocessing is character-level Unicode (NFKD + a codepoint
// table), and synthesis is an in-process Go API.
//
// Everything here is CGo-free — it depends only on the onnx.Backend/Session
// abstraction, so the package builds and is fully unit-tested (with fake sessions)
// in the default CGO_ENABLED=0 build. Real synthesis needs the onnx-tagged build
// plus the downloaded model assets; without them the engine reports itself
// unavailable and Synthesize returns ErrUnavailable.
package tts

import (
	"context"
	"errors"
	"sync"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// NativeSampleRate is Supertonic's output rate: 44.1 kHz mono float32.
const NativeSampleRate = 44100

// ErrUnavailable is returned by Synthesize when the engine has no usable runtime
// or model assets (the default build, or assets not installed).
var ErrUnavailable = errors.New("tts: local TTS unavailable (needs the onnx build + installed model assets)")

// Options selects per-request synthesis parameters. Zero fields fall back to the
// engine defaults.
type Options struct {
	Voice string  // preset voice id, e.g. "M1" (no cloning — preset voices only)
	Lang  string  // Supertonic language tag, e.g. "en"
	Speed float64 // tempo multiplier; >1 is faster. 0 -> default (1.05)
	Steps int     // flow-matching denoise steps. 0 -> default (8)
}

// Segment is one synthesized sentence/chunk: mono float32 PCM in [-1,1] at
// SampleRate, with the source text it renders.
type Segment struct {
	Index      int
	Text       string
	PCM        []float32
	SampleRate int
}

// Stream is the result of Synthesize: a channel of Segments produced by a
// background goroutine. Range over Segments until it closes, then check Err for a
// synthesis or cancellation error and Truncated for a cap-shortened reply.
type Stream struct {
	Segments   <-chan Segment
	sampleRate int

	mu        sync.Mutex
	err       error
	truncated bool
}

// NewStream builds a Stream over a caller-provided Segments channel at the given
// sample rate. Engine implementations (and tests) use it to return a stream; the
// producer must close the channel when synthesis ends and may set a terminal
// error with SetErr before closing.
func NewStream(segments <-chan Segment, sampleRate int) *Stream {
	return &Stream{Segments: segments, sampleRate: sampleRate}
}

// SetErr records a terminal error on the stream (visible via Err after the
// channel closes). For engine implementations producing their own streams.
func (s *Stream) SetErr(err error) { s.setErr(err) }

// Truncated reports whether the reply was cut short by a synthesis cap (per-
// request audio budget or per-sentence length cap) rather than fully rendering
// the text. The already-produced audio is valid; it's just incomplete. Check
// after the stream closes.
func (s *Stream) Truncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

func (s *Stream) markTruncated() {
	s.mu.Lock()
	s.truncated = true
	s.mu.Unlock()
}

// SampleRate is the PCM rate of every Segment in the stream (44100).
func (s *Stream) SampleRate() int { return s.sampleRate }

// Err returns the terminal error (synthesis failure or context cancellation), or
// nil if the stream completed cleanly. Call after Segments is drained.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Stream) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Engine synthesizes speech. Implementations are safe for concurrent use.
type Engine interface {
	// Synthesize streams synthesized audio for text. It returns quickly; the
	// model load (if cold) and synthesis run in the returned Stream's goroutine.
	// A non-nil error here is a synchronous rejection (unavailable / empty text /
	// unknown voice); per-segment failures surface via Stream.Err.
	Synthesize(ctx context.Context, text string, opts Options) (*Stream, error)
	// Available reports whether synthesis can run (runtime linked + assets present).
	Available() bool
	// Status is the admin/doctor snapshot of the engine + runtime.
	Status() Status
	Close() error
}

// Status is the admin/doctor view of the TTS engine and its runtime.
type Status struct {
	Available       bool      `json:"available"`
	Loaded          bool      `json:"loaded"` // models currently resident (warm)
	Voice           string    `json:"voice"`
	Lang            string    `json:"lang"`
	Steps           int       `json:"steps"`
	Reason          string    `json:"reason,omitempty"` // why unavailable
	Runtime         onnx.Info `json:"runtime"`
	ColdLoadMillis  int64     `json:"cold_load_ms"`  // last cold model-load latency
	LastSynthMillis int64     `json:"last_synth_ms"` // last per-sentence synth latency
	SynthCount      int64     `json:"synth_count"`
	ErrorCount      int64     `json:"error_count"`
	LastError       string    `json:"last_error,omitempty"`
}

// EventSink publishes engine lifecycle events (RuntimeModelLoaded/Unloaded/Error)
// for observability. It is the events bus's PublishEphemeral in production; nil
// is allowed (no-op). Mirrors rtc.EventSink so the wiring stays uniform.
type EventSink interface {
	PublishEphemeral(events.Event)
}
