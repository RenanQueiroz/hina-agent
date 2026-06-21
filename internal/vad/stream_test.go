package vad

import (
	"testing"
	"time"
)

// scriptModel returns a scripted probability per window, independent of the audio.
// Probe advances through the script (holding the last value once exhausted).
type scriptModel struct {
	probs  []float32
	i      int
	closed bool
}

func (m *scriptModel) Probe(_ []float32) (float32, error) {
	p := float32(0)
	if len(m.probs) > 0 {
		if m.i < len(m.probs) {
			p = m.probs[m.i]
		} else {
			p = m.probs[len(m.probs)-1]
		}
	}
	m.i++
	return p, nil
}
func (m *scriptModel) Reset()       { m.i = 0 }
func (m *scriptModel) Close() error { m.closed = true; return nil }

// window returns a 512-sample window filled with value v (so pre-roll content is
// identifiable in tests).
func window(v float32) []float32 {
	w := make([]float32, WindowSize)
	for i := range w {
		w[i] = v
	}
	return w
}

// collect feeds windows one at a time and returns all events in order.
func collect(t *testing.T, s *Stream, windows [][]float32) []Event {
	t.Helper()
	var all []Event
	for _, w := range windows {
		evs, err := s.Write(w)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		all = append(all, evs...)
	}
	return all
}

func TestStreamEmitsStartWithPreroll(t *testing.T) {
	// PreSpeech 64ms = 2 windows of pre-roll. Two silence windows then speech: the
	// EvStart PCM must be preroll(2 windows) + the onset window = 3 windows long,
	// and carry the two preceding (silence) windows' audio as the pre-roll.
	p := Params{PreSpeech: 64 * time.Millisecond}
	m := &scriptModel{probs: []float32{0.0, 0.0, 0.9}}
	s := NewStream(m, p)
	evs := collect(t, s, [][]float32{window(1), window(2), window(9)})
	if len(evs) != 1 || evs[0].Kind != EvStart {
		t.Fatalf("events = %+v, want a single EvStart", kinds(evs))
	}
	if got, want := len(evs[0].PCM), 3*WindowSize; got != want {
		t.Fatalf("EvStart PCM = %d samples, want %d (2 preroll + 1 onset)", got, want)
	}
	// First WindowSize samples are the oldest pre-roll window (value 1), then 2, then 9.
	if evs[0].PCM[0] != 1 || evs[0].PCM[WindowSize] != 2 || evs[0].PCM[2*WindowSize] != 9 {
		t.Fatalf("pre-roll ordering wrong: [%v %v %v]", evs[0].PCM[0], evs[0].PCM[WindowSize], evs[0].PCM[2*WindowSize])
	}
}

func TestStreamRoutesSpeechAndEnd(t *testing.T) {
	p := Params{PreSpeech: 0, MinSilence: 96 * time.Millisecond, MinSpeech: 32 * time.Millisecond}
	silenceWins := windowsFor(p.MinSilence)
	// 1 onset + 5 speech windows, then enough silence to end.
	probs := []float32{0.9, 0.9, 0.9, 0.9, 0.9, 0.9}
	wins := [][]float32{window(0.9), window(0.9), window(0.9), window(0.9), window(0.9), window(0.9)}
	for i := 0; i < silenceWins; i++ {
		probs = append(probs, 0.0)
		wins = append(wins, window(0))
	}
	m := &scriptModel{probs: probs}
	s := NewStream(m, p)
	evs := collect(t, s, wins)

	if evs[0].Kind != EvStart {
		t.Fatalf("first event = %v, want EvStart", evs[0].Kind)
	}
	// Windows 2..6 are EvSpeech (ongoing speech forwarded to ASR).
	speech := 0
	for _, e := range evs {
		if e.Kind == EvSpeech {
			speech++
			if len(e.PCM) != WindowSize {
				t.Fatalf("EvSpeech PCM = %d, want one window", len(e.PCM))
			}
		}
	}
	if speech < 5 {
		t.Fatalf("got %d EvSpeech, want >=5 ongoing-speech windows", speech)
	}
	if last := evs[len(evs)-1]; last.Kind != EvEnd {
		t.Fatalf("terminal event = %v, want EvEnd", last.Kind)
	}
}

func TestStreamBuffersAcrossWrites(t *testing.T) {
	// Feed non-window-aligned chunks; the Stream must reassemble full windows. With
	// a flat all-speech script and a single big silence tail, it still detects the
	// onset + end correctly regardless of chunk boundaries.
	p := Params{PreSpeech: 0, MinSilence: 64 * time.Millisecond, MinSpeech: 32 * time.Millisecond}
	m := &scriptModel{probs: []float32{0.9}} // holds 0.9; we override via two streams below
	_ = m
	// Build one contiguous buffer: 4 speech windows then silenceWins silence windows.
	silenceWins := windowsFor(p.MinSilence)
	var probs []float32
	for i := 0; i < 4; i++ {
		probs = append(probs, 0.9)
	}
	for i := 0; i < silenceWins+1; i++ {
		probs = append(probs, 0.0)
	}
	total := len(probs) * WindowSize
	buf := make([]float32, total)
	sm := &scriptModel{probs: probs}
	s := NewStream(sm, p)
	// Feed in odd-sized chunks (300 samples) so windows straddle Write boundaries.
	var evs []Event
	for off := 0; off < total; off += 300 {
		end := off + 300
		if end > total {
			end = total
		}
		e, err := s.Write(buf[off:end])
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e...)
	}
	starts, ends := 0, 0
	for _, e := range evs {
		switch e.Kind {
		case EvStart:
			starts++
		case EvEnd:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("chunked feed: starts=%d ends=%d, want 1/1 (%v)", starts, ends, kinds(evs))
	}
}

func TestStreamCloseReleasesModel(t *testing.T) {
	m := &scriptModel{probs: []float32{0.0}}
	s := NewStream(m, Params{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !m.closed {
		t.Fatal("Stream.Close should close the underlying model")
	}
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
