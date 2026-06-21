package voice

import (
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/vad"
)

func TestTurnDetectionNormalizeDefaults(t *testing.T) {
	got := TurnDetection{}.Normalize()
	if got.Type != ServerVAD {
		t.Fatalf("default type = %q, want server_vad", got.Type)
	}
	if got.Threshold != defaultThreshold || got.PrefixPaddingMs != defaultPrefixPaddingMs || got.SilenceDurationMs != defaultSilenceMs {
		t.Fatalf("defaults not filled: %+v", got)
	}
	// Unknown type falls back to server_vad; bad threshold clamps.
	if (TurnDetection{Type: "bogus", Threshold: 5}).Normalize().Type != ServerVAD {
		t.Fatal("unknown detection type should fall back to server_vad")
	}
	// Semantic without eagerness defaults to auto.
	if got := (TurnDetection{Type: SemanticVAD}).Normalize().Eagerness; got != EagerAuto {
		t.Fatalf("semantic eagerness default = %q, want auto", got)
	}
	// Hostile/huge client timings are clamped to safe bounds (no unbounded pre-roll
	// or never-commit silence).
	clamped := TurnDetection{PrefixPaddingMs: 5_000_000, SilenceDurationMs: 9_999_999}.Normalize()
	if clamped.PrefixPaddingMs != maxPrefixPaddingMs {
		t.Fatalf("prefix_padding_ms clamp = %d, want %d", clamped.PrefixPaddingMs, maxPrefixPaddingMs)
	}
	if clamped.SilenceDurationMs != maxSilenceDurationMs {
		t.Fatalf("silence_duration_ms clamp = %d, want %d", clamped.SilenceDurationMs, maxSilenceDurationMs)
	}
}

func TestCreatesAndInterruptsResponseDefaults(t *testing.T) {
	d := TurnDetection{}
	if !d.CreatesResponse() || !d.InterruptsResponse() {
		t.Fatal("create/interrupt response should default to true")
	}
	no := false
	d2 := TurnDetection{CreateResponse: &no, InterruptResponse: &no}
	if d2.CreatesResponse() || d2.InterruptsResponse() {
		t.Fatal("explicit false should disable create/interrupt response")
	}
}

func TestVADParamsMapping(t *testing.T) {
	// Server VAD: silence maps straight through.
	sv := TurnDetection{Type: ServerVAD, SilenceDurationMs: 900, PrefixPaddingMs: 250, Threshold: 0.6}.VADParams()
	if sv.MinSilence != 900*time.Millisecond || sv.PreSpeech != 250*time.Millisecond || sv.Threshold != 0.6 {
		t.Fatalf("server VAD params wrong: %+v", sv)
	}
	// Semantic VAD: the raw detector silence is shortened to the semantic base so a
	// candidate end is reported quickly (the semantic layer extends the wait).
	semv := TurnDetection{Type: SemanticVAD, SilenceDurationMs: 5000}.VADParams()
	if semv.MinSilence != semanticBaseSilence {
		t.Fatalf("semantic VAD MinSilence = %v, want %v", semv.MinSilence, semanticBaseSilence)
	}
	_ = vad.Params{} // ensure the vad import is used in this test file
}
