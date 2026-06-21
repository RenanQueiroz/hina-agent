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
