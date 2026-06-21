// Package voice is Hina's live-conversation turn-detection layer (Phase 6). It
// turns the raw Silero VAD speech/silence segments (internal/vad) into natural
// turn boundaries by adding, on top of the server-VAD silence rule, a v1 semantic
// turn detector (commit fast on a complete-looking utterance, wait on a trailing
// "umm…"), a backchannel filter (short acknowledgements during assistant speech
// don't interrupt), and playback-aware echo suppression (the assistant's own audio
// isn't mistaken for the user). It exposes an OpenAI-shaped turn_detection config
// so local and cloud (Phase 10) modes feel consistent.
//
// Everything here is pure Go and unit-testable with synthetic inputs (speech
// probabilities, partial transcripts, frame energies) — the Pipeline drives a
// vad.Stream that, in turn, runs over the build-tagged ONNX backend, so the
// decision logic is exercised without ORT or CGo. The live rtc loop and the
// benchmark harness drive the SAME Pipeline, so measured behavior matches shipped
// behavior.
package voice

import (
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/vad"
)

// DetectionType selects the turn-detection strategy, mirroring OpenAI Realtime.
type DetectionType string

const (
	// ServerVAD commits a turn purely on a trailing-silence duration (the Silero
	// segment end). Predictable and low-latency; no content awareness.
	ServerVAD DetectionType = "server_vad"
	// SemanticVAD adds a content classifier: it commits quickly on a complete-
	// looking utterance and waits (up to an eagerness-derived cap) on an incomplete
	// one ("umm…"), so it doesn't cut the user off mid-thought.
	SemanticVAD DetectionType = "semantic_vad"
)

// Eagerness tunes how eagerly semantic VAD commits, mapped (per research-findings
// B8) to a maximum wait before force-committing an incomplete-looking utterance:
// high ≈ 2 s, medium/auto ≈ 4 s, low ≈ 8 s.
type Eagerness string

const (
	EagerLow    Eagerness = "low"
	EagerMedium Eagerness = "medium"
	EagerHigh   Eagerness = "high"
	EagerAuto   Eagerness = "auto"
)

// TurnDetection is the OpenAI-shaped turn-detection config, accepted from the
// client (a SessionUpdate) and surfaced in the admin/runtime view so local and
// cloud modes share one shape. Zero fields fall back to defaults via Normalize.
type TurnDetection struct {
	Type              DetectionType `json:"type"`
	Threshold         float64       `json:"threshold,omitempty"`           // Silero speech-onset probability (server_vad)
	PrefixPaddingMs   int           `json:"prefix_padding_ms,omitempty"`   // pre-roll kept before onset
	SilenceDurationMs int           `json:"silence_duration_ms,omitempty"` // trailing silence that ends a turn (server_vad)
	CreateResponse    *bool         `json:"create_response,omitempty"`     // auto-run the agent on commit (default true)
	InterruptResponse *bool         `json:"interrupt_response,omitempty"`  // allow barge-in to interrupt the reply (default true)
	Eagerness         Eagerness     `json:"eagerness,omitempty"`           // semantic_vad commit eagerness
}

// Defaults + hard bounds for turn detection. The bounds clamp a buggy or hostile
// client's SessionUpdate so it can't, e.g., pin an oversized pre-roll history or
// hold turns open indefinitely; the 16 KB control-frame limit bounds the message
// size but not these numeric effects. Server-VAD silence is generous enough for a
// natural pause but snappy for a finished sentence.
const (
	defaultThreshold       = 0.5
	defaultPrefixPaddingMs = 300
	defaultSilenceMs       = 700
	maxPrefixPaddingMs     = 2000  // 2 s of pre-roll is already far beyond any onset
	maxSilenceDurationMs   = 10000 // 10 s; longer "silence" is effectively never-commit
)

// Normalize returns a copy with defaults filled and values clamped to safe bounds,
// so a partial or hostile config is always usable.
func (t TurnDetection) Normalize() TurnDetection {
	switch t.Type {
	case ServerVAD, SemanticVAD:
	default:
		t.Type = ServerVAD
	}
	if t.Threshold <= 0 || t.Threshold > 1 {
		t.Threshold = defaultThreshold
	}
	if t.PrefixPaddingMs <= 0 {
		t.PrefixPaddingMs = defaultPrefixPaddingMs
	} else if t.PrefixPaddingMs > maxPrefixPaddingMs {
		t.PrefixPaddingMs = maxPrefixPaddingMs
	}
	if t.SilenceDurationMs <= 0 {
		t.SilenceDurationMs = defaultSilenceMs
	} else if t.SilenceDurationMs > maxSilenceDurationMs {
		t.SilenceDurationMs = maxSilenceDurationMs
	}
	if t.Type == SemanticVAD {
		switch t.Eagerness {
		case EagerLow, EagerMedium, EagerHigh, EagerAuto:
		default:
			t.Eagerness = EagerAuto
		}
	}
	return t
}

// CreatesResponse reports whether the agent should run automatically when a turn
// commits (default true).
func (t TurnDetection) CreatesResponse() bool { return t.CreateResponse == nil || *t.CreateResponse }

// InterruptsResponse reports whether barge-in may interrupt the assistant reply
// (default true).
func (t TurnDetection) InterruptsResponse() bool {
	return t.InterruptResponse == nil || *t.InterruptResponse
}

// VADParams maps the turn-detection config to the low-level Silero detector
// tunables. For semantic VAD the raw silence is set to a short base (the detector
// reports a candidate end quickly) and the semantic layer decides whether that end
// is really the turn; for server VAD the silence is the configured duration.
func (t TurnDetection) VADParams() vad.Params {
	t = t.Normalize()
	silence := time.Duration(t.SilenceDurationMs) * time.Millisecond
	if t.Type == SemanticVAD {
		// Detect a plausible end at the semantic "complete" pause; the semantic layer
		// extends the wait when the utterance looks unfinished.
		if base := semanticBaseSilence; base < silence {
			silence = base
		}
	}
	return vad.Params{
		Threshold:  t.Threshold,
		MinSilence: silence,
		PreSpeech:  time.Duration(t.PrefixPaddingMs) * time.Millisecond,
	}
}
