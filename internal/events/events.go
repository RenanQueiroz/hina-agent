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
