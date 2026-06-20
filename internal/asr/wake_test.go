package asr

import "testing"

func TestWakeStrip(t *testing.T) {
	w := NewWakeMatcher("Hina", []string{"Nina", "hey computer"})
	cases := []struct {
		in       string
		detected bool
		body     string
	}{
		{"Hina, what's the weather?", true, "what's the weather?"},
		{"hina what time is it", true, "what time is it"},
		{"hey Hina turn on the lights", true, "turn on the lights"},
		{"Nina set a timer", true, "set a timer"},             // alias
		{"hey computer play music", true, "play music"},       // multi-word alias
		{"what's the weather?", false, "what's the weather?"}, // no address
		{"Hinata is a name", false, "Hinata is a name"},       // word-boundary: no false match
		{"  Hina   hello  ", true, "hello"},                   // surrounding space
		{"Hina", true, ""},                                    // address only, empty body
	}
	for _, c := range cases {
		gotDet, gotBody := w.Strip(c.in)
		if gotDet != c.detected || gotBody != c.body {
			t.Errorf("Strip(%q) = (%v, %q), want (%v, %q)", c.in, gotDet, gotBody, c.detected, c.body)
		}
	}
}

func TestWakeStripMisTranscriptionPreservesBody(t *testing.T) {
	// The name is mis-heard as "Tina", which is NOT an alias. Wake detection
	// fails, but the request body is preserved intact (not corrupted).
	w := NewWakeMatcher("Hina", []string{"Nina"})
	det, body := w.Strip("Tina what is two plus two")
	if det {
		t.Fatal("unknown mishearing should not be detected as a wake word")
	}
	if body != "Tina what is two plus two" {
		t.Fatalf("body = %q, want the request preserved verbatim", body)
	}
}

func TestWakeMatcherInert(t *testing.T) {
	w := NewWakeMatcher("", nil)
	if w.Enabled() {
		t.Fatal("empty matcher must be inert")
	}
	det, body := w.Strip("Hina hello")
	if det || body != "Hina hello" {
		t.Fatalf("inert Strip = (%v, %q), want (false, unchanged)", det, body)
	}
}
