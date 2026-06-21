// Package events defines Hina's typed event envelope and an in-process pub/sub
// bus. The same envelope is delivered over the user/admin SSE streams now and
// over the WebRTC RTCDataChannel in the voice phases, so the wire shape is fixed
// here once. The events table is the durable source of truth behind replay.
package events

import (
	"encoding/json"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
)

// Source identifies where an event originated.
const (
	SourceClient         = "client"
	SourceServer         = "server"
	SourceRuntime        = "runtime"
	SourceSandbox        = "sandbox"
	SourceOpenAIRealtime = "openai_realtime"
)

// Core lifecycle event types implemented in Phase 1. The audio/tool/automation
// types from the plan are added in their phases but share this envelope.
const (
	TypeSessionCreated     = "SessionCreated"
	TypeSessionResumed     = "SessionResumed"
	TypeUserTextSubmitted  = "UserTextSubmitted"
	TypeTurnStarted        = "TurnStarted"
	TypeTurnCommitted      = "TurnCommitted"
	TypeAgentTextDelta     = "AgentTextDelta"
	TypeAgentTextCompleted = "AgentTextCompleted"
	TypeError              = "ErrorEvent"
)

// Phase 3 audio/live event types. They share the same envelope and flow over
// both the SSE stream (admin/owner observability) and the WebRTC events
// datachannel (the live client). In Phase 3 they are delivered live-only
// (ephemeral, seq==0) — transport proof, not durable transcript; the durable
// voice-turn record lands in Phase 6. ModeChanged doubles as the client→server
// control to select the outbound source (loopback|tone|idle) and the
// server→client acknowledgement.
const (
	TypeModeChanged      = "ModeChanged"
	TypeAudioInputFrame  = "AudioInputFrame"
	TypeAudioOutputFrame = "AudioOutputFrame"
	TypePlaybackStarted  = "PlaybackStarted"
	TypePlaybackProgress = "PlaybackProgress"
	TypePlaybackStopped  = "PlaybackStopped"
	TypeUserInterrupted  = "UserInterrupted"
)

// Phase 4 local-TTS event types. TTSStarted/TTSCompleted bracket a spoken reply
// (per conversation, alongside PlaybackStarted/Stopped on the audio path). The
// RuntimeModel* types report the shared ONNX runtime's lazy-load / idle-unload
// lifecycle and load failures — global (no conversation), for admin
// observability of the local-inference backend.
const (
	TypeSpeakText            = "SpeakText" // client->server: speak this text into the live session
	TypeTTSStarted           = "TTSStarted"
	TypeTTSCompleted         = "TTSCompleted"
	TypeRuntimeModelLoaded   = "RuntimeModelLoaded"
	TypeRuntimeModelUnloaded = "RuntimeModelUnloaded"
	TypeRuntimeModelError    = "RuntimeModelError"
)

// Phase 5 local-ASR event types. ListenStarted/ListenStopped are the
// client→server controls that delimit a speech segment (turn boundaries are
// Phase 6's VAD; here the client marks them). While listening, the server emits
// ASRPartial per decoded chunk and one ASRFinal on the segment commit, carrying
// the wake-detection result + the address-stripped request body.
const (
	TypeListenStarted = "ListenStarted" // client->server: begin feeding mic audio to ASR
	TypeListenStopped = "ListenStopped" // client->server: commit the segment -> ASRFinal
	TypeASRPartial    = "ASRPartial"
	TypeASRFinal      = "ASRFinal"
)

// Phase 6 live-voice event types. SessionUpdate is the client→server control that
// turns the live conversation loop on/off and carries the OpenAI-shaped
// turn_detection config (null turn_detection exits live mode); SessionUpdated is
// the ack. Server-VAD turn boundaries are reported as SpeechStarted/SpeechStopped
// (distinct from the manual ListenStarted/Stopped). On a confirmed barge-in the
// server emits UserInterrupted (already defined, Phase 3) + ConversationTruncated,
// which carries the assistant turn truncated to the last actually-played audio.
const (
	TypeSessionUpdate         = "SessionUpdate"  // client->server: enable/disable live mode + turn_detection
	TypeSessionUpdated        = "SessionUpdated" // server->client: live-mode ack with the active config
	TypeSpeechStarted         = "SpeechStarted"  // server->client: VAD detected speech onset (turn opened)
	TypeSpeechStopped         = "SpeechStopped"  // server->client: VAD committed/cancelled the turn
	TypeConversationTruncated = "ConversationTruncated"
)

// Phase 7 sandbox tool-execution event types. A model-requested tool runs inside
// the user's sbx sandbox; the server emits ToolCallRequested when the call is
// raised (carrying a redacted summary + whether it awaits approval), and
// ToolCallCompleted when it finishes (exit/ok/decision). They are LIVE-ONLY
// (ephemeral, not persisted/replayed) so the chat UI can render an approval card
// and tool activity in real time, but a server restart never replays a stale card
// whose in-memory pending decision is gone — the durable record of a run is the
// sandbox_runs audit table. Source is SourceSandbox; payloads NEVER carry secret
// values.
const (
	TypeToolCallRequested = "ToolCallRequested"
	TypeToolCallCompleted = "ToolCallCompleted"
)

// Event is the typed envelope. JSON field names are the wire contract; note
// ConversationID serializes as "session_id" (the product-level "session").
type Event struct {
	EventID        string          `json:"event_id"`
	ConversationID string          `json:"session_id,omitempty"`
	UserID         string          `json:"user_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	Seq            int64           `json:"seq"`
	ServerTS       time.Time       `json:"server_ts"`
	Source         string          `json:"source"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// New builds an event with a fresh id and a JSON-marshaled payload. Pass nil
// payload for an empty object.
func New(source, typ, conversationID, userID, turnID string, payload any) (Event, error) {
	raw := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, err
		}
		raw = b
	}
	return Event{
		EventID:        id.New("evt"),
		ConversationID: conversationID,
		UserID:         userID,
		TurnID:         turnID,
		Source:         source,
		Type:           typ,
		Payload:        raw,
	}, nil
}
