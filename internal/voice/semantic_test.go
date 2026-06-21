package voice

import (
	"testing"
	"time"
)

func TestLooksComplete(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"what time is it", true},
		{"what time is it?", true},
		{"turn on the lights.", true},
		{"i want to", false},           // trailing "to"
		{"can you um", false},          // trailing filler
		{"so", false},                  // single conjunction
		{"the", false},                 // trailing article
		{"play some music and", false}, // trailing conjunction
		{"play some music", true},
		{"umm", false},
		{"Hello there", true},
	}
	for _, c := range cases {
		if got := LooksComplete(c.text); got != c.want {
			t.Errorf("LooksComplete(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestSemanticCommitsCompleteFastWaitsIncomplete(t *testing.T) {
	s := NewSemantic(EagerMedium) // maxWait 4s, completeWait ~480ms
	// Complete utterance commits once trailing silence reaches completeWait.
	if s.Commit("what time is it", 200*time.Millisecond) {
		t.Fatal("should not commit a complete utterance before completeWait")
	}
	if !s.Commit("what time is it", completeWait) {
		t.Fatal("should commit a complete utterance at completeWait")
	}
	// Incomplete utterance waits past completeWait...
	if s.Commit("i want to", completeWait) {
		t.Fatal("should NOT commit an incomplete utterance at completeWait")
	}
	if s.Commit("i want to", 3*time.Second) {
		t.Fatal("should still be waiting on an incomplete utterance before maxWait")
	}
	// ...but force-commits at maxWait regardless of content.
	if !s.Commit("i want to", 4*time.Second) {
		t.Fatal("should force-commit at maxWait even when incomplete")
	}
}

func TestEagernessMapsToMaxWait(t *testing.T) {
	cases := map[Eagerness]time.Duration{
		EagerHigh: 2 * time.Second, EagerMedium: 4 * time.Second,
		EagerLow: 8 * time.Second, EagerAuto: 4 * time.Second,
	}
	for e, want := range cases {
		if got := NewSemantic(e).MaxWait(); got != want {
			t.Errorf("eagerness %q -> maxWait %v, want %v", e, got, want)
		}
	}
}
