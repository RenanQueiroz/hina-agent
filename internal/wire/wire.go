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

// ErrorResponse is the shape of all error bodies.
type ErrorResponse struct {
	Error string `json:"error"`
}
