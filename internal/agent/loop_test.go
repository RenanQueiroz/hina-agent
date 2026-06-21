package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
)

// fakeProvider streams a fixed sequence of deltas, optionally erroring or blocking
// (to exercise cancellation). It implements llm.Provider.
type fakeProvider struct {
	deltas    []string
	midErr    error         // set on a delta to fail mid-stream
	startErr  error         // fail synchronously at Stream
	block     chan struct{} // if non-nil, the producer waits on it after each delta (for cancellation)
	completed bool
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	ch := make(chan llm.Delta)
	go func() {
		defer close(ch)
		for _, d := range f.deltas {
			select {
			case ch <- llm.Delta{Text: d}:
			case <-ctx.Done():
				return
			}
			if f.block != nil {
				select {
				case <-f.block:
				case <-ctx.Done():
					return
				}
			}
		}
		if f.midErr != nil {
			select {
			case ch <- llm.Delta{Err: f.midErr}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case ch <- llm.Delta{Done: true}:
			f.completed = true
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func TestLoopRunStreamsAndAccumulates(t *testing.T) {
	l := NewLoop(&fakeProvider{deltas: []string{"Hello", ", ", "world"}}, nil)
	var got strings.Builder
	res := l.Run(context.Background(), nil, func(s string) { got.WriteString(s) })
	if res.Err != nil || res.Interrupted {
		t.Fatalf("clean run: err=%v interrupted=%v", res.Err, res.Interrupted)
	}
	if res.Text != "Hello, world" {
		t.Fatalf("text = %q, want %q", res.Text, "Hello, world")
	}
	if got.String() != res.Text {
		t.Fatalf("onDelta saw %q, accumulated %q — must match", got.String(), res.Text)
	}
}

func TestLoopRunClassifiesBackendError(t *testing.T) {
	boom := errors.New("backend exploded")
	l := NewLoop(&fakeProvider{deltas: []string{"par", "tial"}, midErr: boom}, nil)
	res := l.Run(context.Background(), nil, nil)
	if !errors.Is(res.Err, boom) {
		t.Fatalf("err = %v, want %v", res.Err, boom)
	}
	if res.Interrupted {
		t.Fatal("a backend error is not an interrupt")
	}
	if res.Text != "partial" {
		t.Fatalf("partial text = %q, want %q", res.Text, "partial")
	}
}

func TestLoopRunStartErrorIsBackendError(t *testing.T) {
	boom := errors.New("no provider")
	l := NewLoop(&fakeProvider{startErr: boom}, nil)
	res := l.Run(context.Background(), nil, nil)
	if !errors.Is(res.Err, boom) || res.Interrupted {
		t.Fatalf("start error: err=%v interrupted=%v", res.Err, res.Interrupted)
	}
}

// toolProvider emits a tool call on the first round and, once it sees a tool
// result in the context, a final text reply.
type toolProvider struct {
	calls int
}

func (p *toolProvider) Name() string { return "tool" }

func (p *toolProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Delta, error) {
	hasResult := false
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			hasResult = true
		}
	}
	ch := make(chan llm.Delta, 2)
	go func() {
		defer close(ch)
		if hasResult {
			ch <- llm.Delta{Text: "done"}
			ch <- llm.Delta{Done: true}
			return
		}
		p.calls++
		ch <- llm.Delta{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`}}}
		ch <- llm.Delta{Done: true}
	}()
	return ch, nil
}

func TestLoopRunsToolThenAnswers(t *testing.T) {
	var gotCall ToolCall
	hook := func(_ context.Context, c ToolCall) (ToolResult, error) {
		gotCall = c
		return ToolResult{Content: "file1\nfile2"}, nil
	}
	l := NewLoop(&toolProvider{}, hook)
	res := l.Run(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "list files"}}, nil)
	if res.Err != nil || res.Interrupted {
		t.Fatalf("run: err=%v interrupted=%v", res.Err, res.Interrupted)
	}
	if res.Text != "done" {
		t.Fatalf("final text = %q, want %q", res.Text, "done")
	}
	if gotCall.Name != "shell" {
		t.Fatalf("hook saw tool %q, want shell", gotCall.Name)
	}
	if string(gotCall.Arguments) != `{"command":"ls"}` {
		t.Fatalf("hook saw args %q", gotCall.Arguments)
	}
}

// alwaysToolProvider never stops requesting tools, to exercise the round cap.
type alwaysToolProvider struct{}

func (alwaysToolProvider) Name() string { return "loop" }
func (alwaysToolProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta, 2)
	go func() {
		defer close(ch)
		ch <- llm.Delta{ToolCalls: []llm.ToolCall{{ID: "c", Name: "shell", Arguments: "{}"}}}
		ch <- llm.Delta{Done: true}
	}()
	return ch, nil
}

func TestLoopToolRoundLimit(t *testing.T) {
	hook := func(_ context.Context, _ ToolCall) (ToolResult, error) { return ToolResult{Content: "x"}, nil }
	l := NewLoop(alwaysToolProvider{}, hook)
	res := l.Run(context.Background(), nil, nil)
	if !errors.Is(res.Err, ErrToolRoundLimit) {
		t.Fatalf("err = %v, want ErrToolRoundLimit", res.Err)
	}
}

func TestLoopToolCallsIgnoredWithoutHook(t *testing.T) {
	// A provider emitting tool calls but no hook wired must not loop — it returns
	// whatever text it produced (here none) without error.
	l := NewLoop(&toolProvider{}, nil)
	res := l.Run(context.Background(), nil, nil)
	if res.Err != nil || res.Interrupted {
		t.Fatalf("run: err=%v interrupted=%v", res.Err, res.Interrupted)
	}
}

func TestLoopRunCancellationIsInterruptNotError(t *testing.T) {
	// A provider that delivers one delta then blocks; cancelling mid-stream must be
	// classified as an interrupt with the partial preserved, never a backend error.
	fp := &fakeProvider{deltas: []string{"so far"}, block: make(chan struct{}), midErr: errors.New("should not surface")}
	l := NewLoop(fp, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- l.Run(ctx, nil, func(string) { cancel() }) }()
	select {
	case res := <-done:
		if !res.Interrupted {
			t.Fatalf("cancelled run should be interrupted, got %+v", res)
		}
		if res.Err != nil {
			t.Fatalf("interrupt must not carry a backend error, got %v", res.Err)
		}
		if res.Text != "so far" {
			t.Fatalf("partial = %q, want %q", res.Text, "so far")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
