package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/RenanQueiroz/hina-agent/internal/vad"
	"github.com/RenanQueiroz/hina-agent/internal/voice"
	"github.com/pion/webrtc/v4"
)

// stringMsg builds a client control-channel message (a JSON event envelope) for the
// handleControlMessage path.
func stringMsg(t *testing.T, typ string, payload any) webrtc.DataChannelMessage {
	t.Helper()
	e, err := events.New(events.SourceClient, typ, "", "", "", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return webrtc.DataChannelMessage{IsString: true, Data: raw}
}

// vadEnergyModel is a fake vad.Model whose speech probability tracks frame energy,
// so loud test frames read as speech and zero frames as silence — exercising the
// real VAD state machine + pipeline without ORT.
type vadEnergyModel struct{}

func (vadEnergyModel) Probe(w []float32) (float32, error) {
	var s float64
	for _, x := range w {
		s += float64(x) * float64(x)
	}
	if len(w) > 0 && s/float64(len(w)) >= 0.001 {
		return 0.95, nil
	}
	return 0.02, nil
}
func (vadEnergyModel) Reset()       {}
func (vadEnergyModel) Close() error { return nil }

type fakeVADEngine struct {
	avail   bool
	block   chan struct{} // if non-nil, NewStream waits on it (simulates a cold load)
	entered chan struct{} // if non-nil, signaled when NewStream is entered
}

func (e *fakeVADEngine) Available() bool { return e.avail }
func (e *fakeVADEngine) NewStream(ctx context.Context, p vad.Params) (*vad.Stream, error) {
	if !e.avail {
		return nil, vad.ErrUnavailable
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
	return vad.NewStream(vadEnergyModel{}, p), nil
}

// fakeAgent records turns and returns a canned reply, optionally blocking (to keep a
// reply "in flight" for barge-in tests). It also records MarkTurnInterrupted calls.
type fakeAgent struct {
	reply       string
	calls       atomic.Int64
	lastReq     atomic.Value // string
	block       chan struct{}
	entered     chan struct{}
	interrupted atomic.Int64  // count of MarkTurnInterrupted calls
	lastTurnID  atomic.Value  // string: the turn id marked interrupted
	markEntered atomic.Bool   // set when MarkTurnInterrupted is entered
	markBlock   chan struct{} // if non-nil, MarkTurnInterrupted waits on it (ordering tests)
	reserves    atomic.Int64  // total BeginInterrupt (fence reserve) calls
	reserved    atomic.Int64  // currently-held fences (reserve - release)
	afterRun    func()        // if set, called right before RunTurn returns (races the commit handoff)
	reqMu       sync.Mutex
	reqs        []string // ordered transcripts seen by RunTurn (serial-order assertions)
}

func (a *fakeAgent) requests() []string {
	a.reqMu.Lock()
	defer a.reqMu.Unlock()
	return append([]string(nil), a.reqs...)
}

func (a *fakeAgent) RunTurn(ctx context.Context, _, _, transcript string, onDelta func(string), onCommitted func(string)) (string, string, error) {
	n := a.calls.Add(1)
	a.lastReq.Store(transcript)
	a.reqMu.Lock()
	a.reqs = append(a.reqs, transcript)
	a.reqMu.Unlock()
	if a.entered != nil {
		select {
		case a.entered <- struct{}{}:
		default:
		}
	}
	if a.block != nil {
		select {
		case <-a.block:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if onDelta != nil {
		onDelta(a.reply)
	}
	turnID := fmt.Sprintf("trn_%d", n)
	if a.afterRun != nil {
		a.afterRun() // simulate a barge-in/stop racing the post-commit handoff
	}
	if onCommitted != nil {
		onCommitted(turnID) // the durable-commit callback (still under the turn lock in prod)
	}
	return a.reply, turnID, nil
}

func (a *fakeAgent) BeginInterrupt(_ string) func() {
	a.reserves.Add(1) // count of fences reserved (the interrupt happens-before edge)
	a.reserved.Add(1) // currently-held fences
	return func() { a.reserved.Add(-1) }
}

func (a *fakeAgent) MarkTurnInterrupted(ctx context.Context, _, _, turnID string, _ int64) error {
	a.markEntered.Store(true)
	if a.markBlock != nil {
		select {
		case <-a.markBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Store the id BEFORE bumping the count, so a test that waits on interrupted==N then
	// reads lastTurnID can't observe the count before the id is recorded.
	a.lastTurnID.Store(turnID)
	a.interrupted.Add(1)
	return nil
}

func liveSpeechFrame() []float32 {
	w := make([]float32, vad.WindowSize)
	for i := range w {
		w[i] = 0.2
	}
	return w
}
func liveSilenceFrame() []float32 { return make([]float32, vad.WindowSize) }

func feedFrames(s *Session, frame []float32, n int) {
	for i := 0; i < n; i++ {
		s.feedLive(frame)
	}
}

// newLiveSession builds a DSP session wired with fake VAD/ASR/TTS/Agent for the
// live loop. Each ASR NewStream (the keep-warm pin + one per logical turn) returns
// a fresh fakeASRStream from finals[] in order (the keep-warm takes index 0; it is
// never finalized so its final is irrelevant).
func newLiveSession(t *testing.T, ag *fakeAgent, finals ...asr.Final) (*Session, *fakeSink) {
	t.Helper()
	s, sink := newDSPSession()
	var idx int
	var mu sync.Mutex
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		f := asr.Final{}
		if idx < len(finals) {
			f = finals[idx]
		}
		idx++
		return &fakeASRStream{final: f}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	return s, sink
}

func startLiveAndWait(t *testing.T, s *Session, sink *fakeSink, td voice.TurnDetection) {
	t.Helper()
	s.startLive(td)
	waitSink(t, sink, events.TypeSessionUpdated)
}

func TestLiveTurnCommitsAndReplies(t *testing.T) {
	ag := &fakeAgent{reply: "Sure, lights on."}
	// finals[0] = keep-warm (unused); finals[1] = turn 1.
	s, sink := newLiveSession(t, ag,
		asr.Final{},
		asr.Final{Text: "hina turn on the lights", WakeDetected: true, Body: "turn on the lights"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12) // a spoken request
	if !sinkHas(sink, events.TypeSpeechStarted) {
		t.Fatal("SpeechStarted not emitted on speech onset")
	}
	feedFrames(s, liveSilenceFrame(), 6) // trailing silence commits the turn

	waitSink(t, sink, events.TypeASRFinal)
	// The agent runs with the wake-stripped request, and the reply is spoken.
	waitSinkCond(t, func() bool { return ag.calls.Load() == 1 })
	if got, _ := ag.lastReq.Load().(string); got != "turn on the lights" {
		t.Fatalf("agent request = %q, want the wake-stripped body", got)
	}
	waitSink(t, sink, events.TypeTTSStarted)
	s.stopLive()
}

// TestLiveTwoTurnsKeepDistinctTranscripts guards the per-turn ASR-stream design:
// each logical turn finalizes its OWN recognizer, so a fast second turn cannot have
// its audio interleaved with the first turn's finalize and corrupt either
// transcript. The two turns must surface their two distinct finals in order.
func TestLiveTwoTurnsKeepDistinctTranscripts(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag,
		asr.Final{}, // keep-warm
		asr.Final{Text: "first request", Body: "first request"},   // turn 1
		asr.Final{Text: "second request", Body: "second request"}) // turn 2
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 1 })
	// Turn 2 immediately after.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 2 })

	var reqs []string
	for _, e := range sink.events() {
		if e.Type == events.TypeASRFinal {
			var p struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			reqs = append(reqs, p.Body)
		}
	}
	if len(reqs) != 2 || reqs[0] != "first request" || reqs[1] != "second request" {
		t.Fatalf("turn transcripts = %v, want [first request, second request] (no cross-turn corruption)", reqs)
	}
	s.stopLive()
}

// TestLiveBargeInWhileTTSStillPlaying is the #1 regression: the agent reply finishes
// GENERATING (replyCancel cleared) while the TTS source is still draining. Barge-in
// must stay armed off the actual playback state — not just reply generation — so
// user speech over the still-playing reply interrupts it.
func TestLiveBargeInWhileTTSStillPlaying(t *testing.T) {
	ag := &fakeAgent{reply: "A spoken answer."} // returns immediately
	s, sink := newLiveSession(t, ag,
		asr.Final{},
		asr.Final{Text: "what time is it", Body: "what time is it"},
		asr.Final{Text: "actually never mind", Body: "actually never mind"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 commits and the reply is spoken; generation completes immediately but the
	// TTS source keeps "playing" (the DSP session's pacer has no datachannel to drain).
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSink(t, sink, events.TypeTTSStarted)
	waitSinkCond(t, func() bool { return s.out.isTTSPlaying() })

	// The user talks over the still-playing reply -> barge-in.
	feedFrames(s, liveSpeechFrame(), 6)
	waitSink(t, sink, events.TypeUserInterrupted)
	if !sinkHas(sink, events.TypeConversationTruncated) {
		t.Fatal("barge-in during TTS playback should emit ConversationTruncated")
	}
	// The already-committed assistant turn (generation finished before the barge-in)
	// must be DURABLY marked interrupted at the played boundary.
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	if got, _ := ag.lastTurnID.Load().(string); got == "" {
		t.Fatal("MarkTurnInterrupted should carry the committed turn id")
	}
	s.stopLive()
}

func TestLiveRejectsWithoutConversation(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	s.conversationID = "" // a standalone (loopback/tone) call has no conversation
	s.startLive(voice.TurnDetection{Type: voice.ServerVAD})
	if !sinkHas(sink, events.TypeError) {
		t.Fatal("live mode without a conversation should emit an error")
	}
	if s.liveActive() {
		t.Fatal("live must not activate without a conversation")
	}
}

// TestLiveFinalizeTimeoutAborts is the #3 regression: a turn whose recognizer
// Finalize stalls must be aborted by the watchdog (the stream force-closed, the
// goroutine freed) and must NOT emit a stale ASRFinal.
func TestLiveFinalizeTimeoutAborts(t *testing.T) {
	defer func(d int64) { finalizeTimeoutNs.Store(d) }(finalizeTimeoutNs.Load())
	finalizeTimeoutNs.Store(int64(40 * time.Millisecond))

	ag := &fakeAgent{reply: "ok"}
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	// A turn recognizer whose Finalize blocks until Close (closeCh) — i.e. it stalls.
	stalled := &fakeASRStream{finalizeBlock: make(chan struct{}), closeCh: make(chan struct{})}
	var idx int
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		idx++
		if idx == 1 {
			return &fakeASRStream{} // keep-warm
		}
		return stalled // the turn
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6) // commit -> finalize stalls -> watchdog fires

	// The watchdog force-closes the stalled stream and never emits an ASRFinal.
	waitSinkCond(t, func() bool { return stalled.closed.Load() })
	// It DOES emit a terminal (SpeechStopped) so the client clears its speaking state.
	waitSink(t, sink, events.TypeSpeechStopped)
	if sinkHas(sink, events.TypeASRFinal) {
		t.Fatal("a timed-out finalize must not emit ASRFinal")
	}
	if ag.calls.Load() != 0 {
		t.Fatal("a timed-out finalize must not run the agent reply")
	}
	s.stopLive()
}

// TestLiveIgnoresManualControlsWhileConversing covers the round-4 fix: while live
// mode owns playback + recognition, every manual playback/listen/interrupt control
// (SpeakText, ModeChanged, ListenStarted, UserInterrupted) must be ignored so it
// can't stop/replace a live reply outside the live-aware truncation path.
func TestLiveIgnoresManualControlsWhileConversing(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Manual SpeakText -> no TTS.
	s.handleControlMessage(stringMsg(t, events.TypeSpeakText, map[string]any{"text": "manual demo"}))
	if sinkHas(sink, events.TypeTTSStarted) {
		t.Fatal("manual SpeakText must be ignored while conversing")
	}
	// Manual ModeChanged -> the outbound source is not switched to tone.
	s.handleControlMessage(stringMsg(t, events.TypeModeChanged, map[string]any{"mode": ModeTone}))
	if s.out.mode() == ModeTone {
		t.Fatal("manual ModeChanged must be ignored while conversing")
	}
	// Manual ListenStarted -> the manual listen path stays inactive (no ListenStarted ack).
	s.handleControlMessage(stringMsg(t, events.TypeListenStarted, map[string]any{"language": "en"}))
	if sinkHas(sink, events.TypeListenStarted) {
		t.Fatal("manual ListenStarted must be ignored while conversing")
	}
	// Manual UserInterrupted -> ignored (no effect); the session stays live.
	s.handleControlMessage(stringMsg(t, events.TypeUserInterrupted, map[string]any{"epoch": 1, "played_samples": 0}))
	if !s.liveActive() {
		t.Fatal("a manual UserInterrupted must not tear down live mode")
	}
	s.stopLive()
}

// TestManagerSpeakRejectedWhileLive covers the HTTP /realtime/speak path: Manager.Speak
// must reject a manual speak while the user's session is in live mode.
func TestManagerSpeakRejectedWhileLive(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	mgr := &Manager{sessions: map[string]*Session{s.userID: s}}
	if err := mgr.Speak(s.userID, "hi", tts.Options{}); err != ErrLiveActive {
		t.Fatalf("Manager.Speak while live = %v, want ErrLiveActive", err)
	}
	s.stopLive()
}

func TestLiveStartUnavailableWithoutEngines(t *testing.T) {
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: false} // VAD off
	s.asr = &fakeASR{avail: true, stream: &fakeASRStream{}}
	s.agent = &fakeAgent{}
	s.startLive(voice.TurnDetection{Type: voice.ServerVAD})
	if !sinkHas(sink, events.TypeError) {
		t.Fatal("startLive without a VAD engine should emit an error")
	}
	if s.liveActive() {
		t.Fatal("live must not activate when the VAD engine is unavailable")
	}
}

func TestLiveBargeInTruncatesAndCancelsReply(t *testing.T) {
	ag := &fakeAgent{reply: "A long winded answer.", block: make(chan struct{}), entered: make(chan struct{}, 1)}
	s, sink := newLiveSession(t, ag,
		asr.Final{}, asr.Final{Text: "hina what time is it", Body: "what time is it"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1: a request that commits and starts a (blocking) reply -> reply in flight.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	select {
	case <-ag.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("agent reply never started")
	}

	// Barge-in: the user talks over the assistant. The fake ASR partial ("hina hel")
	// is non-backchannel, so it confirms the interruption.
	feedFrames(s, liveSpeechFrame(), 4)
	waitSink(t, sink, events.TypeUserInterrupted)
	if !sinkHas(sink, events.TypeConversationTruncated) {
		t.Fatal("a barge-in should emit ConversationTruncated")
	}
	// The interrupted reply's context was cancelled, so RunTurn returns.
	waitSinkCond(t, func() bool { return ag.calls.Load() >= 1 })
	close(ag.block)
	s.stopLive()
}

// TestLiveEchoGatedThroughPacer is the #4 regression: production must feed the
// assistant's outbound TTS frames into echo suppression via the pacer (not only in
// tests). It attaches a real audio datachannel so the pacer drains a LOUD TTS reply
// and calls observePlayback, then proves quiet residual echo on the mic during that
// playback does NOT open a new turn — without the test ever touching the pipeline's
// ObservePlayback directly.
func TestLiveEchoGatedThroughPacer(t *testing.T) {
	ag := &fakeAgent{reply: "a loud spoken reply"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Text: "play music", Body: "play music"})
	s.out.attach(&fakeDC{}) // a working audio channel so the pacer actually drains + observes
	// A loud, ~1 s reply so the echo guard rises well above the quiet echo level.
	loud := make([]float32, tts.NativeSampleRate)
	for i := range loud {
		loud[i] = 0.6
	}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{loud}}
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 -> the reply is spoken; the pacer drains the loud TTS and feeds the echo
	// guard.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSink(t, sink, events.TypeTTSStarted)
	waitSinkCond(t, func() bool { return s.out.isTTSPlaying() })
	time.Sleep(200 * time.Millisecond) // let the pacer feed several loud frames

	before := countType(sink, events.TypeSpeechStarted)
	// Quiet residual echo on the mic during playback must be gated -> no new turn.
	quiet := make([]float32, vad.WindowSize)
	for i := range quiet {
		quiet[i] = 0.02
	}
	feedFrames(s, quiet, 20)
	if got := countType(sink, events.TypeSpeechStarted); got != before {
		t.Fatalf("quiet echo during playback opened %d new turn(s); want 0 (echo not gated through the pacer)", got-before)
	}
	s.stopLive()
}

// TestLiveDroppedTTSFramesDoNotSuppress is the #4 regression: on a degraded link
// where outbound TTS frames are dropped for backpressure (never delivered), they
// must NOT raise the echo guard — otherwise real user speech would be falsely gated
// as echo and barge-in would fail exactly when the link is bad.
func TestLiveDroppedTTSFramesDoNotSuppress(t *testing.T) {
	ag := &fakeAgent{reply: "a loud spoken reply"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Text: "x", Body: "x"})
	dc := &fakeDC{}
	dc.buffered.Store(maxBufferedBytes + 1) // sustained backpressure: every send is dropped
	s.out.attach(dc)
	loud := make([]float32, tts.NativeSampleRate)
	for i := range loud {
		loud[i] = 0.6
	}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{loud}}
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSink(t, sink, events.TypeTTSStarted)
	waitSinkCond(t, func() bool { return s.out.isTTSPlaying() })
	time.Sleep(150 * time.Millisecond) // pacer pulls + DROPS loud frames (never delivered)

	before := countType(sink, events.TypeSpeechStarted)
	// The user speaks while the (undelivered) reply "plays": it must open a turn, not
	// be gated as echo — the dropped frames never raised the guard.
	feedFrames(s, liveSpeechFrame(), 8)
	if got := countType(sink, events.TypeSpeechStarted); got <= before {
		t.Fatal("user speech during an UNDELIVERED (dropped) TTS reply must not be suppressed as echo")
	}
	s.stopLive()
}

func countType(sink *fakeSink, typ string) int {
	n := 0
	for _, ty := range sink.types() {
		if ty == typ {
			n++
		}
	}
	return n
}

// TestLiveEntryResetsManualState is the round-5 regression: entering live mode must
// clear pre-existing manual playback (and a manual ASR segment), so they can't keep
// running underneath the live loop.
func TestLiveEntryResetsManualState(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	// A manual tone is playing AND a manual listen segment is open before live mode.
	s.setMode(ModeTone)
	if s.out.mode() != ModeTone {
		t.Fatal("precondition: tone should be playing")
	}
	s.startListen("en")
	waitSink(t, sink, events.TypeListenStarted)

	// Enter live mode.
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// The manual tone playback is stopped (the live loop owns outbound audio now).
	if s.out.mode() == ModeTone {
		t.Fatal("entering live mode must stop the pre-existing manual tone playback")
	}
	// The manual ASR segment is torn down (no active manual stream).
	s.asrMu.Lock()
	manualOpen := s.asrStream != nil
	s.asrMu.Unlock()
	if manualOpen {
		t.Fatal("entering live mode must close a pre-existing manual ASR segment")
	}
	s.stopLive()
}

// TestLiveStaleStartDoesNotResetManualState covers the round-6 race: a live start
// that is superseded (stopped) while its engines cold-load must NOT, when it
// resumes, tear down manual playback/listen the user created after exiting startup.
func TestLiveStaleStartDoesNotResetManualState(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	s.vad = &fakeVADEngine{avail: true, block: block, entered: entered}

	// Begin live start; finishLive blocks opening the VAD stream (cold load).
	s.startLive(voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	<-entered

	// The user exits live startup, then sets up fresh manual state.
	s.stopLive() // bumps liveGen, clears liveStarting
	s.setMode(ModeTone)
	s.startListen("en")
	waitSink(t, sink, events.TypeListenStarted)

	// The stale finishLive resumes (VAD cold-load unblocks) and must discard itself
	// WITHOUT resetting manual state. Give it time to fully run its discard path.
	close(block)
	time.Sleep(100 * time.Millisecond)
	if s.liveActive() {
		t.Fatal("a superseded live start must not become active")
	}
	// The manual tone + listen the user created survive.
	if s.out.mode() != ModeTone {
		t.Fatal("a superseded live start must not stop the user's new manual tone")
	}
	s.asrMu.Lock()
	manualOpen := s.asrStream != nil
	s.asrMu.Unlock()
	if !manualOpen {
		t.Fatal("a superseded live start must not close the user's new manual ASR segment")
	}
	s.stopListen()
}

// TestFeedLiveGatedUntilReady covers the round-7 staging fix: while a live start is
// installed but not yet ready (the window where pre-existing manual state is being
// cleared), inbound mic audio must NOT be fed into the live pipeline (so manual
// playback echo can't open/barge a live turn before cleanup completes).
func TestFeedLiveGatedUntilReady(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Simulate the install->cleanup window: live installed but not ready.
	s.liveMu.Lock()
	s.liveReady = false
	s.liveMu.Unlock()

	feedFrames(s, liveSpeechFrame(), 20) // loud speech that WOULD open a turn if processed
	if sinkHas(sink, events.TypeSpeechStarted) {
		t.Fatal("inbound audio must not open a live turn while liveReady is false")
	}
	s.stopLive()
}

// TestLiveStaleEventsDropped is the round-8 regression: a turn-detection event
// captured from a pipeline that has since been stopped/superseded (the Write->handle
// boundary crossed a stop) must be dropped by the ownership check — it must not open
// a recognizer, interrupt playback, run a reply, or emit live turn events on the
// now-current session state.
func TestLiveStaleEventsDropped(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	s.liveMu.Lock()
	staleLp := s.livePipeline
	staleGen := s.liveGen
	s.liveMu.Unlock()

	// Stop live mode, then start a manual tone the stale events must not disturb.
	s.stopLive()
	s.setMode(ModeTone)
	beforeStarted := countType(sink, events.TypeSpeechStarted)
	beforeInterrupt := countType(sink, events.TypeUserInterrupted)

	// Replay stale events directly, exactly as feedLive's post-Write handle loop would.
	s.handleLiveEvent(staleLp, staleGen, voice.Event{Kind: voice.Open, PCM: liveSpeechFrame()})
	s.handleLiveEvent(staleLp, staleGen, voice.Event{Kind: voice.Audio, PCM: liveSpeechFrame()})
	s.handleLiveEvent(staleLp, staleGen, voice.Event{Kind: voice.BargeIn})
	s.handleLiveEvent(staleLp, staleGen, voice.Event{Kind: voice.Commit})
	s.handleLiveEvent(staleLp, staleGen, voice.Event{Kind: voice.Cancel})

	s.liveMu.Lock()
	turnOpen := s.turnRecog != nil
	s.liveMu.Unlock()
	if turnOpen {
		t.Fatal("a stale Open must not install a recognizer on the stopped session")
	}
	if countType(sink, events.TypeSpeechStarted) != beforeStarted {
		t.Fatal("a stale Open must not emit SpeechStarted")
	}
	if countType(sink, events.TypeUserInterrupted) != beforeInterrupt {
		t.Fatal("a stale BargeIn must not interrupt the now-current session")
	}
	if s.out.mode() != ModeTone {
		t.Fatal("a stale BargeIn must not truncate the manual tone started after stop")
	}
	if ag.calls.Load() != 0 {
		t.Fatal("a stale Commit must not run an agent reply")
	}
	// A stale terminal emit (e.g. a SpeechStarted from an openTurn that raced a stop)
	// is dropped by the ownership-checked emitLive.
	beforeTerminal := countType(sink, events.TypeSpeechStopped)
	s.emitLive(staleGen, staleLp, events.TypeSpeechStopped, map[string]any{"reason": "x"})
	if countType(sink, events.TypeSpeechStopped) != beforeTerminal {
		t.Fatal("emitLive must drop a live terminal for a superseded pipeline")
	}
}

// TestLiveNextTurnDuringFinalizeNotBargeIn is the round-12 regression: the
// assistant-playing signal must NOT be raised during the pure ASR-finalize window
// (before any reply), so a next user turn that begins while the previous turn is
// still finalizing is a normal turn — not a false barge-in with a spurious
// UserInterrupted/ConversationTruncated.
func TestLiveNextTurnDuringFinalizeNotBargeIn(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	slow := &fakeASRStream{
		finalizeBlock: make(chan struct{}),
		closeCh:       make(chan struct{}),
		final:         asr.Final{Text: "first", Body: "first"},
	}
	var idx int
	var mu sync.Mutex
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		idx++
		if idx == 2 { // index 1 = keep-warm, index 2 = turn 1
			return slow
		}
		return &fakeASRStream{final: asr.Final{Text: "second", Body: "second"}}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	s.out.attach(&fakeDC{}) // a working audio channel so the pacer drains each reply (the worker awaits playback)
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 commits; its finalize blocks (slow recognizer), so it is mid-finalize.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return slow.finalized.Load() })

	beforeInterrupt := countType(sink, events.TypeUserInterrupted)
	// Turn 2 begins + commits WHILE turn 1 is still finalizing (no reply has started).
	// It must be a normal turn (SpeechStarted), never a barge-in.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	if countType(sink, events.TypeUserInterrupted) != beforeInterrupt {
		t.Fatal("a next user turn during the previous turn's finalize must not be a barge-in")
	}
	if sinkHas(sink, events.TypeConversationTruncated) {
		t.Fatal("no ConversationTruncated should be emitted for a turn that begins during finalize")
	}
	// Turn 1 is still finalizing (serial worker), so NO reply has run yet — turn 2 is
	// queued behind it, not answered ahead of it.
	if ag.calls.Load() != 0 {
		t.Fatalf("no reply should run while turn 1 is still finalizing (calls=%d)", ag.calls.Load())
	}

	// Release turn 1's finalize: the serial worker now answers BOTH turns IN ORDER —
	// turn 1 ("first") then the queued turn 2 ("second"). Neither is dropped.
	close(slow.finalizeBlock)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 2 })
	if got := ag.requests(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("serial worker answered %v, want [first second] in order", got)
	}
	s.stopLive()
}

// TestLiveSerializesOverlappingTurns is the round-15 regression: turns are answered
// strictly in commit order by the serial worker, so an older turn whose finalize is
// slow is NOT dropped or overtaken by a newer one — it is answered first, then the
// newer turn. (A newer segment that turns out to be a blip never enqueues at all, so
// it likewise can't discard the older turn.)
func TestLiveSerializesOverlappingTurns(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	slow := &fakeASRStream{
		finalizeBlock: make(chan struct{}),
		closeCh:       make(chan struct{}),
		final:         asr.Final{Text: "older", Body: "older"},
	}
	var idx int
	var mu sync.Mutex
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		idx++
		if idx == 2 { // index 1 = keep-warm, index 2 = turn 1
			return slow
		}
		return &fakeASRStream{final: asr.Final{Text: "newer", Body: "newer"}}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	s.out.attach(&fakeDC{}) // a working audio channel so the pacer drains each reply (the worker awaits playback)
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 commits; finalize blocks (mid-finalize on the serial worker).
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return slow.finalized.Load() })

	// Turn 2 fully commits while turn 1 is still finalizing -> it is queued.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	time.Sleep(40 * time.Millisecond)
	if ag.calls.Load() != 0 {
		t.Fatalf("the queued newer turn must not be answered ahead of the older one (calls=%d)", ag.calls.Load())
	}

	// Release the older turn: both are answered, older FIRST, and the older reply's
	// audio PLAYS OUT before the newer reply speaks — the worker waits on playback
	// completion, so the newer reply never truncates the older one's TTS outside a
	// barge-in. Both spoken replies therefore complete un-truncated.
	close(slow.finalizeBlock)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 2 })
	if got := ag.requests(); len(got) != 2 || got[0] != "older" || got[1] != "newer" {
		t.Fatalf("answered %v, want [older newer] — the older turn must not be dropped or overtaken", got)
	}
	waitSinkCond(t, func() bool { return countType(sink, events.TypeTTSCompleted) == 2 })
	if sinkHas(sink, events.TypeConversationTruncated) {
		t.Fatal("neither serial reply should be truncated: the worker awaits playback before the next reply")
	}
	for _, e := range sink.events() {
		if e.Type == events.TypeTTSCompleted {
			var p struct {
				Truncated bool `json:"truncated"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.Truncated {
				t.Fatal("a serial reply's audio was truncated; the next reply cut it off")
			}
		}
	}
	s.stopLive()
}

// TestLiveQueueFullDropsWithTerminal is the round-16 regression: when the serial reply
// queue is full (head-of-line blocking), a further committed turn is dropped — but NOT
// silently: it emits a turn-tagged Error + SpeechStopped{queue_full} (so the client
// doesn't hang on its SpeechStarted) and closes the recognizer (no leak).
func TestLiveQueueFullDropsWithTerminal(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream { return &fakeASRStream{} }}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	s.liveMu.Lock()
	lp := s.livePipeline
	myGen := s.liveGen
	s.liveMu.Unlock()

	// Block the worker on a job whose finalize stalls (then errors, so cleanup is fast
	// with no reply), then FILL the queue so a further commit must be dropped.
	blocker := &fakeASRStream{finalizeBlock: make(chan struct{}), closeCh: make(chan struct{}), finalErr: errors.New("x")}
	lp.commits <- commitJob{myGen: myGen, myTurn: 100, recog: blocker}
	waitSinkCond(t, func() bool { return blocker.finalized.Load() }) // worker now stuck mid-finalize
	for i := 0; i < liveCommitQueue; i++ {
		lp.commits <- commitJob{myGen: myGen, myTurn: uint64(200 + i), recog: &fakeASRStream{finalErr: errors.New("x")}}
	}

	// Stage + commit a turn against the now-full queue: it must be dropped with a
	// terminal and its recognizer closed.
	dropped := &fakeASRStream{}
	s.liveMu.Lock()
	s.turnSeq = 300
	s.turnRecog = dropped
	s.liveMu.Unlock()
	s.commitTurn(lp, myGen)

	if !dropped.closed.Load() {
		t.Fatal("a queue-full dropped turn must have its recognizer closed (no leak)")
	}
	stop, errEv := false, false
	for _, e := range sink.events() {
		if e.Type == events.TypeSpeechStopped {
			var p struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.Reason == "queue_full" {
				stop = true
			}
		}
		if e.Type == events.TypeError {
			errEv = true
		}
	}
	if !stop {
		t.Fatal("a queue-full drop must emit SpeechStopped{reason:queue_full} so the client doesn't hang")
	}
	if !errEv {
		t.Fatal("a queue-full drop must emit an Error terminal for the lost segment")
	}
	if s.metrics.snapshot().DroppedTurns != 1 {
		t.Fatalf("DroppedTurns = %d, want 1", s.metrics.snapshot().DroppedTurns)
	}

	close(blocker.finalizeBlock) // let the worker drain
	s.stopLive()
}

// TestWaitPlayedOutReleasesOnClientCursor is the round-17 regression: the live worker
// waits for the CLIENT to play out the sent audio (its reported cursor reaches the sent
// samples), not just the server-side drain — so the next reply's worklet reset can't
// drop the previous reply's still-buffered tail.
func TestWaitPlayedOutReleasesOnClientCursor(t *testing.T) {
	s, _ := newDSPSession()
	o := s.out
	o.mu.Lock()
	o.epoch = 1
	o.sent = 24000 // 1s @ 24 kHz still unplayed by the client
	o.playedSamples = 0
	o.mu.Unlock()

	released := make(chan struct{})
	go func() { o.waitPlayedOut(context.Background(), 1); close(released) }()

	// Must NOT release on server drain alone — the client hasn't played it yet.
	select {
	case <-released:
		t.Fatal("waitPlayedOut released before the client played out the sent audio")
	case <-time.After(60 * time.Millisecond):
	}
	// The client reports it has now played everything that was sent -> release.
	o.recordCursor(1, 24000, 0, 0)
	select {
	case <-released:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("waitPlayedOut did not release after the client cursor reached the sent samples")
	}
}

// TestWaitPlayedOutBoundedAndCancelable: a silent client (cursor never catches up) is
// bounded by the deadline, and a barge-in/stop (ctx cancel) returns at once.
func TestWaitPlayedOutBoundedAndCancelable(t *testing.T) {
	s, _ := newDSPSession()
	o := s.out
	o.mu.Lock()
	o.epoch = 1
	o.sent = 24000 * 30 // 30s: far beyond maxPlayoutWait, and the client never reports
	o.playedSamples = 0
	o.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{})
	go func() { o.waitPlayedOut(ctx, 1); close(released) }()
	cancel() // a barge-in / stop cancels the reply ctx
	select {
	case <-released:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("waitPlayedOut must return immediately on ctx cancel (barge-in/stop)")
	}
}

// TestLiveWorkerStopClosesQueuedCommit is the round-18/19 regression, exercising REAL
// stopLive: a turn queued behind a slow finalize when stop runs must be CLOSED, never
// finalized. The worker re-checks ownership UNDER liveMu (synchronized with stopLive's
// liveMu-held state clear), so a queued job dequeued after stop is closed, not run.
func TestLiveWorkerStopClosesQueuedCommit(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	slow := &fakeASRStream{
		finalizeBlock: make(chan struct{}),
		closeCh:       make(chan struct{}),
		finalErr:      errors.New("x"), // errors after release -> fast cleanup, no reply
	}
	var idx int
	var mu sync.Mutex
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		idx++
		if idx == 2 { // index 1 = keep-warm, index 2 = turn 1 (slow)
			return slow
		}
		return &fakeASRStream{final: asr.Final{Body: "must not run"}}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	s.liveMu.Lock()
	lp := s.livePipeline
	myGen := s.liveGen
	s.liveMu.Unlock()

	// Turn 1 commits; the worker is now blocked in turn 1's finalize.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return slow.finalized.Load() })

	// Queue turn 2 behind it, then run REAL stopLive.
	rec := &fakeASRStream{final: asr.Final{Body: "must not run"}}
	lp.commits <- commitJob{myGen: myGen, myTurn: 50, recog: rec}
	s.stopLive()

	// Release turn 1's finalize so the worker reaches turn 2: it must CLOSE turn 2's
	// recognizer (ownership lost), not finalize/reply it.
	close(slow.finalizeBlock)
	waitSinkCond(t, func() bool { return rec.closed.Load() })
	if ag.calls.Load() != 0 {
		t.Fatalf("a turn queued when stop ran must be closed, not finalized (agent calls=%d)", ag.calls.Load())
	}
}

// TestBargeInMarksInterruptedAfterServerDrain is the round-18/19 regression for both
// barge-in windows where the pacer is NOT running yet interruptPlayback reports
// was=false but the committed turn must still be durably marked interrupted:
//   - the post-drain client-playout TAIL (round 18), and
//   - the post-generation / pre-TTS window (round 19), where RunTurn has committed the
//     turn and replyTurnID is recorded but out.start hasn't run.
//
// Both present the same state — replyTurnID set, no pacer — so a single state-level test
// covers them (an end-to-end synth-blocking variant would deadlock, since speakStream
// runs under liveMu). The mark must fire (gated on turnID, not `was`).
func TestBargeInMarksInterruptedAfterServerDrain(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream { return &fakeASRStream{} }}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	s.liveMu.Lock()
	lp := s.livePipeline
	myGen := s.liveGen
	s.liveMu.Unlock()

	// Stage the tail window: a committed assistant turn is being spoken (replyTurnID
	// set), but the server pacer is NOT running (out never started -> was=false).
	ctx, cancel := context.WithCancel(context.Background())
	s.liveMu.Lock()
	s.replyCtx, s.replyCancel = ctx, cancel
	s.replyTurnID = "trn_tail"
	s.liveMu.Unlock()

	s.bargeIn(lp, myGen)
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	if id, _ := ag.lastTurnID.Load().(string); id != "trn_tail" {
		t.Fatalf("marked turn %q interrupted, want trn_tail", id)
	}
	s.stopLive()
}

// TestLiveStopAbortsInFlightWorkerFinalize is the round-20 regression: a stopLive that
// lands while the serial worker is mid-finalize (past its ownership re-check) must ABORT
// the finalize — it closes the registered workerRecog, cancelling Finalize — so no
// expensive stale ASR work runs after stop and no reply is produced.
func TestLiveStopAbortsInFlightWorkerFinalize(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	slow := &fakeASRStream{
		finalizeBlock: make(chan struct{}),
		closeCh:       make(chan struct{}),
		final:         asr.Final{Body: "x"},
	}
	var idx int
	var mu sync.Mutex
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		idx++
		if idx == 2 { // index 1 = keep-warm, index 2 = turn 1 (slow finalize)
			return slow
		}
		return &fakeASRStream{}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 commits -> the worker is now mid-finalize (workerRecog registered).
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return slow.finalized.Load() })

	// stopLive aborts it: it closes workerRecog -> Finalize returns -> no reply runs.
	s.stopLive()
	waitSinkCond(t, func() bool { return slow.closed.Load() })
	time.Sleep(50 * time.Millisecond)
	if ag.calls.Load() != 0 {
		t.Fatalf("stop must abort the in-flight finalize before any reply (agent calls=%d)", ag.calls.Load())
	}
}

// TestClearWorkerRecogIdentityGuarded is the round-21 regression: a stale (old-session)
// worker unwinding must NOT clear a restarted session's worker recognizer handle, or a
// later stop couldn't abort that in-flight finalize.
func TestClearWorkerRecogIdentityGuarded(t *testing.T) {
	s, _ := newDSPSession()
	recogA := &fakeASRStream{}
	recogB := &fakeASRStream{}
	s.liveMu.Lock()
	s.workerRecog = recogB // the NEW session's worker registered recogB
	s.liveMu.Unlock()

	// The OLD worker unwinds and tries to deregister ITS recog (recogA): recogB survives.
	s.clearWorkerRecog(recogA)
	s.liveMu.Lock()
	got := s.workerRecog
	s.liveMu.Unlock()
	if got != asr.Stream(recogB) {
		t.Fatal("clearWorkerRecog wiped a newer session's worker registration")
	}
	// Deregistering the registered one clears it.
	s.clearWorkerRecog(recogB)
	s.liveMu.Lock()
	got = s.workerRecog
	s.liveMu.Unlock()
	if got != nil {
		t.Fatal("clearWorkerRecog should deregister its own recog")
	}
}

// TestPacerSkipsLeadingTTSSilence is the round-21 regression: a TTS source whose first
// segment hasn't arrived (real==0) must NOT advance the cursor bound (o.sent), so a
// barge-in before any audible reply records played_ms 0 ("[interrupted]"), not a phantom
// heard prefix from silent pre-roll. Real audio IS counted once it flows.
func TestPacerSkipsLeadingTTSSilence(t *testing.T) {
	s, _ := newDSPSession()
	s.out.attach(&fakeDC{})
	src := newTTSSource() // no audio produced yet
	s.out.start(src)

	time.Sleep(90 * time.Millisecond) // several pacer ticks of leading silence
	s.out.mu.Lock()
	sentSilence := s.out.sent
	s.out.mu.Unlock()
	if sentSilence != 0 {
		t.Fatalf("leading TTS silence advanced the cursor bound (o.sent=%d, want 0)", sentSilence)
	}

	// Once real audio arrives it IS counted.
	src.Feed(make([]float32, 2400)) // 100 ms @ 24 kHz of audible samples
	src.End()
	time.Sleep(130 * time.Millisecond)
	s.out.mu.Lock()
	sentReal := s.out.sent
	s.out.mu.Unlock()
	if sentReal == 0 {
		t.Fatal("real TTS audio was not counted in the cursor bound")
	}
	s.out.stop()
}

// TestLiveOverlongReplyMarkedInterrupted is the round-22 regression: when TTS REJECTS a
// reply before any playback (here, text over maxSpeakBytes), the durably-committed
// assistant turn — which was never spoken — must be marked interrupted at played_ms 0, so
// BuildContext doesn't feed the full unheard text back to the model.
func TestLiveOverlongReplyMarkedInterrupted(t *testing.T) {
	ag := &fakeAgent{reply: strings.Repeat("a", maxSpeakBytes+10)} // exceeds the TTS cap
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 1 }) // the reply is committed

	// TTS rejects the over-long reply -> the committed turn is marked interrupted.
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	s.stopLive()
}

// TestLiveDroppedTTSMarkedTruncated is the round-22 regression: when the spoken reply is
// cut short by dropped frames (a degraded link), the committed turn must be marked
// interrupted at the heard boundary rather than treated as fully delivered.
func TestLiveDroppedTTSMarkedTruncated(t *testing.T) {
	ag := &fakeAgent{reply: "a spoken reply"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	dc := &fakeDC{}
	dc.buffered.Store(maxBufferedBytes) // a degraded link: every audio frame is dropped
	s.out.attach(dc)
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return ag.calls.Load() == 1 }) // the reply is committed

	// The reply's TTS frames are all dropped -> truncated -> the turn is marked.
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	s.stopLive()
}

// TestStopLiveMarksInFlightReply is the round-23 regression: stopping live mode (or
// disconnecting) while a committed assistant turn is being spoken must durably mark that
// turn interrupted, so a reload / restarted session doesn't feed the full unheard reply.
func TestStopLiveMarksInFlightReply(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// A committed reply is being spoken (replyTurnID set).
	ctx, cancel := context.WithCancel(context.Background())
	s.liveMu.Lock()
	s.replyCtx, s.replyCancel = ctx, cancel
	s.replyTurnID = "trn_speaking"
	s.liveMu.Unlock()

	s.stopLive()
	// stopLive reserves the interrupt fence SYNCHRONOUSLY (under liveMu) before returning.
	if ag.reserves.Load() < 1 {
		t.Fatal("stopLive must reserve the interrupt fence before acking the stop")
	}
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	if id, _ := ag.lastTurnID.Load().(string); id != "trn_speaking" {
		t.Fatalf("stopLive marked turn %q interrupted, want trn_speaking", id)
	}
}

// TestLiveTruncatedMarkBlocksNextTurn is the round-23 regression: the interrupted-
// metadata write for a truncated reply must commit BEFORE the serial worker releases the
// next queued turn — whose RunTurn reads conversation context — or the next model request
// could read the full unheard reply. With a blocking MarkTurnInterrupted, the next turn
// must not run until it unblocks. Turn 2 is queued during turn 1's FINALIZE (nothing
// playing yet), so it is a clean queued turn, not a barge-in.
func TestLiveTruncatedMarkBlocksNextTurn(t *testing.T) {
	ag := &fakeAgent{reply: "r", markBlock: make(chan struct{})}
	slow := &fakeASRStream{
		finalizeBlock: make(chan struct{}),
		closeCh:       make(chan struct{}),
		final:         asr.Final{Body: "one"},
	}
	var idx int
	var mu sync.Mutex
	s, sink := newDSPSession()
	s.vad = &fakeVADEngine{avail: true}
	s.asr = &fakeASR{avail: true, makeStream: func() *fakeASRStream {
		mu.Lock()
		defer mu.Unlock()
		idx++
		if idx == 2 { // index 1 = keep-warm, index 2 = turn 1 (slow finalize)
			return slow
		}
		return &fakeASRStream{final: asr.Final{Body: "two"}}
	}}
	s.tts = &fakeEngine{available: true, sampleRate: tts.NativeSampleRate, segs: [][]float32{make([]float32, 4410)}}
	s.agent = ag
	dc := &fakeDC{}
	dc.buffered.Store(maxBufferedBytes) // drop every frame -> each reply is truncated -> mark
	s.out.attach(dc)
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	// Turn 1 commits; its finalize blocks, so the worker is busy and nothing is playing.
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return slow.finalized.Load() })
	// Turn 2 commits + QUEUES (a clean turn, not a barge-in — no reply is playing).
	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)

	// Release turn 1's finalize: its reply runs (call 1), is truncated (dropped frames),
	// and the SYNCHRONOUS mark blocks the worker on markBlock.
	close(slow.finalizeBlock)
	waitSinkCond(t, func() bool { return ag.markEntered.Load() })
	// The interrupt fence is HELD across the mark (reserved before waitPlayedOut), so a
	// concurrent text POST would block at awaitInterrupts.
	if ag.reserved.Load() < 1 {
		t.Fatal("the interrupt fence must be held while the truncation mark is in flight")
	}
	time.Sleep(70 * time.Millisecond)
	if ag.calls.Load() != 1 {
		t.Fatalf("next turn ran before the interrupted metadata committed (calls=%d)", ag.calls.Load())
	}

	close(ag.markBlock) // the mark commits -> the worker may release the next turn
	waitSinkCond(t, func() bool { return ag.calls.Load() == 2 })
	s.stopLive()
}

// TestBargeInMarkIsSynchronous is the round-24 regression: a barge-in's interrupted
// mark is SYNCHRONOUS on the feedLive goroutine, so it commits before that goroutine can
// process the barge-in utterance into a new turn — closing the window where the next
// turn's RunTurn would build context from the stale full reply.
func TestBargeInMarkIsSynchronous(t *testing.T) {
	ag := &fakeAgent{reply: "ok", markBlock: make(chan struct{})}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	s.liveMu.Lock()
	lp := s.livePipeline
	myGen := s.liveGen
	ctx, cancel := context.WithCancel(context.Background())
	s.replyCtx, s.replyCancel = ctx, cancel
	s.replyTurnID = "trn_speaking"
	s.liveMu.Unlock()

	done := make(chan struct{})
	go func() { s.bargeIn(lp, myGen); close(done) }()
	waitSinkCond(t, func() bool { return ag.markEntered.Load() })
	select {
	case <-done:
		t.Fatal("bargeIn returned before its interrupted mark committed (must be synchronous)")
	case <-time.After(60 * time.Millisecond):
	}
	close(ag.markBlock)
	<-done
}

// TestBargeInRacingCommitMarksTurn is the round-27 regression: if a barge-in / stop
// races the window between RunTurn durably committing the assistant turn and runLiveReply
// installing replyTurnID (bumping replyGen so the install loses), the committed FULL turn
// must STILL be marked interrupted at played_ms=0 — otherwise the next turn reads it as
// fully delivered though the user heard none of it.
func TestBargeInRacingCommitMarksTurn(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	// Right before RunTurn returns the committed turn, bump replyGen as a concurrent
	// barge-in would — so runLiveReply's replyTurnID install sees a stale gen.
	ag.afterRun = func() {
		s.liveMu.Lock()
		s.replyGen++
		s.liveMu.Unlock()
	}
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	if id, _ := ag.lastTurnID.Load().(string); id != "trn_1" {
		t.Fatalf("raced-commit turn marked %q, want trn_1 (the committed reply)", id)
	}
	// The raced-commit mark runs inside onCommitted, UNDER RunTurn's per-conversation turn
	// lock, so it needs no separate fence — the turn lock orders it before the next turn.
	s.stopLive()
}

// TestLiveReplyFenceReservedAtCommit is the round-28/29 regression: the playback fence is
// reserved by the onCommitted callback — which RunTurn invokes AFTER the durable commit
// but BEFORE it releases the per-conversation turn lock — and held through playback. So a
// concurrent text POST (which must claim the same lock) can't read the just-committed
// reply during the commit handoff or playback before its final state is settled. (The
// fence is reserved at RunTurn's END, never before it, so RunTurn's own awaitInterrupts —
// which runs at its START — can never self-deadlock on it.)
func TestLiveReplyFenceReservedAtCommit(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	s.out.attach(&fakeDC{}) // a draining channel so the reply completes + releases the fence
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	feedFrames(s, liveSpeechFrame(), 12)
	feedFrames(s, liveSilenceFrame(), 6)
	// The reply commits -> onCommitted reserves the playback fence (held through playout).
	waitSinkCond(t, func() bool { return ag.reserved.Load() >= 1 })
	// ...and it is released once the reply fully resolves.
	waitSinkCond(t, func() bool { return ag.reserved.Load() == 0 })
	s.stopLive()
}

// TestCloseMarksInFlightReply is the round-30 regression: a disconnect (Close) while a
// committed assistant reply is being spoken must durably mark it interrupted — Close
// tears down live mode BEFORE cancelling the session/speak context, so the reply's turn
// id is captured + marked rather than lost when runLiveReply unwinds on cancellation.
func TestCloseMarksInFlightReply(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Body: "hi"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})

	ctx, cancel := context.WithCancel(context.Background())
	s.liveMu.Lock()
	s.replyCtx, s.replyCancel = ctx, cancel
	s.replyTurnID = "trn_speaking"
	s.liveMu.Unlock()

	s.Close()
	waitSinkCond(t, func() bool { return ag.interrupted.Load() == 1 })
	if id, _ := ag.lastTurnID.Load().(string); id != "trn_speaking" {
		t.Fatalf("Close marked turn %q interrupted, want trn_speaking", id)
	}
}

func TestLiveStopTearsDown(t *testing.T) {
	ag := &fakeAgent{reply: "ok"}
	s, sink := newLiveSession(t, ag, asr.Final{}, asr.Final{Text: "hi", Body: "hi"})
	startLiveAndWait(t, s, sink, voice.TurnDetection{Type: voice.ServerVAD, SilenceDurationMs: 96})
	if !s.liveActive() {
		t.Fatal("live should be active after start")
	}
	s.stopLive()
	if s.liveActive() {
		t.Fatal("live should be inactive after stopLive")
	}
	// A second stop is a no-op (idempotent).
	s.stopLive()
}

// waitSinkCond polls cond until true or a timeout, for async goroutine effects.
func waitSinkCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
