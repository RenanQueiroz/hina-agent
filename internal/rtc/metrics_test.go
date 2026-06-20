package rtc

import "testing"

func TestMetricsCounters(t *testing.T) {
	m := newMetrics()
	m.incRTP()
	m.incRTP()
	m.incDecodeErr()
	m.addOut(976)
	m.addOut(976)
	m.incDropped()
	m.markInterrupt()
	m.setCursor(120, 5000)
	m.setCaptureMs(80)

	st := m.snapshot()
	if st.RTPPacketsIn != 2 || st.DecodeErrors != 1 {
		t.Fatalf("inbound counters: %+v", st)
	}
	if st.FramesOut != 2 || st.BytesOut != 1952 || st.FramesDropped != 1 {
		t.Fatalf("outbound counters: %+v", st)
	}
	if st.Interrupts != 1 || st.PlayedMs != 120 || st.AppRTTMicros != 5000 || st.CaptureMs != 80 {
		t.Fatalf("cursor/latency: %+v", st)
	}
}

func TestSetCursorKeepsLastRTTWhenUnknown(t *testing.T) {
	m := newMetrics()
	m.setCursor(10, 3000)
	m.setCursor(20, 0) // a report without a fresh RTT must not zero it out
	if got := m.snapshot().AppRTTMicros; got != 3000 {
		t.Fatalf("AppRTTMicros=%d, want 3000 retained", got)
	}
}
