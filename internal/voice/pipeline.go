package voice

import (
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/vad"
)

// EventKind classifies a turn-detection decision the Pipeline emits.
type EventKind int

const (
	// Open: a new user turn began — open an ASR segment and feed PCM (the pre-roll).
	Open EventKind = iota
	// Audio: ongoing user speech for the active segment — feed PCM to the ASR.
	Audio
	// Commit: the user turn is complete — finalize the ASR segment and run the agent.
	Commit
	// Cancel: discard the active segment (a blip, or a backchannel heard during the
	// assistant's reply) — it is not a turn.
	Cancel
	// BargeIn: a confirmed user interruption during the assistant's reply — truncate
	// playback and cancel the in-flight reply. The capture continues toward a Commit.
	BargeIn
)

// Event is one turn-detection decision with any associated audio (Open/Audio carry
// PCM; the others carry nil). PCM is freshly allocated and may be retained.
type Event struct {
	Kind EventKind
	PCM  []float32
}

// pState is the Pipeline's logical-turn state, sitting above the VAD's raw speech
// segments (one logical turn can span several VAD segments when semantic VAD waits
// through a pause for the user to continue).
type pState int

const (
	stIdle    pState = iota // no active user turn
	stCapture               // capturing a user turn (ASR segment open)
	stAwait                 // semantic VAD: turn paused on an incomplete utterance, awaiting continuation
)

// defaultBargeInConfirm is the sustained-speech fallback for confirming a barge-in
// when ASR partials lag: speech this long during playback that isn't a known
// backchannel interrupts the reply even before any partial classifies it.
const defaultBargeInConfirm = 700 * time.Millisecond

// Pipeline turns a raw VAD stream into natural turn boundaries by layering the
// semantic turn detector, the backchannel filter, and echo suppression on top. The
// live rtc loop and the benchmark harness drive the same Pipeline: feed it mic
// frames + whether the assistant is currently playing (Write), feed back ASR
// partials (Observe), and feed it outbound TTS frames for echo tracking
// (ObservePlayback). It is single-goroutine (drive from one loop), like vad.Stream.
type Pipeline struct {
	cfg  TurnDetection
	vad  *vad.Stream
	sem  *Semantic // nil for server_vad
	bc   *Backchannel
	echo *EchoSuppressor

	bargeInConfirm time.Duration

	state                 pState
	partial               string        // latest ASR partial for the active segment
	awaitSilence          time.Duration // trailing silence accrued while stAwait
	playing               bool          // assistant playback state for the current Write
	speechSamples         int64         // confirmed-speech samples in the active turn (barge-in fallback)
	bargeConfirmed        bool          // a barge-in was already confirmed this turn
	startedDuringPlayback bool          // the active turn opened while the assistant was speaking
}

// NewPipeline builds a Pipeline over an already-constructed vad.Stream (the rtc
// engine or a test builds it). bc/echo may be nil (defaults are used); the
// semantic detector is created only for semantic_vad.
func NewPipeline(cfg TurnDetection, v *vad.Stream, bc *Backchannel, echo *EchoSuppressor) *Pipeline {
	cfg = cfg.Normalize()
	var sem *Semantic
	if cfg.Type == SemanticVAD {
		sem = NewSemantic(cfg.Eagerness)
	}
	if bc == nil {
		bc = NewBackchannel(nil, 0, true)
	}
	if echo == nil {
		echo = NewEchoSuppressor(0, 0, 0)
	}
	return &Pipeline{cfg: cfg, vad: v, sem: sem, bc: bc, echo: echo, bargeInConfirm: defaultBargeInConfirm}
}

// Config returns the normalized turn-detection config in effect.
func (p *Pipeline) Config() TurnDetection { return p.cfg }

// Observe feeds the latest ASR partial transcript for the active segment, used by
// the semantic commit + backchannel barge-in decisions.
func (p *Pipeline) Observe(partial string) { p.partial = partial }

// ObservePlayback records one outbound TTS frame so echo suppression knows how loud
// the assistant currently is. Call it for each frame sent to the browser.
func (p *Pipeline) ObservePlayback(pcm []float32) { p.echo.ObservePlayback(pcm) }

// ResetEcho clears the echo suppressor's tracked playback energy (call when the
// assistant's reply stops, e.g. a barge-in), so a stale guard can't linger.
func (p *Pipeline) ResetEcho() { p.echo.Reset() }

// Write consumes one chunk of 16 kHz mono mic PCM plus whether the assistant is
// currently playing, and returns the ordered turn-detection events. Echo-likely
// frames are gated to silence before the VAD so the assistant's own audio doesn't
// open a turn.
func (p *Pipeline) Write(pcm []float32, assistantPlaying bool) ([]Event, error) {
	p.playing = assistantPlaying
	fed := pcm
	if assistantPlaying && p.echo.Suppress(pcm, true) {
		// Likely the assistant's residual echo: feed silence so the VAD's window
		// cadence + silence timing stay intact, but it can't trigger a (false) turn.
		fed = make([]float32, len(pcm))
	}
	vadEvents, err := p.vad.Write(fed)
	var out []Event
	for _, ev := range vadEvents {
		out = append(out, p.handle(ev)...)
	}
	// Per-chunk accounting: semantic await-timeout + the barge-in sustained-speech
	// fallback (both advance by this chunk's duration).
	out = append(out, p.tick(frameDuration(pcm))...)
	return out, err
}

// handle maps one VAD event to the logical-turn state machine.
func (p *Pipeline) handle(ev vad.Event) []Event {
	switch ev.Kind {
	case vad.EvStart:
		switch p.state {
		case stIdle:
			p.state = stCapture
			p.resetTurn()
			return []Event{{Kind: Open, PCM: ev.PCM}}
		case stAwait:
			// Continuation of the same logical turn after a semantic pause: keep the
			// ASR segment open, bridge the gap with this onset's pre-roll.
			p.state = stCapture
			p.awaitSilence = 0
			return []Event{{Kind: Audio, PCM: ev.PCM}}
		}
	case vad.EvSpeech:
		if p.state == stCapture {
			p.speechSamples += int64(len(ev.PCM))
			return []Event{{Kind: Audio, PCM: ev.PCM}}
		}
	case vad.EvEnd:
		if p.state == stCapture {
			var out []Event
			if p.startedDuringPlayback && !p.bargeConfirmed {
				// Discard the segment as a backchannel ONLY when a NON-EMPTY partial
				// classifies it as one. An empty / not-yet-arrived partial (the ASR
				// callback lagging behind a short utterance) is NOT assumed to be a
				// backchannel — it's treated as a real interruption below, so a short
				// command like "stop" with no partial yet still truncates the reply and
				// commits (the Commit finalizes the recognizer, capturing the late audio)
				// rather than being silently dropped while the assistant talks on.
				if p.partial != "" && p.bc.IsBackchannel(p.partial) {
					p.state = stIdle
					return []Event{{Kind: Cancel}}
				}
				// A real (non-backchannel) utterance over the assistant that ended before
				// the sustained-speech fallback or the >=minWords partial confirmed it (a
				// short one-word command like "stop") IS still an interruption: confirm the
				// barge-in now so the reply is truncated before this turn commits.
				if p.cfg.InterruptsResponse() {
					p.bargeConfirmed = true
					out = append(out, Event{Kind: BargeIn})
				}
			}
			if p.sem != nil && !p.sem.Commit(p.partial, semanticBaseSilence) {
				// Looks unfinished — hold the turn open for a continuation (after any
				// barge-in already emitted above).
				p.state = stAwait
				p.awaitSilence = semanticBaseSilence
				return out
			}
			p.state = stIdle
			return append(out, Event{Kind: Commit})
		}
	case vad.EvCancel:
		if p.state == stCapture {
			p.state = stIdle
			return []Event{{Kind: Cancel}}
		}
	case vad.EvMax:
		if p.state == stCapture || p.state == stAwait {
			p.state = stIdle
			return []Event{{Kind: Commit}}
		}
	}
	return nil
}

// tick advances the per-chunk timers: the semantic await-timeout and the barge-in
// sustained-speech fallback.
func (p *Pipeline) tick(dur time.Duration) []Event {
	if p.state == stAwait && p.sem != nil {
		p.awaitSilence += dur
		if p.sem.Commit(p.partial, p.awaitSilence) {
			p.state = stIdle
			return []Event{{Kind: Commit}}
		}
	}
	if p.playing && p.state == stCapture && !p.bargeConfirmed && p.cfg.InterruptsResponse() {
		if p.confirmBargeIn() {
			p.bargeConfirmed = true
			return []Event{{Kind: BargeIn}}
		}
	}
	return nil
}

// confirmBargeIn decides whether the in-progress speech during playback is a real
// interruption: confirmed once a partial accumulates enough non-backchannel words,
// or as a fallback once speech sustains past bargeInConfirm and the partial so far
// isn't a known backchannel (an empty partial — ASR lag — still allows the
// fallback so a barge-in isn't missed when partials are slow).
func (p *Pipeline) confirmBargeIn() bool {
	if p.bc.Interrupts(p.partial) {
		return true
	}
	knownBackchannel := p.partial != "" && p.bc.IsBackchannel(p.partial)
	return !knownBackchannel && time.Duration(p.speechSamples)*time.Second/vad.SampleRate >= p.bargeInConfirm
}

// resetTurn clears per-turn state at a fresh Open.
func (p *Pipeline) resetTurn() {
	p.partial = ""
	p.awaitSilence = 0
	p.speechSamples = 0
	p.bargeConfirmed = false
	p.startedDuringPlayback = p.playing
}

// Reset returns the Pipeline to idle and resets the underlying VAD + echo state
// (e.g. when a live session restarts the conversation).
func (p *Pipeline) Reset() {
	p.state = stIdle
	p.resetTurn()
	p.echo.Reset()
	p.vad.Reset()
}

// Close releases the underlying VAD stream's model bundle.
func (p *Pipeline) Close() error { return p.vad.Close() }

// frameDuration is the wall-clock span of a 16 kHz mono PCM chunk.
func frameDuration(pcm []float32) time.Duration {
	return time.Duration(len(pcm)) * time.Second / vad.SampleRate
}
