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

// outbound is the server→browser PCM pipeline: a paced writer that pulls frames
// from the active Source, frames them with a binary header, and sends them on
// the unreliable audio datachannel. It also holds the playback cursor reported
// back by the browser.
type outbound struct {
	s *Session

	mu      sync.Mutex
	dc      *webrtc.DataChannel
	source  Source
	running bool
	cancel  context.CancelFunc
	epoch   uint32 // bumped on every (re)start; stamped into each frame
	sent    int64  // samples actually sent this epoch (cursor upper bound)

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
func (o *outbound) attach(dc *webrtc.DataChannel) {
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

// stop halts the pacer and clears the source, emitting PlaybackStopped with the
// last reported cursor. Safe to call when already stopped.
func (o *outbound) stop() { o.halt("stopped", false, 0) }

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
	if o.halt("interrupt", true, epoch) {
		o.s.metrics.markInterrupt()
	}
}

// halt stops the pacer and clears the source, emitting PlaybackStopped with the
// last cursor. When expectEpoch is non-zero it acts only if that epoch is still
// the active one. It returns whether it actually stopped a running playback.
func (o *outbound) halt(reason string, truncated bool, expectEpoch uint32) bool {
	o.mu.Lock()
	if expectEpoch != 0 && expectEpoch != o.epoch {
		o.mu.Unlock()
		return false // stale: the active playback has since advanced
	}
	wasRunning := o.running
	cancel := o.cancel
	o.cancel = nil
	o.running = false
	o.source = nil
	playedMs := o.playedMs
	playedSamples := o.playedSamples
	o.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if !wasRunning && !truncated {
		return false // nothing was playing and this isn't an explicit barge-in
	}
	o.s.emit(events.TypePlaybackStopped, map[string]any{
		"reason":         reason,
		"truncated":      truncated,
		"cursor_ms":      playedMs,
		"played_samples": playedSamples,
	})
	return wasRunning
}

// feedLoopback forwards decoded mic PCM (24 kHz) to the active source. Only the
// loopback source consumes it; for the tone source (or idle) it is dropped.
func (o *outbound) feedLoopback(samples []float32) {
	o.mu.Lock()
	src := o.source
	o.mu.Unlock()
	if src != nil {
		src.Feed(samples)
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

// mode reports the active source name, or ModeIdle when stopped.
func (o *outbound) mode() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.running || o.source == nil {
		return ModeIdle
	}
	return o.source.Name()
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
			// Drop if sending this frame WOULD push the queue past the budget, so
			// buffered audio never exceeds maxBufferedBytes (a plain > check could
			// still admit one more frame at the threshold).
			if dc.BufferedAmount()+uint64(len(msg)) > maxBufferedBytes {
				o.s.metrics.incDropped()
				continue
			}
			real := src.Next(frame)
			seq++
			sendMicros := o.s.nowMicros()
			n := audio.EncodeAudioFrame(msg, seq, epoch, sendMicros, frame)
			if err := dc.Send(msg[:n]); err != nil {
				o.s.log.Debug("rtc: send audio frame", "err", err)
				continue
			}
			o.s.metrics.addOut(uint64(n))
			o.mu.Lock()
			if epoch == o.epoch { // ignore the rare frame sent across an epoch swap
				o.sent += int64(len(frame))
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
