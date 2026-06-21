package vad

import (
	"testing"
	"time"
)

// windowsFor returns how many 32 ms windows span at least d.
func windowsFor(d time.Duration) int {
	n := int(d / windowDuration)
	if d%windowDuration != 0 {
		n++
	}
	return n
}

// drive feeds a slice of probabilities to a Detector and returns the decision per
// window.
func drive(d *Detector, probs []float64) []Decision {
	out := make([]Decision, len(probs))
	for i, p := range probs {
		out[i] = d.Push(p)
	}
	return out
}

func TestDetectorStartOnThresholdCrossing(t *testing.T) {
	d := NewDetector(Params{})
	// Two silent windows, then a super-threshold one triggers Start exactly once.
	got := drive(d, []float64{0.1, 0.2, 0.9, 0.95})
	if got[0] != Continue || got[1] != Continue {
		t.Fatalf("silence windows = %v, want Continue", got[:2])
	}
	if got[2] != Start {
		t.Fatalf("onset window = %v, want Start", got[2])
	}
	if got[3] != Continue || !d.InSpeech() {
		t.Fatalf("post-onset = %v inSpeech=%v, want Continue + in speech", got[3], d.InSpeech())
	}
}

func TestDetectorEndsAfterMinSilence(t *testing.T) {
	d := NewDetector(Params{}) // defaults: MinSilence 700ms, MinSpeech 250ms
	var probs []float64
	probs = append(probs, 0.9) // Start
	for i := 0; i < 20; i++ {  // ~21 windows of speech (>250ms)
		probs = append(probs, 0.9)
	}
	silenceWindows := windowsFor(d.Params().MinSilence)
	for i := 0; i < silenceWindows; i++ {
		probs = append(probs, 0.05)
	}
	got := drive(d, probs)
	ends := 0
	var endIdx int
	for i, dec := range got {
		if dec == End {
			ends++
			endIdx = i
		}
		if dec == Cancel || dec == Max {
			t.Fatalf("unexpected %v at window %d", dec, i)
		}
	}
	if ends != 1 {
		t.Fatalf("got %d End decisions, want exactly 1", ends)
	}
	// End fires on the window where trailing silence first reaches MinSilence.
	wantEnd := 21 + silenceWindows - 1
	if endIdx != wantEnd {
		t.Fatalf("End at window %d, want %d", endIdx, wantEnd)
	}
	if d.InSpeech() {
		t.Fatal("detector should be idle after End")
	}
}

func TestDetectorCancelsShortBlip(t *testing.T) {
	d := NewDetector(Params{}) // MinSpeech 250ms (~8 windows)
	var probs []float64
	// Only 3 windows of speech (~96ms < 250ms) then a full silence gap -> Cancel.
	probs = append(probs, 0.9, 0.9, 0.9)
	for i := 0; i < windowsFor(d.Params().MinSilence); i++ {
		probs = append(probs, 0.0)
	}
	got := drive(d, probs)
	cancels, ends := 0, 0
	for _, dec := range got {
		switch dec {
		case Cancel:
			cancels++
		case End:
			ends++
		}
	}
	if cancels != 1 || ends != 0 {
		t.Fatalf("got cancels=%d ends=%d, want a single Cancel (short blip discarded)", cancels, ends)
	}
}

func TestDetectorHysteresisHoldsThroughDip(t *testing.T) {
	// A dip into the ambiguous band [neg, threshold) must NOT start a silence run
	// or end the turn — only a sub-neg_threshold run does.
	d := NewDetector(Params{Threshold: 0.5}) // neg = 0.35
	probs := []float64{0.9}                  // Start
	for i := 0; i < 30; i++ {
		probs = append(probs, 0.4) // in [0.35, 0.5): ambiguous, hold speech open
	}
	got := drive(d, probs)
	for i, dec := range got[1:] {
		if dec != Continue {
			t.Fatalf("ambiguous window %d = %v, want Continue (no end during a dip)", i+1, dec)
		}
	}
	if !d.InSpeech() {
		t.Fatal("speech should still be open through an ambiguous dip")
	}
}

func TestDetectorMaxDurationForceCommits(t *testing.T) {
	d := NewDetector(Params{MaxDuration: 320 * time.Millisecond}) // ~10 windows
	var probs []float64
	for i := 0; i < 20; i++ {
		probs = append(probs, 0.9) // continuous speech, never a natural end
	}
	got := drive(d, probs)
	maxes := 0
	for _, dec := range got {
		if dec == Max {
			maxes++
		}
	}
	if maxes == 0 {
		t.Fatal("continuous speech past MaxDuration should force-commit (Max)")
	}
	// After a Max the detector is idle and can re-trigger on the next speech window.
	if d.InSpeech() {
		t.Fatal("detector should be idle immediately after Max")
	}
}

func TestParamsNormalizeFillsDefaults(t *testing.T) {
	p := Params{}.normalize()
	if p.Threshold != defaultThreshold || p.MinSilence != defaultMinSilence ||
		p.PreSpeech != defaultPreSpeech || p.MinSpeech != defaultMinSpeech || p.MaxDuration != defaultMaxDuration {
		t.Fatalf("normalize did not fill all defaults: %+v", p)
	}
	// Out-of-range threshold falls back; in-range is kept.
	if got := (Params{Threshold: 2.0}).normalize().Threshold; got != defaultThreshold {
		t.Fatalf("threshold 2.0 -> %v, want default", got)
	}
	if got := (Params{Threshold: 0.7}).normalize().Threshold; got != 0.7 {
		t.Fatalf("threshold 0.7 -> %v, want kept", got)
	}
}
