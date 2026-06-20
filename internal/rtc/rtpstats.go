package rtc

// rtpStats derives receiver-side network quality (packets received, cumulative
// loss, and RFC 3550 interarrival jitter) from the RTP stream the inbound reader
// already consumes. Computing these ourselves — instead of calling
// pc.GetStats() — keeps stats gathering entirely on the inbound goroutine and
// avoids Pion's GetStats/Close data race (GetStats reads an unlocked field that
// Close nils). It is owned by a single goroutine (the read loop), so it needs no
// locking.
type rtpStats struct {
	started bool

	baseSeq uint16
	maxSeq  uint16 // highest in-order sequence (16-bit)
	cycles  uint64 // accumulated wraps, in multiples of seqMod

	received uint64

	haveTransit bool
	prevTransit float64
	jitter      float64 // in RTP timestamp units
}

const (
	// rtpClockRate is the Opus RTP clock (48 kHz); jitter is divided by it to get
	// seconds.
	rtpClockRate = 48000
	// RFC 3550 sequence-number validity bounds: a forward delta below maxDropout
	// is a real advance (in order or a loss gap); a delta above seqMod-maxMisorder
	// is a small backward step (a late/reordered/duplicate packet) that must NOT
	// advance the high-water mark — otherwise a delayed pre-wrap packet would look
	// like a ~65k packet-loss spike.
	maxDropout  = 3000
	maxMisorder = 100
	seqMod      = 1 << 16
)

// observe records one received packet. arrivalMicros is a monotonic timestamp
// (microseconds) from the server clock.
func (r *rtpStats) observe(seq uint16, ts uint32, arrivalMicros int64) {
	r.received++

	if !r.started {
		r.started = true
		r.baseSeq = seq
		r.maxSeq = seq
	} else {
		udelta := seq - r.maxSeq // 16-bit wrapping delta
		switch {
		case udelta < maxDropout:
			// In order or a permissible forward gap (loss). Detect a 16-bit wrap.
			if seq < r.maxSeq {
				r.cycles += seqMod
			}
			r.maxSeq = seq
		case udelta <= seqMod-maxMisorder:
			// A very large forward jump (likely a restart): count it but don't let
			// it inflate the expected count.
		default:
			// Small backward step: a late/reordered/duplicate packet. Count it as
			// received but do not advance the high-water mark.
		}
	}

	// RFC 3550 interarrival jitter. The constant network-delay offset cancels in
	// the difference of consecutive transits, so the random RTP-timestamp origin
	// doesn't matter. Over a Phase 3 session neither clock wraps.
	arrivalRTP := float64(arrivalMicros) * rtpClockRate / 1e6
	transit := arrivalRTP - float64(ts)
	if r.haveTransit {
		d := transit - r.prevTransit
		if d < 0 {
			d = -d
		}
		r.jitter += (d - r.jitter) / 16
	}
	r.prevTransit = transit
	r.haveTransit = true
}

// snapshot returns the current received count, cumulative loss, and jitter in
// seconds. Loss is expected-minus-received and is clamped at zero (so duplicates
// or late packets can't drive it negative).
func (r *rtpStats) snapshot() (received uint64, lost int64, jitterSeconds float64) {
	received = r.received
	if r.started {
		extMax := r.cycles + uint64(r.maxSeq)
		expected := extMax - uint64(r.baseSeq) + 1
		if expected > r.received {
			lost = int64(expected - r.received)
		}
	}
	return received, lost, r.jitter / rtpClockRate
}
