package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/events"
)

// fakeASRStream scripts a recognition stream: each Write fires an onPartial, and
// Finalize returns a canned result (or finalErr).
type fakeASRStream struct {
	onPartial     func(asr.Partial)
	final         asr.Final
	finalErr      error
	finalizeBlock chan struct{} // if non-nil, Finalize waits on it (to race with Close)
	closeCh       chan struct{} // if non-nil, Close closes it to unblock a blocked Finalize (models ctx-cancel)
	closeOnce     sync.Once
	tryWriteFull  atomic.Bool // when true, TryWrite drops (simulates backpressure)
	writes        atomic.Int64
	closed        atomic.Bool
	finalized     atomic.Bool // set when Finalize starts
	wroteAfterFin atomic.Bool // a Write arrived after Finalize started (a race violation)
}

func (s *fakeASRStream) Write(pcm []float32) error {
	if s.finalized.Load() {
		s.wroteAfterFin.Store(true)
	}
	if s.closed.Load() {
		return errors.New("closed")
	}
	s.writes.Add(1)
	if s.onPartial != nil {
		s.onPartial(asr.Partial{Text: "hina hel"})
	}
	return nil
}
func (s *fakeASRStream) TryWrite(pcm []float32) bool {
	if s.closed.Load() || s.tryWriteFull.Load() {
		return false // simulate a full input buffer (recognizer behind) -> frame dropped
	}
	_ = s.Write(pcm)
	return true
}
func (s *fakeASRStream) Finalize() (asr.Final, error) {
	s.finalized.Store(true)
	if s.finalizeBlock != nil {
		// Unblock either when released, or when Close cancels us (the real stream's
		// Finalize returns on its context being cancelled by Close).
		select {
		case <-s.finalizeBlock:
		case <-s.closeCh:
			return asr.Final{}, context.Canceled
		}
	}
	return s.final, s.finalErr
}
func (s *fakeASRStream) Close() error {
	s.closed.Store(true)
	if s.closeCh != nil {
		s.closeOnce.Do(func() { close(s.closeCh) })
	}
	return nil
}

type fakeASR struct {
	avail      bool
	stream     *fakeASRStream
	makeStream func() *fakeASRStream // if set, returns a fresh stream per NewStream (multi-segment tests)
	block      chan struct{}         // if non-nil, NewStream waits on it (simulates a cold load)
	entered    chan struct{}         // if non-nil, signaled when NewStream is entered
}

func (e *fakeASR) NewStream(ctx context.Context, _ asr.Options, onPartial func(asr.Partial)) (asr.Stream, error) {
	if !e.avail {
		return nil, asr.ErrUnavailable
	}
	if e.entered != nil {
		select {
		case e.entered <- struct{}{}:
		default:
		}
	}
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	st := e.stream
	if e.makeStream != nil {
		st = e.makeStream()
	}
	st.onPartial = onPartial
	return st, nil
}
func (e *fakeASR) Available() bool    { return e.avail }
func (e *fakeASR) Status() asr.Status { return asr.Status{Available: e.avail} }
func (e *fakeASR) Close() error       { return nil }

func TestListenEmitsPartialsAndFinal(t *testing.T) {
	s, sink := newDSPSession()
	stream := &fakeASRStream{final: asr.Final{Text: "hina hello", WakeDetected: true, Body: "hello"}}
	s.asr = &fakeASR{avail: true, stream: stream}

	s.startListen("en")
	if !sinkHas(sink, events.TypeListenStarted) {
		t.Fatal("ListenStarted not emitted")
	}
	// Feeding mic audio while listening drives partials.
	s.feedASR(make([]float32, 320))
	if !sinkHas(sink, events.TypeASRPartial) {
		t.Fatal("ASRPartial not emitted on fed audio")
	}
	if stream.writes.Load() == 0 {
		t.Fatal("audio was not written to the stream")
	}

	s.stopListen()
	waitSink(t, sink, events.TypeASRFinal)
	e, _ := findEvent(sink, events.TypeASRFinal)
	var p struct {
		Text         string `json:"text"`
		WakeDetected bool   `json:"wake_detected"`
		Body         string `json:"body"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("parse ASRFinal: %v", err)
	}
	if p.Text != "hina hello" || !p.WakeDetected || p.Body != "hello" {
		t.Fatalf("ASRFinal payload = %+v, want hina hello / true / hello", p)
	}
	if !stream.closed.Load() {
		t.Fatal("stream should be closed after stopListen")
	}
	// After stopping, audio is no longer routed anywhere (no active stream).
	before := stream.writes.Load()
	s.feedASR(make([]float32, 320))
	if stream.writes.Load() != before {
		t.Fatal("audio fed after stop should not reach the closed stream")
	}
}

func TestListenUnavailableEmitsError(t *testing.T) {
	s, sink := newDSPSession()
	s.asr = &fakeASR{avail: false, stream: &fakeASRStream{}}
	s.startListen("en")
	if !sinkHas(sink, events.TypeError) {
		t.Fatal("expected an error event when ASR is unavailable")
	}
	if sinkHas(sink, events.TypeListenStarted) {
		t.Fatal("ListenStarted must not be emitted when ASR is unavailable")
	}
}

func TestListenCloseTearsDownStream(t *testing.T) {
	s, _ := newDSPSession()
	stream := &fakeASRStream{}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.closeListen()
	if !stream.closed.Load() {
		t.Fatal("closeListen must close the active stream")
	}
}

// When mic frames are dropped under recognizer backpressure, the segment's
// ASRFinal must flag the transcript as truncated (not present silent loss as a
// clean result).
func TestListenFinalFlagsTruncatedOnDroppedFrames(t *testing.T) {
	s, sink := newDSPSession()
	stream := &fakeASRStream{final: asr.Final{Text: "partial"}}
	stream.tryWriteFull.Store(true) // every frame is dropped
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.feedASR(make([]float32, 320)) // dropped (TryWrite false) -> counted
	s.feedASR(make([]float32, 320))
	s.stopListen()

	waitSink(t, sink, events.TypeASRFinal)
	e, _ := findEvent(sink, events.TypeASRFinal)
	var p struct {
		Truncated     bool  `json:"truncated"`
		DroppedFrames int64 `json:"dropped_frames"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("parse ASRFinal: %v", err)
	}
	if !p.Truncated || p.DroppedFrames < 2 {
		t.Fatalf("ASRFinal truncated=%v dropped=%d, want truncated with >=2 dropped frames", p.Truncated, p.DroppedFrames)
	}
}

// A clean segment (no drops) must NOT be flagged truncated.
func TestListenFinalNotTruncatedWhenNoDrops(t *testing.T) {
	s, sink := newDSPSession()
	stream := &fakeASRStream{final: asr.Final{Text: "ok"}}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.feedASR(make([]float32, 320)) // accepted
	s.stopListen()
	waitSink(t, sink, events.TypeASRFinal)
	e, _ := findEvent(sink, events.TypeASRFinal)
	var p struct {
		Truncated bool `json:"truncated"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	if p.Truncated {
		t.Fatal("a segment with no dropped frames must not be flagged truncated")
	}
}

// A ListenStopped that arrives WHILE startListen is still cold-loading the model
// must cancel the pending start: the stream is discarded (closed), never
// installed, and no ListenStarted is emitted — so mic audio isn't routed to ASR
// after the segment was ended.
func TestListenStopDuringColdStart(t *testing.T) {
	s, sink := newDSPSession()
	block := make(chan struct{})
	stream := &fakeASRStream{}
	s.asr = &fakeASR{avail: true, stream: stream, block: block, entered: make(chan struct{}, 1)}

	go s.startListen("en")
	<-s.asr.(*fakeASR).entered // NewStream entered: asrStarting=true, gen captured

	s.stopListen() // ends the segment mid cold-load (bumps listenGen)
	close(block)   // let NewStream return the (now-superseded) stream

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.asrMu.Lock()
		starting := s.asrStarting
		installed := s.asrStream
		s.asrMu.Unlock()
		if !starting {
			if installed != nil {
				t.Fatal("a stream installed despite a stop during cold-start")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startListen never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if !stream.closed.Load() {
		t.Fatal("the cold-started stream must be closed when a stop superseded it")
	}
	if sinkHas(sink, events.TypeListenStarted) {
		t.Fatal("ListenStarted must not be emitted after a stop during cold-start")
	}
}

// Each listening segment must carry a distinct id, and an ASRFinal must be tagged
// with the id of the segment it finalized — so the client can drop a stale final
// from a prior segment that finalized after the next segment started. (The client
// filter is in web/src/lib/rtc.ts; this proves the server-side tagging contract.)
func TestListenSegmentIdsAreDistinctAndTagged(t *testing.T) {
	s, sink := newDSPSession()
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream { return &fakeASRStream{final: asr.Final{Text: "x"}} }}

	segOf := func(e events.Event) float64 {
		var p struct {
			Seg float64 `json:"seg"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("parse seg: %v", err)
		}
		return p.Seg
	}
	all := func(typ string) []events.Event {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		var out []events.Event
		for _, e := range sink.all {
			if e.Type == typ {
				out = append(out, e)
			}
		}
		return out
	}

	// Two full segments.
	s.startListen("en")
	s.stopListen()
	waitSink(t, sink, events.TypeASRFinal)
	s.startListen("en")
	s.stopListen()
	deadline := time.Now().Add(2 * time.Second)
	for len(all(events.TypeASRFinal)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second ASRFinal never arrived")
		}
		time.Sleep(time.Millisecond)
	}

	starts := all(events.TypeListenStarted)
	finals := all(events.TypeASRFinal)
	if len(starts) != 2 || len(finals) != 2 {
		t.Fatalf("got %d ListenStarted / %d ASRFinal, want 2 each", len(starts), len(finals))
	}
	if segOf(starts[0]) == segOf(starts[1]) {
		t.Fatal("the two segments must have distinct ids")
	}
	// Each ASRFinal is tagged with its own segment's id (final[i] pairs start[i]).
	for i := range finals {
		if segOf(finals[i]) != segOf(starts[i]) {
			t.Fatalf("ASRFinal #%d seg %v != its segment's start seg %v", i, segOf(finals[i]), segOf(starts[i]))
		}
	}
}

// A ListenStopped that runs AFTER the synchronous reservation but while the start
// is still cold-loading must invalidate the start: the control handler reserves +
// captures the generation synchronously (beginListen) before spawning the load
// (finishListen), so a stop that bumps the generation in between causes the start
// to discard its stream rather than install it after the segment was ended. This
// is the ordering the real handleControlMessage path uses.
func TestListenStopOvertakesAsyncStart(t *testing.T) {
	s, sink := newDSPSession()
	block := make(chan struct{})
	stream := &fakeASRStream{}
	s.asr = &fakeASR{avail: true, stream: stream, block: block}

	// Mirror handleControlMessage: reserve synchronously, then load off-goroutine.
	gen, ok := s.beginListen()
	if !ok {
		t.Fatal("beginListen should reserve the segment")
	}
	// A ListenStopped arrives before the load completes (bumps the generation).
	s.stopListen()
	go s.finishListen("en", gen)
	close(block) // let NewStream return the now-superseded stream

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.asrMu.Lock()
		starting, installed := s.asrStarting, s.asrStream
		s.asrMu.Unlock()
		if !starting {
			if installed != nil {
				t.Fatal("a stream was installed despite a stop overtaking the start")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finishListen never completed")
		}
		time.Sleep(time.Millisecond)
	}
	if !stream.closed.Load() {
		t.Fatal("the superseded stream must be closed")
	}
	if sinkHas(sink, events.TypeListenStarted) {
		t.Fatal("ListenStarted must not be emitted after a stop overtook the start")
	}
}

// feedASR (inbound goroutine) and stopListen (control goroutine) run
// concurrently; a frame must never reach the stream after its Finalize began.
// feedASR holds asrMu across Write, so a write is fully enqueued before a stop
// can take the stream, or the stop wins and the frame is dropped — never an
// out-of-order write after finalize. Run under -race for the interleavings.
func TestFeedASRNeverWritesAfterFinalize(t *testing.T) {
	for trial := 0; trial < 200; trial++ {
		s, _ := newDSPSession()
		stream := &fakeASRStream{final: asr.Final{Text: "x"}}
		s.asr = &fakeASR{avail: true, stream: stream}
		s.startListen("en")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.feedASR(make([]float32, 320)) }()
		go func() { defer wg.Done(); s.stopListen() }()
		wg.Wait()

		// Wait for the async finalize to complete, then assert no write raced past it.
		deadline := time.Now().Add(time.Second)
		for !stream.finalized.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if stream.wroteAfterFin.Load() {
			t.Fatalf("trial %d: a frame was written to the stream after Finalize began", trial)
		}
	}
}

// A stale max-duration timer (captured for segment A) must NOT finalize or
// invalidate a newer segment B that took over after A was stopped — timedStop's
// generation check + capture are atomic under asrMu, so it is a clean no-op here.
func TestListenTimedStopIgnoresSupersededSegment(t *testing.T) {
	s, sink := newDSPSession()
	sA := &fakeASRStream{final: asr.Final{Text: "A"}}
	sB := &fakeASRStream{final: asr.Final{Text: "B"}}
	streams := []*fakeASRStream{sA, sB}
	idx := 0
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream { st := streams[idx]; idx++; return st }}

	// Segment A starts and is stopped normally.
	genA, ok := s.beginListen()
	if !ok {
		t.Fatal("begin A")
	}
	s.finishListen("en", genA)
	s.stopListen()
	waitSink(t, sink, events.TypeASRFinal) // A finalized

	// Segment B starts and stays active.
	genB, ok := s.beginListen()
	if !ok {
		t.Fatal("begin B")
	}
	s.finishListen("en", genB)

	// Segment A's stale timer fires now — must be a no-op.
	s.timedStop(genA)

	s.asrMu.Lock()
	active := s.asrStream
	s.asrMu.Unlock()
	if active != sB {
		t.Fatal("segment B was wrongly stopped/replaced by segment A's stale timer")
	}
	if sB.closed.Load() {
		t.Fatal("segment B's stream was wrongly closed by a stale timer")
	}
}

// A normal stop whose Finalize hangs (stalled recognizer, session still live)
// must not orphan the stream: the finalize watchdog times out, force-closes the
// stream, and emits a terminal — without waiting unbounded or needing the whole
// session to close.
func TestListenFinalizeTimeoutAbortsStalled(t *testing.T) {
	defer func(d int64) { finalizeTimeoutNs.Store(d) }(finalizeTimeoutNs.Load())
	finalizeTimeoutNs.Store(int64(30 * time.Millisecond))

	s, sink := newDSPSession()
	// Finalize blocks forever unless Close unblocks it (closeCh), modeling a real
	// stream whose Finalize only returns when its context is cancelled by Close.
	stream := &fakeASRStream{finalizeBlock: make(chan struct{}), closeCh: make(chan struct{})}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.stopListen() // finalizeSegment's Finalize hangs -> watchdog must fire

	waitSink(t, sink, events.TypeListenStopped) // timeout terminal
	if !stream.closed.Load() {
		t.Fatal("a stalled stream must be force-closed on finalize timeout")
	}
	if sinkHas(sink, events.TypeASRFinal) {
		t.Fatal("a timed-out finalize must not emit ASRFinal")
	}
}

// Even a NON-COOPERATIVE stream (whose Finalize/Close never honors cancellation)
// must not pin the finalize goroutine or leave the client listening: the watchdog
// emits the terminal independent of the stream actually shutting down.
func TestListenFinalizeTimeoutTerminalIndependentOfStream(t *testing.T) {
	defer func(d int64) { finalizeTimeoutNs.Store(d) }(finalizeTimeoutNs.Load())
	finalizeTimeoutNs.Store(int64(30 * time.Millisecond))

	s, sink := newDSPSession()
	block := make(chan struct{})
	// closeCh nil -> Close does NOT unblock Finalize (non-cooperative).
	stream := &fakeASRStream{finalizeBlock: block}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")

	done := make(chan struct{})
	go func() { s.stopListen(); close(done) }()

	// The terminal must be emitted on the timeout even though Finalize never returns.
	waitSink(t, sink, events.TypeListenStopped)
	if sinkHas(sink, events.TypeASRFinal) {
		t.Fatal("a timed-out finalize must not emit ASRFinal")
	}
	// stopListen itself returns promptly (it just spawns finalizeSegment).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopListen blocked")
	}
	close(block) // let the (otherwise-leaked) inner Finalize goroutine exit cleanly
}

// A segment the client never stops must be auto-finalized after maxListenDuration
// (emitting ASRFinal), so it can't pin ASR indefinitely.
func TestListenAutoFinalizesAfterMaxDuration(t *testing.T) {
	defer func(d time.Duration) { maxListenDuration = d }(maxListenDuration)
	maxListenDuration = 30 * time.Millisecond

	s, sink := newDSPSession()
	stream := &fakeASRStream{final: asr.Final{Text: "auto"}}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	// Do NOT call stopListen — the max-duration timer must finalize on its own.
	waitSink(t, sink, events.TypeASRFinal)
	if !stream.closed.Load() {
		t.Fatal("auto-finalized stream should be closed")
	}
	// A max-duration auto-finalize cut the segment short -> truncated.
	e, _ := findEvent(sink, events.TypeASRFinal)
	var p struct {
		Truncated bool   `json:"truncated"`
		Reason    string `json:"truncation_reason"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	if !p.Truncated || p.Reason != "max_duration" {
		t.Fatalf("auto-finalized ASRFinal truncated=%v reason=%q, want truncated/max_duration", p.Truncated, p.Reason)
	}
	// The segment is now inactive: a late client ListenStopped is a no-op.
	s.stopListen()
}

// A terminal decode failure surfaced by Finalize must emit an ErrorEvent plus a
// terminal ListenStopped (so the client clears its listening state), not a
// (false) ASRFinal.
func TestListenFinalizeErrorEmitsError(t *testing.T) {
	s, sink := newDSPSession()
	stream := &fakeASRStream{finalErr: errors.New("decode failed")}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.stopListen()
	waitSink(t, sink, events.TypeError)
	waitSink(t, sink, events.TypeListenStopped)
	if sinkHas(sink, events.TypeASRFinal) {
		t.Fatal("a Finalize error must emit ErrorEvent + ListenStopped, not ASRFinal")
	}
}

// If the session is torn down while a stopped segment is still finalizing, the
// finalize goroutine must NOT publish a stale ASRFinal/ErrorEvent after teardown,
// and the stream must be closed (releasing its bundle ref).
func TestListenSessionCloseDuringFinalizeNoStaleEvent(t *testing.T) {
	s, sink := newDSPSession()
	block := make(chan struct{})
	stream := &fakeASRStream{finalizeBlock: block, final: asr.Final{Text: "stale"}}
	s.asr = &fakeASR{avail: true, stream: stream}
	s.startListen("en")
	s.stopListen() // launches the finalize goroutine, which blocks in Finalize
	// Simulate Session.Close's teardown (the DSP test session has no peer
	// connection, so the full Close would nil-panic on pc.Close): mark closed,
	// tear down listen state, and cancel the session context.
	s.closed.Store(true)
	s.closeListen()
	s.cancel()
	close(block) // let Finalize return

	// The goroutine should observe isClosed() and skip emitting; the stream closes.
	deadline := time.Now().Add(2 * time.Second)
	for !stream.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("finalizing stream was never closed after session teardown")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // allow any (erroneous) emit to land
	if sinkHas(sink, events.TypeASRFinal) {
		t.Fatal("a stale ASRFinal was published after session teardown")
	}
}
