package rtc

import (
	"context"
	"errors"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/events"
)

// maxListenDuration bounds a single ASR listening segment: a client that never
// sends ListenStopped (or a stuck UI) has its segment auto-finalized after this,
// so it can't pin inference indefinitely. Generous for one spoken turn (Phase 6
// VAD ends turns far sooner). A var so tests can shorten it.
var maxListenDuration = 5 * time.Minute

// beginListen SYNCHRONOUSLY reserves an ASR listening segment and captures the
// listen generation, returning (gen, true) when the caller should proceed to
// finishListen. It must run on the control-handler goroutine (not a spawned one)
// so the reservation + generation capture are ordered before any subsequent
// ListenStopped on the same datachannel — otherwise a stop could bump the
// generation before the start captured it and the start would install a stream
// after the segment was ended. Returns false (a no-op) if ASR is unavailable or a
// segment is already active/starting.
func (s *Session) beginListen() (uint64, bool) {
	if s.asr == nil || !s.asr.Available() {
		s.emit(events.TypeError, map[string]string{"error": "local ASR unavailable"})
		return 0, false
	}
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	if s.asrStream != nil || s.asrStarting {
		return 0, false // already listening or starting
	}
	s.asrStarting = true
	return s.listenGen, true
}

// finishListen opens the recognition stream (cold-loading the model if needed)
// off the control goroutine, so a slow load never stalls the control channel.
// Interim transcripts are emitted as ASRPartial; the inbound 16 kHz mic frames
// are routed to the installed stream. Turn boundaries are client-driven here
// (ListenStarted/Stopped); VAD lands in Phase 6. myGen is the generation captured
// by beginListen: if a stop/close bumped it (or the session closed) while loading,
// the freshly-built stream is discarded rather than installed — so it can't leak
// past teardown or keep listening after the segment was ended.
func (s *Session) finishListen(language string, myGen uint64) {
	// Tag every event for this segment with its id (myGen, unique per installed
	// segment since each stop bumps the generation). The client tracks the current
	// segment and drops events from an older one — so a stale ASRPartial/ASRFinal
	// from a segment still finalizing can't clear or corrupt the UI of the next
	// segment that already started.
	stream, err := s.asr.NewStream(s.ctx, asr.Options{Language: language}, func(p asr.Partial) {
		s.emit(events.TypeASRPartial, map[string]any{"text": p.Text, "seg": myGen})
	})

	s.asrMu.Lock()
	s.asrStarting = false
	if err != nil || s.isClosed() || s.listenGen != myGen {
		closed := s.isClosed()
		s.asrMu.Unlock()
		if stream != nil {
			_ = stream.Close()
		}
		// Report a genuine start failure, but stay quiet on a teardown-driven
		// cancellation (session closing) so no stale error publishes after the call.
		if err != nil && !closed && !errors.Is(err, context.Canceled) {
			s.log.Warn("rtc: start ASR stream", "err", err)
			s.emit(events.TypeError, map[string]string{"error": "could not start recognition"})
		}
		return
	}
	s.asrStream = stream
	s.activeSeg = myGen
	s.asrDropped = 0 // fresh segment: reset the dropped-frame counter
	// Arm the max-duration backstop: if the client never stops this segment, it is
	// auto-finalized so it can't pin inference forever.
	s.listenTimer = time.AfterFunc(maxListenDuration, func() { s.timedStop(myGen) })
	s.asrMu.Unlock()
	s.emit(events.TypeListenStarted, map[string]any{
		"sample_rate": 16000,
		"language":    orAuto(language),
		"seg":         myGen,
	})
}

// timedStop auto-finalizes a segment that hit maxListenDuration — but ONLY that
// exact segment. The generation check, the stream capture, and the generation
// bump all happen under a single asrMu hold, so a stale timer that fires just as
// the client stops the old segment (or a newer one starts) cannot finalize the
// wrong stream or discard a new in-flight start.
func (s *Session) timedStop(gen uint64) {
	s.asrMu.Lock()
	if s.asrStream == nil || s.activeSeg != gen {
		s.asrMu.Unlock()
		return // already stopped, or a newer segment took over — no-op
	}
	s.listenGen++
	s.stopListenTimerLocked()
	stream := s.asrStream
	seg := s.activeSeg
	dropped := s.asrDropped
	s.asrStream = nil
	s.asrMu.Unlock()
	s.log.Info("rtc: ASR segment reached the max duration; auto-finalizing", "seconds", maxListenDuration.Seconds())
	go s.finalizeSegment(stream, seg, dropped, true) // byTimeout: the segment was cut short
}

// stopListenTimer stops + clears the active segment's max-duration timer. Caller
// must hold asrMu.
func (s *Session) stopListenTimerLocked() {
	if s.listenTimer != nil {
		s.listenTimer.Stop()
		s.listenTimer = nil
	}
}

// startListen reserves + opens a listening segment inline (begin then finish on
// the calling goroutine). The control handler instead calls beginListen
// synchronously and finishListen on a goroutine; this convenience keeps the
// reserve→finish ordering for direct callers (tests).
func (s *Session) startListen(language string) {
	if gen, ok := s.beginListen(); ok {
		s.finishListen(language, gen)
	}
}

// stopListen commits the active listening segment: it finalizes the stream
// (flushing the trailing audio), emits the ASRFinal with the wake-detection
// result + address-stripped body, and releases the model bundle. Runs the
// blocking Finalize on its own goroutine. A no-op if not listening.
func (s *Session) stopListen() {
	s.asrMu.Lock()
	// Bump the generation so a start still cold-loading is invalidated (it will
	// discard its stream instead of installing it and routing mic audio).
	s.listenGen++
	s.stopListenTimerLocked()
	stream := s.asrStream
	seg := s.activeSeg // tag this segment's terminal event with its id
	dropped := s.asrDropped
	s.asrStream = nil
	s.asrMu.Unlock()
	if stream == nil {
		return // not listening, or an in-flight start was just invalidated above
	}
	go s.finalizeSegment(stream, seg, dropped, false)
}

// finalizeTimeout bounds how long a stopped segment's Finalize may run before it
// is force-aborted. Finalizing flushes only the trailing audio, so it normally
// completes in well under a second; the bound guards against a stalled/hung
// recognizer orphaning the stream (and its model-bundle ref) on a normal stop,
// when the session context is still live and nothing else would cancel it. A var
// so tests can shorten it.
var finalizeTimeout = 10 * time.Second

// finalizeSegment finalizes a captured segment stream off-goroutine and publishes
// its terminal event (ASRFinal on success, ErrorEvent + ListenStopped on a decode
// failure or a timeout), tagged with the segment id. Both stopListen and
// timedStop capture their stream + bump the generation under asrMu first, then
// hand the captured stream here — so this never races a newer segment's state.
func (s *Session) finalizeSegment(stream asr.Stream, seg uint64, dropped int64, byTimeout bool) {
	type result struct {
		final asr.Final
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		final, err := stream.Finalize()
		ch <- result{final, err}
	}()

	var r result
	select {
	case r = <-ch:
		_ = stream.Close()
	case <-time.After(finalizeTimeout):
		// The recognizer stalled. Emit the terminal and return WITHOUT waiting on the
		// stream's shutdown — terminal delivery must not depend on a cooperative
		// Close/Finalize. Close is triggered in the background (for the concrete
		// stream it cancels the context, Terminates the ORT run, and releases the
		// bundle; the buffered ch means the inner Finalize goroutine never leaks once
		// it returns). A pathological non-cooperative stream can't pin this goroutine
		// or leave the client listening.
		go stream.Close()
		s.log.Warn("rtc: ASR finalize timed out; segment aborted", "seconds", finalizeTimeout.Seconds())
		if !s.isClosed() {
			s.emit(events.TypeError, map[string]string{"error": "recognition timed out"})
			s.emit(events.TypeListenStopped, map[string]any{"reason": "timeout", "seg": seg})
		}
		return
	}

	// Session teardown (ctx cancelled) while finalizing: the segment is abandoned —
	// emit nothing, so no stale terminal event publishes after the call is gone.
	if errors.Is(r.err, context.Canceled) || s.isClosed() {
		return
	}
	if r.err != nil {
		// A terminal decode failure: report the error AND a terminal ListenStopped
		// so the client clears its listening state (ASRFinal, the other terminal,
		// only fires on success).
		s.emit(events.TypeError, map[string]string{"error": "recognition failed"})
		s.emit(events.TypeListenStopped, map[string]any{"reason": "error", "seg": seg})
		return
	}
	// The transcript is incomplete (truncated) for ANY of: dropped mic frames under
	// backpressure, the recognizer hitting its own segment cap, or the server
	// auto-finalizing the segment at maxListenDuration — all server-side cuts the
	// user should be warned about, not just the backpressure case.
	reason := ""
	switch {
	case dropped > 0:
		reason = "dropped"
	case r.final.Truncated:
		reason = "capped"
	case byTimeout:
		reason = "max_duration"
	}
	s.emit(events.TypeASRFinal, map[string]any{
		"text":              r.final.Text,
		"wake_detected":     r.final.WakeDetected,
		"body":              r.final.Body,
		"seg":               seg,
		"truncated":         reason != "",
		"truncation_reason": reason,
		"dropped_frames":    dropped,
	})
}

// closeListen tears down any active ASR stream during session Close (releasing
// the shared model bundle). It does not finalize — the segment is abandoned.
func (s *Session) closeListen() {
	s.asrMu.Lock()
	s.listenGen++ // invalidate an in-flight start so it discards rather than installs
	s.stopListenTimerLocked()
	stream := s.asrStream
	s.asrStream = nil
	s.asrMu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

// feedASR routes one chunk of 16 kHz mono mic PCM to the active recognition
// stream, if any. Called from the single inbound readLoop goroutine. The stream
// copies the slice, so the caller may reuse it.
//
// The enqueue is done while holding asrMu (not just the capture): stopListen /
// timedStop / closeListen also take asrMu before removing+finalizing the stream,
// so a frame is either fully enqueued to the stream BEFORE a stop can take it, or
// the stop wins and this sees a nil stream and drops the frame. Without this, a
// frame captured just before a stop could be ordered AFTER the finalize marker on
// the stream's channel — dropping the segment's tail (and possibly processing a
// late frame past the reset).
//
// It uses the NON-BLOCKING TryWrite so it never stalls while holding asrMu: a slow
// recognizer drops frames (acceptable for real-time audio, like the audio-out
// path) instead of wedging the inbound read loop or — worse — blocking
// closeListen/stopListen and, via the lock, session teardown.
func (s *Session) feedASR(pcm []float32) {
	if len(pcm) == 0 {
		return
	}
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	if s.asrStream != nil && !s.asrStream.TryWrite(pcm) {
		// The recognizer is behind real time and the buffer is full: the frame is
		// dropped. Count it so the segment's ASRFinal can flag the transcript as
		// incomplete rather than presenting a silently-lossy result as clean.
		s.asrDropped++
	}
}

// orAuto mirrors the engine's default-language label for the ListenStarted ack.
func orAuto(s string) string {
	if s == "" {
		return "auto"
	}
	return s
}
