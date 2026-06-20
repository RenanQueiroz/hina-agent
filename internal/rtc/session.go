package rtc

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/pion/webrtc/v4"
)

// Session is one live talk session: a peer connection, the inbound (mic) and
// outbound (PCM) pipelines, the control datachannel, and per-session metrics.
// Its lifetime is bounded by ctx; Close is idempotent.
type Session struct {
	id             string
	userID         string
	conversationID string
	gen            uint64 // negotiation generation, for two-phase commit
	pc             *webrtc.PeerConnection
	log            *slog.Logger
	sink           EventSink
	tts            tts.Engine // optional local speech engine (nil/unavailable -> SpeakText rejected)

	start  time.Time // monotonic reference for send timestamps / cursors
	ctx    context.Context
	cancel context.CancelFunc

	in      *inbound
	out     *outbound
	metrics *Metrics

	mu         sync.Mutex // guards the datachannel refs + open flags
	eventsDC   *webrtc.DataChannel
	audioDC    *webrtc.DataChannel
	eventsOpen bool // control channel open
	audioOpen  bool // audio channel open with the correct (unreliable) contract

	audioTrackClaimed atomic.Bool   // ensures exactly one inbound readLoop
	disconnectGen     atomic.Uint64 // bumped on each connect/disconnect to invalidate stale grace timers
	commitMu          sync.Mutex    // serializes Close's closed-flag set with Manager.Commit
	closed            atomic.Bool   // set the instant Close begins
	closeOnce         sync.Once
	onClose           func()

	readyOnce    sync.Once   // onReady fires exactly once
	onReady      func()      // called when the control channel opens (commit signal)
	pendingTimer *time.Timer // rolls the session back if it never becomes ready

	speakMu     sync.Mutex         // guards speakCancel + speakGen
	speakCancel context.CancelFunc // cancels the in-flight spoken reply, if any
	speakGen    uint64             // arrival generation: a speak only commits if it's still current
}

// maxControlBytes bounds a single events-channel control message. Control frames
// are tiny JSON; anything larger is rejected before json.Unmarshal so an
// authenticated peer can't force large allocations over the reliable channel
// (the events channel has no HTTP MaxBytesReader in front of it).
const maxControlBytes = 16 * 1024

// isClosed reports whether Close has been initiated.
func (s *Session) isClosed() bool { return s.closed.Load() }

// channelsOpen reports whether both the control and audio datachannels are
// currently open — the precondition for committing the session as active.
func (s *Session) channelsOpen() bool {
	s.mu.Lock()
	e, a := s.eventsDC, s.audioDC
	s.mu.Unlock()
	return e != nil && a != nil &&
		e.ReadyState() == webrtc.DataChannelStateOpen &&
		a.ReadyState() == webrtc.DataChannelStateOpen
}

func newSession(sid, userID, conversationID string, pc *webrtc.PeerConnection, log *slog.Logger, sink EventSink, engine tts.Engine) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		id:             sid,
		userID:         userID,
		conversationID: conversationID,
		pc:             pc,
		log:            log.With("session", sid, "user", userID),
		sink:           sink,
		tts:            engine,
		start:          time.Now(),
		ctx:            ctx,
		cancel:         cancel,
		metrics:        newMetrics(),
	}
	s.in = newInbound(s)
	s.out = newOutbound(s)
	return s
}

// wire installs the peer-connection callbacks. Called before SetRemoteDescription
// so no track/datachannel/state event is missed.
func (s *Session) wire() {
	s.pc.OnTrack(s.handleTrack)
	s.pc.OnDataChannel(s.handleDataChannel)
	s.pc.OnConnectionStateChange(s.handleConnState)
}

func (s *Session) handleTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	// Exactly one inbound pipeline per session: the inbound DSP (stateful
	// resamplers + capture cursor) is single-reader. A peer that offers extra
	// audio tracks must not spawn concurrent readLoops over the shared state.
	if !s.claimAudioTrack() {
		s.log.Warn("rtc: ignoring extra audio track", "track", track.ID())
		return
	}
	go s.in.readLoop(track)
}

// claimAudioTrack returns true only for the first audio track of the session,
// so at most one inbound readLoop ever runs over the shared DSP state.
func (s *Session) claimAudioTrack() bool {
	return s.audioTrackClaimed.CompareAndSwap(false, true)
}

func (s *Session) handleDataChannel(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case channelEvents:
		s.mu.Lock()
		s.eventsDC = dc
		s.mu.Unlock()
		dc.OnMessage(s.handleControlMessage)
		dc.OnOpen(func() { s.markChannelReady(true) })
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			s.markChannelReady(true)
		}
		// The control channel is the session lifeline: when the browser ends the
		// call it closes the datachannels, which the server sees well before ICE
		// eventually fails. Tear the session down on that close so a dead session
		// (and its pacer) doesn't linger.
		dc.OnClose(s.Close)
	case channelAudio:
		// The audio channel MUST be unreliable + unordered (maxRetransmits:0): the
		// pacer's latency/backpressure budget assumes a lost frame is skipped, not
		// retransmitted with head-of-line blocking. Reject a version-skewed or
		// hostile client that negotiates a reliable/ordered audio channel; leaving
		// audioOpen false means this session can never become ready/commit and is
		// rolled back on timeout — so it can't replace a working call.
		if dc.Ordered() || dc.MaxRetransmits() == nil || *dc.MaxRetransmits() != 0 {
			s.log.Warn("rtc: audio channel has wrong reliability contract; closing",
				"ordered", dc.Ordered(), "max_retransmits", dc.MaxRetransmits())
			s.emit(events.TypeError, map[string]string{
				"error": "audio channel must be unordered with maxRetransmits=0",
			})
			_ = dc.Close()
			return
		}
		s.mu.Lock()
		s.audioDC = dc
		s.mu.Unlock()
		s.out.attach(dc)
		dc.OnOpen(func() { s.markChannelReady(false) })
		// Losing the audio channel makes the session unable to play outbound PCM,
		// so treat its close as terminal (like the control channel) rather than
		// leaving a half-working "active" session.
		dc.OnClose(s.Close)
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			s.markChannelReady(false)
		}
	default:
		s.log.Debug("rtc: ignoring unknown datachannel", "label", dc.Label())
	}
}

// markChannelReady records that the events (isEvents) or audio channel opened.
// The session is "ready" — and commits as the user's active session — only when
// BOTH the control AND the audio channels are open with the correct contracts.
// A client that omits or mis-negotiates the audio channel therefore never
// becomes ready, so it can't replace a working active session (it times out and
// rolls back instead). Fires onReady exactly once.
func (s *Session) markChannelReady(isEvents bool) {
	s.mu.Lock()
	if isEvents {
		s.eventsOpen = true
	} else {
		s.audioOpen = true
	}
	both := s.eventsOpen && s.audioOpen
	eventsDC, audioDC := s.eventsDC, s.audioDC
	s.mu.Unlock()
	if !both || s.isClosed() {
		return
	}
	// Re-verify both channels are actually open right now: a flag was set when a
	// channel opened, but it may have closed since. Only a session whose control
	// AND audio channels are both currently open is committed.
	if eventsDC == nil || audioDC == nil ||
		eventsDC.ReadyState() != webrtc.DataChannelStateOpen ||
		audioDC.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	s.readyOnce.Do(func() {
		if s.onReady != nil {
			s.onReady()
		}
	})
}

// disconnectGrace is how long a Disconnected ICE state is tolerated before the
// session is torn down. ICE may briefly disconnect and recover on a transient
// blip, so we don't close immediately; if it hasn't recovered after the grace it
// is treated as gone (faster than waiting for the eventual Failed).
const disconnectGrace = 5 * time.Second

func (s *Session) handleConnState(state webrtc.PeerConnectionState) {
	s.log.Debug("rtc: connection state", "state", state.String())
	switch state {
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		s.Close()
	case webrtc.PeerConnectionStateConnected:
		// Recovery: invalidate any pending disconnect-grace timer so it can't tear
		// the session down for an outage that has since healed.
		s.disconnectGen.Add(1)
	case webrtc.PeerConnectionStateDisconnected:
		// A disconnect may be a transient ICE blip. Start a grace timer tagged with
		// this episode's generation; it tears down only if this same episode is
		// still the current one (no recovery/re-disconnect since) AND still
		// disconnected after the grace.
		gen := s.disconnectGen.Add(1)
		go func() {
			select {
			case <-s.ctx.Done():
				return // session already closed
			case <-time.After(disconnectGrace):
			}
			if s.shouldCloseAfterGrace(gen, s.pc.ConnectionState()) {
				s.log.Info("rtc: tearing down session after disconnect grace")
				s.Close()
			}
		}()
	}
}

// shouldCloseAfterGrace reports whether a disconnect-grace timer for episode
// `gen` should close the session: only if it is still the current episode (no
// later connect/disconnect bumped the generation) and the connection is still
// disconnected.
func (s *Session) shouldCloseAfterGrace(gen uint64, state webrtc.PeerConnectionState) bool {
	return s.disconnectGen.Load() == gen && state == webrtc.PeerConnectionStateDisconnected
}

// handleControlMessage processes a client→server control event (JSON envelope)
// on the events channel. Unknown or malformed messages are ignored so a buggy
// or hostile client can't crash the session.
func (s *Session) handleControlMessage(msg webrtc.DataChannelMessage) {
	if !msg.IsString {
		return // control is JSON text; binary here is unexpected
	}
	if len(msg.Data) > maxControlBytes {
		s.log.Warn("rtc: oversized control message dropped", "bytes", len(msg.Data))
		return
	}
	var e events.Event
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		return
	}
	switch e.Type {
	case events.TypeModeChanged:
		var p struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.setMode(p.Mode)
	case events.TypeSpeakText:
		var p struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
			Lang  string `json:"lang"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.speak(p.Text, tts.Options{Voice: p.Voice, Lang: p.Lang})
	case events.TypeUserInterrupted:
		var p struct {
			Epoch         uint32 `json:"epoch"`
			PlayedSamples int64  `json:"played_samples"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.cancelSpeak() // a barge-in also stops any in-flight spoken reply
		s.out.interrupt(p.Epoch, p.PlayedSamples, s.nowMicros())
	case events.TypePlaybackProgress:
		var p struct {
			Epoch         uint32 `json:"epoch"`
			PlayedSamples int64  `json:"played_samples"`
			AckSendMicros int64  `json:"ack_send_micros"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.out.recordCursor(p.Epoch, p.PlayedSamples, p.AckSendMicros, s.nowMicros())
	}
}

// setMode selects the outbound audio source. "idle" stops playback; "loopback"
// echoes the decoded mic; "tone" plays the generated test tone. Any mode change
// also cancels an in-flight spoken reply (its audio source is being replaced).
func (s *Session) setMode(mode string) {
	s.cancelSpeak()
	switch mode {
	case ModeIdle:
		s.out.stop()
		s.emit(events.TypeModeChanged, map[string]string{"mode": ModeIdle})
	case ModeLoopback:
		epoch := s.out.start(newLoopbackSource())
		s.emit(events.TypeModeChanged, map[string]string{"mode": ModeLoopback})
		s.emitPlaybackStarted(ModeLoopback, epoch)
	case ModeTone:
		epoch := s.out.start(newToneSource())
		s.emit(events.TypeModeChanged, map[string]string{"mode": ModeTone})
		s.emitPlaybackStarted(ModeTone, epoch)
	default:
		s.log.Debug("rtc: ignoring unknown mode", "mode", mode)
	}
}

func (s *Session) emitPlaybackStarted(mode string, epoch uint32) {
	s.emit(events.TypePlaybackStarted, map[string]any{
		"source":      mode,
		"sample_rate": audio.OutputSampleRate,
		"channels":    1,
		"epoch":       epoch,
	})
}

// nowMicros is the monotonic microseconds elapsed since the session started — a
// single clock both the send-timestamp and the round-trip latency use.
func (s *Session) nowMicros() int64 { return time.Since(s.start).Microseconds() }

// emit sends a server-sourced event to the live client (over the events
// channel) and mirrors it onto the SSE stream for observability.
func (s *Session) emit(typ string, payload any) {
	e, err := events.New(events.SourceServer, typ, s.conversationID, s.userID, "", payload)
	if err != nil {
		s.log.Error("rtc: build event", "type", typ, "err", err)
		return
	}
	e.ServerTS = time.Now().UTC()
	s.sendToClient(e)
	s.sink.PublishEphemeral(e)
}

func (s *Session) sendToClient(e events.Event) {
	s.mu.Lock()
	dc := s.eventsDC
	s.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := dc.SendText(string(data)); err != nil {
		s.log.Debug("rtc: send control event", "type", e.Type, "err", err)
	}
}

// stats snapshots this session's metrics. The network stats (loss/jitter/RTT)
// come from the cached values the sampler goroutine maintains, so this never
// calls pc.GetStats() on the request path (avoiding the Pion GetStats/close
// race).
func (s *Session) stats() SessionStats {
	st := s.metrics.snapshot()
	st.SessionID = s.id
	st.UserID = s.userID
	st.ConversationID = s.conversationID
	st.Mode = s.out.mode()
	st.UptimeMs = time.Since(s.start).Milliseconds()
	return st
}

// Close tears the session down exactly once: cancel goroutines, stop the pacer,
// close the peer connection (which unblocks ReadRTP and closes the channels),
// and deregister from the manager.
func (s *Session) Close() {
	// Mark closed under commitMu so Manager.Commit can't observe this session as
	// viable and promote it (closing the displaced active session) while it is
	// being torn down. The actual teardown still runs exactly once.
	s.commitMu.Lock()
	s.closed.Store(true)
	s.commitMu.Unlock()
	s.closeOnce.Do(func() {
		s.cancelSpeak() // invalidate any pending speak so it can't start after teardown
		s.cancel()
		s.out.stop()
		if err := s.pc.Close(); err != nil {
			s.log.Debug("rtc: close peer connection", "err", err)
		}
		if s.onClose != nil {
			s.onClose()
		}
		// Final observability signal (the client is likely gone, so SSE only).
		if e, err := events.New(events.SourceServer, events.TypeModeChanged, s.conversationID, s.userID, "",
			map[string]string{"mode": ModeIdle, "reason": "closed"}); err == nil {
			e.ServerTS = time.Now().UTC()
			s.sink.PublishEphemeral(e)
		}
	})
}
