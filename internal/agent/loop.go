package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
)

// maxToolRounds caps how many tool-call rounds one turn may run before the loop
// stops, so a misbehaving model that keeps requesting tools can't spin forever.
const maxToolRounds = 8

// ErrToolRoundLimit is returned when a turn exceeds maxToolRounds.
var ErrToolRoundLimit = errors.New("agent: tool-call round limit reached")

// Loop runs a single agent turn: it streams assistant text from the LLM provider,
// accumulates the reply, and (once providers surface them) routes tool calls to
// the approval/sandbox layer. It is the ONE turn-execution path that text mode and
// live voice both use, so the two interaction modes can never drift — text chat
// streams it to the timeline, the voice loop streams it into TTS.
//
// Cancellation via ctx (a client abort, or a barge-in mid-reply) is classified as
// an interrupt, NOT a backend error: the partial text is preserved and the turn is
// committed as interrupted rather than failed. A genuine mid-stream provider error
// is surfaced as Result.Err.
type Loop struct {
	provider llm.Provider
	tools    ToolHook
}

// ToolHook is the extension point for tool calls. On a tool call the loop routes
// it here, where Phase 7 plugs in per-user approval + the sbx sandbox; a
// cloud-hosted-tools provider may also satisfy it server-side. It MUST honor ctx
// cancellation. nil disables tool execution (the Phase 6 default): the hook exists
// and is threaded, but the streaming provider abstraction does not yet emit tool
// calls, so it is dormant until Phase 7 wires a tool-capable provider.
type ToolHook func(ctx context.Context, call ToolCall) (ToolResult, error)

// ToolCall is one model-requested tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the outcome of a tool call, fed back into the model context. Err
// (non-empty) reports a tool failure the model should see and recover from.
type ToolResult struct {
	Content string `json:"content"`
	Err     string `json:"err,omitempty"`
}

// Result is the outcome of one turn. Text is the accumulated assistant reply (it
// may be partial when Interrupted or Err). Interrupted means ctx was cancelled
// (client abort / barge-in) — not a failure, so the partial is kept. Err is a
// genuine backend/stream error (never set when Interrupted).
type Result struct {
	Text        string
	Interrupted bool
	Err         error
}

// NewLoop builds a Loop over a provider. tools may be nil (no tool execution).
func NewLoop(provider llm.Provider, tools ToolHook) *Loop {
	return &Loop{provider: provider, tools: tools}
}

// Run streams one assistant turn for msgs. onDelta (optional) is invoked for each
// text delta as it arrives — text mode publishes it to the timeline, the voice
// loop feeds it to sentence-chunked TTS. It blocks until the stream completes, ctx
// is cancelled, or a backend error occurs, then returns the accumulated reply with
// its interrupted/errored classification. The caller owns persistence: Run never
// touches the store, so the durable-turn semantics stay with each mode's commit.
func (l *Loop) Run(ctx context.Context, msgs []llm.Message, onDelta func(string)) Result {
	var full strings.Builder
	// work is the running context: the caller's messages, extended each round with
	// the assistant's text and the tool results so the model sees them next round.
	work := append([]llm.Message(nil), msgs...)

	for round := 0; ; round++ {
		text, calls, streamErr := l.stream(ctx, work, onDelta)
		full.WriteString(text)

		// A client abort / barge-in can surface as either a stream error or a context
		// cancellation; either way it's an interrupt, not a backend failure — so the
		// partial reply is preserved and committed as interrupted, never errored.
		if ctx.Err() != nil {
			return Result{Text: full.String(), Interrupted: true}
		}
		if streamErr != nil {
			return Result{Text: full.String(), Err: streamErr}
		}
		// No tool calls (or no hook to run them) — this is the final assistant reply.
		if len(calls) == 0 || l.tools == nil {
			return Result{Text: full.String()}
		}
		if round >= maxToolRounds {
			return Result{Text: full.String(), Err: ErrToolRoundLimit}
		}

		// Execute each requested tool through the hook (per-user approval + sandbox)
		// and feed the results back as RoleTool messages for the next round.
		if text != "" {
			work = append(work, llm.Message{Role: llm.RoleAssistant, Content: text})
		}
		for _, c := range calls {
			result, err := l.tools(ctx, ToolCall{ID: c.ID, Name: c.Name, Arguments: json.RawMessage(c.Arguments)})
			if ctx.Err() != nil {
				return Result{Text: full.String(), Interrupted: true}
			}
			work = append(work, llm.Message{Role: llm.RoleTool, Content: toolResultContent(c.Name, result, err)})
		}
	}
}

// stream consumes one provider Stream, returning the accumulated text, any tool
// calls the provider requested, and a backend/stream error. onDelta (optional) is
// invoked for each text delta.
func (l *Loop) stream(ctx context.Context, msgs []llm.Message, onDelta func(string)) (string, []llm.ToolCall, error) {
	stream, err := l.provider.Stream(ctx, llm.Request{Messages: msgs})
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	var calls []llm.ToolCall
	for d := range stream {
		if d.Err != nil {
			return sb.String(), calls, d.Err
		}
		if d.Done {
			break
		}
		if len(d.ToolCalls) > 0 {
			calls = append(calls, d.ToolCalls...)
		}
		if d.Text != "" {
			sb.WriteString(d.Text)
			if onDelta != nil {
				onDelta(d.Text)
			}
		}
	}
	return sb.String(), calls, nil
}

// toolResultContent renders a tool result (or an execution error) as the content
// of the RoleTool message fed back to the model.
func toolResultContent(name string, result ToolResult, err error) string {
	if err != nil {
		return "tool " + name + " failed: " + err.Error()
	}
	if result.Err != "" {
		return "tool " + name + " error: " + result.Err
	}
	return result.Content
}
