package rtc

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/RenanQueiroz/hina-agent/internal/voice"
)

// markInterruptedTimeout bounds the detached durable write that records a barge-in
// truncation, so it can't run forever after the session is gone.
const markInterruptedTimeout = 5 * time.Second

// livePipeline bundles a session's voice turn-detection pipeline with a "keep-warm"
// ASR stream. Each logical turn gets its OWN fresh recognizer (opened on Open,
// finalized + closed on Commit/Cancel), so a turn's audio and its finalize marker
// can never be interleaved with the next turn's on a shared FIFO — eliminating the
// transcript-corruption race a single reused recognizer would have. keepWarm is an
// unfed recognizer held for the whole session purely to PIN the model bundle (via
// the idle-unload lifecycle's refcount), so each per-turn open is warm and never
// cold-loads on the mic read loop.
type livePipeline struct {
	pipe     *voice.Pipeline
	keepWarm asr.Stream

	// commits is the SERIAL reply queue: commitTurn enqueues each committed turn and a
	// single replyWorker finalizes + replies to them strictly IN ORDER. Serializing
	// here is what makes turn ordering correct without fragile "older vs newer"
	// supersession tokens: a slow turn's reply completes (or is barged) before the next
	// turn's finalize starts, and a newer turn that turns out to be a blip (Cancel) is
	// simply never enqueued, so it can't discard the older real turn. done wakes the
	// worker on live exit (so it never blocks forever on an empty queue after a stop).
	commits chan commitJob
	done    chan struct{}
}

// commitJob is one committed live turn handed to the serial reply worker.
type commitJob struct {
	myGen  uint64
	myTurn uint64
	recog  asr.Stream
}

// liveCommitQueue bounds how many committed turns can await the serial worker before
// the inbound loop drops one rather than blocking. Turns are one per utterance and a
// barge-in truncates an in-flight reply, so the queue rarely holds more than one.
const liveCommitQueue = 8

// startLive turns on the live conversation loop with the given turn-detection
// config. It validates the engines, then opens the VAD + ASR streams off the
// control goroutine (cold load) and installs the pipeline, acking SessionUpdated
// when ready. A null/idle update (live=false) exits live mode instead.
func (s *Session) startLive(td voice.TurnDetection) {
	if s.vad == nil || !s.vad.Available() || s.asr == nil || !s.asr.Available() {
		s.emit(events.TypeError, map[string]string{"error": "live voice unavailable (needs local VAD + ASR)"})
		return
	}
	if s.agent == nil {
		s.emit(events.TypeError, map[string]string{"error": "live voice unavailable (no agent configured)"})
		return
	}
	// The live loop durably persists voice turns, which need an owned conversation.
	// A standalone (loopback/tone) call has no conversation id; reject it clearly here
	// rather than failing later when the first turn can't persist.
	if s.conversationID == "" {
		s.emit(events.TypeError, map[string]string{"error": "live voice needs a conversation (reconnect from a conversation)"})
		return
	}
	td = td.Normalize()

	s.liveMu.Lock()
	if s.live || s.liveStarting {
		s.liveMu.Unlock()
		return // already live or starting; a config change should exit then re-enter
	}
	s.liveStarting = true
	myGen := s.liveGen
	s.liveMu.Unlock()

	go s.finishLive(td, myGen)
}

// finishLive opens the VAD + ASR streams (cold-loading if needed) and installs the
// pipeline, off the control goroutine. If a stop/close bumped the generation while
// loading, the freshly built pipeline is discarded rather than installed.
func (s *Session) finishLive(td voice.TurnDetection, myGen uint64) {
	vadStream, err := s.vad.NewStream(s.ctx, td.VADParams())
	if err != nil {
		s.failLiveStart("could not start VAD", err, myGen)
		return
	}
	// Open one unfed recognizer that stays open for the session to PIN the ASR model
	// bundle (its lifecycle ref keeps the models resident), so each per-turn
	// recognizer opens warm and never cold-loads on the mic read loop. This is also
	// where the (one-time) cold load happens — off the control goroutine.
	keepWarm, err := s.asr.NewStream(s.ctx, asr.Options{}, nil)
	if err != nil {
		_ = vadStream.Close()
		s.failLiveStart("could not start recognition", err, myGen)
		return
	}
	lp := &livePipeline{
		pipe:     voice.NewPipeline(td, vadStream, nil, nil),
		keepWarm: keepWarm,
		commits:  make(chan commitJob, liveCommitQueue),
		done:     make(chan struct{}),
	}

	// Phase 1 — verify ownership, then install the pipeline but NOT ready yet. The
	// staleness check is done BEFORE clearing liveStarting: a superseded start (a
	// stop/close, OR a newer start that took over liveStarting after a stop) leaves
	// liveStarting alone — the current start owns it — and discards only its own
	// streams. The current start installs with liveReady=FALSE so the inbound loop
	// can't feed the new pipeline yet, while liveActive() (live=true) already blocks
	// new manual controls.
	s.liveMu.Lock()
	if s.liveGen != myGen || s.isClosed() {
		s.liveMu.Unlock()
		_ = vadStream.Close()
		_ = keepWarm.Close()
		return
	}
	s.liveStarting = false
	s.live = true
	s.liveReady = false
	s.livePipeline = lp
	s.liveMu.Unlock()
	// Start the serial reply worker now (no commits arrive until liveReady), so each
	// committed turn is finalized + answered strictly in order. stopLive closes lp.done.
	go s.replyWorker(lp)

	// Phase 2 — with live=true (manual controls blocked) and liveReady=false (feedLive
	// is still a no-op), clear any PRE-EXISTING manual state so a loopback/tone/
	// manual-TTS playback can't run underneath the live loop and a manual ASR segment
	// can't linger. Only then is mic audio allowed into the pipeline.
	s.out.stop()    // stop loopback/tone/manual-TTS outbound playback (truncating)
	s.cancelSpeak() // invalidate any pending/in-flight manual speak
	s.closeListen() // tear down any manual ASR segment

	// Phase 3 — re-check ownership (a stop/close during cleanup already tore the
	// pipeline down), then go ready and ack.
	s.liveMu.Lock()
	if s.liveGen != myGen || !s.live || s.livePipeline != lp {
		s.liveMu.Unlock()
		return // superseded during cleanup; stopLive/closeListen already cleaned up
	}
	s.liveReady = true
	s.liveMu.Unlock()

	s.emit(events.TypeSessionUpdated, map[string]any{
		"live":           true,
		"turn_detection": lp.pipe.Config(),
	})
}

// failLiveStart reports a live-mode start failure (unless the session is tearing
// down) and clears the starting flag.
func (s *Session) failLiveStart(msg string, err error, myGen uint64) {
	s.liveMu.Lock()
	if s.liveGen == myGen {
		s.liveStarting = false
	}
	closing := s.isClosed()
	s.liveMu.Unlock()
	if closing || isCanceled(err) {
		return
	}
	s.log.Warn("rtc: start live", "msg", msg, "err", err)
	s.emit(events.TypeError, map[string]string{"error": msg})
}

// stopLive exits live mode: it invalidates any in-flight setup/reply, tears down the
// pipeline + recognizer, and acks. A no-op if not live.
func (s *Session) stopLive() {
	s.liveMu.Lock()
	s.liveGen++ // invalidate an in-flight finishLive + any reply
	wasLive := s.live || s.liveStarting
	s.live = false
	s.liveReady = false
	s.liveStarting = false
	s.replyGen++ // invalidate any in-flight reply generation
	s.turnSeq++  // invalidate any in-flight per-turn open + its partials
	// A committed assistant turn being spoken when the user stops live mode / disconnects
	// was NOT fully heard. Capture its id + the played boundary so we durably mark it
	// interrupted below — otherwise the next context (a reload / a restarted session)
	// would feed the full unheard reply back. Cleared here under liveMu so exactly one of
	// {stopLive, bargeIn} captures it (bargeIn re-checks ownership, which now fails).
	interruptedTurn := s.replyTurnID
	interruptedAt := s.out.cursorMs() // played cursor (lock order liveMu -> out.mu)
	s.replyTurnID = ""
	// RESERVE the interrupt fence NOW, UNDER liveMu — before unlocking, cancelling the
	// reply, or acking live=false — so a concurrent text POST / restarted-session turn
	// scheduled in any later window already sees the pending interrupt and waits at
	// awaitInterrupts until the mark commits. Lock order liveMu -> turnMu matches bargeIn.
	var interruptRelease func()
	if interruptedTurn != "" && s.agent != nil {
		interruptRelease = s.agent.BeginInterrupt(s.conversationID)
	}
	lp := s.livePipeline
	s.livePipeline = nil
	turnRecog := s.turnRecog
	s.turnRecog = nil
	workerRecog := s.workerRecog // the recognizer the serial worker is finalizing, if any
	s.workerRecog = nil
	cancel := s.replyCancel
	s.replyCancel = nil
	s.replyCtx = nil
	s.liveMu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.cancelSpeak()
	if interruptRelease != nil {
		// Mark async + release: the fence (reserved above) holds until the durable write
		// commits. Detached ctx (not s.ctx): a stop/close must not lose the durable mark.
		convID, userID, agent := s.conversationID, s.userID, s.agent
		go func() {
			defer interruptRelease()
			ctx, cancel := context.WithTimeout(context.Background(), markInterruptedTimeout)
			defer cancel()
			if err := agent.MarkTurnInterrupted(ctx, convID, userID, interruptedTurn, interruptedAt); err != nil {
				s.log.Warn("rtc: mark live turn interrupted on stop", "err", err)
			}
		}()
	}
	if turnRecog != nil {
		_ = turnRecog.Close() // tear down the active turn's recognizer (abandons it)
	}
	if workerRecog != nil {
		// Abort an in-flight worker finalize that started (or is about to start) across
		// the worker's post-ownership-check gap: closing the recognizer cancels its
		// Finalize (idempotent Close) so no expensive stale ASR work runs after stop and
		// the model-bundle ref isn't pinned until finalizeTimeout.
		_ = workerRecog.Close()
	}
	if lp != nil {
		// The live state was already cleared above UNDER liveMu, so the worker's
		// ownership re-check (also under liveMu) now fails for any job it dequeues —
		// it closes the recognizer rather than finalizing. done wakes the worker if it
		// is idle on an empty queue.
		close(lp.done)          // stop the serial reply worker (it drains + closes queued recognizers)
		_ = lp.pipe.Close()     // closes the VAD stream
		_ = lp.keepWarm.Close() // releases the ASR bundle pin
	}
	if wasLive {
		s.emit(events.TypeSessionUpdated, map[string]any{"live": false})
	}
}

// liveActive reports whether the live conversation loop is on (or starting), so the
// inbound loop routes mic audio to the pipeline rather than the manual listen path.
func (s *Session) liveActive() bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live || s.liveStarting
}

// observeLive feeds an ASR partial into the pipeline (for the semantic + backchannel
// decisions) and surfaces it as ASRPartial. It is dropped unless it belongs to the
// CURRENT live generation AND the current turn with an installed recognizer — so a
// late partial from a turn that already committed/cancelled (or from before a
// stop/restart) can't corrupt the next turn's semantic/barge-in state.
func (s *Session) observeLive(myGen, myTurn uint64, p asr.Partial) {
	s.liveMu.Lock()
	if !s.live || s.liveGen != myGen || s.turnSeq != myTurn || s.turnRecog == nil || s.livePipeline == nil {
		s.liveMu.Unlock()
		return
	}
	s.livePipeline.pipe.Observe(p.Text)
	s.liveMu.Unlock()
	s.emit(events.TypeASRPartial, map[string]any{"text": p.Text, "seg": myTurn})
}

// observePlayback feeds one outbound TTS frame to the live pipeline's echo
// suppressor (called from the outbound pacer for TTS audio). A no-op when not live.
func (s *Session) observePlayback(pcm []float32) {
	s.liveMu.Lock()
	if s.live && s.livePipeline != nil {
		s.livePipeline.pipe.ObservePlayback(pcm)
	}
	s.liveMu.Unlock()
}

// feedLive routes one chunk of 16 kHz mic PCM into the live pipeline and acts on the
// turn-detection events. Called from the single inbound read loop. Heavy work
// (finalize, agent turn, TTS) is dispatched to goroutines so the read loop never
// blocks; lightweight ASR writes use the non-blocking TryWrite.
func (s *Session) feedLive(pcm []float32) {
	// Run the pipeline under liveMu (it is single-threaded — observeLive also takes
	// the lock), but capture the events and act on them AFTER releasing the lock:
	// writing audio to the recognizer can, synchronously in some implementations,
	// surface a partial back through observeLive (which needs liveMu), so handling
	// events under the lock would self-deadlock.
	s.liveMu.Lock()
	if !s.live || !s.liveReady || s.livePipeline == nil {
		s.liveMu.Unlock()
		return
	}
	lp := s.livePipeline
	myGen := s.liveGen
	// "Assistant playing" must cover the WHOLE spoken reply, not just LLM generation:
	// once generation finishes the reply context is cleared but the TTS source keeps
	// draining, and barge-in / backchannel / echo gating must stay active until that
	// playback actually stops. So OR the reply-generating signal with live TTS.
	playing := s.replyCancel != nil || s.out.isTTSPlaying()
	evs, err := lp.pipe.Write(pcm, playing)
	s.liveMu.Unlock()

	// Each event carries the (lp, myGen) ownership token captured above. The handlers
	// re-validate it under liveMu before acting, so a stop/restart that crosses this
	// Write->handle boundary can't let a stale Open/Audio/Commit/Cancel/BargeIn from a
	// superseded pipeline mutate the new session's state.
	for _, ev := range evs {
		s.handleLiveEvent(lp, myGen, ev)
	}
	if err != nil {
		s.log.Debug("rtc: live pipeline write", "err", err)
	}
}

// liveOwnsLocked reports whether (myGen, lp) is still the installed, current live
// pipeline — the gate every event handler checks before acting on a captured event.
// Caller must hold liveMu.
func (s *Session) liveOwnsLocked(myGen uint64, lp *livePipeline) bool {
	return s.live && s.liveGen == myGen && s.livePipeline == lp
}

// emitLive publishes a live turn-lifecycle event ONLY if (myGen, lp) is still the
// current pipeline, ATOMICALLY (the emit happens under liveMu). This prevents a
// stale terminal — e.g. a SpeechStarted/SpeechStopped from an event whose handler
// ran past a stop/restart — from leaving a phantom active-turn state on the client
// (which accepts these unconditionally). Holding liveMu across emit is safe: emit
// takes only the datachannel/bus locks, never liveMu, so there is no re-entrancy.
func (s *Session) emitLive(myGen uint64, lp *livePipeline, typ string, payload any) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveOwnsLocked(myGen, lp) {
		s.emit(typ, payload)
	}
}

// handleLiveEvent acts on one turn-detection event from the (lp, myGen) pipeline.
// Runs WITHOUT liveMu held; the helpers re-acquire it and re-validate ownership.
// feedLive is the only caller and runs on the single inbound read loop, so events are
// processed in order and never concurrently with the next chunk.
func (s *Session) handleLiveEvent(lp *livePipeline, myGen uint64, ev voice.Event) {
	switch ev.Kind {
	case voice.Open:
		s.openTurn(lp, myGen, ev.PCM)
	case voice.Audio:
		s.writeTurn(lp, myGen, ev.PCM)
	case voice.BargeIn:
		s.bargeIn(lp, myGen)
	case voice.Commit:
		s.commitTurn(lp, myGen)
	case voice.Cancel:
		s.cancelTurn(lp, myGen)
	}
}

// openTurn opens a FRESH recognizer for a new logical turn and feeds it the
// pre-roll. The recognizer open happens off the lock (it's warm — keepWarm pins the
// bundle — so it returns promptly without cold-loading). Partials are tagged with
// this turn's id so a stale one from a prior turn is dropped (observeLive). Ownership
// is re-checked before AND after the open, so a stale Open can't install a recognizer
// under a stopped or newer live start.
func (s *Session) openTurn(lp *livePipeline, myGen uint64, preroll []float32) {
	s.liveMu.Lock()
	if !s.liveOwnsLocked(myGen, lp) {
		s.liveMu.Unlock()
		return // stale event (stop/restart crossed the Write->handle boundary)
	}
	if old := s.turnRecog; old != nil {
		// Defensive: the pipeline serializes turns, so a leftover recognizer here is
		// unexpected, but abandon it rather than leak it.
		s.turnRecog = nil
		go old.Close()
	}
	s.turnSeq++
	myTurn := s.turnSeq
	s.liveMu.Unlock()

	recog, err := s.asr.NewStream(s.ctx, asr.Options{}, func(p asr.Partial) { s.observeLive(myGen, myTurn, p) })
	if err != nil {
		if !s.isClosed() && !isCanceled(err) {
			s.emit(events.TypeError, map[string]string{"error": "could not start recognition"})
		}
		return
	}
	s.liveMu.Lock()
	if !s.liveOwnsLocked(myGen, lp) || s.turnSeq != myTurn {
		s.liveMu.Unlock()
		_ = recog.Close() // stopped / superseded while opening — discard
		return
	}
	s.turnRecog = recog
	s.liveMu.Unlock()
	// TryWrite stays OUTSIDE liveMu (a synchronous recognizer can surface a partial
	// back through observeLive, which needs liveMu). The SpeechStarted terminal is
	// emitted ownership-checked under liveMu, so a stop/restart between the install and
	// here can't leak a phantom active-turn event to the client.
	recog.TryWrite(preroll)
	// Tag the turn's lifecycle events with its seg (myTurn) so the client drops a late
	// terminal from an OLDER turn (a slow/timed-out finalize) that would otherwise
	// clobber a newer turn's live UI — pipeline ownership alone (emitLive) doesn't
	// distinguish turns within one live session.
	s.emitLive(myGen, lp, events.TypeSpeechStarted, map[string]any{"sample_rate": 16000, "seg": myTurn})
}

// writeTurn feeds ongoing speech audio to the active turn's recognizer (a no-op if
// the event is stale or no turn is active — e.g. an Audio event raced a stop).
func (s *Session) writeTurn(lp *livePipeline, myGen uint64, pcm []float32) {
	s.liveMu.Lock()
	var recog asr.Stream
	if s.liveOwnsLocked(myGen, lp) {
		recog = s.turnRecog
	}
	s.liveMu.Unlock()
	if recog != nil {
		recog.TryWrite(pcm)
	}
}

// commitTurn detaches the active turn's recognizer and ENQUEUES it on the serial reply
// worker. Each recognizer is this turn's OWN stream (never reused). Serializing the
// finalize+reply here (rather than spawning a goroutine per commit) is what makes turn
// ordering correct: turns are answered strictly in commit order, a slow turn can't be
// overtaken by a newer one, and a newer segment that turns out to be a blip (Cancel) is
// never enqueued — so it can't discard an older real turn. The reply is NOT reserved
// here; the assistant-playing signal is raised only when the worker actually starts the
// agent turn (so the pure finalize window isn't misread as a barge-in).
func (s *Session) commitTurn(lp *livePipeline, myGen uint64) {
	s.liveMu.Lock()
	if !s.liveOwnsLocked(myGen, lp) {
		s.liveMu.Unlock()
		return // stale event
	}
	recog := s.turnRecog
	myTurn := s.turnSeq // this committing turn's seg (stable between its Open and Commit)
	s.turnRecog = nil
	// Enqueue UNDER liveMu, atomically with the ownership check. stopLive sets live=false
	// under the same lock before closing lp.done, so the enqueue can't race the shutdown
	// into orphaning a recognizer: a job accepted here is always drained by the worker
	// (normal processing OR the done-drain), and after a stop the ownership check above
	// fails so nothing is enqueued (stopLive closes the still-attached turnRecog itself).
	enqueued := false
	if recog != nil {
		select {
		case lp.commits <- commitJob{myGen: myGen, myTurn: myTurn, recog: recog}:
			enqueued = true
		default:
		}
	}
	s.liveMu.Unlock()
	if recog != nil && !enqueued {
		// The serial queue is full (many turns backed up): drop this commit rather than
		// block the inbound mic loop. Emit a turn-tagged terminal so the client doesn't
		// hang on SpeechStarted with no pair, count the loss, and close the recognizer
		// so it doesn't leak. (Done OUTSIDE liveMu — emitLive re-takes it.)
		s.log.Warn("rtc: live reply queue full; dropping a committed turn", "seg", myTurn)
		s.metrics.markDroppedTurn()
		s.emitLive(myGen, lp, events.TypeError, map[string]any{"error": "voice reply queue full", "seg": myTurn})
		s.emitLive(myGen, lp, events.TypeSpeechStopped, map[string]any{"reason": "queue_full", "seg": myTurn})
		_ = recog.Close()
	}
}

// replyWorker drains the serial commit queue, finalizing + replying to each turn IN
// ORDER (one at a time). It exits when the pipeline's done channel closes (stopLive),
// draining any queued jobs to close their recognizers so none leak.
func (s *Session) replyWorker(lp *livePipeline) {
	drain := func() {
		// Stop requested: never start another finalize — just close every queued
		// recognizer so none leak and no stale/expensive finalize runs after exit.
		for {
			select {
			case job := <-lp.commits:
				if job.recog != nil {
					_ = job.recog.Close()
				}
			default:
				return
			}
		}
	}
	for {
		select {
		case job := <-lp.commits:
			// A select with both cases ready may pick this commit even though done is
			// already closed. Re-check ownership UNDER liveMu (synchronized with stopLive,
			// which clears live state under the same lock): if a stop/restart has begun,
			// close the recognizer instead of starting a now-stale finalize. The ownership
			// check also catches a restart (a new pipeline took over). If still owned,
			// REGISTER the recognizer as workerRecog in the SAME critical section, so a
			// stopLive landing after this unlock (but before/while finalize runs) can close
			// it and abort the finalize — closing the post-check gap.
			s.liveMu.Lock()
			owned := s.liveOwnsLocked(job.myGen, lp)
			if owned {
				s.workerRecog = job.recog
			}
			s.liveMu.Unlock()
			if !owned {
				if job.recog != nil {
					_ = job.recog.Close()
				}
				drain()
				return
			}
			s.finalizeAndReply(lp, job.myGen, job.myTurn, job.recog)
			s.clearWorkerRecog(job.recog) // deregister ONLY our own recog (see below)
		case <-lp.done:
			drain()
			return
		}
	}
}

// clearWorkerRecog deregisters recog as the worker's in-flight recognizer, but ONLY if
// it is still the registered one. A stop-to-restart race can interpose: the OLD worker
// registers recog1, stopLive closes it, a NEW live session's worker registers recog2,
// then the old worker unwinds here — an unconditional clear would wipe recog2's handle
// so a later stop of the new session couldn't abort it. The identity guard prevents
// clobbering a newer registration.
func (s *Session) clearWorkerRecog(recog asr.Stream) {
	s.liveMu.Lock()
	if s.workerRecog == recog {
		s.workerRecog = nil
	}
	s.liveMu.Unlock()
}

// liveCurrent reports whether (myGen, lp) is still the installed, current live pipeline
// (not stopped/restarted) — the gate the serial worker checks after the (possibly slow)
// finalize. Turn ORDERING is handled by the serial worker, so only stop/restart
// ownership matters here. Caller must NOT hold liveMu.
func (s *Session) liveCurrent(myGen uint64, lp *livePipeline) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.liveOwnsLocked(myGen, lp)
}

// beginReplyOwned atomically verifies (myGen, lp) is still the current live pipeline
// and, if so, reserves the reply — closing the window where a stop/restart lands
// between the post-finalize check and the reservation. Returns ok=false to skip.
func (s *Session) beginReplyOwned(myGen uint64, lp *livePipeline) (uint64, bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if !s.liveOwnsLocked(myGen, lp) {
		return 0, false
	}
	return s.beginReplyLocked(), true
}

// cancelTurn detaches + tears down the active turn's recognizer for a discarded
// segment (a blip or a backchannel during playback): no transcript, no reply. A
// stale event (a stop/restart crossed the Write->handle boundary) is dropped.
func (s *Session) cancelTurn(lp *livePipeline, myGen uint64) {
	s.liveMu.Lock()
	if !s.liveOwnsLocked(myGen, lp) {
		s.liveMu.Unlock()
		return
	}
	recog := s.turnRecog
	myTurn := s.turnSeq
	s.turnRecog = nil
	s.liveMu.Unlock()
	if recog != nil {
		go func() { _, _, _ = finalizeWithTimeout(recog) }()
	}
	s.emitLive(myGen, lp, events.TypeSpeechStopped, map[string]any{"reason": "cancel", "seg": myTurn})
}

// finalizeWithTimeout finalizes recog behind the shared finalize watchdog and
// closes it. On a stall it force-closes in the background (cancelling the inner
// Finalize's context) and returns timedOut=true so the caller abandons the turn —
// a hung recognizer can never pin the goroutine + model-bundle ref indefinitely.
func finalizeWithTimeout(recog asr.Stream) (final asr.Final, err error, timedOut bool) {
	type result struct {
		final asr.Final
		err   error
	}
	ch := make(chan result, 1) // buffered: the inner goroutine never leaks on timeout
	go func() {
		f, e := recog.Finalize()
		ch <- result{f, e}
	}()
	select {
	case r := <-ch:
		_ = recog.Close()
		return r.final, r.err, false
	case <-time.After(finalizeTimeout()):
		go recog.Close()
		return asr.Final{}, nil, true
	}
}

// beginReplyLocked reserves a fresh reply generation + cancel context so a barge-in
// or a superseding turn can invalidate this reply. Caller MUST hold liveMu (e.g.
// beginReplyOwned, which does it atomically with the ownership check).
func (s *Session) beginReplyLocked() uint64 {
	if s.replyCancel != nil {
		s.replyCancel() // supersede any still-running reply
	}
	s.replyGen++
	s.replyTurnID = "" // a fresh reply; the prior turn id no longer applies
	ctx, cancel := context.WithCancel(s.ctx)
	s.replyCancel = cancel
	s.replyCtx = ctx
	return s.replyGen
}

// bargeIn is the server-initiated barge-in: truncate playback to the last
// actually-played boundary, cancel the in-flight reply, and emit the truncation
// events. A stale event (a stop/restart crossed the Write->handle boundary) is
// dropped under the ownership check, so it can't interrupt a newer/stopped session's
// playback.
func (s *Session) bargeIn(lp *livePipeline, myGen uint64) {
	// Hold liveMu across the ENTIRE barge-in — the ownership check, the playback
	// truncation, the reply cancel, and the truncation emits — so it is atomic with
	// respect to a concurrent live stop (stopLive also takes liveMu). Without this, a
	// stop could land after the ownership check and a newer manual/TTS playback could
	// begin before interruptPlayback, letting this barge-in truncate the WRONG playback
	// and emit for a stopped session. The lock order liveMu -> out.mu (interruptPlayback)
	// -> speakMu (cancelSpeak) is consistent with every other path; the pacer's
	// observePlayback (liveMu only, after releasing out.mu) and feedLive (liveMu ->
	// out.mu) can't form a cycle with it.
	s.liveMu.Lock()
	if !s.liveOwnsLocked(myGen, lp) {
		s.liveMu.Unlock()
		return // stale: a stop/restart crossed the Write->handle boundary
	}
	turnID := s.replyTurnID // the committed assistant turn being spoken, if any
	// RESERVE the interrupt fence FIRST — before interruptPlayback (which halts playback
	// and emits the observable PlaybackStopped) or any other observable truncation point —
	// so a concurrent text POST that reacts to the truncation can't slip past
	// awaitInterrupts (with pendingInterrupts still 0) and read the pre-interrupt full
	// reply. Gated on turnID, NOT on `was`: a post-drain TAIL barge-in has the pacer halted
	// (was=false) but the client is still playing the buffer, so it must still be recorded;
	// a GENERATION barge-in has no turnID (RunTurn marks that case via its own metadata).
	var release func()
	if turnID != "" && s.agent != nil {
		release = s.agent.BeginInterrupt(s.conversationID)
	}
	cursorMs, _ := s.out.interruptPlayback() // truncates THIS session's playback (ownership held)
	s.replyGen++                             // invalidate the interrupted reply's generation
	cancel := s.replyCancel
	s.replyCancel = nil
	s.replyCtx = nil
	s.replyTurnID = ""
	// Clear the echo guard so a stale playback-energy level can't linger into the
	// user's new utterance (serialized with the pacer's ObservePlayback + feedLive).
	lp.pipe.ResetEcho()
	if cancel != nil {
		cancel() // cancels the in-flight agent turn (interrupt during generation)
	}
	s.cancelSpeak() // stop any TTS already playing (speakMu)
	s.emit(events.TypeUserInterrupted, map[string]any{"cursor_ms": cursorMs})
	s.emit(events.TypeConversationTruncated, map[string]any{"cursor_ms": cursorMs})
	s.liveMu.Unlock()

	// Do the durable mark SYNCHRONOUSLY, off liveMu, on THIS (feedLive) goroutine: the
	// barge-in's utterance can only commit a NEW turn AFTER this returns (feedLive
	// processes its later frames sequentially), and the fence (reserved above) blocks a
	// concurrent text POST until release. The detached/bounded ctx survives a hang-up.
	if release != nil {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), markInterruptedTimeout)
		defer cancel()
		if err := s.agent.MarkTurnInterrupted(ctx, s.conversationID, s.userID, turnID, cursorMs); err != nil {
			s.log.Warn("rtc: mark live turn interrupted", "err", err)
		}
	}
}

// finalizeAndReply finalizes this turn's own recognizer (then closes it) and, on a
// usable transcript, runs the agent turn + speaks the reply. Runs on its own
// goroutine so the finalize/agent/TTS never block the mic read loop.
func (s *Session) finalizeAndReply(lp *livePipeline, myGen, myTurn uint64, recog asr.Stream) {
	if recog == nil {
		return
	}
	final, err, timedOut := finalizeWithTimeout(recog)
	if timedOut {
		s.log.Warn("rtc: live ASR finalize timed out; turn aborted", "seconds", finalizeTimeout().Seconds())
		// Emit a terminal so the client clears its "speaking" state (it set it on
		// SpeechStarted) rather than hanging — ownership- AND turn-tagged so a stop/
		// restart OR a newer turn doesn't get a stale terminal applied to its UI.
		s.emitLive(myGen, lp, events.TypeError, map[string]any{"error": "recognition timed out", "seg": myTurn})
		s.emitLive(myGen, lp, events.TypeSpeechStopped, map[string]any{"reason": "timeout", "seg": myTurn})
		return
	}
	// Drop a finalize that completed after live mode moved on (stop / restart). Turn
	// ORDERING (a newer turn superseding this one) is handled by the serial worker — by
	// the time this returns, any newer turn is still queued behind it — so only stop/
	// restart ownership matters. The atomic re-check in beginReplyOwned closes the
	// remaining window before the reply.
	if !s.liveCurrent(myGen, lp) {
		return
	}
	if err != nil {
		if !isCanceled(err) {
			s.emitLive(myGen, lp, events.TypeError, map[string]any{"error": "recognition failed", "seg": myTurn})
		}
		return
	}
	// The committed transcript: the wake-stripped Body is what the agent hears; the
	// full Text is surfaced for the timeline. Terminals are ownership- + turn-tagged so
	// a late turn-N terminal can't clobber a newer turn N+1's live UI on the client.
	request := strings.TrimSpace(final.Body)
	s.emitLive(myGen, lp, events.TypeASRFinal, map[string]any{
		"text": final.Text, "wake_detected": final.WakeDetected, "body": final.Body, "seg": myTurn,
	})
	s.emitLive(myGen, lp, events.TypeSpeechStopped, map[string]any{"reason": "commit", "seg": myTurn})
	if request == "" {
		return // nothing to respond to (empty / pure wake word)
	}
	if !lp.pipe.Config().CreatesResponse() {
		return // turn detection configured not to auto-respond
	}
	// Reserve the reply ATOMICALLY with a final ownership check — raising the assistant-
	// playing signal only as the agent turn begins (NOT during the finalize window
	// above, so the next turn's capture isn't misread as a barge-in) and only if a
	// stop/restart hasn't intervened.
	gen, ok := s.beginReplyOwned(myGen, lp)
	if !ok {
		return
	}
	s.runLiveReply(gen, myTurn, request)
}

// runLiveReply runs the agent turn (streaming deltas to the timeline) and speaks the
// full reply. The reply context (replyCtx, captured under liveMu) is cancelled by a
// barge-in / stopLive, which interrupts both the agent generation and the TTS.
func (s *Session) runLiveReply(gen, myTurn uint64, request string) {
	ctx := s.replyContext(gen)
	if ctx == nil {
		s.endReply(gen)
		return // already superseded/cancelled
	}
	// onCommitted runs INSIDE RunTurn, AFTER the durable commit but BEFORE RunTurn releases
	// the per-conversation turn lock. Under that lock we either record the committed turn
	// id + reserve the PLAYBACK FENCE (so a concurrent text POST — which must claim the
	// same lock — can't read this just-committed reply during the post-commit handoff or
	// during playback before its final state is settled), OR, if a barge-in/stop already
	// raced (bumped replyGen), mark the committed FULL turn interrupted at played_ms=0.
	// The fence is reserved here (RunTurn's END), NOT before RunTurn — RunTurn's own
	// awaitInterrupts ran at its START, so it can never wait on this fence (no self-deadlock).
	var playFence func()
	onCommitted := func(committedTurnID string) {
		s.liveMu.Lock()
		current := s.live && s.replyGen == gen
		if current {
			s.replyTurnID = committedTurnID
			s.out.resetForNextReply() // a pre-playback barge-in marks at 0, not the stale cursor
		}
		s.liveMu.Unlock()
		if current {
			playFence = s.agent.BeginInterrupt(s.conversationID)
		} else {
			s.markInterruptedDirect(committedTurnID, 0)
		}
	}
	// AgentTextDelta carries the turn's seg so the client shows the reply against the
	// right turn and drops deltas from a superseded/cancelled reply.
	reply, turnID, err := s.agent.RunTurn(ctx, s.conversationID, s.userID, request, func(delta string) {
		s.emit(events.TypeAgentTextDelta, map[string]any{"delta": delta, "seg": myTurn})
	}, onCommitted)
	if playFence != nil {
		defer playFence() // released once the reply fully resolves (below)
	}
	if err != nil && !isCanceled(err) {
		s.emit(events.TypeError, map[string]any{"error": "agent turn failed", "seg": myTurn})
	}
	// Speak only a clean, non-empty reply with TTS available (an ASR-only deployment
	// streams the text reply without voicing it). The gen check + speak are done
	// ATOMICALLY under liveMu so a stop / barge-in / superseding turn that invalidates
	// this reply between the check and the speak cannot let a stale reply start playing.
	// The playFence (reserved in onCommitted) covers the TTS ErrorEvent/PlaybackStopped/
	// TTSCompleted that speakStream/runSpeak emit.
	if err == nil && strings.TrimSpace(reply) != "" && s.tts != nil && s.tts.Available() {
		var done <-chan struct{}
		var res *speakResult
		var epoch uint32
		var speakErr error
		s.liveMu.Lock()
		if s.live && s.replyGen == gen && ctx.Err() == nil {
			done, res, epoch, speakErr = s.speakStream(reply, tts.Options{})
		}
		s.liveMu.Unlock()
		switch {
		case speakErr != nil:
			// TTS REJECTED before any playback (over-long text / too many segments): the
			// committed turn was never spoken — mark it at played_ms=0. The reply fence
			// (held above) already covered the ErrorEvent speakStream emitted.
			s.markInterruptedDirect(turnID, 0)
		case done != nil:
			// Block the serial worker until this reply's audio FINISHES playing — first the
			// server-side send (done: pacer drained + halted), THEN the CLIENT playout
			// (waitPlayedOut). Only then may the worker dequeue the next turn, so the next
			// reply's PlaybackStarted can't clip this one outside the barge-in path. A
			// barge-in / stop cancels ctx, returning from both waits at once.
			<-done
			s.out.waitPlayedOut(ctx, epoch)
			// A synthesis cap or dropped frames cut the spoken reply short (a barge-in /
			// stop, res.cancelled, marks the turn itself). The reply fence covered the
			// PlaybackStopped/TTSCompleted runSpeak emitted; record the heard boundary now.
			if res != nil && res.truncated && !res.cancelled {
				s.markInterruptedDirect(turnID, s.out.cursorMs())
			}
		}
	}
	s.endReply(gen)
}

// markInterruptedDirect durably marks an assistant turn interrupted (the reply-level
// fence is already held by runLiveReply, so this is just the write). Detached, bounded
// context survives session teardown.
func (s *Session) markInterruptedDirect(turnID string, playedMs int64) {
	if turnID == "" || s.agent == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), markInterruptedTimeout)
	defer cancel()
	if err := s.agent.MarkTurnInterrupted(ctx, s.conversationID, s.userID, turnID, playedMs); err != nil {
		s.log.Warn("rtc: mark live reply interrupted", "err", err)
	}
}

// replyContext returns the reply's context if gen is still the current reply, else
// nil (it was superseded/cancelled).
func (s *Session) replyContext(gen uint64) context.Context {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.replyGen != gen || s.replyCancel == nil {
		return nil
	}
	return s.replyCtx
}

// endReply clears the in-flight reply state if gen is still current. It also clears
// replyTurnID so a barge-in BETWEEN replies can't mark an already-completed turn
// interrupted (a barge-in that races the interrupt path itself already cleared it).
func (s *Session) endReply(gen uint64) {
	s.liveMu.Lock()
	if s.replyGen == gen && s.replyCancel != nil {
		s.replyCancel()
		s.replyCancel = nil
		s.replyCtx = nil
		s.replyTurnID = ""
	}
	s.liveMu.Unlock()
}

// isCanceled reports whether err is a context cancellation (a teardown/barge-in
// signal, not a real failure).
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
