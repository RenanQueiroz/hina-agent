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
	"context"
	"log/slog"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/RenanQueiroz/hina-agent/internal/vad"
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

// VADEngine produces per-session voice-activity-detection streams for the live
// loop. *vad.Engine satisfies it; an interface keeps rtc testable with a fake VAD.
type VADEngine interface {
	Available() bool
	NewStream(ctx context.Context, p vad.Params) (*vad.Stream, error)
}

// AgentService runs one conversational turn for the live-voice loop and persists
// it to the shared timeline, so spoken turns render alongside text turns and a
// text↔live switch preserves context (no audio rehydration). httpapi implements it
// over the store + event bus + the shared agent.Loop; an interface keeps rtc free
// of those dependencies and easy to test. onDelta streams assistant text as it is
// generated (for the live timeline + TTS). ctx cancellation (barge-in / teardown)
// interrupts the reply with the partial preserved.
type AgentService interface {
	// RunTurn persists the user transcript + runs the agent, returning the assistant
	// reply text and the durable assistant turn id. A non-nil error means the turn was
	// NOT durably persisted, so the caller must not speak it. onCommitted, if non-nil, is
	// invoked with the committed assistant turn id AFTER the durable commit but BEFORE
	// RunTurn releases the per-conversation turn lock — so the live loop can record the
	// turn id (and reserve the interrupt/playback fence) under that lock, closing the gap
	// where a next turn could read the just-committed reply.
	RunTurn(ctx context.Context, convID, userID, transcript string, onDelta func(string), onCommitted func(turnID string)) (reply, turnID string, err error)
	// MarkTurnInterrupted durably records that an already-committed assistant turn was
	// interrupted by a barge-in that truncated its spoken playback at playedMs, so a
	// reload / the next model context reflects that the user heard only a prefix.
	MarkTurnInterrupted(ctx context.Context, convID, userID, turnID string, playedMs int64) error
	// BeginInterrupt reserves a per-conversation interrupt FENCE and returns its release.
	// The live loop calls it SYNCHRONOUSLY before an interrupt becomes observable, then
	// MarkTurnInterrupted, then release — so a next turn (voice or a concurrent text POST)
	// can't build context until the interrupt mark commits.
	BeginInterrupt(convID string) (release func())
}

// Config configures the Manager. ICEServers is optional: localhost and most LAN
// setups connect on host candidates alone, so the default (none) works without
// any external STUN/TURN dependency. TTS is the optional local speech engine
// (Phase 4) and ASR the optional local recognition engine (Phase 5); when nil or
// unavailable, SpeakText / ListenStarted requests are rejected.
type Config struct {
	ICEServers []webrtc.ICEServer
	TTS        tts.Engine
	ASR        asr.Engine
	VAD        VADEngine    // optional local VAD engine (Phase 6); nil disables live mode
	Agent      AgentService // optional agent turn-runner (Phase 6); nil disables live replies
	Log        *slog.Logger
}
