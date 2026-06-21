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
	RoleTool      = "tool" // a tool-call result fed back into context
)

// Message is one entry of model context.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolCall is one model-requested tool invocation surfaced by a tool-capable
// provider. Arguments is the raw JSON arguments object. The agent loop routes it
// to the per-user approval + sandbox layer (Phase 7) and feeds the result back as
// a RoleTool message for the next round.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Request is a streaming completion request.
type Request struct {
	Messages []Message
}

// Delta is one streamed event. Exactly one of Text/ToolCalls/Err is meaningful,
// or Done marks normal completion. A provider that wants to call tools emits a
// delta with ToolCalls (and no Text); the loop executes them and re-streams.
type Delta struct {
	Text      string
	ToolCalls []ToolCall
	Err       error
	Done      bool
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
