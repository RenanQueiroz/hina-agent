package llm

import (
	"context"
	"strings"
	"time"
)

// MockProvider streams a deterministic reply word-by-word with a small inter-word
// delay, so the streaming UI and cancellation path are exercised without any
// credentials. It echoes the last user message so behavior is observable.
type MockProvider struct {
	// WordDelay between streamed words (default 40ms).
	WordDelay time.Duration
}

// NewMockProvider builds a mock provider.
func NewMockProvider() *MockProvider { return &MockProvider{WordDelay: 40 * time.Millisecond} }

// Name implements Provider.
func (m *MockProvider) Name() string { return "mock" }

// Stream implements Provider.
func (m *MockProvider) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			lastUser = req.Messages[i].Content
			break
		}
	}
	reply := "You said: " + strings.TrimSpace(lastUser) +
		". This is a streamed mock reply — configure an LLM backend in [llm] to use a real model."

	delay := m.WordDelay
	if delay <= 0 {
		delay = 40 * time.Millisecond
	}

	out := make(chan Delta)
	go func() {
		defer close(out)
		words := strings.Fields(reply)
		for i, word := range words {
			chunk := word
			if i < len(words)-1 {
				chunk += " "
			}
			select {
			case <-ctx.Done():
				return // cancelled: stop streaming, channel closes
			case out <- Delta{Text: chunk}:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		select {
		case <-ctx.Done():
		case out <- Delta{Done: true}:
		}
	}()
	return out, nil
}
