// Package rtc is Hina's WebRTC media bridge (research-findings B5). A browser
// peer captures the mic and sends it as Opus over WebRTC; the server decodes it
// in pure Go, and sends audio BACK as raw 24 kHz s16 PCM over an unreliable
// RTCDataChannel (no Opus encoder needed). A second, reliable datachannel
// carries the Phase 1 typed event envelope as JSON control messages in both
// directions.
//
// This phase proves the transport with no models attached: the inbound PCM is
// metered and discarded (Phase 5/6 attach ASR), and the outbound PCM comes from
// a loopback of the decoded mic or a generated tone. Everything is CGo-free
// (Pion v4 + pion/opus + the pure-Go resampler), so it cross-compiles to every
// Tier-1 target with no native toolchain.
package rtc

import (
	"log/slog"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/pion/webrtc/v4"
)

// Datachannel labels. The browser creates both channels in its offer; the
// server picks them up via OnDataChannel (no server-initiated renegotiation).
// "events" mirrors OpenAI's reliable control channel; "audio" is unordered and
// unreliable (maxRetransmits:0) so a lost PCM frame is skipped, never
// head-of-line-blocking the live stream.
const (
	channelEvents = "events"
	channelAudio  = "audio"
)

// Outbound source modes, selected by the client over the events channel with a
// ModeChanged{mode} message and echoed back by the server. ModeTTS is entered by
// a SpeakText request (not ModeChanged) and reported in stats while a synthesized
// reply is playing.
const (
	ModeIdle     = "idle"
	ModeLoopback = "loopback"
	ModeTone     = "tone"
	ModeTTS      = "tts"
)

// toneFrequencyHz / toneAmplitude define the built-in test tone (a calm A4 at a
// modest level so it can't clip or startle on headphones).
const (
	toneFrequencyHz = 440.0
	toneAmplitude   = 0.3
)

// EventSink receives events the live session also wants visible on the SSE
// stream (admin/owner observability). It is the bus's PublishEphemeral in
// production; an interface keeps rtc decoupled from httpapi and easy to test.
type EventSink interface {
	PublishEphemeral(e events.Event)
}

// Config configures the Manager. ICEServers is optional: localhost and most LAN
// setups connect on host candidates alone, so the default (none) works without
// any external STUN/TURN dependency. TTS is the optional local speech engine
// (Phase 4); when nil or unavailable, SpeakText requests are rejected.
type Config struct {
	ICEServers []webrtc.ICEServer
	TTS        tts.Engine
	Log        *slog.Logger
}
