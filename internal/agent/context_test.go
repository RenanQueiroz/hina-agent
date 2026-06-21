package agent

import (
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func TestBuildContextTruncatesInterruptedVoiceTurn(t *testing.T) {
	full := "Sure, the weather today is sunny with a high of seventy two degrees and clear skies"
	turns := []store.Turn{
		{Role: "user", CanonicalText: "what's the weather"},
		// Interrupted after ~1s of playback — the model must NOT see the full unheard reply.
		{Role: "assistant", CanonicalText: full, Metadata: `{"interrupted":true,"played_ms":1000}`},
		{Role: "user", CanonicalText: "ok thanks"},
	}
	var assistant string
	for _, m := range BuildContext("", turns) {
		if m.Role == llm.RoleAssistant {
			assistant = m.Content
		}
	}
	if assistant == full {
		t.Fatal("interrupted voice turn must not feed the full unheard reply to the model")
	}
	if !strings.HasSuffix(assistant, "[interrupted]") {
		t.Fatalf("interrupted assistant content = %q, want an [interrupted] marker", assistant)
	}
	if len(assistant) >= len(full) {
		t.Fatalf("interrupted content (%d) should be shorter than the full reply (%d)", len(assistant), len(full))
	}
	heard := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(assistant), "[interrupted]"))
	if heard != "" && !strings.HasPrefix(full, heard) {
		t.Fatalf("heard prefix %q is not a clean prefix of the reply", heard)
	}
}

func TestBuildContextKeepsTextInterruptPartial(t *testing.T) {
	// A text-mode interrupt (no played_ms) keeps its partial canonical text — only a
	// voice playback barge-in (played_ms > 0) truncates to the heard prefix.
	turns := []store.Turn{
		{Role: "user", CanonicalText: "q"},
		{Role: "assistant", Mode: "text", CanonicalText: "partial answer", Metadata: `{"interrupted":true}`},
	}
	var got string
	for _, m := range BuildContext("", turns) {
		if m.Role == llm.RoleAssistant {
			got = m.Content
		}
	}
	if got != "partial answer" {
		t.Fatalf("text-interrupt assistant content = %q, want the partial preserved", got)
	}
}

// TestBuildContextZeroPlayedMsInterrupt is the round-20 regression: a voice turn barged
// in BEFORE any audio played carries played_ms == 0 (PRESENT, value 0). The user heard
// nothing, so the model must NOT see the full generated reply — only "[interrupted]".
// This differs from a text-mode interrupt (no played_ms), which keeps its partial text.
func TestBuildContextZeroPlayedMsInterrupt(t *testing.T) {
	turns := []store.Turn{
		{Role: "user", CanonicalText: "q"},
		{Role: "assistant", Mode: "voice", CanonicalText: "a full unheard answer",
			Metadata: `{"interrupted":true,"played_ms":0}`},
	}
	var got string
	for _, m := range BuildContext("", turns) {
		if m.Role == llm.RoleAssistant {
			got = m.Content
		}
	}
	if got != "[interrupted]" {
		t.Fatalf("zero-played_ms voice interrupt content = %q, want \"[interrupted]\" (user heard nothing)", got)
	}
}

func TestHeardPrefixWordAligned(t *testing.T) {
	text := "hello there friend"
	got := heardPrefix(text, 600) // ~9 chars -> word-aligned back to "hello"
	if got != "" && !strings.HasPrefix(text, got) {
		t.Fatalf("heardPrefix %q is not a prefix of %q", got, text)
	}
	if strings.Contains(got, "the") && !strings.Contains(got, "there") {
		t.Fatalf("heardPrefix %q cut mid-word", got)
	}
	if heardPrefix(text, 0) != "" {
		t.Fatal("zero played_ms -> empty prefix")
	}
	if heardPrefix(text, 100000) != text {
		t.Fatal("huge played_ms -> full text")
	}
}

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
