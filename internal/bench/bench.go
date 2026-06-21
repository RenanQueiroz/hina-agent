// Package bench is Hina's live-voice benchmark harness (Phase 6). It replays
// labeled audio fixtures through the REAL turn-detection pipeline (internal/voice
// over internal/vad) and reports per-fixture metrics with percentiles: false / missed
// VAD starts, end-of-turn delay, interruption delay, false-interruption rate, and
// backchannel-suppression accuracy. It is non-interactive and runs on every Tier-1
// host: by default it drives the pipeline with a deterministic energy-based VAD
// model (no ORT, no assets), so the turn-detection LOGIC is measured everywhere
// (including Windows CI); an onnx-tagged build can swap in the real Silero model for
// richer numbers. `hina bench` runs the built-in suite.
//
// The harness measures what the pure pipeline produces deterministically (the
// turn-detection metrics that gate each Phase 6 step). End-to-end latency numbers
// (STT/first-token/first-audio/total-turn) require the real ASR+LLM+TTS engines and
// are reported by the engine-backed path; the percentile machinery here is shared.
package bench

import (
	"math"
	"sort"

	"github.com/RenanQueiroz/hina-agent/internal/vad"
	"github.com/RenanQueiroz/hina-agent/internal/voice"
)

// RegionKind labels a ground-truth span of a fixture's timeline.
type RegionKind int

const (
	// Speech is a real user turn the detector should open and commit.
	Speech RegionKind = iota
	// Backchannel is a short acknowledgement during assistant playback that must
	// NOT interrupt or commit a turn.
	Backchannel
	// Noise is non-speech audio (room noise / echo) that must not open a turn.
	Noise
)

// Region is one labeled span of the fixture, in milliseconds.
type Region struct {
	StartMs int
	EndMs   int
	Kind    RegionKind
}

func (r Region) contains(ms int) bool { return ms >= r.StartMs && ms <= r.EndMs }

// ScriptedPartial is an ASR partial transcript fed at a given time, so the semantic
// + backchannel decisions have content (the harness has no real ASR by default).
type ScriptedPartial struct {
	AtMs int
	Text string
}

// PlaybackWindow is a span during which the assistant is "playing" (for echo /
// backchannel / interruption fixtures).
type PlaybackWindow struct {
	StartMs int
	EndMs   int
}

// Fixture is one labeled benchmark scenario.
type Fixture struct {
	Name     string
	Audio    []float32 // 16 kHz mono
	Regions  []Region
	Partials []ScriptedPartial
	Playback []PlaybackWindow
	TD       voice.TurnDetection
}

func (f Fixture) playingAt(ms int) bool {
	for _, w := range f.Playback {
		if ms >= w.StartMs && ms <= w.EndMs {
			return true
		}
	}
	return false
}

// nearRegion returns the index of the first region of the given kind whose
// [Start-startSlack, End+endSlack] window contains ms, and whether one matched.
func (f Fixture) nearRegion(ms int, kind RegionKind, startSlack, endSlack int) (int, bool) {
	for i, r := range f.Regions {
		if r.Kind == kind && ms >= r.StartMs-startSlack && ms <= r.EndMs+endSlack {
			return i, true
		}
	}
	return -1, false
}

// Stats are percentile summaries of a latency sample set (milliseconds).
type Stats struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

// Result is one fixture's measured metrics.
type Result struct {
	Fixture               string `json:"fixture"`
	Frames                int    `json:"frames"`
	Opens                 int    `json:"opens"`
	Commits               int    `json:"commits"`
	Cancels               int    `json:"cancels"`
	BargeIns              int    `json:"barge_ins"`
	FalseStarts           int    `json:"false_starts"`           // onsets outside any speech/backchannel region
	MissedStarts          int    `json:"missed_starts"`          // speech regions with no onset
	FalseInterruptions    int    `json:"false_interruptions"`    // barge-ins triggered by a backchannel
	BackchannelTotal      int    `json:"backchannel_total"`      // backchannel regions in the fixture
	BackchannelSuppressed int    `json:"backchannel_suppressed"` // backchannels that did not leak a turn
	EndOfTurnDelayMs      Stats  `json:"end_of_turn_delay_ms"`   // commit time minus the truth speech end
	InterruptionDelayMs   Stats  `json:"interruption_delay_ms"`  // barge-in time minus the interruption start
}

// endSlackMs lets a commit that lands after a labeled region edge still attribute
// to that region (a turn commit trails the truth by the silence window).
// onsetSlackMs lets an onset that fires just before the labeled start (the window
// straddling the boundary already has enough energy) still count as on-time.
const (
	endSlackMs   = 1500
	onsetSlackMs = 200
)

// playbackGuardAmp is the simulated assistant loudness fed to echo suppression
// while the assistant is "playing"; residual echo below it is gated, a real user
// barge-in at or above it passes.
const playbackGuardAmp = 0.3

// constFrame returns one window filled with amp (a stand-in for a frame of audio at
// a given loudness).
func constFrame(amp float32) []float32 {
	w := make([]float32, vad.WindowSize)
	for i := range w {
		w[i] = amp
	}
	return w
}

// Run replays one fixture through the real pipeline driven by model and returns the
// measured metrics. The pipeline (voice.Pipeline over vad.Stream) is exactly the
// one the live rtc loop runs, so measured behavior matches shipped behavior.
func Run(f Fixture, model vad.Model) Result {
	stream := vad.NewStream(model, f.TD.VADParams())
	pipe := voice.NewPipeline(f.TD, stream, nil, nil)

	r := Result{Fixture: f.Name}
	var eot, intr []float64
	openedSpeech := make([]bool, len(f.Regions)) // speech region index -> got an onset
	leaked := make([]bool, len(f.Regions))       // backchannel region index -> leaked a turn

	partialIdx := 0
	lastOnsetMs := -1
	for off := 0; off < len(f.Audio); off += vad.WindowSize {
		end := off + vad.WindowSize
		if end > len(f.Audio) {
			end = len(f.Audio)
		}
		ms := off * 1000 / vad.SampleRate
		for partialIdx < len(f.Partials) && f.Partials[partialIdx].AtMs <= ms {
			pipe.Observe(f.Partials[partialIdx].Text)
			partialIdx++
		}
		playing := f.playingAt(ms)
		if playing {
			// Simulate the assistant's outbound audio at a fixed loudness so echo
			// suppression has a guard level: quiet residual echo is gated, while a user
			// speaking at or above that level (a real barge-in) passes through.
			pipe.ObservePlayback(constFrame(playbackGuardAmp))
		}
		evs, _ := pipe.Write(f.Audio[off:end], playing)
		for _, ev := range evs {
			switch ev.Kind {
			case voice.Open:
				r.Opens++
				lastOnsetMs = ms
				si, sok := f.nearRegion(ms, Speech, onsetSlackMs, 0)
				_, bok := f.nearRegion(ms, Backchannel, onsetSlackMs, 0)
				if sok {
					openedSpeech[si] = true
				}
				if !sok && !bok {
					r.FalseStarts++ // an onset where there is no speech/backchannel
				}
			case voice.Commit:
				r.Commits++
				if i, ok := f.nearRegion(lastOnsetMs, Speech, onsetSlackMs, endSlackMs); ok {
					eot = append(eot, float64(ms-f.Regions[i].EndMs))
				}
				if i, ok := f.nearRegion(lastOnsetMs, Backchannel, onsetSlackMs, endSlackMs); ok {
					leaked[i] = true // a backchannel that committed a turn leaked
				}
			case voice.Cancel:
				r.Cancels++
			case voice.BargeIn:
				r.BargeIns++
				if i, ok := f.nearRegion(ms, Backchannel, onsetSlackMs, 0); ok {
					r.FalseInterruptions++
					leaked[i] = true
				}
				if i, ok := f.nearRegion(ms, Speech, onsetSlackMs, endSlackMs); ok {
					intr = append(intr, float64(ms-f.Regions[i].StartMs))
				}
			}
		}
		r.Frames++
	}

	for i, reg := range f.Regions {
		switch reg.Kind {
		case Speech:
			if !openedSpeech[i] {
				r.MissedStarts++
			}
		case Backchannel:
			r.BackchannelTotal++
			if !leaked[i] {
				r.BackchannelSuppressed++
			}
		}
	}
	r.EndOfTurnDelayMs = percentiles(eot)
	r.InterruptionDelayMs = percentiles(intr)
	return r
}

// RunSuite runs every fixture with a fresh model from newModel and returns the
// results in order.
func RunSuite(fixtures []Fixture, newModel func() vad.Model) []Result {
	out := make([]Result, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, Run(f, newModel()))
	}
	return out
}

// percentiles summarizes a sample set. An empty set yields a zero Stats.
func percentiles(xs []float64) Stats {
	if len(xs) == 0 {
		return Stats{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return Stats{
		Count: len(s),
		P50:   quantile(s, 0.50),
		P90:   quantile(s, 0.90),
		P99:   quantile(s, 0.99),
		Max:   s[len(s)-1],
	}
}

// quantile returns the q-quantile of a sorted slice via nearest-rank.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
