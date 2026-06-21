package bench

import (
	"github.com/RenanQueiroz/hina-agent/internal/vad"
	"github.com/RenanQueiroz/hina-agent/internal/voice"
)

// EnergyModel is the default synthetic VAD model: it reports speech when a window's
// energy clears a threshold, so the harness drives the REAL turn-detection pipeline
// deterministically on every host (no ORT, no assets). The onnx-tagged `--real`
// path swaps in the actual Silero model for noise-discrimination numbers.
type EnergyModel struct{ Threshold float64 }

// NewEnergyModel returns the default energy model (threshold tuned to the fixture
// amplitudes: speech bursts ~0.3 clear it, sub-threshold noise/echo do not).
func NewEnergyModel() vad.Model { return &EnergyModel{Threshold: 0.001} }

func (m *EnergyModel) Probe(w []float32) (float32, error) {
	if len(w) == 0 {
		return 0, nil
	}
	var sum float64
	for _, s := range w {
		sum += float64(s) * float64(s)
	}
	if sum/float64(len(w)) >= m.Threshold {
		return 0.95, nil
	}
	return 0.02, nil
}
func (m *EnergyModel) Reset()       {}
func (m *EnergyModel) Close() error { return nil }

// sr16 is the fixture sample rate.
const sr16 = vad.SampleRate

// tone fills [startMs,endMs) of audio with a constant amplitude (a stand-in for a
// speech/backchannel/echo burst at that loudness).
func paint(audio []float32, startMs, endMs int, amp float32) {
	s := startMs * sr16 / 1000
	e := endMs * sr16 / 1000
	if e > len(audio) {
		e = len(audio)
	}
	for i := s; i < e; i++ {
		audio[i] = amp
	}
}

// silence allocates durMs of 16 kHz silence.
func silence(durMs int) []float32 { return make([]float32, durMs*sr16/1000) }

// Fixtures returns the built-in benchmark suite. Each fixture exercises one Phase 6
// exit criterion against the real pipeline. Amplitudes: ~0.3 = speech/backchannel
// (clears the energy VAD and the echo guard); ~0.05 = residual echo (gated); 0 =
// silence.
func Fixtures() []Fixture {
	serverVAD := voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 700, PrefixPaddingMs: 300}
	semantic := voice.TurnDetection{Type: voice.SemanticVAD, Eagerness: voice.EagerMedium}

	return []Fixture{
		cleanTurn(serverVAD),
		twoTurns(serverVAD),
		noiseOnly(serverVAD),
		backchannelDuringPlayback(serverVAD),
		interruptionDuringPlayback(serverVAD),
		echoDuringPlayback(serverVAD),
		semanticIncomplete(semantic),
	}
}

// cleanTurn: one spoken request surrounded by silence -> one onset + one commit.
func cleanTurn(td voice.TurnDetection) Fixture {
	a := silence(4000)
	paint(a, 500, 1800, 0.3) // 1.3 s of speech
	return Fixture{
		Name:    "clean_turn",
		Audio:   a,
		Regions: []Region{{StartMs: 500, EndMs: 1800, Kind: Speech}},
		TD:      td,
	}
}

// twoTurns: two separated requests -> two onsets + two commits, no false starts.
func twoTurns(td voice.TurnDetection) Fixture {
	a := silence(7000)
	paint(a, 400, 1600, 0.3)
	paint(a, 4000, 5200, 0.3)
	return Fixture{
		Name:  "two_turns",
		Audio: a,
		Regions: []Region{
			{StartMs: 400, EndMs: 1600, Kind: Speech},
			{StartMs: 4000, EndMs: 5200, Kind: Speech},
		},
		TD: td,
	}
}

// noiseOnly: sub-threshold room noise, no speech -> zero onsets.
func noiseOnly(td voice.TurnDetection) Fixture {
	a := silence(4000)
	paint(a, 300, 3700, 0.02) // quiet noise, below the speech threshold
	return Fixture{
		Name:    "noise_only",
		Audio:   a,
		Regions: []Region{{StartMs: 300, EndMs: 3700, Kind: Noise}},
		TD:      td,
	}
}

// backchannelDuringPlayback: the assistant is speaking; the user says "yeah". It
// must be suppressed (no barge-in, no committed turn).
func backchannelDuringPlayback(td voice.TurnDetection) Fixture {
	a := silence(4000)
	paint(a, 800, 1200, 0.3) // a short "yeah" while the assistant plays
	return Fixture{
		Name:     "backchannel_playback",
		Audio:    a,
		Regions:  []Region{{StartMs: 800, EndMs: 1200, Kind: Backchannel}},
		Partials: []ScriptedPartial{{AtMs: 850, Text: "yeah"}},
		Playback: []PlaybackWindow{{StartMs: 0, EndMs: 4000}},
		TD:       td,
	}
}

// interruptionDuringPlayback: the assistant is speaking; the user says a real
// request over it. It must barge in.
func interruptionDuringPlayback(td voice.TurnDetection) Fixture {
	a := silence(4000)
	paint(a, 800, 2400, 0.5) // louder than the playback guard -> passes the echo gate
	return Fixture{
		Name:     "interruption_playback",
		Audio:    a,
		Regions:  []Region{{StartMs: 800, EndMs: 2400, Kind: Speech}},
		Partials: []ScriptedPartial{{AtMs: 950, Text: "stop the music please"}},
		Playback: []PlaybackWindow{{StartMs: 0, EndMs: 2400}},
		TD:       td,
	}
}

// echoDuringPlayback: only the assistant's quiet residual echo on the mic while it
// plays -> zero onsets (the echo gate drops it).
func echoDuringPlayback(td voice.TurnDetection) Fixture {
	a := silence(4000)
	paint(a, 200, 3800, 0.05) // residual echo, well below the playback guard
	return Fixture{
		Name:     "echo_playback",
		Audio:    a,
		Regions:  []Region{{StartMs: 200, EndMs: 3800, Kind: Noise}},
		Playback: []PlaybackWindow{{StartMs: 0, EndMs: 4000}},
		TD:       td,
	}
}

// semanticIncomplete: the user trails off ("I want to…"), pauses, then completes.
// Semantic VAD must hold the turn open through the pause and commit once, on the
// completed utterance — not on the mid-thought pause.
func semanticIncomplete(td voice.TurnDetection) Fixture {
	a := silence(6000)
	paint(a, 500, 1300, 0.3)  // "I want to"
	paint(a, 2200, 3400, 0.3) // "...play some music" after a ~900 ms pause
	return Fixture{
		Name:  "semantic_incomplete",
		Audio: a,
		// One logical turn spanning both bursts (semantic VAD bridges the pause).
		Regions: []Region{{StartMs: 500, EndMs: 3400, Kind: Speech}},
		Partials: []ScriptedPartial{
			{AtMs: 600, Text: "i want to"},
			{AtMs: 2300, Text: "i want to play some music"},
		},
		TD: td,
	}
}
