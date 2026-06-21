package rtc

import "sync"

// SessionStats is a point-in-time snapshot of one live session, combining
// app-level counters with Pion's network stats (loss/jitter/RTT). It is the
// shape the admin metrics endpoint serves.
type SessionStats struct {
	SessionID      string `json:"session_id"`
	UserID         string `json:"user_id"`
	ConversationID string `json:"conversation_id"`
	Mode           string `json:"mode"`
	UptimeMs       int64  `json:"uptime_ms"`

	// Inbound (mic) + outbound (PCM) app counters.
	RTPPacketsIn  uint64 `json:"rtp_packets_in"`
	DecodeErrors  uint64 `json:"decode_errors"`
	FramesOut     uint64 `json:"frames_out"`
	BytesOut      uint64 `json:"bytes_out"`
	FramesDropped uint64 `json:"frames_dropped"`
	Interrupts    uint64 `json:"interrupts"`
	DroppedTurns  uint64 `json:"dropped_turns"` // committed live turns dropped because the reply queue was full

	// Cursor + latency reported by the browser AudioWorklet.
	PlayedMs     int64 `json:"played_ms"`
	CaptureMs    int64 `json:"capture_ms"`
	AppRTTMicros int64 `json:"app_rtt_micros"`

	// Receiver-side network stats, derived from the inbound RTP stream.
	PacketsReceived uint32  `json:"packets_received"`
	PacketsLost     int32   `json:"packets_lost"`
	JitterSeconds   float64 `json:"jitter_seconds"`
}

// Metrics accumulates per-session counters. Several goroutines touch it (the
// inbound reader, the outbound pacer, the control handler), so every field is
// guarded by the mutex.
type Metrics struct {
	mu sync.Mutex

	rtpPacketsIn  uint64
	decodeErrors  uint64
	framesOut     uint64
	bytesOut      uint64
	framesDropped uint64
	interrupts    uint64
	droppedTurns  uint64

	playedMs  int64
	captureMs int64
	rttMicros int64

	// Receiver-side network stats, computed from the inbound RTP stream by the
	// read loop (no pc.GetStats(), so nothing on the request path can race
	// Pion's teardown).
	netPacketsReceived uint32
	netPacketsLost     int32
	netJitter          float64
}

func newMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) incRTP() {
	m.mu.Lock()
	m.rtpPacketsIn++
	m.mu.Unlock()
}

func (m *Metrics) incDecodeErr() {
	m.mu.Lock()
	m.decodeErrors++
	m.mu.Unlock()
}

func (m *Metrics) addOut(bytes uint64) {
	m.mu.Lock()
	m.framesOut++
	m.bytesOut += bytes
	m.mu.Unlock()
}

func (m *Metrics) incDropped() {
	m.mu.Lock()
	m.framesDropped++
	m.mu.Unlock()
}

func (m *Metrics) markInterrupt() {
	m.mu.Lock()
	m.interrupts++
	m.mu.Unlock()
}

// markDroppedTurn counts a committed live turn discarded because the serial reply
// queue was full (bounded backpressure) — surfaced so the loss isn't silent.
func (m *Metrics) markDroppedTurn() {
	m.mu.Lock()
	m.droppedTurns++
	m.mu.Unlock()
}

func (m *Metrics) setCursor(playedMs, rttMicros int64) {
	m.mu.Lock()
	m.playedMs = playedMs
	if rttMicros > 0 {
		m.rttMicros = rttMicros
	}
	m.mu.Unlock()
}

func (m *Metrics) setCaptureMs(ms int64) {
	m.mu.Lock()
	m.captureMs = ms
	m.mu.Unlock()
}

func (m *Metrics) setNetwork(received uint64, lost int64, jitter float64) {
	m.mu.Lock()
	m.netPacketsReceived = uint32(received)
	m.netPacketsLost = int32(lost)
	m.netJitter = jitter
	m.mu.Unlock()
}

func (m *Metrics) snapshot() SessionStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return SessionStats{
		RTPPacketsIn:    m.rtpPacketsIn,
		DecodeErrors:    m.decodeErrors,
		FramesOut:       m.framesOut,
		BytesOut:        m.bytesOut,
		FramesDropped:   m.framesDropped,
		Interrupts:      m.interrupts,
		DroppedTurns:    m.droppedTurns,
		PlayedMs:        m.playedMs,
		CaptureMs:       m.captureMs,
		AppRTTMicros:    m.rttMicros,
		PacketsReceived: m.netPacketsReceived,
		PacketsLost:     m.netPacketsLost,
		JitterSeconds:   m.netJitter,
	}
}
