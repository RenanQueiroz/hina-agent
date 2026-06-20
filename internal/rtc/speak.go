package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
)

// maxSpeakBytes caps a single SpeakText request. The ttsSource buffers the whole
// utterance (synthesis outruns realtime playback), so the text length is the
// memory bound; 4 KB is ~a few paragraphs, far beyond any voice reply.
const maxSpeakBytes = 4000

// cancelSpeak stops any in-flight spoken reply AND invalidates any pending speak
// that is still synthesizing (by bumping the speak generation), so a barge-in /
// mode switch / close can't be overtaken by a slow request that started before
// it. Idempotent.
func (s *Session) cancelSpeak() {
	s.speakMu.Lock()
	s.speakGen++
	if s.speakCancel != nil {
		s.speakCancel()
		s.speakCancel = nil
	}
	s.speakMu.Unlock()
}

// speak synthesizes text with the local TTS engine and streams it to the browser
// over the outbound PCM path, resampling the engine's 44.1 kHz output down to the
// 24 kHz datachannel rate. It supersedes any in-flight spoken reply (the new
// audio source replaces the old). A synchronous rejection (unavailable engine,
// empty/over-long text, unknown voice, too many sentences) is both returned (so an
// HTTP caller can map it to a status) and emitted as an error event (so the
// datachannel/SSE client sees it); the active reply is left untouched on rejection.
func (s *Session) speak(text string, opts tts.Options) error {
	if s.tts == nil || !s.tts.Available() {
		return s.rejectSpeak(tts.ErrUnavailable)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return s.rejectSpeak(errors.New("tts: empty text"))
	}
	if len(text) > maxSpeakBytes {
		return s.rejectSpeak(fmt.Errorf("tts: text too long (%d bytes, max %d)", len(text), maxSpeakBytes))
	}

	// Record this request's ARRIVAL generation. The winner among concurrent speaks
	// is the latest to ARRIVE, not the first to finish synthesizing — so a slow
	// older request can't override a newer one that already started.
	s.speakMu.Lock()
	s.speakGen++
	myGen := s.speakGen
	s.speakMu.Unlock()

	// Validate + start synthesis on a fresh context BEFORE disturbing any current
	// reply: a synchronous rejection (unknown voice, too many sentences) leaves the
	// in-flight reply playing untouched — a bad new request can't wedge a good one.
	ctx, cancel := context.WithCancel(s.ctx)
	stream, err := s.tts.Synthesize(ctx, text, opts)
	if err != nil {
		cancel()
		return s.rejectSpeak(err)
	}

	// Commit ATOMICALLY under speakMu, but only if we're STILL the current request:
	// a newer speak, or a cancel/mode/interrupt/close, may have bumped speakGen
	// while we were synthesizing — then abandon (the producer unwinds on ctx
	// cancel). When current, supersede the previous reply and bump the playback
	// epoch via out.start without releasing the lock. (Lock order is speakMu ->
	// out.mu, matching setMode/interrupt; nothing takes them the other way.)
	s.speakMu.Lock()
	if s.speakGen != myGen {
		s.speakMu.Unlock()
		cancel()
		return nil // superseded before we could start — not an error
	}
	if s.speakCancel != nil {
		s.speakCancel()
	}
	s.speakCancel = cancel
	src := newTTSSource()
	epoch := s.out.start(src)
	s.speakMu.Unlock()

	s.emit(events.TypeTTSStarted, map[string]any{"text": text, "epoch": epoch})
	s.emitPlaybackStarted(ModeTTS, epoch)

	go s.runSpeak(ctx, cancel, src, epoch, stream)
	return nil
}

// rejectSpeak emits an error event (datachannel/SSE observability) and returns the
// error (for an HTTP caller to map to a status).
func (s *Session) rejectSpeak(err error) error {
	s.emit(events.TypeError, map[string]string{"error": err.Error()})
	return err
}

// runSpeak drives one spoken reply: consume the synthesis stream, resample, feed
// the source, then wait for the pacer to drain it (or for a barge-in/supersede/
// close) before stopping playback and emitting TTSCompleted.
func (s *Session) runSpeak(ctx context.Context, cancel context.CancelFunc, src *ttsSource, epoch uint32, stream *tts.Stream) {
	defer cancel()

	rs, err := audio.NewResampler(stream.SampleRate(), audio.OutputSampleRate)
	if err != nil {
		s.emit(events.TypeError, map[string]string{"error": "tts resampler: " + err.Error()})
		s.out.halt("tts-error", true, epoch) // an error stop is truncating (drop buffered audio)
		return
	}

	// Consume segments via a select over ctx so cancellation is honored even while
	// blocked waiting for the next segment (cold load / a stalled producer) — not
	// only between segments. EVERY cancellation path below halts this epoch's pacer
	// (epoch-guarded), so a rejected request, unknown mode change, or barge-in can
	// never leave the pacer paving silence in tts mode.
consume:
	for {
		select {
		case <-ctx.Done():
			break consume
		case seg, ok := <-stream.Segments:
			if !ok {
				break consume // stream closed (normal completion or producer error)
			}
			if ctx.Err() != nil {
				break consume
			}
			out, perr := rs.Process(seg.PCM)
			if perr != nil {
				s.log.Debug("rtc: tts resample", "err", perr)
				continue
			}
			src.Feed(out)
		}
	}
	if ctx.Err() != nil {
		// A cancellation (barge-in / supersede / unknown mode change / close) is a
		// TRUNCATING stop: the reply did not reach its natural end, so the browser
		// drops buffered audio + closes its gate. truncated=false is reserved for the
		// fully-drained completion below.
		s.out.halt("tts-cancelled", true, epoch)
		return
	}
	// A synthesis error (not a cancellation) ends the reply with an error event.
	if serr := stream.Err(); serr != nil {
		s.emit(events.TypeError, map[string]string{"error": "tts: " + serr.Error()})
		s.out.halt("tts-error", true, epoch)
		return
	}

	// Flush the streaming resampler's filter tail (a finite stream just ended) so
	// the very end of the reply isn't clipped, then mark the utterance complete.
	if tail, ferr := rs.Flush(); ferr != nil {
		s.log.Debug("rtc: tts resampler flush", "err", ferr)
	} else if len(tail) > 0 {
		src.Feed(tail)
	}

	src.End()
	select {
	case <-src.Done():
		// All audio played out: stop this epoch's pacer and report completion. This
		// is the ONE non-truncating stop — the browser keeps its gate open so the
		// last in-flight frame isn't dropped. halt is epoch-guarded, so a reply
		// superseded between End and here is a no-op.
		if ok, dropped := s.out.halt("tts-complete", false, epoch); ok {
			// The audio drains normally (truncated=false on PlaybackStopped), but the
			// SPOKEN reply is incomplete if synthesis was capped OR frames were dropped
			// (backpressure / send error) on a slow channel — report that on
			// TTSCompleted so the client knows the text wasn't fully delivered. The drop
			// count is captured atomically inside halt for THIS epoch.
			truncated := stream.Truncated() || dropped > 0
			s.emit(events.TypeTTSCompleted, map[string]any{"epoch": epoch, "truncated": truncated})
		}
	case <-ctx.Done():
		// Cancelled DURING the drain wait (barge-in / supersede / unknown mode /
		// close): a premature stop, so it's TRUNCATING — the browser drops buffered
		// audio + closes its gate. Epoch-guarded, so a newer playback that already
		// replaced us is untouched.
		s.out.halt("tts-cancelled", true, epoch)
		return
	}
}
