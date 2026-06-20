package rtc

import (
	"encoding/json"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/events"
)

// setSent simulates the pacer having sent n samples this epoch (so cursor
// reports up to n are accepted by recordCursor).
func setSent(s *Session, n int64) {
	s.out.mu.Lock()
	s.out.sent = n
	s.out.mu.Unlock()
}

func playedMs(s *Session) int64 {
	s.out.mu.Lock()
	defer s.out.mu.Unlock()
	return s.out.playedMs
}

// lastPlaybackStopped returns the payload of the most recent PlaybackStopped
// event the session emitted to the sink.
func lastPlaybackStopped(t *testing.T, sink *fakeSink) (truncated bool, cursorMs int64) {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	found := false
	for _, e := range sink.all {
		if e.Type != events.TypePlaybackStopped {
			continue
		}
		var p struct {
			Truncated bool  `json:"truncated"`
			CursorMs  int64 `json:"cursor_ms"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal PlaybackStopped: %v", err)
		}
		truncated, cursorMs, found = p.Truncated, p.CursorMs, true
	}
	if !found {
		t.Fatal("no PlaybackStopped emitted")
	}
	return truncated, cursorMs
}

// An immediate barge-in after a new playback starts must report a 0 truncation
// cursor — the previous playback's cursor must not leak through.
func TestInterruptAfterNewEpochReportsFreshCursor(t *testing.T) {
	s, sink := newDSPSession()
	defer s.cancel()

	ep1 := s.out.start(newToneSource())
	setSent(s, 480)
	s.out.recordCursor(ep1, 480, 0, s.nowMicros()) // epoch 1 played 20 ms
	if playedMs(s) != 20 {
		t.Fatalf("epoch-1 cursor = %d ms, want 20", playedMs(s))
	}
	s.out.stop()

	ep2 := s.out.start(newToneSource()) // new epoch resets the cursor + sent count
	if ep2 == ep1 {
		t.Fatal("epoch must advance on restart")
	}
	s.out.interrupt(ep2, 0, s.nowMicros()) // barge-in before any epoch-2 audio

	truncated, cursorMs := lastPlaybackStopped(t, sink)
	if !truncated {
		t.Fatal("interrupt must emit a truncated PlaybackStopped")
	}
	if cursorMs != 0 {
		t.Fatalf("truncation cursor=%d ms, want 0 (no epoch-2 audio heard yet)", cursorMs)
	}
}

// A progress report tagged with a superseded epoch must be ignored, so it can't
// overwrite the current playback's cursor.
func TestStaleEpochCursorIgnored(t *testing.T) {
	s, _ := newDSPSession()
	defer s.cancel()

	ep1 := s.out.start(newToneSource())
	setSent(s, 480)
	s.out.recordCursor(ep1, 480, 0, s.nowMicros())
	s.out.start(newToneSource()) // epoch 2: cursor + sent reset to 0

	s.out.recordCursor(ep1, 99999, 0, s.nowMicros()) // stale epoch-1 report

	if got := playedMs(s); got != 0 {
		t.Fatalf("stale-epoch cursor leaked: playedMs=%d, want 0", got)
	}
}

// A stale/duplicate interrupt for a superseded epoch must not stop the newer
// playback that is now active.
func TestStaleInterruptIgnored(t *testing.T) {
	s, sink := newDSPSession()
	defer s.cancel()

	ep1 := s.out.start(newToneSource())
	ep2 := s.out.start(newToneSource()) // epoch advances; still running on epoch 2
	if ep2 == ep1 {
		t.Fatal("epoch must advance")
	}

	s.out.interrupt(ep1, 100, s.nowMicros()) // stale interrupt for epoch 1

	if s.out.mode() == ModeIdle {
		t.Fatal("stale interrupt stopped the active (epoch 2) playback")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.all {
		if e.Type != events.TypePlaybackStopped {
			continue
		}
		var p struct {
			Truncated bool `json:"truncated"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Truncated {
			t.Fatal("stale interrupt emitted a truncated PlaybackStopped")
		}
	}
}

// A zero/missing epoch on UserInterrupted (malformed payload) must be ignored,
// not fall through to an unguarded stop of the active playback.
func TestZeroEpochInterruptIgnored(t *testing.T) {
	s, sink := newDSPSession()
	defer s.cancel()

	s.out.start(newToneSource()) // epoch 1, running
	s.out.interrupt(0, 100, s.nowMicros())

	if s.out.mode() == ModeIdle {
		t.Fatal("zero-epoch interrupt stopped the active playback")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.all {
		if e.Type != events.TypePlaybackStopped {
			continue
		}
		var p struct {
			Truncated bool `json:"truncated"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Truncated {
			t.Fatal("zero-epoch interrupt emitted a truncated PlaybackStopped")
		}
	}
}

// recordCursor rejects negative and regressing cursors and clamps over-sent
// values to what the server actually sent.
func TestCursorSanity(t *testing.T) {
	s, _ := newDSPSession()
	defer s.cancel()
	ep := s.out.start(newToneSource())
	setSent(s, 1000)

	s.out.recordCursor(ep, -5, 0, s.nowMicros()) // negative -> ignored
	if got := playedMs(s); got != 0 {
		t.Fatalf("negative cursor accepted: %d", got)
	}
	s.out.recordCursor(ep, 480, 0, s.nowMicros()) // 20 ms
	if got := playedMs(s); got != 20 {
		t.Fatalf("cursor = %d ms, want 20", got)
	}
	s.out.recordCursor(ep, 200, 0, s.nowMicros()) // regressing -> ignored
	if got := playedMs(s); got != 20 {
		t.Fatalf("regressing cursor accepted: %d", got)
	}
	s.out.recordCursor(ep, 5000, 0, s.nowMicros()) // over-sent -> clamp to 1000 (41 ms)
	s.out.mu.Lock()
	gotSamples := s.out.playedSamples
	s.out.mu.Unlock()
	if gotSamples != 1000 {
		t.Fatalf("over-sent cursor not clamped: playedSamples=%d, want 1000", gotSamples)
	}
}
