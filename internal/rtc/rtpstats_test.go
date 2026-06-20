package rtc

import (
	"math"
	"testing"
)

func TestRTPStatsNoLoss(t *testing.T) {
	var r rtpStats
	for i := 0; i < 10; i++ {
		r.observe(uint16(100+i), uint32(i*960), int64(i)*20000) // 20 ms spacing
	}
	received, lost, _ := r.snapshot()
	if received != 10 {
		t.Fatalf("received=%d, want 10", received)
	}
	if lost != 0 {
		t.Fatalf("lost=%d, want 0", lost)
	}
}

func TestRTPStatsCountsLoss(t *testing.T) {
	var r rtpStats
	// seqs 100,101, [skip 102,103], 104 -> 2 lost out of 5 expected.
	for _, seq := range []uint16{100, 101, 104} {
		r.observe(seq, uint32(seq)*960, int64(seq)*20000)
	}
	received, lost, _ := r.snapshot()
	if received != 3 {
		t.Fatalf("received=%d, want 3", received)
	}
	if lost != 2 {
		t.Fatalf("lost=%d, want 2 (expected 5, received 3)", lost)
	}
}

func TestRTPStatsHandlesSequenceWrap(t *testing.T) {
	var r rtpStats
	seqs := []uint16{65534, 65535, 0, 1, 2}
	for i, seq := range seqs {
		r.observe(seq, uint32(i)*960, int64(i)*20000)
	}
	received, lost, _ := r.snapshot()
	if received != 5 || lost != 0 {
		t.Fatalf("across a 16-bit wrap: received=%d lost=%d, want 5/0", received, lost)
	}
}

// A late packet from just before a 16-bit wrap (arriving after the wrap) must
// not be misclassified as a second wrap and inflate the loss count.
func TestRTPStatsLatePreWrapPacket(t *testing.T) {
	var r rtpStats
	seqs := []uint16{65534, 65535, 0, 1, 2, 65535 /* late, pre-wrap */}
	for i, seq := range seqs {
		r.observe(seq, uint32(i)*960, int64(i)*20000)
	}
	received, lost, _ := r.snapshot()
	if received != 6 {
		t.Fatalf("received=%d, want 6", received)
	}
	if lost != 0 {
		t.Fatalf("lost=%d, want 0 (a late pre-wrap packet must not spike loss)", lost)
	}
}

// Duplicate packets must not drive loss negative or inflate the expected count.
func TestRTPStatsDuplicate(t *testing.T) {
	var r rtpStats
	for _, seq := range []uint16{100, 101, 101, 102} {
		r.observe(seq, uint32(seq)*960, int64(seq)*20000)
	}
	received, lost, _ := r.snapshot()
	if received != 4 {
		t.Fatalf("received=%d, want 4", received)
	}
	if lost != 0 {
		t.Fatalf("lost=%d, want 0 with a duplicate present", lost)
	}
}

func TestRTPStatsJitter(t *testing.T) {
	// Perfectly even arrival matching the RTP timestamp cadence -> ~0 jitter.
	var even rtpStats
	for i := 0; i < 50; i++ {
		even.observe(uint16(i), uint32(i*960), int64(i)*20000) // 960 ticks == 20 ms @48k
	}
	if _, _, j := even.snapshot(); j > 1e-4 {
		t.Fatalf("even stream jitter=%v, want ~0", j)
	}

	// Irregular arrivals (alternating early/late) -> non-zero jitter.
	var jittery rtpStats
	for i := 0; i < 50; i++ {
		arrival := int64(i) * 20000
		if i%2 == 1 {
			arrival += 8000 // 8 ms late on odd packets
		}
		jittery.observe(uint16(i), uint32(i*960), arrival)
	}
	if _, _, j := jittery.snapshot(); j <= 0 || math.IsNaN(j) {
		t.Fatalf("irregular stream jitter=%v, want > 0", j)
	}
}
