package llm

import (
	"context"
	"encoding/json"
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

// toolTrigger is the prefix a user message uses to make the mock request a
// sandboxed shell tool call (e.g. "/sh ls -la"). It lets the default credential-
// free build exercise the full Phase 7 tool path (approval + sbx) end to end.
const toolTrigger = "/sh "

// Stream implements Provider.
func (m *MockProvider) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			lastUser = req.Messages[i].Content
			break
		}
	}

	// Tool-call path: if the latest user message begins with the trigger AND no tool
	// result has come back yet this turn, request a shell tool call. Once the loop
	// feeds the result back (a RoleTool message after the user turn), summarize it.
	if cmd, ok := strings.CutPrefix(strings.TrimSpace(lastUser), strings.TrimSpace(toolTrigger)); ok {
		if result, done := lastToolResult(req.Messages); done {
			return m.streamText(ctx, "Ran it in the sandbox. Output: "+strings.TrimSpace(result)), nil
		}
		out := make(chan Delta)
		go func() {
			defer close(out)
			args, _ := json.Marshal(map[string]string{"command": strings.TrimSpace(cmd)})
			select {
			case <-ctx.Done():
				return
			case out <- Delta{ToolCalls: []ToolCall{{ID: "call_1", Name: "shell", Arguments: string(args)}}}:
			}
			select {
			case <-ctx.Done():
			case out <- Delta{Done: true}:
			}
		}()
		return out, nil
	}

	reply := "You said: " + strings.TrimSpace(lastUser) +
		". This is a streamed mock reply — configure an LLM backend in [llm] to use a real model."
	return m.streamText(ctx, reply), nil
}

// lastToolResult returns the most recent tool-result content in the context, and
// whether one exists (i.e. a tool already ran this turn).
func lastToolResult(msgs []Message) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleTool {
			return msgs[i].Content, true
		}
		if msgs[i].Role == RoleUser {
			return "", false
		}
	}
	return "", false
}

// streamText streams reply word-by-word with the configured inter-word delay.
func (m *MockProvider) streamText(ctx context.Context, reply string) <-chan Delta {
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
	return out
}
