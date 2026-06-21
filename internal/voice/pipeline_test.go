package voice

import (
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/vad"
)

// energyModel is a fake vad.Model whose speech probability tracks the frame's
// energy, so the Pipeline's echo gating (which zeroes suppressed frames) is
// exercised end-to-end: a loud frame reads as speech, a zeroed/quiet one as
// silence.
type energyModel struct{ thr float64 }

func (m *energyModel) Probe(w []float32) (float32, error) {
	if energy(w) >= m.thr {
		return 0.95, nil
	}
	return 0.02, nil
}
func (m *energyModel) Reset()       {}
func (m *energyModel) Close() error { return nil }

func fill(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func speechFrame() []float32  { return fill(vad.WindowSize, 0.2) } // energy 0.04
func silenceFrame() []float32 { return make([]float32, vad.WindowSize) }

// newPipe builds a Pipeline over an energy-driven fake VAD with cfg's VAD params.
func newPipe(cfg TurnDetection) *Pipeline {
	stream := vad.NewStream(&energyModel{thr: 0.001}, cfg.VADParams())
	return NewPipeline(cfg, stream, nil, nil)
}

// feed pushes n copies of frame (each one window) with the given playing flag and
// collects all events.
func feed(t *testing.T, p *Pipeline, frame []float32, n int, playing bool) []Event {
	t.Helper()
	var all []Event
	for i := 0; i < n; i++ {
		evs, err := p.Write(frame, playing)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		all = append(all, evs...)
	}
	return all
}

func count(evs []Event, k EventKind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func TestPipelineServerVADTurn(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96, PrefixPaddingMs: 32}
	p := newPipe(cfg)
	var evs []Event
	// 12 speech windows (>250ms min-speech) then enough silence to commit.
	evs = append(evs, feed(t, p, speechFrame(), 12, false)...)
	evs = append(evs, feed(t, p, silenceFrame(), 6, false)...)
	if count(evs, Open) != 1 {
		t.Fatalf("Open count = %d, want 1 (%v)", count(evs, Open), kindsOf(evs))
	}
	if count(evs, Audio) < 10 {
		t.Fatalf("Audio count = %d, want >=10 ongoing-speech windows", count(evs, Audio))
	}
	if count(evs, Commit) != 1 {
		t.Fatalf("Commit count = %d, want 1", count(evs, Commit))
	}
	if count(evs, Cancel) != 0 {
		t.Fatal("a full-length turn must not Cancel")
	}
}

func TestPipelineSemanticWaitsThenCommitsOnContinuation(t *testing.T) {
	cfg := TurnDetection{Type: SemanticVAD, Eagerness: EagerHigh}
	p := newPipe(cfg)
	silWins := int(semanticBaseSilence/(time.Duration(vad.WindowSize)*time.Second/vad.SampleRate)) + 2

	// Phase 1: an incomplete utterance, then a pause -> semantic holds the turn open.
	evs := feed(t, p, speechFrame(), 1, false)
	p.Observe("i want to")
	evs = append(evs, feed(t, p, speechFrame(), 11, false)...)
	evs = append(evs, feed(t, p, silenceFrame(), silWins, false)...)
	if count(evs, Commit) != 0 {
		t.Fatalf("semantic VAD committed an incomplete utterance on a pause (%v)", kindsOf(evs))
	}
	if count(evs, Open) != 1 {
		t.Fatalf("phase 1 Open count = %d, want 1", count(evs, Open))
	}

	// Phase 2: the user continues and completes -> commit, WITHOUT a second Open
	// (it's the same logical turn).
	p.Observe("i want to play some music")
	evs2 := feed(t, p, speechFrame(), 12, false)
	evs2 = append(evs2, feed(t, p, silenceFrame(), silWins, false)...)
	if count(evs2, Open) != 0 {
		t.Fatalf("continuation must not Open a new turn (%v)", kindsOf(evs2))
	}
	if count(evs2, Commit) != 1 {
		t.Fatalf("completed continuation should Commit once, got %d", count(evs2, Commit))
	}
}

func TestPipelineSemanticForceCommitsAtMaxWait(t *testing.T) {
	cfg := TurnDetection{Type: SemanticVAD, Eagerness: EagerHigh} // maxWait 2s
	p := newPipe(cfg)
	evs := feed(t, p, speechFrame(), 1, false)
	p.Observe("i want to") // stays incomplete
	evs = append(evs, feed(t, p, speechFrame(), 11, false)...)
	// Enough trailing silence to exceed maxWait (2s) from the base; force-commit.
	maxWaitWins := int(2*time.Second/(time.Duration(vad.WindowSize)*time.Second/vad.SampleRate)) + 4
	evs = append(evs, feed(t, p, silenceFrame(), maxWaitWins, false)...)
	if count(evs, Commit) != 1 {
		t.Fatalf("incomplete utterance should force-commit at maxWait, got %d Commit (%v)", count(evs, Commit), kindsOf(evs))
	}
}

func TestPipelineBackchannelDuringPlaybackDiscarded(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// ~10 speech windows (>250ms min-speech, but <700ms barge fallback) while the
	// assistant plays, then silence to end the segment. The partial (a backchannel)
	// arrives after the segment opens, as it does from the live ASR.
	evs := feed(t, p, speechFrame(), 1, true)
	p.Observe("yeah") // a backchannel partial for the open segment
	evs = append(evs, feed(t, p, speechFrame(), 9, true)...)
	evs = append(evs, feed(t, p, silenceFrame(), 6, true)...)
	if count(evs, BargeIn) != 0 {
		t.Fatalf("a backchannel during playback must not barge in (%v)", kindsOf(evs))
	}
	if count(evs, Commit) != 0 {
		t.Fatal("a backchannel during playback must not commit as a turn")
	}
	if count(evs, Cancel) != 1 {
		t.Fatalf("a backchannel during playback should be discarded (Cancel), got %d", count(evs, Cancel))
	}
}

func TestPipelineBargeInConfirmedByContent(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// The segment opens, then the ASR partial (real interruption content) arrives.
	evs := feed(t, p, speechFrame(), 1, true)
	p.Observe("stop the music please")
	evs = append(evs, feed(t, p, speechFrame(), 9, true)...)
	if count(evs, BargeIn) != 1 {
		t.Fatalf("non-backchannel speech during playback should barge in once, got %d (%v)", count(evs, BargeIn), kindsOf(evs))
	}
}

func TestPipelineBargeInFallbackOnSustainedSpeech(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// No partial observed (ASR lagging): sustained speech past the fallback window
	// must still interrupt.
	fallbackWins := int(defaultBargeInConfirm/(time.Duration(vad.WindowSize)*time.Second/vad.SampleRate)) + 3
	evs := feed(t, p, speechFrame(), fallbackWins, true)
	if count(evs, BargeIn) != 1 {
		t.Fatalf("sustained speech during playback should barge in via fallback, got %d (%v)", count(evs, BargeIn), kindsOf(evs))
	}
}

func TestPipelineEchoSuppressesQuietPlaybackEcho(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// The assistant is loud; observe its playback so the echo guard rises.
	for i := 0; i < 5; i++ {
		p.ObservePlayback(fill(vad.WindowSize, 0.8))
	}
	// Quiet mic frames during playback (residual echo) must be gated to silence and
	// never open a turn.
	quiet := fill(vad.WindowSize, 0.02) // energy 4e-4, well below 0.8 playback * 0.6
	evs := feed(t, p, quiet, 15, true)
	if count(evs, Open) != 0 {
		t.Fatalf("quiet echo during playback should not open a turn (%v)", kindsOf(evs))
	}
}

func TestPipelineShortInterruptionDuringPlaybackBargesInAtCommit(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// The user says a short one-word command ("stop") over the assistant: too short to
	// hit the sustained-speech fallback and only one non-backchannel word (below
	// minWords=2), so it isn't confirmed mid-stream — but it must still barge in when
	// the segment ends, before committing the new turn.
	evs := feed(t, p, speechFrame(), 1, true)
	p.Observe("stop")
	evs = append(evs, feed(t, p, speechFrame(), 9, true)...) // ~320ms, < 700ms fallback
	evs = append(evs, feed(t, p, silenceFrame(), 6, true)...)
	if count(evs, BargeIn) != 1 {
		t.Fatalf("a short non-backchannel interruption during playback should barge in once, got %d (%v)", count(evs, BargeIn), kindsOf(evs))
	}
	if count(evs, Commit) != 1 {
		t.Fatalf("the interruption should also commit as a turn, got %d Commit", count(evs, Commit))
	}
	// The BargeIn must precede the Commit (truncate the old reply before the new turn).
	var bargeIdx, commitIdx int = -1, -1
	for i, e := range evs {
		if e.Kind == BargeIn && bargeIdx < 0 {
			bargeIdx = i
		}
		if e.Kind == Commit && commitIdx < 0 {
			commitIdx = i
		}
	}
	if bargeIdx > commitIdx {
		t.Fatalf("BargeIn (%d) must come before Commit (%d)", bargeIdx, commitIdx)
	}
}

func TestPipelineShortInterruptionNoPartialBargesIn(t *testing.T) {
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96}
	p := newPipe(cfg)
	// A short utterance over the assistant whose ASR partial NEVER arrives before the
	// segment ends (ASR lag). An empty/unknown partial must NOT be treated as a
	// backchannel-cancel — it's a real interruption: barge in + commit (not discard).
	evs := feed(t, p, speechFrame(), 10, true) // ~320ms, no Observe()
	evs = append(evs, feed(t, p, silenceFrame(), 6, true)...)
	if count(evs, Cancel) != 0 {
		t.Fatalf("a short interruption with no partial must not be discarded as a backchannel (%v)", kindsOf(evs))
	}
	if count(evs, BargeIn) != 1 || count(evs, Commit) != 1 {
		t.Fatalf("short no-partial interruption: bargeIns=%d commits=%d, want 1/1 (%v)", count(evs, BargeIn), count(evs, Commit), kindsOf(evs))
	}
}

func TestPipelineInterruptResponseDisabledNoBargeIn(t *testing.T) {
	no := false
	cfg := TurnDetection{Type: ServerVAD, SilenceDurationMs: 96, InterruptResponse: &no}
	p := newPipe(cfg)
	p.Observe("stop the music please")
	evs := feed(t, p, speechFrame(), 10, true)
	if count(evs, BargeIn) != 0 {
		t.Fatal("interrupt_response=false must suppress barge-in")
	}
}

func kindsOf(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
