package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/pion/webrtc/v4"
)

// fakeDC is a stand-in audio datachannel for pacer tests: a fixed BufferedAmount
// (to force/relieve backpressure), a send counter, and an optional Send error.
type fakeDC struct {
	buffered atomic.Uint64
	sent     atomic.Int64
	sendErr  error
}

func (f *fakeDC) BufferedAmount() uint64              { return f.buffered.Load() }
func (f *fakeDC) ReadyState() webrtc.DataChannelState { return webrtc.DataChannelStateOpen }
func (f *fakeDC) Send([]byte) error                   { f.sent.Add(1); return f.sendErr }

// gatedEngine blocks each Synthesize until released (or ctx cancels), so a test
// can hold a request in synthesis while another action supersedes it.
type gatedEngine struct {
	release    chan struct{}
	sampleRate int
}

func (e *gatedEngine) Available() bool    { return true }
func (e *gatedEngine) Status() tts.Status { return tts.Status{Available: true} }
func (e *gatedEngine) Close() error       { return nil }
func (e *gatedEngine) Synthesize(ctx context.Context, _ string, _ tts.Options) (*tts.Stream, error) {
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ch := make(chan tts.Segment)
	close(ch) // empty stream: completes immediately once started
	return tts.NewStream(ch, e.sampleRate), nil
}

func waitGen(t *testing.T, s *Session, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.speakMu.Lock()
		g := s.speakGen
		s.speakMu.Unlock()
		if g >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("speakGen never reached %d", want)
}

// A speak superseded WHILE synthesizing (by a barge-in / mode switch / close /
// newer speak) must abandon rather than starting playback — a slow older request
// can't resurrect after the thing that superseded it.
func TestSpeakSupersededDuringSynthesisAbandons(t *testing.T) {
	s, sink := newDSPSession()
	release := make(chan struct{})
	s.tts = &gatedEngine{release: release, sampleRate: tts.NativeSampleRate}

	go func() { _ = s.speak("hi", tts.Options{}) }()
	waitGen(t, s, 1) // the speak has bumped the generation and is blocked in Synthesize

	s.cancelSpeak() // a barge-in / mode switch / close arrives -> gen 2
	close(release)  // let the stale Synthesize return

	time.Sleep(30 * time.Millisecond)
	if sinkHas(sink, events.TypeTTSStarted) {
		t.Fatal("a speak superseded during synthesis must not start playback")
	}
	if s.out.mode() != ModeIdle {
		t.Fatalf("mode = %s, want idle", s.out.mode())
	}
}

// A frame whose Send fails is consumed but not delivered, so the reply completes
// truncated rather than reporting a clean completion.
func TestSpeakTruncatedOnSendError(t *testing.T) {
	s, sink := newDSPSession()
	dc := &fakeDC{sendErr: errors.New("send failed")} // no backpressure, but every Send fails
	s.out.attach(dc)
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}

	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSCompleted)
	e, _ := findEvent(sink, events.TypeTTSCompleted)
	var p struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("parse TTSCompleted payload: %v", err)
	}
	if !p.Truncated {
		t.Fatal("a completion with failed sends must report truncated=true")
	}
}

func findEvent(sink *fakeSink, typ string) (events.Event, bool) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.all {
		if e.Type == typ {
			return e, true
		}
	}
	return events.Event{}, false
}

func eventTruncated(t *testing.T, e events.Event) bool {
	t.Helper()
	var p struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("parse PlaybackStopped payload: %v", err)
	}
	return p.Truncated
}

// An explicit stop (idle / mode switch) is a TRUNCATING PlaybackStopped, so the
// browser drops buffered audio + closes its gate. Only a natural TTS completion
// is non-truncating.
func TestExplicitStopIsTruncating(t *testing.T) {
	s, sink := newDSPSession()
	s.out.start(newToneSource())
	s.out.stop()
	e, ok := findEvent(sink, events.TypePlaybackStopped)
	if !ok {
		t.Fatalf("no PlaybackStopped emitted; saw %v", sink.types())
	}
	if !eventTruncated(t, e) {
		t.Fatal("explicit stop must emit truncated=true")
	}
}

// A reply whose frames were dropped for backpressure completes but is reported
// truncated (the spoken audio was not fully delivered), not a clean completion.
func TestSpeakTruncatedWhenFramesDropped(t *testing.T) {
	s, sink := newDSPSession()
	dc := &fakeDC{}
	dc.buffered.Store(maxBufferedBytes + 1) // always over budget -> every send dropped
	s.out.attach(dc)
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}

	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSCompleted)
	e, _ := findEvent(sink, events.TypeTTSCompleted)
	var p struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("parse TTSCompleted payload: %v", err)
	}
	if !p.Truncated {
		t.Fatal("a completion with dropped frames must report truncated=true")
	}
}

// Under sustained backpressure (a slow/stuck-but-open channel) the pacer still
// pulls frames from the source, so a finite TTS source reaches Done (completion)
// instead of hanging; the SENDS are what get dropped to bound latency.
func TestTTSSourceDrainsUnderBackpressure(t *testing.T) {
	s, _ := newDSPSession()
	dc := &fakeDC{}
	dc.buffered.Store(maxBufferedBytes + 1) // always over budget -> every send dropped
	s.out.attach(dc)

	src := newTTSSource()
	src.Feed(make([]float32, audio.OutputFrameSamples*3)) // 3 frames of audio
	src.End()
	s.out.start(src)

	select {
	case <-src.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ttsSource never drained under sustained backpressure")
	}
	if dc.sent.Load() != 0 {
		t.Fatalf("all sends should be dropped under backpressure, sent=%d", dc.sent.Load())
	}
	s.out.stop()
}

// fakeEngine is a tts.Engine stand-in that streams pre-canned PCM segments.
type fakeEngine struct {
	available  bool
	sampleRate int
	segs       [][]float32
	synthErr   error // if set, Synthesize rejects synchronously
}

func (e *fakeEngine) Available() bool    { return e.available }
func (e *fakeEngine) Status() tts.Status { return tts.Status{Available: e.available} }
func (e *fakeEngine) Close() error       { return nil }
func (e *fakeEngine) Synthesize(ctx context.Context, _ string, _ tts.Options) (*tts.Stream, error) {
	if !e.available {
		return nil, tts.ErrUnavailable
	}
	if e.synthErr != nil {
		return nil, e.synthErr
	}
	ch := make(chan tts.Segment)
	go func() {
		defer close(ch)
		for i, pcm := range e.segs {
			select {
			case ch <- tts.Segment{Index: i, PCM: pcm, SampleRate: e.sampleRate}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return tts.NewStream(ch, e.sampleRate), nil
}

func sinkHas(sink *fakeSink, typ string) bool {
	for _, t := range sink.types() {
		if t == typ {
			return true
		}
	}
	return false
}

func waitSink(t *testing.T, sink *fakeSink, typ string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sinkHas(sink, typ) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %q not emitted; saw %v", typ, sink.types())
}

func TestTTSSourceDrainAndDone(t *testing.T) {
	src := newTTSSource()
	src.Feed([]float32{1, 2, 3})

	dst := make([]float32, 2)
	if n := src.Next(dst); n != 2 || dst[0] != 1 || dst[1] != 2 {
		t.Fatalf("Next = %d %v, want 2 [1 2]", n, dst)
	}
	select {
	case <-src.Done():
		t.Fatal("Done closed before End")
	default:
	}

	src.End()
	// One sample remains, so Done isn't closed until it drains.
	select {
	case <-src.Done():
		t.Fatal("Done closed while a sample remained")
	default:
	}
	if n := src.Next(dst); n != 1 || dst[0] != 3 || dst[1] != 0 {
		t.Fatalf("drain Next = %d %v, want 1 [3 0]", n, dst)
	}
	select {
	case <-src.Done():
	default:
		t.Fatal("Done not closed after ended + drained")
	}
	// Feed after End is ignored.
	src.Feed([]float32{9})
	if n := src.Next(dst); n != 0 {
		t.Fatalf("Feed after End should be ignored, got %d samples", n)
	}
}

// Draining a large utterance preserves sample order and ends cleanly even though
// Next compacts only occasionally (head-index drain, not compact-every-read).
func TestTTSSourceLargeDrainInOrder(t *testing.T) {
	src := newTTSSource()
	const total = 50000
	in := make([]float32, total)
	for i := range in {
		in[i] = float32(i)
	}
	src.Feed(in)
	src.End()

	out := make([]float32, 0, total)
	buf := make([]float32, 480)
	for {
		n := src.Next(buf)
		out = append(out, buf[:n]...)
		if n == 0 {
			break
		}
	}
	if len(out) != total {
		t.Fatalf("drained %d samples, want %d", len(out), total)
	}
	for i := range out {
		if out[i] != float32(i) {
			t.Fatalf("sample %d = %v, want %v (order corrupted)", i, out[i], float32(i))
		}
	}
	select {
	case <-src.Done():
	default:
		t.Fatal("Done not closed after full drain")
	}
}

func TestSpeakUnavailableEmitsError(t *testing.T) {
	s, sink := newDSPSession() // s.tts is nil
	s.speak("hello", tts.Options{})
	if !sinkHas(sink, events.TypeError) {
		t.Fatalf("expected error event for unavailable TTS, saw %v", sink.types())
	}
	if s.out.mode() != ModeIdle {
		t.Fatalf("no playback should start; mode=%s", s.out.mode())
	}
}

func TestSpeakStreamsAndCompletes(t *testing.T) {
	s, sink := newDSPSession()
	// 44.1kHz engine: two short segments. The session resamples to 24kHz.
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{
		make([]float32, 4410), // 0.1s
		make([]float32, 4410),
	}}

	s.speak("Hello there.", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)

	// No real datachannel/pacer here, so drive the drain ourselves to emulate the
	// pacer pulling frames until the utterance ends.
	s.out.mu.Lock()
	src, _ := s.out.source.(*ttsSource)
	s.out.mu.Unlock()
	if src == nil {
		t.Fatal("expected a ttsSource to be the active source")
	}
	buf := make([]float32, 480)
	done := false
	deadline := time.Now().Add(2 * time.Second)
	for !done && time.Now().Before(deadline) {
		src.Next(buf)
		select {
		case <-src.Done():
			src.Next(buf) // let the final drain close it out
			done = true
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if !done {
		t.Fatal("ttsSource never drained")
	}
	waitSink(t, sink, events.TypeTTSCompleted)
	waitSink(t, sink, events.TypePlaybackStopped)
	// A natural completion is the one non-truncating stop (the browser drains its
	// tail rather than dropping it).
	if e, ok := findEvent(sink, events.TypePlaybackStopped); ok && eventTruncated(t, e) {
		t.Fatal("a natural TTS completion must emit truncated=false")
	}
}

// A synchronous Synthesize rejection (e.g. too many sentences) emits an error and
// never starts playback or TTSStarted.
func TestSpeakSynchronousRejectStartsNoPlayback(t *testing.T) {
	s, sink := newDSPSession()
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, synthErr: tts.ErrTooManySegments}
	s.speak("a. a. a.", tts.Options{})
	if !sinkHas(sink, events.TypeError) {
		t.Fatalf("expected error event, saw %v", sink.types())
	}
	if sinkHas(sink, events.TypeTTSStarted) {
		t.Fatal("playback must not start on a synchronous rejection")
	}
	if s.out.mode() != ModeIdle {
		t.Fatalf("mode = %s, want idle", s.out.mode())
	}
}

// A rejected new SpeakText must NOT disturb an already-playing reply: validation
// happens before the active reply is superseded.
func TestSpeakRejectKeepsActiveReply(t *testing.T) {
	s, sink := newDSPSession()
	eng := &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 44100)}}
	s.tts = eng

	s.speak("first reply", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)
	s.out.mu.Lock()
	active := s.out.source
	s.out.mu.Unlock()
	if active == nil {
		t.Fatal("expected an active TTS source")
	}

	// The next request rejects synchronously; the active reply must keep playing.
	eng.synthErr = tts.ErrTooManySegments
	s.speak("a. a. a.", tts.Options{})
	if !sinkHas(sink, events.TypeError) {
		t.Fatalf("expected an error event, saw %v", sink.types())
	}
	s.out.mu.Lock()
	stillActive := s.out.source
	s.out.mu.Unlock()
	if stillActive != active {
		t.Fatal("a rejected request must not replace/stop the active reply")
	}
}

// An unknown ModeChanged cancels an in-flight spoken reply and stops the pacer
// (no lingering silence): the cancelled runSpeak halts its own epoch.
func TestUnknownModeStopsTTS(t *testing.T) {
	s, sink := newDSPSession()
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 44100)}}
	s.speak("reply", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)

	s.setMode("bogus") // unknown mode: cancels speak, starts nothing
	// The cancelled reply halts its epoch, returning the pacer to idle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.out.mode() == ModeIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pacer wedged in %q after unknown mode change", s.out.mode())
}

// A cancellation that lands during the drain wait (after the stream closed and
// src.End, before the pacer finishes draining) is a TRUNCATING stop, so buffered
// audio is dropped rather than leaked. (newDSPSession has no pacer/datachannel, so
// the source never drains on its own and runSpeak blocks in the drain-wait select.)
func TestSpeakCancelDuringDrainIsTruncating(t *testing.T) {
	s, sink := newDSPSession()
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)

	time.Sleep(30 * time.Millisecond) // let the stream close + runSpeak reach the drain wait
	s.cancelSpeak()
	waitSink(t, sink, events.TypePlaybackStopped)
	e, _ := findEvent(sink, events.TypePlaybackStopped)
	if !eventTruncated(t, e) {
		t.Fatal("cancellation during the drain wait must emit truncated=true")
	}
	if sinkHas(sink, events.TypeTTSCompleted) {
		t.Fatal("a cancelled reply must not report TTSCompleted")
	}
}

func TestSpeakCancelStops(t *testing.T) {
	s, sink := newDSPSession()
	// A blocking engine: it sends one segment then waits for cancellation.
	s.tts = &blockingEngine{released: make(chan struct{})}
	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)
	s.cancelSpeak() // barge-in / supersede
	// The synth goroutine should observe cancellation and stop (no panic / leak).
	time.Sleep(20 * time.Millisecond)
}

type blockingEngine struct{ released chan struct{} }

func (b *blockingEngine) Available() bool    { return true }
func (b *blockingEngine) Status() tts.Status { return tts.Status{Available: true} }
func (b *blockingEngine) Close() error       { return nil }
func (b *blockingEngine) Synthesize(ctx context.Context, _ string, _ tts.Options) (*tts.Stream, error) {
	ch := make(chan tts.Segment)
	go func() {
		defer close(ch)
		select {
		case ch <- tts.Segment{PCM: make([]float32, 4410), SampleRate: tts.NativeSampleRate}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done() // block until cancelled
	}()
	return tts.NewStream(ch, tts.NativeSampleRate), nil
}

// stallEngine returns a stream over a channel the test controls (never closed by
// a producer), to exercise cancellation while runSpeak is blocked waiting.
type stallEngine struct{ ch chan tts.Segment }

func (e *stallEngine) Available() bool    { return true }
func (e *stallEngine) Status() tts.Status { return tts.Status{Available: true} }
func (e *stallEngine) Close() error       { return nil }
func (e *stallEngine) Synthesize(_ context.Context, _ string, _ tts.Options) (*tts.Stream, error) {
	return tts.NewStream(e.ch, tts.NativeSampleRate), nil
}

func waitIdle(t *testing.T, s *Session, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.out.mode() == ModeIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s: pacer wedged in %q", msg, s.out.mode())
}

// Cancelling while the synthesis stream is stalled (blocked, never closing) must
// still halt the pacer — runSpeak selects on ctx, it doesn't only range.
func TestSpeakCancelWhileStreamStalled(t *testing.T) {
	s, sink := newDSPSession()
	s.tts = &stallEngine{ch: make(chan tts.Segment)} // never sends/closes
	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)
	s.cancelSpeak()
	waitIdle(t, s, "cancel during stalled stream")
}

// A segment delivered AFTER cancellation must not keep the pacer running.
func TestSpeakSegmentAfterCancelHalts(t *testing.T) {
	s, sink := newDSPSession()
	ch := make(chan tts.Segment, 1)
	s.tts = &stallEngine{ch: ch}
	s.speak("hi", tts.Options{})
	waitSink(t, sink, events.TypeTTSStarted)
	s.cancelSpeak()
	ch <- tts.Segment{PCM: make([]float32, 4410), SampleRate: tts.NativeSampleRate} // post-cancel
	waitIdle(t, s, "post-cancel segment")
}

// Mic PCM (via feedLoopback) must reach ONLY the loopback source — never a TTS or
// tone source, which would otherwise mix live mic into synthesized playback.
func TestFeedLoopbackOnlyFeedsLoopbackSource(t *testing.T) {
	s, _ := newDSPSession()

	ttsSrc := newTTSSource()
	s.out.start(ttsSrc)
	s.out.feedLoopback([]float32{1, 1, 1})
	ttsSrc.mu.Lock()
	got := len(ttsSrc.buf)
	ttsSrc.mu.Unlock()
	if got != 0 {
		t.Fatalf("tts source received %d mic samples, want 0", got)
	}
	s.out.stop()

	// Tone source: mic is dropped (no panic).
	s.out.start(newToneSource())
	s.out.feedLoopback([]float32{1, 1, 1})
	s.out.stop()

	// Loopback source: mic IS consumed.
	lb := newLoopbackSource()
	s.out.start(lb)
	s.out.feedLoopback([]float32{1, 1, 1})
	lb.mu.Lock()
	ln := len(lb.buf)
	lb.mu.Unlock()
	if ln != 3 {
		t.Fatalf("loopback source received %d mic samples, want 3", ln)
	}
	s.out.stop()
}

// Concurrent SpeakText requests must supersede atomically: the winner's epoch is
// the active one and its cancel is installed, so a single cancelSpeak returns the
// pacer to idle (a stale, pre-start request can't wedge a newer playback). Run
// under -race to catch the interleaving.
func TestConcurrentSpeakSupersession(t *testing.T) {
	s, _ := newDSPSession()
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.speak("x", tts.Options{})
		}()
	}
	wg.Wait()
	s.cancelSpeak()
	waitIdle(t, s, "after concurrent speaks")
}

func TestManagerSpeakNoSession(t *testing.T) {
	mgr, _ := testManager(t)
	if err := mgr.Speak("nobody", "hi", tts.Options{}); err != ErrNoSession {
		t.Fatalf("Speak with no session = %v, want ErrNoSession", err)
	}
}
