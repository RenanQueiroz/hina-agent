// Package wire defines the JSON DTOs exchanged with the web client. These Go
// structs are the source of truth for the generated TypeScript types
// (web/src/lib/api.gen.ts, produced by tygo — see tygo.yaml). Keep the JSON tags
// stable; regenerate after changes (`make gen-ts`).
package wire

import "time"

// User is the safe public projection of an account.
type User struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
}

// LoginResponse is returned by POST /auth/login and GET /auth/me.
type LoginResponse struct {
	User User `json:"user"`
}

// Conversation is a chat/session summary.
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConversationList is the GET /conversations response.
type ConversationList struct {
	Conversations []Conversation `json:"conversations"`
}

// Turn is one message in a conversation.
type Turn struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Mode      string    `json:"mode"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// TurnList is the GET /conversations/{id}/turns response.
type TurnList struct {
	Turns []Turn `json:"turns"`
}

// PostMessageResponse is returned when a streamed turn completes.
type PostMessageResponse struct {
	UserTurnID      string `json:"user_turn_id"`
	AssistantTurnID string `json:"assistant_turn_id"`
	Text            string `json:"text"`
	Interrupted     bool   `json:"interrupted"`
}

// ConfigInfo is non-sensitive server info for the client header.
type ConfigInfo struct {
	AgentName   string `json:"agent_name"`
	LLMProvider string `json:"llm_provider"`
}

// AdminLLMInfo describes the active text backend (admin-only).
type AdminLLMInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
}

// RTCSessionStats is one live voice session's metrics for the admin view: app
// counters plus Pion's network stats (loss/jitter/RTT). Mirrors
// rtc.SessionStats; the realtime package stays decoupled from this wire format.
type RTCSessionStats struct {
	SessionID      string `json:"session_id"`
	UserID         string `json:"user_id"`
	ConversationID string `json:"conversation_id"`
	Mode           string `json:"mode"`
	UptimeMs       int64  `json:"uptime_ms"`

	RTPPacketsIn  uint64 `json:"rtp_packets_in"`
	DecodeErrors  uint64 `json:"decode_errors"`
	FramesOut     uint64 `json:"frames_out"`
	BytesOut      uint64 `json:"bytes_out"`
	FramesDropped uint64 `json:"frames_dropped"`
	Interrupts    uint64 `json:"interrupts"`
	DroppedTurns  uint64 `json:"dropped_turns"`

	PlayedMs     int64 `json:"played_ms"`
	CaptureMs    int64 `json:"capture_ms"`
	AppRTTMicros int64 `json:"app_rtt_micros"`

	PacketsReceived uint32  `json:"packets_received"`
	PacketsLost     int32   `json:"packets_lost"`
	JitterSeconds   float64 `json:"jitter_seconds"`
}

// RTCStats is the GET /admin/rtc response: every active live session.
type RTCStats struct {
	Sessions []RTCSessionStats `json:"sessions"`
}

// AdminRuntime is the GET /admin/runtime response: local-inference backend status.
type AdminRuntime struct {
	LLMProvider string     `json:"llm_provider"`
	TTS         TTSRuntime `json:"tts"`
	ASR         ASRRuntime `json:"asr"`
	VAD         VADRuntime `json:"vad"`
}

// VADRuntime is the local Silero VAD engine + ONNX runtime status for the admin
// view. The live conversation loop (Phase 6) needs this plus TTS + ASR available.
type VADRuntime struct {
	Enabled    bool       `json:"enabled"` // [voice] enabled
	Available  bool       `json:"available"`
	Loaded     bool       `json:"loaded"` // model currently resident (warm)
	Reason     string     `json:"reason,omitempty"`
	Runtime    ORTRuntime `json:"runtime"`
	ColdLoadMs int64      `json:"cold_load_ms"`
	ProbeCount int64      `json:"probe_count"`
	ErrorCount int64      `json:"error_count"`
	LastError  string     `json:"last_error,omitempty"`
}

// ASRRuntime is the local streaming ASR (Nemotron) engine + ONNX runtime status
// for the admin view.
type ASRRuntime struct {
	Enabled     bool       `json:"enabled"`
	Available   bool       `json:"available"`
	Loaded      bool       `json:"loaded"` // models currently resident (warm)
	Language    string     `json:"language"`
	Biasing     bool       `json:"biasing"` // agent-name biasing active
	Reason      string     `json:"reason,omitempty"`
	Runtime     ORTRuntime `json:"runtime"`
	ColdLoadMs  int64      `json:"cold_load_ms"`  // last cold model-load latency
	LastChunkMs int64      `json:"last_chunk_ms"` // last chunk decode latency
	ChunkCount  int64      `json:"chunk_count"`
	ErrorCount  int64      `json:"error_count"`
	LastError   string     `json:"last_error,omitempty"`
}

// TTSRuntime is the local TTS engine + ONNX runtime status for the admin view.
type TTSRuntime struct {
	Enabled     bool       `json:"enabled"`
	Available   bool       `json:"available"`
	Loaded      bool       `json:"loaded"` // models currently resident (warm)
	Voice       string     `json:"voice"`
	Lang        string     `json:"lang"`
	Steps       int        `json:"steps"`
	Reason      string     `json:"reason,omitempty"` // why unavailable
	Runtime     ORTRuntime `json:"runtime"`
	ColdLoadMs  int64      `json:"cold_load_ms"`  // last cold model-load latency
	LastSynthMs int64      `json:"last_synth_ms"` // last per-sentence synth latency
	SynthCount  int64      `json:"synth_count"`
	ErrorCount  int64      `json:"error_count"`
	LastError   string     `json:"last_error,omitempty"`
}

// ORTRuntime is the ONNX Runtime library status (version/provider/lib path).
type ORTRuntime struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Provider  string `json:"provider,omitempty"`
	LibPath   string `json:"lib_path,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AdminUser is a user row in the admin list.
type AdminUser struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	Role               string    `json:"role"`
	Status             string    `json:"status"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
}

// AdminUserList is the GET /admin/users response.
type AdminUserList struct {
	Users []AdminUser `json:"users"`
}

// --- Phase 7: sandbox + secret vault ---

// SandboxEnvironment is a user's Sandbox Environment policy (GET/PUT
// /sandbox/environment). AvailableTools is server-supplied (the built-in tool
// set) so the UI can offer the full list; AllowedTools is the user's selection.
type SandboxEnvironment struct {
	AllowedTools   []string             `json:"allowed_tools"`
	AvailableTools []string             `json:"available_tools"`
	MCPServers     []SandboxMCPServer   `json:"mcp_servers"`
	Network        SandboxNetworkPolicy `json:"network"`
	WritableMounts []string             `json:"writable_mounts"`
	SecretGrants   []SandboxSecretGrant `json:"secret_grants"`
}

// SandboxMCPServer is a configured MCP server (name + url).
type SandboxMCPServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SandboxNetworkPolicy is the default posture plus the host:port allow-list.
type SandboxNetworkPolicy struct {
	Default string               `json:"default"` // deny | allow
	Allow   []SandboxNetworkRule `json:"allow"`
}

// SandboxNetworkRule is one host:port allow-list entry.
type SandboxNetworkRule struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// SandboxSecretGrant binds a vaulted secret to its injected env-var name.
type SandboxSecretGrant struct {
	SecretID string `json:"secret_id"`
	EnvName  string `json:"env_name"`
}

// Secret is the non-sensitive metadata for a vaulted secret (never the value).
type Secret struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SecretList is the GET /sandbox/secrets response.
type SecretList struct {
	Secrets []Secret `json:"secrets"`
}

// AdminSandbox is the GET /admin/sandbox response: runtime status + usage.
type AdminSandbox struct {
	Runtime SandboxRuntime     `json:"runtime"`
	Users   []SandboxUserUsage `json:"users"`
	Runs    []SandboxRunInfo   `json:"runs"`
}

// SandboxRuntime is the sbx runner status for the admin view.
type SandboxRuntime struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Pinned    string `json:"pinned"`
	Path      string `json:"path,omitempty"`
	Approval  string `json:"approval"`
	Reason    string `json:"reason,omitempty"`
}

// SandboxUserUsage is one user's sandbox storage + run footprint.
type SandboxUserUsage struct {
	UserID         string `json:"user_id"`
	Username       string `json:"username"`
	WorkspaceBytes int64  `json:"workspace_bytes"`
	RunCount       int    `json:"run_count"`
}

// SandboxRunInfo is one audit-log row (no secret values; command is redacted).
type SandboxRunInfo struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	ConversationID string    `json:"conversation_id"`
	Tool           string    `json:"tool"`
	Decision       string    `json:"decision"`
	ExitCode       int       `json:"exit_code"`
	DurationMs     int64     `json:"duration_ms"`
	Command        string    `json:"command"`
	Error          string    `json:"error"`
	CreatedAt      time.Time `json:"created_at"`
}

// ErrorResponse is the shape of all error bodies.
type ErrorResponse struct {
	Error string `json:"error"`
}
