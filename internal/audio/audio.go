// Package audio holds Hina's pure-Go audio primitives for the WebRTC voice
// path: sample-rate conversion, PCM<->float32 conversion, a tone generator, and
// the binary framing used to push raw PCM over the RTCDataChannel. Everything
// here is CGo-free and deterministic so it cross-compiles to every Tier-1 target
// and is unit-testable without a browser or sound device.
//
// The fixed rates mirror research-findings B5: mic audio arrives as 48 kHz Opus
// (decoded upstream), is downsampled to 16 kHz mono for the (Phase 6) ASR
// consumer, and outbound audio is sent to the browser as 24 kHz s16 mono PCM to
// keep datachannel bandwidth modest (~384 kbps) while needing no Opus encoder.
package audio

const (
	// InputSampleRate is the PCM rate the Opus decoder emits (WebRTC Opus is
	// 48 kHz).
	InputSampleRate = 48000
	// ASRSampleRate is the downsample target for the speech/ASR consumer.
	ASRSampleRate = 16000
	// OutputSampleRate is the rate of PCM pushed to the browser over the
	// datachannel.
	OutputSampleRate = 24000
	// Channels is the channel count across the voice path: mono everywhere.
	Channels = 1
	// FrameMillis is the outbound pacing/frame size in milliseconds (~20 ms is
	// one Opus packet's worth and a comfortable datachannel write cadence).
	FrameMillis = 20
	// OutputFrameSamples is the number of 24 kHz mono samples in one 20 ms
	// outbound frame.
	OutputFrameSamples = OutputSampleRate * FrameMillis / 1000 // 480
)
