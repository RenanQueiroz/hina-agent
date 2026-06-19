package store

import "time"

// User is an authenticated account.
type User struct {
	ID                 string
	Username           string
	Role               string // "admin" | "user"
	PasswordHash       string
	Status             string
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// IsAdmin reports whether the user has the admin role.
func (u User) IsAdmin() bool { return u.Role == "admin" }

// AuthSession is a browser/login session (the token is stored only as a hash).
type AuthSession struct {
	ID        string
	UserID    string
	TokenHash string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Conversation is a durable chat/session history.
type Conversation struct {
	ID          string
	OwnerUserID string
	Title       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Turn is one message in a conversation; canonical text is the model-context
// source of truth for both text and voice.
type Turn struct {
	ID             string
	ConversationID string
	Role           string // user|assistant|system|tool
	Mode           string // text|voice
	CanonicalText  string
	Metadata       string // JSON
	CreatedAt      time.Time
}

// Event is one entry in the append-only event log.
type Event struct {
	EventID        string
	ConversationID string // "" for global/admin-scoped events
	UserID         string
	TurnID         string
	Seq            int64
	Source         string
	Type           string
	Payload        string // JSON
	ServerTS       time.Time
}
