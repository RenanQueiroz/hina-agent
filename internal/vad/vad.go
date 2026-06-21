// Package vad is Hina's local voice-activity detector: a pure-Go port of the
// Silero VAD streaming pipeline (research-findings B4). It turns a continuous
// 16 kHz mono mic stream into turn boundaries — speech-start (with pre-roll),
// speech-end (trailing silence), and force-commit (max duration) — that the live
// voice loop (Phase 6) uses to know when to open an ASR segment, when to commit a
// turn, and when the user is barging in over the assistant.
//
// The state machine here is build-tag-free and fully unit-testable with a fake
// probability Model (no ORT, no CGo): it consumes per-window speech probabilities
// in order and applies the standard Silero hysteresis + min-speech / min-silence /
// pre-roll / max-duration tunables (ported from V1). The real Silero ONNX model
// (a 512-sample stateful LSTM) is isolated behind the `onnx` build tag
// (model_onnx.go); the default build compiles a stub and reports VAD unavailable.
package vad

import "time"

// Audio geometry. Silero VAD consumes fixed 512-sample windows of 16 kHz mono
// float32 (32 ms each), stateful and in order; it is not valid at other rates or
// window sizes.
const (
	SampleRate = 16000
	WindowSize = 512
)

// windowDuration is one 512-sample window's wall-clock span (32 ms).
const windowDuration = time.Duration(WindowSize) * time.Second / SampleRate

// Default tunables (ported from V1's Silero config, matching Silero's own
// get_speech_timestamps defaults where V1 didn't override).
const (
	defaultThreshold   = 0.5
	negThresholdMargin = 0.15 // neg_threshold = threshold - margin (hysteresis)
	defaultMinSilence  = 700 * time.Millisecond
	defaultPreSpeech   = 300 * time.Millisecond
	defaultMinSpeech   = 250 * time.Millisecond
	defaultMaxDuration = 30 * time.Second
)

// Params are the VAD tunables. Zero fields fall back to the defaults; clamp/normalize
// happens in normalize so a partial Params is always usable.
type Params struct {
	// Threshold is the speech-onset probability (0..1). A window at or above it
	// while silent triggers speech; the offset uses Threshold-NegMargin for
	// hysteresis so a brief dip doesn't end the turn.
	Threshold float64
	// MinSilence is the trailing sub-threshold span that ends a turn (server-VAD
	// silence_duration). Shorter gaps are treated as within-utterance pauses.
	MinSilence time.Duration
	// PreSpeech is how much audio before the onset is kept and prepended to the
	// segment (prefix padding) so the first phoneme isn't clipped from the ASR.
	PreSpeech time.Duration
	// MinSpeech discards a segment shorter than this as a blip/false start (the
	// detector reports Cancel instead of End), so a cough or stray click doesn't
	// open a turn or barge in.
	MinSpeech time.Duration
	// MaxDuration force-commits a turn that has run this long without a natural
	// end (reported as Max), bounding how long one segment can pin inference.
	MaxDuration time.Duration
}

// normalize returns a copy with non-positive / out-of-range fields replaced by
// defaults, so the detector always runs with sane values.
func (p Params) normalize() Params {
	if p.Threshold <= 0 || p.Threshold > 1 {
		p.Threshold = defaultThreshold
	}
	if p.MinSilence <= 0 {
		p.MinSilence = defaultMinSilence
	}
	if p.PreSpeech <= 0 {
		p.PreSpeech = defaultPreSpeech
	}
	if p.MinSpeech <= 0 {
		p.MinSpeech = defaultMinSpeech
	}
	if p.MaxDuration <= 0 {
		p.MaxDuration = defaultMaxDuration
	}
	return p
}

// negThreshold is the speech-offset probability (hysteresis lower bound).
func (p Params) negThreshold() float64 {
	n := p.Threshold - negThresholdMargin
	if n < 0 {
		n = 0
	}
	return n
}

// Decision is the turn-boundary outcome the Detector reports for one window.
type Decision int

const (
	// Continue: no boundary this window (silence, or speech in progress).
	Continue Decision = iota
	// Start: speech onset confirmed this window — open the segment (with pre-roll).
	Start
	// End: trailing silence reached MinSilence — commit the turn.
	End
	// Cancel: the segment that just ended was shorter than MinSpeech — discard it
	// (a blip/false start), don't commit a turn or count it as a barge-in.
	Cancel
	// Max: the segment hit MaxDuration without a natural end — force-commit it.
	Max
)

// Detector is the pure-Go online Silero turn-boundary state machine. Feed it the
// per-window speech probability (from a Model), in order; it returns the boundary
// Decision for that window. It owns no audio: Stream wraps it with the sample
// buffering + pre-roll ring. Not safe for concurrent use (single-stream state).
type Detector struct {
	params Params

	triggered  bool  // inside a speech segment
	window     int64 // count of windows seen (monotonic clock in window units)
	speechFrom int64 // window index where the current segment started
	tempEnd    int64 // window index where the current trailing-silence run began (0 = none)
}

// NewDetector builds a Detector with the given params (defaults fill the zero
// fields).
func NewDetector(p Params) *Detector {
	return &Detector{params: p.normalize()}
}

// Params returns the normalized tunables in effect.
func (d *Detector) Params() Params { return d.params }

// InSpeech reports whether the detector is currently inside a speech segment
// (after a Start, before the matching End/Cancel/Max).
func (d *Detector) InSpeech() bool { return d.triggered }

// Reset clears the segment state (not the params), for reuse on a new stream.
func (d *Detector) Reset() {
	d.triggered = false
	d.window = 0
	d.speechFrom = 0
	d.tempEnd = 0
}

// Push advances the state machine by one window with speech probability prob and
// returns the boundary Decision. The algorithm is Silero's online VADIterator with
// V1's tunables: rising edge at Threshold triggers Start immediately (low-latency
// onset / barge-in); a sub-NegThreshold run lasting MinSilence ends the turn,
// reported End if the segment met MinSpeech else Cancel; a segment reaching
// MaxDuration is force-committed as Max.
func (d *Detector) Push(prob float64) Decision {
	cur := d.window
	d.window++
	thr := d.params.Threshold
	neg := d.params.negThreshold()

	if !d.triggered {
		if prob >= thr {
			d.triggered = true
			d.speechFrom = cur
			d.tempEnd = 0
			return Start
		}
		return Continue
	}

	// Inside a segment. Force-commit if it has run too long.
	if dur := time.Duration(cur-d.speechFrom+1) * windowDuration; dur >= d.params.MaxDuration {
		d.triggered = false
		d.tempEnd = 0
		return Max
	}

	if prob >= thr {
		// Speech (re)confirmed: any tentative trailing-silence run is void.
		d.tempEnd = 0
		return Continue
	}
	if prob < neg {
		if d.tempEnd == 0 {
			d.tempEnd = cur // silence run begins here
		}
		if silence := time.Duration(cur-d.tempEnd+1) * windowDuration; silence >= d.params.MinSilence {
			d.triggered = false
			start, end := d.speechFrom, d.tempEnd
			d.tempEnd = 0
			// MinSpeech is measured to where speech actually stopped (tempEnd), not
			// including the trailing silence, so a short blip followed by a long gap is
			// still correctly discarded.
			if speech := time.Duration(end-start) * windowDuration; speech < d.params.MinSpeech {
				return Cancel
			}
			return End
		}
	}
	// prob in [neg, thr): ambiguous — hold the segment open without resetting a
	// silence run already in progress (matches Silero: only >=thr clears tempEnd).
	return Continue
}
