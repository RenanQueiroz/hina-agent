package asr

import "testing"

func TestBiasContextEnabled(t *testing.T) {
	if NewBiasContext(nil, 0, 0).Enabled() {
		t.Fatal("nil phrases must be inert")
	}
	if NewBiasContext([][]int{{}}, 0, 0).Enabled() {
		t.Fatal("empty phrase must be inert")
	}
	if !NewBiasContext([][]int{{1, 2}}, 0, 0).Enabled() {
		t.Fatal("a real phrase must enable biasing")
	}
}

func TestBiasApplyAndAdvance(t *testing.T) {
	// Phrase ▁H(10) in(20) a(30). Defaults: depth1 boost = contextScore (1.0),
	// depth>=2 boost = contextScore*depthScaling (2.0).
	b := NewBiasContext([][]int{{10, 20, 30}}, 0, 0)
	logits := make([]float32, 40)
	cur := b.cursor()
	b.apply(logits, cur)
	if logits[10] != float32(DefaultContextScore) {
		t.Fatalf("root child 10 boost = %g, want %g", logits[10], DefaultContextScore)
	}
	if logits[20] != 0 || logits[30] != 0 {
		t.Fatal("only the first token should be boosted at the root")
	}
	// Advance into the phrase; the next token is a depth-2 boost.
	cur = b.advance(cur, 10)
	logits2 := make([]float32, 40)
	b.apply(logits2, cur)
	if logits2[20] != float32(DefaultContextScore*DefaultDepthScaling) {
		t.Fatalf("depth-2 boost = %g, want %g", logits2[20], DefaultContextScore*DefaultDepthScaling)
	}
	// Mismatch resets to root.
	reset := b.advance(cur, 99)
	if reset != b.root {
		t.Fatal("a mismatching token must reset the cursor to root")
	}
	// Completing the phrase returns to root (ready for the next occurrence).
	cur = b.advance(b.advance(b.cursor(), 10), 20) // at depth-2 node (child {30})
	end := b.advance(cur, 30)
	if end != b.root {
		t.Fatal("completing a phrase must return to root")
	}
}

func TestBiasReentersNewPhrase(t *testing.T) {
	// Two phrases starting with distinct tokens. A mismatch that itself begins a
	// phrase should re-enter rather than idle at root.
	b := NewBiasContext([][]int{{10, 20}, {30, 40}}, 0, 0)
	cur := b.advance(b.cursor(), 10) // mid phrase 1
	cur = b.advance(cur, 30)         // breaks phrase 1, but 30 starts phrase 2
	logits := make([]float32, 50)
	b.apply(logits, cur)
	if logits[40] == 0 {
		t.Fatal("after re-entering phrase 2, its continuation (40) should be boosted")
	}
}

func TestBiasInertContextNoBoost(t *testing.T) {
	b := NewBiasContext(nil, 0, 0)
	logits := []float32{1, 2, 3}
	b.apply(logits, b.cursor())
	if logits[0] != 1 || logits[1] != 2 || logits[2] != 3 {
		t.Fatal("inert context must not modify logits")
	}
}
