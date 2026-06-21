package rtc

import (
	"context"
	"sync"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/pion/webrtc/v4"
)

const (
	// frameInterval is the outbound pacing cadence (one 20 ms PCM frame).
	frameInterval = audio.FrameMillis * time.Millisecond
	// maxBufferedFrames bounds the datachannel send queue to the B5 live-latency
	// budget (~120 ms): the audio channel is unreliable, so rather than let a
	// slow/stalled link grow latency without bound we drop frames once this many
	// 20 ms frames are already queued. The browser ring buffer is sized to match.
	maxBufferedFrames = 6
	// metaThrottle limits how often AudioOutputFrame meta events are emitted
	// (the PCM still flows every 20 ms; only the observability meta is throttled).
	metaThrottle = 100 * time.Millisecond
)

// maxBufferedBytes is the ~120 ms backpressure threshold in bytes (one frame is
// the header plus OutputFrameSamples s16 samples).
var maxBufferedBytes = uint64(maxBufferedFrames * audio.EncodedAudioFrameLen(audio.OutputFrameSamples))

// outboundDC is the subset of *webrtc.DataChannel the pacer needs. An interface so
// the backpressure/drain path is unit-testable with a fake datachannel.
type outboundDC interface {
	BufferedAmount() uint64
	ReadyState() webrtc.DataChannelState
	Send([]byte) error
}

// outbound is the server→browser PCM pipeline: a paced writer that pulls frames
// from the active Source, frames them with a binary header, and sends them on
// the unreliable audio datachannel. It also holds the playback cursor reported
// back by the browser.
type outbound struct {
	s *Session

	mu      sync.Mutex
	dc      outboundDC
	source  Source
	running bool
	cancel  context.CancelFunc
	epoch   uint32 // bumped on every (re)start; stamped into each frame
	sent    int64  // samples actually sent this epoch (cursor upper bound)
	dropped int64  // frames dropped for backpressure this epoch (audio not delivered)

	// cursor + RTT reported by the browser's AudioWorklet.
	playedSamples int64
	playedMs      int64
	lastRTTMicros int64
}

// samplesPerMs is the 24 kHz output rate in samples per millisecond; the server
// derives played_ms from played_samples rather than trusting a client-sent ms.
const samplesPerMs = audio.OutputSampleRate / 1000

func newOutbound(s *Session) *outbound { return &outbound{s: s} }

// attach records the audio datachannel once the browser opens it.
func (o *outbound) attach(dc outboundDC) {
	o.mu.Lock()
	o.dc = dc
	o.mu.Unlock()
}

// start sets src as the active source, bumps the playback epoch, and launches
// the pacer if it isn't already running. It returns the new epoch so the
// PlaybackStarted event can carry it; the client uses the epoch to reject frames
// from a superseded playback. Switching sources while running just swaps src
// (the pacer keeps its cadence and frame sequence) but still advances the epoch.
func (o *outbound) start(src Source) uint32 {
	o.mu.Lock()
	o.source = src
	o.epoch++
	epoch := o.epoch
	// A new playback starts with a fresh cursor and sent-sample count so an
	// immediate barge-in reports "nothing heard yet" (0), never the previous
	// playback's stale cursor.
	o.playedSamples = 0
	o.playedMs = 0
	o.sent = 0
	o.dropped = 0
	o.s.metrics.setCursor(0, 0)
	if o.running {
		o.mu.Unlock()
		return epoch
	}
	ctx, cancel := context.WithCancel(o.s.ctx)
	o.cancel = cancel
	o.running = true
	o.mu.Unlock()
	go o.pace(ctx)
	return epoch
}

// stop halts the pacer and clears the source. It is a TRUNCATING stop (an
// explicit idle/mode-switch/teardown, not a natural end), so the browser drops
// any still-buffered audio and closes its playback gate. Safe to call when
// already stopped. Only a fully-drained TTS completion uses truncated=false.
func (o *outbound) stop() { o.halt("stopped", true, 0) }

// interrupt is the barge-in path: record the client's final cursor for the
// active epoch (sanitized), then stop immediately and report that cursor as the
// truncation point. A stale or duplicate interrupt for a superseded playback
// (epoch already advanced) is ignored — both recordCursor and halt verify the
// epoch — so it can never stop the newer playback that is now active. epoch 0 is
// rejected: a real playback epoch is always >= 1, so 0 means a malformed/missing
// payload and must NOT fall through to halt's unguarded (expectEpoch==0) stop.
func (o *outbound) interrupt(epoch uint32, playedSamples, nowMicros int64) {
	if epoch == 0 {
		return
	}
	o.recordCursor(epoch, playedSamples, 0, nowMicros)
	if ok, _ := o.halt("interrupt", true, epoch); ok {
		o.s.metrics.markInterrupt()
	}
}

// interruptPlayback is the server-initiated barge-in (live-voice VAD detected the
// user talking over the assistant): truncate the active playback at the last
// actually-played cursor and report that boundary. The live caller (bargeIn) holds
// liveMu across this call AND the ownership check, so the "current" playback it stops
// is guaranteed to be this session's reply — a concurrent live stop (which also takes
// liveMu) can't interpose a newer manual/TTS playback. Returns the played-cursor
// milliseconds and whether a playback was actually running.
func (o *outbound) interruptPlayback() (cursorMs int64, wasPlaying bool) {
	o.mu.Lock()
	playedMs := o.playedMs
	o.mu.Unlock()
	was, _ := o.halt("barge-in", true, 0) // truncating stop of the current playback
	if was {
		o.s.metrics.markInterrupt()
	}
	return playedMs, was
}

// halt stops the pacer and clears the source, emitting PlaybackStopped with the
// last cursor. When expectEpoch is non-zero it acts only if that epoch is still
// the active one. It returns whether it actually stopped a running playback, plus
// the number of frames dropped during that epoch (captured under the lock so a
// concurrent restart can't reset it between halt and the caller's read).
func (o *outbound) halt(reason string, truncated bool, expectEpoch uint32) (wasRunning bool, dropped int64) {
	o.mu.Lock()
	if expectEpoch != 0 && expectEpoch != o.epoch {
		o.mu.Unlock()
		return false, 0 // stale: the active playback has since advanced
	}
	wasRunning = o.running
	cancel := o.cancel
	o.cancel = nil
	o.running = false
	o.source = nil
	playedMs := o.playedMs
	playedSamples := o.playedSamples
	dropped = o.dropped
	o.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if !wasRunning && !truncated {
		return false, dropped // nothing was playing and this isn't an explicit barge-in
	}
	o.s.emit(events.TypePlaybackStopped, map[string]any{
		"reason":         reason,
		"truncated":      truncated,
		"cursor_ms":      playedMs,
		"played_samples": playedSamples,
	})
	return wasRunning, dropped
}

// cursorMs returns the last reported played-cursor (audible-clamped) for the active
// epoch — the heard boundary the live worker records when a spoken reply was truncated
// (synthesis cap / dropped frames) rather than fully delivered.
func (o *outbound) cursorMs() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.playedMs
}

// resetForNextReply zeroes the playback cursor and advances the epoch WITHOUT starting
// a pacer or emitting PlaybackStarted. The live worker calls it as a reply's committed
// turn id is recorded (after generation, before TTS), so a barge-in in that pre-playback
// window marks the turn at a cursor of 0 (nothing of it heard yet) rather than the
// PREVIOUS reply's stale cursor, and the epoch bump makes a late cursor report for the
// previous epoch be rejected. Safe because the serial reply worker guarantees no
// playback is active at this point.
func (o *outbound) resetForNextReply() {
	o.mu.Lock()
	o.epoch++
	o.playedSamples = 0
	o.playedMs = 0
	o.sent = 0
	o.dropped = 0
	o.mu.Unlock()
	o.s.metrics.setCursor(0, 0)
}

// playoutMargin covers the client's playback ring (RING_CAPACITY, <=120 ms @ 24 kHz)
// plus network slack, added to the wait so the browser drains the last buffered tail.
// maxPlayoutWait bounds the wait so a silent / dead / dropping client can't pin the
// live worker forever.
const (
	playoutMargin  = 160 * time.Millisecond
	maxPlayoutWait = 5 * time.Second
)

// waitPlayedOut blocks until the browser has PLAYED OUT this epoch's sent audio (its
// reported cursor reaches the sent sample count) or a bounded deadline elapses. The
// live reply worker calls this after the server-side drain so it does NOT start the
// NEXT reply — whose PlaybackStarted resets the client worklet, dropping any still-
// buffered tail — before the current reply finished playing on the CLIENT. The pacer
// is already halted (o.sent frozen) when this runs. ctx cancellation (barge-in / stop /
// close) returns immediately; a superseded epoch (a newer playback already started)
// returns too. A clean link releases early via the cursor; a lossy/silent one waits at
// most the bounded deadline.
func (o *outbound) waitPlayedOut(ctx context.Context, epoch uint32) {
	o.mu.Lock()
	sent, played, cur := o.sent, o.playedSamples, o.epoch
	o.mu.Unlock()
	if cur != epoch || sent <= 0 || played >= sent {
		return
	}
	deadline := time.Duration((sent-played)/samplesPerMs)*time.Millisecond + playoutMargin
	if deadline > maxPlayoutWait {
		deadline = maxPlayoutWait
	}
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-tick.C:
			o.mu.Lock()
			done := o.epoch != epoch || o.playedSamples >= o.sent
			o.mu.Unlock()
			if done {
				return
			}
		}
	}
}

// feedLoopback forwards decoded mic PCM (24 kHz) to the active source IFF it is
// the loopback source. A TTS or tone source must never receive mic audio — doing
// so would mix live mic into synthesized/generated playback and grow the buffer
// with real-time audio — so anything other than the loopback source drops it.
func (o *outbound) feedLoopback(samples []float32) {
	o.mu.Lock()
	src := o.source
	o.mu.Unlock()
	if lb, ok := src.(*loopbackSource); ok {
		lb.Feed(samples)
	}
}

// recordCursor stores the browser's playback cursor report and derives the
// app-level round-trip latency from the echoed send timestamp. The report is
// sanitized so a buggy or hostile client can't corrupt the cursor that drives
// the barge-in truncation point:
//   - a report for a stale epoch (superseded playback) is ignored;
//   - played_samples below zero is rejected; above what the server has actually
//     sent this epoch is clamped to that ceiling;
//   - a cursor that would regress is ignored (playback only moves forward);
//   - played_ms is DERIVED from played_samples, never trusted from the client.
func (o *outbound) recordCursor(epoch uint32, playedSamples, ackSendMicros, nowMicros int64) {
	o.mu.Lock()
	if epoch != o.epoch || playedSamples < 0 {
		o.mu.Unlock()
		return
	}
	if playedSamples > o.sent {
		playedSamples = o.sent // can't have played more than was sent
	}
	if playedSamples < o.playedSamples {
		o.mu.Unlock()
		return // regressing cursor: ignore
	}
	o.playedSamples = playedSamples
	o.playedMs = playedSamples / samplesPerMs
	playedMs := o.playedMs
	rtt := o.lastRTTMicros
	if ackSendMicros > 0 && nowMicros >= ackSendMicros {
		rtt = nowMicros - ackSendMicros
		o.lastRTTMicros = rtt
	}
	o.mu.Unlock()
	o.s.metrics.setCursor(playedMs, rtt)
}

// recordDrop counts one undelivered frame against epoch, but only if epoch is
// still the active one — so a source switch between the pacer capturing epoch and
// here can't charge the new epoch for an old frame (and halt captures the count
// under the same lock, so it can't be reset out from under a completion read).
func (o *outbound) recordDrop(epoch uint32) {
	o.mu.Lock()
	if epoch == o.epoch {
		o.dropped++
	}
	o.mu.Unlock()
}

// mode reports the active source name, or ModeIdle when stopped.
func (o *outbound) mode() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.running || o.source == nil {
		return ModeIdle
	}
	return o.source.Name()
}

// isTTSPlaying reports whether the assistant is currently speaking (a synthesized
// reply is the active outbound source). The live loop uses this — not just whether
// a reply is still generating — as the "assistant playing" signal, so barge-in,
// backchannel suppression, and echo gating stay active for the WHOLE spoken reply,
// including after generation finished and the TTS source is still draining.
func (o *outbound) isTTSPlaying() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.running || o.source == nil {
		return false
	}
	_, ok := o.source.(*ttsSource)
	return ok
}

// pace is the pacer goroutine: every frameInterval it pulls one frame from the
// active source, frames it, and sends it on the audio channel (subject to
// backpressure). It exits when the context is cancelled (stop/interrupt/close).
func (o *outbound) pace(ctx context.Context) {
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	frame := make([]float32, audio.OutputFrameSamples)
	msg := make([]byte, audio.EncodedAudioFrameLen(audio.OutputFrameSamples))
	var seq uint32
	var lastMeta time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.mu.Lock()
			src := o.source
			dc := o.dc
			epoch := o.epoch
			o.mu.Unlock()
			if src == nil || dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}
			// Re-check cancellation after capturing state: stop/interrupt may have
			// fired between the tick and here, and the client also drops any frame
			// from a superseded epoch — so a barge-in never leaks a stale frame.
			if ctx.Err() != nil {
				return
			}
			// ALWAYS pull a frame from the source, even when we're about to drop it for
			// backpressure: a finite source (TTS) must keep draining so its completion
			// (ttsSource.Done) is reached under a slow/stuck-but-open channel rather
			// than hanging. The send — not the consume — is what backpressure skips.
			real := src.Next(frame)
			_, isTTS := src.(*ttsSource)
			// Leading silence: a TTS source whose first segment hasn't arrived yet (a cold
			// or slow synth) yields real==0. Don't send OR count it — otherwise the client
			// plays and reports those silent samples as "heard", inflating the barge-in
			// cursor (played_ms) above the audible content. A barge-in before any reply
			// audio would then look like a partial reply instead of "[interrupted]". The
			// finite TTS source still drains: once its audio arrives, real>0.
			if isTTS && real == 0 {
				continue
			}
			// Drop the SEND if it would push the queue past the budget, so buffered
			// audio never exceeds maxBufferedBytes (a plain > check could still admit
			// one more frame at the threshold). The frame is already consumed.
			if dc.BufferedAmount()+uint64(len(msg)) > maxBufferedBytes {
				o.s.metrics.incDropped()
				o.recordDrop(epoch) // this epoch's audio is now incomplete (frame undelivered)
				continue
			}
			seq++
			sendMicros := o.s.nowMicros()
			n := audio.EncodeAudioFrame(msg, seq, epoch, sendMicros, frame)
			if err := dc.Send(msg[:n]); err != nil {
				o.s.log.Debug("rtc: send audio frame", "err", err)
				o.recordDrop(epoch) // consumed but not delivered -> this epoch's audio is incomplete
				continue
			}
			o.s.metrics.addOut(uint64(n))
			// Feed echo suppression ONLY with TTS audio that was actually DELIVERED to
			// the browser (after a successful Send, with real audio) — not frames
			// dropped for backpressure / send errors. Otherwise, on a degraded link,
			// undelivered frames would raise the echo guard and suppress real user
			// speech, breaking barge-in exactly when the link is bad. Energy is computed
			// synchronously, so passing the reused frame buffer is safe.
			if isTTS && real > 0 {
				o.s.observePlayback(frame)
			}
			o.mu.Lock()
			if epoch == o.epoch { // ignore the rare frame sent across an epoch swap
				// Count AUDIBLE samples only (real), not the zero-padding of a partial last
				// frame, so the cursor upper bound the client cursor is clamped to reflects
				// what the user could actually hear. Non-TTS sources fill the frame (real ==
				// len) so this is unchanged for them.
				o.sent += int64(real)
			}
			o.mu.Unlock()

			if elapsed := time.Duration(sendMicros) * time.Microsecond; elapsed-lastMeta >= metaThrottle {
				lastMeta = elapsed
				o.s.emit(events.TypeAudioOutputFrame, map[string]any{
					"frame_seq":    seq,
					"samples":      len(frame),
					"real_samples": real,
					"send_micros":  sendMicros,
				})
			}
		}
	}
}
