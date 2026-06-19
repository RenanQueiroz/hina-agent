package agent

import (
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func TestBuildContext(t *testing.T) {
	turns := []store.Turn{
		{Role: "user", CanonicalText: "hi"},
		{Role: "assistant", CanonicalText: "hello"},
		{Role: "tool", CanonicalText: "ignored"},
		{Role: "user", CanonicalText: "again"},
	}
	msgs := BuildContext("sys", turns)
	want := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
		{Role: llm.RoleUser, Content: "again"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("msg %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}

// TestBuildContextExcludesErroredTurns proves a failed assistant turn's partial
// text is not fed back to the model (it stays in the store for UI replay).
func TestBuildContextExcludesErroredTurns(t *testing.T) {
	turns := []store.Turn{
		{Role: "user", CanonicalText: "q1"},
		{Role: "assistant", CanonicalText: "good"},
		{Role: "user", CanonicalText: "q2"},
		{Role: "assistant", CanonicalText: "truncated junk", Metadata: `{"error":true}`},
		{Role: "user", CanonicalText: "q3"},
	}
	msgs := BuildContext("", turns)
	for _, m := range msgs {
		if m.Content == "truncated junk" {
			t.Fatal("errored assistant turn must be excluded from model context")
		}
	}
	want := []llm.Message{
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "good"},
		{Role: llm.RoleUser, Content: "q2"},
		{Role: llm.RoleUser, Content: "q3"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("msg %d = %+v, want %+v", i, msgs[i], want[i])
		}
	}
}
