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
