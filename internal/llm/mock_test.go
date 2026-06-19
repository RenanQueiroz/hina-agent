package llm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMockProviderStreamsAndCompletes(t *testing.T) {
	p := NewMockProvider()
	p.WordDelay = time.Millisecond
	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hello there"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var sb strings.Builder
	done := false
	for d := range ch {
		if d.Err != nil {
			t.Fatalf("delta err: %v", d.Err)
		}
		if d.Done {
			done = true
			continue
		}
		sb.WriteString(d.Text)
	}
	if !done {
		t.Fatal("never received Done")
	}
	if !strings.Contains(sb.String(), "hello there") {
		t.Fatalf("reply did not echo user message: %q", sb.String())
	}
}

func TestMockProviderCancels(t *testing.T) {
	p := NewMockProvider()
	p.WordDelay = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Stream(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "a b c d e f g h"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	<-ch     // receive first delta
	cancel() // then cancel mid-stream
	gotDone := false
	for d := range ch { // channel must close (drain)
		if d.Done {
			gotDone = true
		}
	}
	if gotDone {
		t.Fatal("cancelled stream should not emit Done")
	}
}

func TestBuildProvider(t *testing.T) {
	if p, _ := Build(Config{}); p.Name() != "mock" {
		t.Fatalf("default provider = %q, want mock", p.Name())
	}
	if _, err := Build(Config{Provider: "openai"}); err == nil {
		t.Fatal("openai without model should error")
	}
	if p, err := Build(Config{Provider: "openai", Model: "x"}); err != nil || p.Name() != "openai" {
		t.Fatalf("openai build: p=%v err=%v", p, err)
	}
	if _, err := Build(Config{Provider: "bogus"}); err == nil {
		t.Fatal("unknown provider should error")
	}
}
