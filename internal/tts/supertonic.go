package tts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// Asset filenames inside the Supertonic onnx/ directory. Exported so the asset
// manager can pin + verify exactly these.
const (
	FileDurationPredictor = "duration_predictor.onnx"
	FileTextEncoder       = "text_encoder.onnx"
	FileVectorEstimator   = "vector_estimator.onnx"
	FileVocoder           = "vocoder.onnx"
	FileConfig            = "tts.json"
	FileIndexer           = "unicode_indexer.json"
)

// Default synthesis parameters (research-findings B2 / upstream CLI defaults).
const (
	defaultVoice = "M1"
	defaultSpeed = 1.05
	defaultSteps = 8
)

// Hard bounds defending against resource exhaustion from a bad config, an
// adversarial request, or a degenerate model output. maxSteps caps the flow loop;
// maxOutputSeconds caps the predicted duration (hence the latent allocation), so
// a near-zero speed or a runaway duration can't OOM or peg the CPU.
const (
	minSpeed         = 0.25
	maxSpeed         = 4.0
	maxSteps         = 100
	maxOutputSeconds = 60
	// Request-level caps so one allowed SpeakText can't explode into unbounded
	// work: a short-but-many-sentences payload (e.g. "a. " repeated) is rejected
	// synchronously by maxSegments, and the total synthesized audio per request is
	// bounded by maxTotalOutputSeconds regardless of per-sentence durations.
	maxSegments           = 64
	maxTotalOutputSeconds = 120
)

// ErrTooManySegments is returned synchronously by Synthesize when the text splits
// into more chunks than a single request may synthesize.
var ErrTooManySegments = errors.New("tts: text has too many sentences for one request")

// Tensor I/O names for the four graphs (verified against the official Go example).
var (
	dpInputs   = []string{"text_ids", "style_dp", "text_mask"}
	dpOutputs  = []string{"duration"}
	teInputs   = []string{"text_ids", "style_ttl", "text_mask"}
	teOutputs  = []string{"text_emb"}
	veInputs   = []string{"noisy_latent", "text_emb", "style_ttl", "latent_mask", "text_mask", "current_step", "total_step"}
	veOutputs  = []string{"denoised_latent"}
	vocInputs  = []string{"latent"}
	vocOutputs = []string{"wav_tts"}
)

// models is the loaded Supertonic graph set (one onnx.Lifecycle bundle).
type models struct {
	dp  onnx.Session
	te  onnx.Session
	ve  onnx.Session
	voc onnx.Session
}

func (m *models) Close() error {
	var errs []error
	for _, s := range []onnx.Session{m.dp, m.te, m.ve, m.voc} {
		if s != nil {
			if err := s.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// loadModels opens the four ONNX graphs. When readAsset is set, each graph is
// loaded from its VERIFIED bytes (onnx.OpenBytes) so the bytes ORT consumes are
// exactly the bytes that were checksum-verified (no reopen-by-path a concurrent
// writer could swap); otherwise it opens by path. It honors ctx between graphs (a
// single ORT open is not itself interruptible, so a barge-in during the large
// vector_estimator open finishes that one open, but no further models load), and
// on any failure/cancellation closes whatever opened so a partial load never
// leaks sessions.
func loadModels(ctx context.Context, b onnx.Backend, onnxDir string, readAsset func(string) ([]byte, error)) (*models, error) {
	m := &models{}
	type spec struct {
		dst     *onnx.Session
		file    string
		in, out []string
	}
	for _, s := range []spec{
		{&m.dp, FileDurationPredictor, dpInputs, dpOutputs},
		{&m.te, FileTextEncoder, teInputs, teOutputs},
		{&m.ve, FileVectorEstimator, veInputs, veOutputs},
		{&m.voc, FileVocoder, vocInputs, vocOutputs},
	} {
		if err := ctx.Err(); err != nil {
			_ = m.Close()
			return nil, err // request cancelled (barge-in / supersede / close) before this open
		}
		sess, err := openGraph(b, onnxDir, s.file, s.in, s.out, readAsset)
		if err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("tts: open %s: %w", s.file, err)
		}
		*s.dst = sess
	}
	return m, nil
}

// openGraph loads one ONNX graph from its verified bytes (preferred) or its path.
func openGraph(b onnx.Backend, onnxDir, file string, in, out []string, readAsset func(string) ([]byte, error)) (onnx.Session, error) {
	if readAsset != nil {
		data, err := readAsset(file)
		if err != nil {
			return nil, err
		}
		return b.OpenBytes(data, in, out)
	}
	return b.Open(filepath.Join(onnxDir, file), in, out)
}

// Config configures a Synthesizer.
type Config struct {
	Backend  onnx.Backend  // shared ONNX runtime (may be the unavailable stub)
	OnnxDir  string        // dir holding the 4 graphs + tts.json + unicode_indexer.json
	VoiceDir string        // dir holding <voice>.json style files
	IdleTTL  time.Duration // unload models after this much idle (0 = keep warm)
	Defaults Options       // default voice/lang/speed/steps
	Sink     EventSink     // runtime lifecycle events (nil = no-op)
	Log      *slog.Logger
	// ReadAsset/ReadVoiceAsset (optional) return the VERIFIED bytes of a model/config
	// file (by base name) or a voice (by id). When set, the engine loads ONNX graphs
	// and voices from these bytes (onnx.OpenBytes / *FromBytes) instead of reopening
	// files by path — so the bytes that were checksum-verified are exactly the bytes
	// fed to ORT, with no reopen window a concurrent writer could swap. Both nil =
	// plain path loading (e.g. tests).
	ReadAsset      func(file string) ([]byte, error)
	ReadVoiceAsset func(id string) ([]byte, error)
}

// Synthesizer is the Supertonic Engine. It lazily loads the graph bundle on first
// synthesis (shared, idle-unloaded via onnx.Lifecycle) and streams 44.1 kHz audio
// sentence-by-sentence. It is safe for concurrent use.
type Synthesizer struct {
	backend  onnx.Backend
	onnxDir  string
	voiceDir string
	params   params
	tok      *Tokenizer
	defaults Options
	sink     EventSink
	log      *slog.Logger

	available      bool
	reason         string
	readAsset      func(file string) ([]byte, error)
	readVoiceAsset func(id string) ([]byte, error)

	lc *onnx.Lifecycle[*models]

	vmu    sync.Mutex
	voices map[string]*Voice // style-vector cache by id

	// status counters (atomic; lastErr guarded by its own mutex)
	coldLoadMs  atomic.Int64
	lastSynthMs atomic.Int64
	synthCount  atomic.Int64
	errCount    atomic.Int64
	errMu       sync.Mutex
	lastErr     string
}

// NewSynthesizer builds the engine. It never fails: a missing runtime or missing
// assets yield an engine that reports Available()==false with a Reason, so the
// server runs (and the admin UI reports the gap) instead of refusing to start.
func NewSynthesizer(cfg Config) *Synthesizer {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	d := cfg.Defaults
	if !validVoice(d.Voice) {
		// An empty or unknown configured voice falls back to the default preset, so
		// a config typo can neither break startup nor become a path traversal.
		d.Voice = defaultVoice
	}
	d.Lang = resolveLang(d.Lang)
	if d.Speed == 0 {
		d.Speed = defaultSpeed
	}
	if d.Steps == 0 {
		d.Steps = defaultSteps
	}
	s := &Synthesizer{
		backend:        cfg.Backend,
		onnxDir:        cfg.OnnxDir,
		voiceDir:       cfg.VoiceDir,
		defaults:       d,
		sink:           cfg.Sink,
		log:            cfg.Log,
		readAsset:      cfg.ReadAsset,
		readVoiceAsset: cfg.ReadVoiceAsset,
		voices:         make(map[string]*Voice),
		params:         defaultParams(),
	}

	if cfg.Backend == nil || !cfg.Backend.Info().Available {
		s.reason = "ONNX runtime unavailable"
		if cfg.Backend != nil {
			if r := cfg.Backend.Info().Reason; r != "" {
				s.reason = r
			}
		}
		return s
	}
	if reason := s.loadStaticAssets(d.Voice); reason != "" {
		s.reason = reason
		return s
	}
	s.available = true
	s.lc = onnx.NewLifecycle(cfg.IdleTTL, s.loadBundle, onnx.Hooks{
		OnLoad: func(dur time.Duration) {
			s.coldLoadMs.Store(dur.Milliseconds())
			s.emitRuntime(events.TypeRuntimeModelLoaded, map[string]any{"models": "supertonic", "load_ms": dur.Milliseconds()})
		},
		OnUnload: func() {
			s.emitRuntime(events.TypeRuntimeModelUnloaded, map[string]any{"models": "supertonic"})
		},
		OnError: func(err error) {
			s.recordErr(err)
			s.emitRuntime(events.TypeRuntimeModelError, map[string]any{"models": "supertonic", "error": err.Error()})
		},
	})
	return s
}

// loadStaticAssets loads tts.json + the tokenizer and checks that all model files
// and the default voice exist. Returns "" on success or a human reason string.
func (s *Synthesizer) loadStaticAssets(defaultVoiceID string) string {
	for _, f := range []string{FileDurationPredictor, FileTextEncoder, FileVectorEstimator, FileVocoder, FileConfig, FileIndexer} {
		if !fileExists(filepath.Join(s.onnxDir, f)) {
			return "missing model asset: " + f
		}
	}
	if !fileExists(voicePath(s.voiceDir, defaultVoiceID)) {
		return "missing default voice: " + defaultVoiceID
	}
	p, err := s.loadParams()
	if err != nil {
		return err.Error()
	}
	tok, err := s.loadTokenizer()
	if err != nil {
		return err.Error()
	}
	s.params = p
	s.tok = tok
	return ""
}

// loadParams / loadTokenizer load the config + tokenizer from verified bytes when
// readAsset is set, else from the file path.
func (s *Synthesizer) loadParams() (params, error) {
	if s.readAsset != nil {
		data, err := s.readAsset(FileConfig)
		if err != nil {
			return defaultParams(), err
		}
		return paramsFromBytes(data)
	}
	return loadParams(filepath.Join(s.onnxDir, FileConfig))
}

func (s *Synthesizer) loadTokenizer() (*Tokenizer, error) {
	if s.readAsset != nil {
		data, err := s.readAsset(FileIndexer)
		if err != nil {
			return nil, err
		}
		return TokenizerFromBytes(data)
	}
	return LoadTokenizer(filepath.Join(s.onnxDir, FileIndexer))
}

func (s *Synthesizer) loadBundle(ctx context.Context) (*models, error) {
	return loadModels(ctx, s.backend, s.onnxDir, s.readAsset)
}

// Available reports whether synthesis can run.
func (s *Synthesizer) Available() bool { return s.available }

// Status snapshots the engine + runtime for the admin UI / doctor.
func (s *Synthesizer) Status() Status {
	st := Status{
		Available:       s.available,
		Voice:           s.defaults.Voice,
		Lang:            s.defaults.Lang,
		Steps:           s.defaults.Steps,
		Reason:          s.reason,
		ColdLoadMillis:  s.coldLoadMs.Load(),
		LastSynthMillis: s.lastSynthMs.Load(),
		SynthCount:      s.synthCount.Load(),
		ErrorCount:      s.errCount.Load(),
	}
	if s.backend != nil {
		st.Runtime = s.backend.Info()
	}
	if s.lc != nil {
		st.Loaded = s.lc.Loaded()
	}
	s.errMu.Lock()
	st.LastError = s.lastErr
	s.errMu.Unlock()
	return st
}

// Close releases the loaded model bundle (if any).
func (s *Synthesizer) Close() error {
	if s.lc != nil {
		s.lc.Close()
	}
	return nil
}

// Synthesize streams synthesized audio for text. See Engine.Synthesize.
func (s *Synthesizer) Synthesize(ctx context.Context, text string, opts Options) (*Stream, error) {
	if !s.available {
		return nil, ErrUnavailable
	}
	opts = s.applyDefaults(opts)
	voice, err := s.voice(opts.Voice)
	if err != nil {
		return nil, err
	}
	chunks := segments(text, opts.Lang)
	if len(chunks) == 0 {
		return nil, errors.New("tts: empty text")
	}
	if len(chunks) > maxSegments {
		// Reject synchronously (before any playback starts) so a short-but-many-
		// sentences payload can't monopolize the shared runtime.
		return nil, fmt.Errorf("%w (%d sentences, max %d)", ErrTooManySegments, len(chunks), maxSegments)
	}

	ch := make(chan Segment)
	stream := &Stream{Segments: ch, sampleRate: s.params.SampleRate}
	go s.run(ctx, stream, ch, chunks, voice, opts)
	return stream, nil
}

// run is the producer goroutine: acquire the model bundle, synthesize each chunk,
// and emit a Segment per non-empty result. It always closes ch and releases the
// bundle, even on cancellation/error.
func (s *Synthesizer) run(ctx context.Context, stream *Stream, ch chan<- Segment, chunks []string, voice *Voice, opts Options) {
	defer close(ch)
	m, release, err := s.lc.Acquire(ctx)
	if err != nil {
		stream.setErr(err)
		return
	}
	defer release()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	noise := func(n int) []float32 { return gaussianNoise(rng, n) }

	idx := 0
	totalSamples := 0
	maxTotal := maxTotalOutputSeconds * s.params.SampleRate
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			stream.setErr(err)
			return
		}
		// Stop before synthesizing past the per-request audio budget; the remaining
		// budget is passed to inferSentence so the last sentence is trimmed to fit
		// rather than overshooting by a full sentence.
		remaining := maxTotal - totalSamples
		if remaining <= 0 {
			s.log.Warn("tts: per-request output cap reached; truncating reply", "max", maxTotal)
			stream.markTruncated()
			return
		}
		ids := s.tok.Encode(preprocessText(chunk, opts.Lang))
		start := time.Now()
		pcm, clamped, err := inferSentence(ctx, m, s.params, voice, ids, opts.Steps, opts.Speed, remaining, noise)
		if err != nil {
			if ctx.Err() != nil {
				stream.setErr(ctx.Err()) // cancellation, not a synthesis failure
				return
			}
			s.recordErr(err)
			stream.setErr(err)
			return
		}
		if clamped {
			stream.markTruncated() // this sentence was cut by the per-sentence/budget cap
		}
		s.lastSynthMs.Store(time.Since(start).Milliseconds())
		s.synthCount.Add(1)
		if len(pcm) == 0 {
			continue // nothing audible for this chunk (e.g. punctuation-only)
		}
		totalSamples += len(pcm)
		select {
		case ch <- Segment{Index: idx, Text: chunk, PCM: pcm, SampleRate: s.params.SampleRate}:
			idx++
		case <-ctx.Done():
			stream.setErr(ctx.Err())
			return
		}
	}
}

func (s *Synthesizer) applyDefaults(o Options) Options {
	if o.Voice == "" {
		o.Voice = s.defaults.Voice
	}
	o.Lang = resolveLang(orStr(o.Lang, s.defaults.Lang))
	if o.Speed == 0 {
		o.Speed = s.defaults.Speed
	}
	if o.Steps == 0 {
		o.Steps = s.defaults.Steps
	}
	return o
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// voice returns the cached style vectors for id, loading + caching on first use.
// id MUST be an allowlisted preset voice; anything else is rejected before it can
// become a filesystem path (no traversal, no probing of arbitrary JSON files).
func (s *Synthesizer) voice(id string) (*Voice, error) {
	if !validVoice(id) {
		return nil, fmt.Errorf("tts: unknown voice %q (choose one of %v)", id, PresetVoices())
	}
	s.vmu.Lock()
	defer s.vmu.Unlock()
	if v, ok := s.voices[id]; ok {
		return v, nil
	}
	// Load from VERIFIED bytes when configured (so the on-demand voice load can't be
	// raced with a swap), else read the file by path.
	var v *Voice
	var err error
	if s.readVoiceAsset != nil {
		var data []byte
		if data, err = s.readVoiceAsset(id); err == nil {
			v, err = VoiceFromBytes(data, id)
		}
	} else {
		v, err = LoadVoice(voicePath(s.voiceDir, id), id)
	}
	if err != nil {
		return nil, err
	}
	s.voices[id] = v
	return v, nil
}

func (s *Synthesizer) recordErr(err error) {
	s.errCount.Add(1)
	s.errMu.Lock()
	s.lastErr = err.Error()
	s.errMu.Unlock()
}

// emitRuntime publishes a global (no-conversation) runtime lifecycle event.
func (s *Synthesizer) emitRuntime(typ string, payload any) {
	if s.sink == nil {
		return
	}
	e, err := events.New(events.SourceServer, typ, "", "", "", payload)
	if err != nil {
		s.log.Error("tts: build runtime event", "type", typ, "err", err)
		return
	}
	e.ServerTS = time.Now().UTC()
	s.sink.PublishEphemeral(e)
}

// inferSentence runs the four-graph Supertonic pipeline for one token sequence
// (batch size 1) and returns mono float32 PCM at the model's sample rate. An
// empty/zero-duration result returns nil with no error (nothing audible). ctx is
// checked before every graph call and flow step so a superseded/barge-in'd
// utterance stops promptly instead of running the whole sentence (and holding the
// serialized ORT session) to completion.
// inferSentence returns the rendered PCM and whether it was CLAMPED (the model's
// predicted length was cut by the per-sentence cap or the caller's remaining
// budget) — a signal the reply is incomplete.
func inferSentence(ctx context.Context, m *models, p params, voice *Voice, ids []int64, steps int, speed float64, budget int, noise func(n int) []float32) (pcm []float32, clamped bool, err error) {
	l := len(ids)
	if l == 0 {
		return nil, false, nil
	}
	if steps < 1 {
		steps = 1
	} else if steps > maxSteps {
		steps = maxSteps
	}
	if speed < minSpeed {
		speed = minSpeed
	} else if speed > maxSpeed {
		speed = maxSpeed
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	textIDs := onnx.NewInt64([]int64{1, int64(l)}, ids)
	textMask := onnx.NewFloat32([]int64{1, 1, int64(l)}, ones(l))
	styleDP := onnx.NewFloat32(voice.DPDims, voice.StyleDP)
	styleTTL := onnx.NewFloat32(voice.TTLDims, voice.StyleTTL)

	// 1. Duration (seconds), adjusted by speed.
	dpOut, err := m.dp.Run(ctx, map[string]onnx.Tensor{"text_ids": textIDs, "style_dp": styleDP, "text_mask": textMask})
	if err != nil {
		return nil, false, fmt.Errorf("duration predictor: %w", err)
	}
	dur := dpOut["duration"]
	if len(dur.Float32) == 0 {
		return nil, false, errors.New("duration predictor returned no value")
	}
	seconds := float64(dur.Float32[0]) / speed
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return nil, false, nil // degenerate/empty duration -> nothing audible
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	// 2. Text encoding (independent of duration).
	teOut, err := m.te.Run(ctx, map[string]onnx.Tensor{"text_ids": textIDs, "style_ttl": styleTTL, "text_mask": textMask})
	if err != nil {
		return nil, false, fmt.Errorf("text encoder: %w", err)
	}
	textEmb := teOut["text_emb"]

	wavLength := int(seconds * float64(p.SampleRate))
	if wavLength <= 0 {
		return nil, false, nil
	}
	// Cap the predicted length so a runaway duration can't drive an unbounded
	// latent allocation / flow loop. The per-sentence cap and the caller's
	// remaining request budget (when smaller) both apply, so the total per request
	// is strictly bounded — no full-sentence overshoot past the request cap.
	limit := maxOutputSeconds * p.SampleRate
	if budget > 0 && budget < limit {
		limit = budget
	}
	if wavLength > limit {
		wavLength = limit
		clamped = true // the reply is cut short of the model's predicted length
	}
	latentDim := p.latentDim()
	latentLen := ceilDiv(wavLength, p.chunkSize())
	if latentLen < 1 {
		latentLen = 1
	}

	// 3. Sample the noisy latent (masked; batch size 1 -> mask is all ones).
	latentShape := []int64{1, int64(latentDim), int64(latentLen)}
	maskShape := []int64{1, 1, int64(latentLen)}
	xt := onnx.NewFloat32(latentShape, noise(latentDim*latentLen))
	latentMask := onnx.NewFloat32(maskShape, ones(latentLen))
	totalStep := onnx.NewFloat32([]int64{1}, []float32{float32(steps)})

	// 4. Flow-matching denoise loop: the ODE step lives in the graph; we feed the
	//    0-based step index and total, replacing the latent with the model output.
	for step := 0; step < steps; step++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		veOut, err := m.ve.Run(ctx, map[string]onnx.Tensor{
			"noisy_latent": xt,
			"text_emb":     textEmb,
			"style_ttl":    styleTTL,
			"latent_mask":  latentMask,
			"text_mask":    textMask,
			"current_step": onnx.NewFloat32([]int64{1}, []float32{float32(step)}),
			"total_step":   totalStep,
		})
		if err != nil {
			return nil, false, fmt.Errorf("vector estimator step %d: %w", step, err)
		}
		den := veOut["denoised_latent"]
		if int64(len(den.Float32)) != int64(latentDim)*int64(latentLen) {
			return nil, false, fmt.Errorf("vector estimator returned %d elements, want %d", len(den.Float32), latentDim*latentLen)
		}
		xt = onnx.NewFloat32(latentShape, den.Float32)
	}

	// 5. Vocoder: latent -> waveform; trim to the predicted duration.
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	vocOut, err := m.voc.Run(ctx, map[string]onnx.Tensor{"latent": xt})
	if err != nil {
		return nil, false, fmt.Errorf("vocoder: %w", err)
	}
	wav := vocOut["wav_tts"].Float32
	if wavLength < len(wav) {
		wav = wav[:wavLength]
	}
	return wav, clamped, nil
}

// gaussianNoise fills n standard-normal samples via Box–Muller (matching the
// reference noisy-latent sampler).
func gaussianNoise(rng *rand.Rand, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		u1 := rng.Float64()
		if u1 < 1e-10 {
			u1 = 1e-10
		}
		u2 := rng.Float64()
		out[i] = float32(math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2))
	}
	return out
}

func ones(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
