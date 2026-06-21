package vad

// Model returns a speech probability for one fixed-size audio window. It is the
// seam between the pure-Go turn logic and the Silero ONNX graph: the real model
// (engine.go) runs the graph over the shared internal/onnx runtime, while tests
// drive the Stream with a synthetic Model. Implementations are STATEFUL (Silero
// carries an LSTM state across windows) and single-flight; Reset clears the carry
// at a stream boundary.
type Model interface {
	// Probe returns the speech probability in [0,1] for one WindowSize-sample
	// window of 16 kHz mono float32.
	Probe(window []float32) (float32, error)
	// Reset clears the recurrent state for a fresh stream/segment.
	Reset()
	// Close releases the model's resources (its share of the shared bundle).
	Close() error
}

// EventKind classifies a Stream event.
type EventKind int

const (
	// EvStart: speech onset. PCM carries the pre-roll (pre-speech padding) followed
	// by the onset window — feed it to the ASR as the segment's leading audio.
	EvStart EventKind = iota
	// EvSpeech: ongoing confirmed-speech audio (one or more windows). Feed to ASR.
	EvSpeech
	// EvEnd: trailing silence committed the turn. Finalize the ASR segment.
	EvEnd
	// EvCancel: the segment was a sub-MinSpeech blip — discard the ASR segment;
	// it is not a turn and (during playback) not a barge-in.
	EvCancel
	// EvMax: the segment hit MaxDuration — force-commit (finalize) it; a new
	// segment may immediately follow.
	EvMax
)

// Event is one turn-boundary signal with any associated audio (EvStart / EvSpeech
// carry PCM; the terminal kinds carry nil). PCM is freshly allocated, so the
// receiver may retain it.
type Event struct {
	Kind EventKind
	PCM  []float32
}

// Stream is one live VAD session: it buffers a continuous 16 kHz mono stream into
// 512-sample windows, probes each with the Model, runs the Detector, and emits
// turn-boundary Events with pre-roll. Not safe for concurrent use — drive it from
// one goroutine (the rtc inbound read loop, or a benchmark replay).
type Stream struct {
	model Model
	det   *Detector

	carry      []float32 // leftover samples (< WindowSize) awaiting a full window
	preroll    []float32 // rolling pre-speech audio (most recent prerollCap samples)
	prerollCap int
}

// NewStream builds a Stream over a Model with the given params. The Model's state
// is reset so a reused Model starts clean.
func NewStream(model Model, p Params) *Stream {
	det := NewDetector(p)
	model.Reset()
	prerollWindows := int(det.params.PreSpeech / windowDuration)
	return &Stream{
		model:      model,
		det:        det,
		prerollCap: prerollWindows * WindowSize,
	}
}

// Write consumes a chunk of 16 kHz mono PCM and returns the ordered turn-boundary
// events it produced (possibly several, e.g. a whole utterance fed at once). The
// input slice is not retained. A Model error aborts the chunk and is returned (the
// already-produced events are still returned alongside it).
func (s *Stream) Write(pcm []float32) ([]Event, error) {
	if len(pcm) == 0 {
		return nil, nil
	}
	// Assemble full windows from any carry + the new audio.
	s.carry = append(s.carry, pcm...)
	var events []Event
	for len(s.carry) >= WindowSize {
		window := s.carry[:WindowSize]
		prob, err := s.model.Probe(window)
		if err != nil {
			// Drop the consumed window and return what we have plus the error.
			s.carry = append([]float32(nil), s.carry[WindowSize:]...)
			return events, err
		}
		ev, ok := s.step(float64(prob), window)
		if ok {
			events = append(events, ev...)
		}
		// Advance past the consumed window (copy the tail so the backing array of the
		// caller's slice isn't pinned across calls).
		s.carry = append([]float32(nil), s.carry[WindowSize:]...)
	}
	return events, nil
}

// step applies the Detector decision for one window and routes the window's audio
// to the right event(s). It returns the events for this window.
func (s *Stream) step(prob float64, window []float32) ([]Event, bool) {
	decision := s.det.Push(prob)
	var events []Event
	switch decision {
	case Start:
		// Pre-roll (pre-speech padding) + the onset window become the segment's
		// leading audio so the first phoneme isn't clipped.
		seg := make([]float32, 0, len(s.preroll)+WindowSize)
		seg = append(seg, s.preroll...)
		seg = append(seg, window...)
		events = append(events, Event{Kind: EvStart, PCM: seg})
	case Continue:
		if s.det.InSpeech() {
			events = append(events, Event{Kind: EvSpeech, PCM: append([]float32(nil), window...)})
		}
	case End:
		events = append(events, Event{Kind: EvEnd})
	case Cancel:
		events = append(events, Event{Kind: EvCancel})
	case Max:
		events = append(events, Event{Kind: EvMax})
	}
	s.pushPreroll(window)
	return events, len(events) > 0
}

// pushPreroll appends one window to the rolling pre-speech buffer, evicting the
// oldest samples beyond prerollCap. During speech the buffer just rolls (it is
// only read at the next Start, by which point >= MinSilence of silence has
// overwritten any speech audio, since prerollCap < MinSilence).
func (s *Stream) pushPreroll(window []float32) {
	if s.prerollCap == 0 {
		return
	}
	s.preroll = append(s.preroll, window...)
	if len(s.preroll) > s.prerollCap {
		s.preroll = append([]float32(nil), s.preroll[len(s.preroll)-s.prerollCap:]...)
	}
}

// Reset clears the buffered audio + detector state for a fresh stream (also resets
// the Model carry), e.g. when a live session restarts listening.
func (s *Stream) Reset() {
	s.carry = nil
	s.preroll = nil
	s.det.Reset()
	s.model.Reset()
}

// Close releases the stream's Model (its share of the shared model bundle). It is
// idempotent insofar as the Model's Close is.
func (s *Stream) Close() error { return s.model.Close() }
