package bench

import "testing"

// result finds a fixture's result by name.
func result(rs []Result, name string) (Result, bool) {
	for _, r := range rs {
		if r.Fixture == name {
			return r, true
		}
	}
	return Result{}, false
}

// TestSuiteMeetsTargets runs the built-in suite through the real pipeline and
// asserts each fixture meets its Phase 6 exit-criterion target. This is both the
// harness's self-test and a behavioral regression guard for turn detection.
func TestSuiteMeetsTargets(t *testing.T) {
	rs := RunSuite(Fixtures(), NewEnergyModel)

	check := func(name string, fn func(t *testing.T, r Result)) {
		r, ok := result(rs, name)
		if !ok {
			t.Fatalf("missing fixture result %q", name)
		}
		t.Run(name, func(t *testing.T) { fn(t, r) })
	}

	check("clean_turn", func(t *testing.T, r Result) {
		if r.Opens != 1 || r.Commits != 1 {
			t.Fatalf("clean turn: opens=%d commits=%d, want 1/1", r.Opens, r.Commits)
		}
		if r.FalseStarts != 0 || r.MissedStarts != 0 {
			t.Fatalf("clean turn: falseStarts=%d missedStarts=%d, want 0/0", r.FalseStarts, r.MissedStarts)
		}
		if r.EndOfTurnDelayMs.Count != 1 {
			t.Fatalf("clean turn: expected one end-of-turn delay sample, got %d", r.EndOfTurnDelayMs.Count)
		}
	})

	check("two_turns", func(t *testing.T, r Result) {
		if r.Opens != 2 || r.Commits != 2 || r.FalseStarts != 0 || r.MissedStarts != 0 {
			t.Fatalf("two turns: %+v, want 2 opens/2 commits/0 false/0 missed", r)
		}
	})

	check("noise_only", func(t *testing.T, r Result) {
		if r.Opens != 0 || r.FalseStarts != 0 {
			t.Fatalf("noise: opens=%d falseStarts=%d, want 0/0 (no onset on sub-threshold noise)", r.Opens, r.FalseStarts)
		}
	})

	check("backchannel_playback", func(t *testing.T, r Result) {
		if r.BargeIns != 0 || r.FalseInterruptions != 0 {
			t.Fatalf("backchannel: bargeIns=%d falseInterruptions=%d, want 0/0", r.BargeIns, r.FalseInterruptions)
		}
		if r.Commits != 0 {
			t.Fatalf("backchannel must not commit a turn, got %d commits", r.Commits)
		}
		if r.BackchannelTotal != 1 || r.BackchannelSuppressed != 1 {
			t.Fatalf("backchannel suppression = %d/%d, want 1/1", r.BackchannelSuppressed, r.BackchannelTotal)
		}
	})

	check("interruption_playback", func(t *testing.T, r Result) {
		if r.BargeIns != 1 {
			t.Fatalf("interruption: bargeIns=%d, want 1", r.BargeIns)
		}
		if r.InterruptionDelayMs.Count == 0 {
			t.Fatal("interruption: expected an interruption-delay sample")
		}
	})

	check("echo_playback", func(t *testing.T, r Result) {
		if r.Opens != 0 || r.FalseStarts != 0 || r.BargeIns != 0 {
			t.Fatalf("echo: opens=%d falseStarts=%d bargeIns=%d, want 0/0/0 (echo gated)", r.Opens, r.FalseStarts, r.BargeIns)
		}
	})

	check("semantic_incomplete", func(t *testing.T, r Result) {
		// The mid-thought pause must NOT commit; the completed utterance commits once.
		if r.Commits != 1 {
			t.Fatalf("semantic: commits=%d, want exactly 1 (bridged the pause)", r.Commits)
		}
		if r.Opens != 1 {
			t.Fatalf("semantic: opens=%d, want 1 logical turn (continuation, not a new open)", r.Opens)
		}
	})
}

func TestPercentiles(t *testing.T) {
	got := percentiles([]float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100})
	if got.Count != 10 || got.Max != 100 {
		t.Fatalf("count/max = %d/%v, want 10/100", got.Count, got.Max)
	}
	if got.P50 != 50 || got.P90 != 90 {
		t.Fatalf("p50/p90 = %v/%v, want 50/90", got.P50, got.P90)
	}
	if (Stats{}) != percentiles(nil) {
		t.Fatal("empty sample set should yield a zero Stats")
	}
}
