package voice

import "math"

// EchoSuppressor is the playback-aware layer of Hina's echo handling: while the
// assistant is speaking, the user's mic also picks up the assistant's own audio
// (residual echo the browser/WebRTC AEC didn't fully cancel). This compares the
// mic frame's energy against the recent outbound TTS energy and suppresses frames
// that are too quiet to be the user speaking over the assistant — so the assistant
// doesn't trigger its own VAD. It is a backstop, not the whole solution: browser
// AEC and the headphone path do the heavy lifting (phase-06 "layered, no single
// trick"). When the assistant is silent it never suppresses.
type EchoSuppressor struct {
	// echoFloor: a mic frame counts as the user (not echo) only when its energy
	// exceeds the tracked playback energy times this factor. The residual echo after
	// AEC is well below the played level, so a user speaking near the mic clears it.
	echoFloor float64
	// decay applies to the tracked playback energy each observed frame so it follows
	// the current loudness (and falls toward zero through pauses in the reply).
	decay float64
	// noiseFloor: energies at or below this are treated as silence (never user
	// speech), so room noise during playback isn't mistaken for a barge-in.
	noiseFloor float64

	playbackEnergy float64 // tracked recent outbound (TTS) frame energy
}

// NewEchoSuppressor builds a suppressor. Non-positive args fall back to defaults
// tuned for AEC-on browser capture.
func NewEchoSuppressor(echoFloor, decay, noiseFloor float64) *EchoSuppressor {
	if echoFloor <= 0 {
		echoFloor = 0.6
	}
	if decay <= 0 || decay >= 1 {
		decay = 0.85
	}
	if noiseFloor <= 0 {
		noiseFloor = 1e-5
	}
	return &EchoSuppressor{echoFloor: echoFloor, decay: decay, noiseFloor: noiseFloor}
}

// ObservePlayback records one outbound (assistant TTS) frame's energy so
// suppression knows how loud the assistant currently is. Call it with each frame
// sent to the browser.
func (e *EchoSuppressor) ObservePlayback(pcm []float32) {
	en := energy(pcm)
	// Track a decaying peak: jump up to a louder frame immediately, ease down so a
	// brief quiet gap mid-reply doesn't drop the guard.
	e.playbackEnergy *= e.decay
	if en > e.playbackEnergy {
		e.playbackEnergy = en
	}
}

// Suppress reports whether a mic frame captured while assistantPlaying should be
// dropped as likely echo (not fed to the VAD/ASR). It suppresses when the frame is
// near-silent (below the noise floor) or quieter than the tracked playback energy
// scaled by echoFloor. When the assistant is not playing it never suppresses.
func (e *EchoSuppressor) Suppress(micFrame []float32, assistantPlaying bool) bool {
	if !assistantPlaying {
		return false
	}
	en := energy(micFrame)
	if en <= e.noiseFloor {
		return true // silence during playback is never a barge-in
	}
	return en < e.playbackEnergy*e.echoFloor
}

// Reset clears the tracked playback energy (e.g. when a reply ends).
func (e *EchoSuppressor) Reset() { e.playbackEnergy = 0 }

// energy is the mean-square (power) of a PCM frame, a cheap loudness proxy.
func energy(pcm []float32) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return sum / float64(len(pcm))
}

// rms is the root-mean-square amplitude (exposed for metrics/tests).
func rms(pcm []float32) float64 { return math.Sqrt(energy(pcm)) }
