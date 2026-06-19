// Package llm is the streaming LLM provider abstraction for text mode. A
// Provider streams assistant text deltas for a request and is cancellable via
// context. The default provider is a credential-free mock so the whole chat
// path is runnable and testable with no setup; real backends (cloud OpenAI or a
// local llama.cpp server) are selected by config and speak the same interface.
package llm

import "context"

// Role values for a message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one entry of model context.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a streaming completion request.
type Request struct {
	Messages []Message
}

// Delta is one streamed event. Exactly one of Text/Err is meaningful, or Done
// marks normal completion.
type Delta struct {
	Text string
	Err  error
	Done bool
}

// Provider streams assistant text for a request. Implementations must stop and
// release resources when ctx is cancelled, and must close the returned channel.
type Provider interface {
	// Name identifies the provider for logs/admin (e.g. "mock", "openai").
	Name() string
	// Stream returns a channel of deltas. The final delta has Done=true (or Err
	// set). The channel is closed when streaming ends.
	Stream(ctx context.Context, req Request) (<-chan Delta, error)
}
