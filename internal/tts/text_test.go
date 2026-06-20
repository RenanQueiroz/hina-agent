package tts

import (
	"strings"
	"testing"
)

func TestSplitSentencesDecimalAware(t *testing.T) {
	got := SplitSentences("I have 3.14 dollars. Really? Yes!")
	want := []string{"I have 3.14 dollars.", "Really?", "Yes!"}
	if len(got) != len(want) {
		t.Fatalf("got %d sentences %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sentence %d = %q, want %q", i, got[i], want[i])
		}
	}
	// The decimal must survive intact (not split into "3" / "14").
	if !strings.Contains(got[0], "3.14") {
		t.Fatalf("decimal split: %q", got[0])
	}
}

func TestSplitSentencesEdgeCases(t *testing.T) {
	// No terminal punctuation -> a single sentence.
	if got := SplitSentences("just some words"); len(got) != 1 || got[0] != "just some words" {
		t.Fatalf("no-terminal = %q, want one sentence", got)
	}
	// Runs of terminals stay with their sentence; version numbers don't split.
	got := SplitSentences("Wait... what?! Version 1.2.3 ships.")
	want := []string{"Wait...", "what?!", "Version 1.2.3 ships."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Empty input -> no sentences.
	if got := SplitSentences("   "); len(got) != 0 {
		t.Fatalf("blank input = %q, want none", got)
	}
}

func TestPreprocessTextNormalizesAndWraps(t *testing.T) {
	out := preprocessText("hi 😀 ﬁle", "en")
	if !strings.HasPrefix(out, "<en>") || !strings.HasSuffix(out, "</en>") {
		t.Fatalf("missing language tags: %q", out)
	}
	if strings.ContainsRune(out, '😀') {
		t.Fatalf("emoji not removed: %q", out)
	}
	// NFKD decomposes the "fi" ligature (U+FB01) into ASCII "fi".
	if !strings.Contains(out, "file") {
		t.Fatalf("NFKD not applied: %q", out)
	}
	// A missing terminal gets one appended (before the closing tag).
	if !strings.Contains(out, ".</en>") {
		t.Fatalf("terminal punctuation not appended: %q", out)
	}
}

func TestPreprocessTextKeepsExistingTerminal(t *testing.T) {
	out := preprocessText("Done!", "en")
	if strings.Contains(out, "!.") {
		t.Fatalf("appended a spurious period: %q", out)
	}
	// Unknown language falls back to en rather than panicking.
	if got := preprocessText("hi", "zzz"); !strings.HasPrefix(got, "<en>") {
		t.Fatalf("unknown lang should fall back to en: %q", got)
	}
}

func TestChunkByRunes(t *testing.T) {
	long := strings.Repeat("ab ", 100) // 300 chars incl spaces
	got := chunkByRunes(strings.TrimSpace(long), 50)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	for _, c := range got {
		if len([]rune(c)) > 50 {
			t.Fatalf("chunk exceeds cap: %d runes", len([]rune(c)))
		}
	}
	// A short sentence is returned unchanged.
	if got := chunkByRunes("short", 50); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short chunk = %q", got)
	}
}

func TestSegmentsCombinesSplitAndChunk(t *testing.T) {
	// Two sentences; the cap is large so each stays whole.
	got := segments("First sentence. Second one.", "en")
	if len(got) != 2 {
		t.Fatalf("segments = %q, want 2", got)
	}
}
