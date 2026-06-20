package rtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// gatherTimeout caps how long Answer waits for ICE gathering before returning
// the answer with whatever candidates it has. Localhost/LAN host candidates
// gather in milliseconds; the cap only bounds a pathological stall.
const gatherTimeout = 10 * time.Second

// pendingTimeout bounds how long a negotiated-but-uncommitted session is kept
// before it is rolled back. A session commits when its control channel opens; if
// the browser never applies the answer (lost/buffered) it never opens, so this
// reclaims the pending session instead of leaking it. A var so tests can shorten
// it.
var pendingTimeout = 20 * time.Second

// ErrManagerClosed is returned by Answer after the manager has shut down.
var ErrManagerClosed = errors.New("rtc: manager closed")

// ErrSuperseded is returned when a newer Answer for the same user started while
// this one was still negotiating, so this (now stale) offer is discarded.
var ErrSuperseded = errors.New("rtc: offer superseded by a newer one")

// Manager owns the shared WebRTC API (media engine + interceptors) and the set
// of live sessions, enforcing one active talk session per user. It is safe for
// concurrent use.
type Manager struct {
	api        *webrtc.API
	iceServers []webrtc.ICEServer
	log        *slog.Logger
	sink       EventSink
	tts        tts.Engine // optional local speech engine, shared across sessions

	mu        sync.Mutex
	sessions  map[string]*Session           // userID -> active (committed) session
	pending   map[string]*Session           // sessionID -> negotiated, not yet committed
	userGen   map[string]uint64             // userID -> latest offer generation
	negCancel map[string]context.CancelFunc // userID -> cancel its in-flight negotiation
	closed    bool
}

// NewManager builds a Manager. The media engine registers the default codecs
// (so the browser's Opus mic track is received) and RegisterDefaultInterceptors
// wires NACK + RTCP reports + stats, which back the loss/jitter/RTT metrics.
func NewManager(cfg Config, sink EventSink) (*Manager, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	// Register ONLY Opus audio — not Pion's full default set (which also includes
	// PCMU/G722 and video). The inbound pipeline decodes every audio track as
	// Opus and has no use for video, so constraining the media engine makes a
	// version-skewed or hostile client unable to negotiate media we'd misdecode
	// or leave unread inside the one allowed session.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    48000,
			Channels:     2,
			SDPFmtpLine:  "minptime=10;useinbandfec=1",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}},
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("rtc: register opus codec: %w", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, fmt.Errorf("rtc: register interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	return &Manager{
		api:        api,
		iceServers: cfg.ICEServers,
		log:        log,
		sink:       sink,
		tts:        cfg.TTS,
		sessions:   make(map[string]*Session),
		pending:    make(map[string]*Session),
		userGen:    make(map[string]uint64),
		negCancel:  make(map[string]context.CancelFunc),
	}, nil
}

// Answer negotiates a new session for the user from the browser's SDP offer and
// returns the SDP answer and the new session id. The session is left PENDING: it
// does NOT yet replace the user's current active session. The caller must Commit
// it once the answer is delivered (which closes the prior session), or
// CloseSession to roll back — so a cancelled/abandoned replacement POST can never
// drop the user's existing live call. conversationID may be "" for a standalone
// test session not tied to a conversation.
func (mgr *Manager) Answer(ctx context.Context, userID, conversationID, offerSDP string) (answerSDP, sessionID string, err error) {
	// Reserve this offer's generation up front and cancel any older in-flight
	// negotiation for the same user. This bounds concurrent PeerConnections to
	// ~one per user and makes the winner the latest request by ARRIVAL — not
	// whichever offer happens to finish gathering first, which could otherwise
	// let a slow stale offer replace a newer connected session.
	mgr.mu.Lock()
	if mgr.closed {
		mgr.mu.Unlock()
		return "", "", ErrManagerClosed
	}
	mgr.userGen[userID]++
	myGen := mgr.userGen[userID]
	if cancel := mgr.negCancel[userID]; cancel != nil {
		cancel() // supersede the previous, now-stale negotiation
	}
	negCtx, negCancel := context.WithCancel(context.Background())
	mgr.negCancel[userID] = negCancel
	mgr.mu.Unlock()
	defer negCancel()

	// Negotiate under a context cancelled if the caller goes away OR a newer
	// offer supersedes this one.
	nctx, ncancel := mergeCancel(ctx, negCtx)
	defer ncancel()
	sess, answerSDP, err := mgr.newSession(nctx, userID, conversationID, offerSDP)

	mgr.mu.Lock()
	superseded := mgr.userGen[userID] != myGen
	closed := mgr.closed
	// If we weren't superseded, the negCancel slot is still ours — stop tracking
	// it. If we were, a newer Answer now owns that slot; leave it alone.
	if !superseded {
		delete(mgr.negCancel, userID)
	}
	switch {
	case err != nil:
		mgr.mu.Unlock()
		return "", "", err
	case closed:
		mgr.mu.Unlock()
		sess.Close()
		return "", "", ErrManagerClosed
	case superseded:
		mgr.mu.Unlock()
		sess.Close()
		return "", "", ErrSuperseded
	case sess.isClosed():
		// The peer connection failed during negotiation and the session already
		// closed itself; never store a dead session.
		mgr.mu.Unlock()
		return "", "", errors.New("rtc: session closed during setup")
	}
	// Evict any older pending session for this user (a newer offer supersedes it),
	// so a client POSTing offers without applying answers can't stockpile pending
	// PeerConnections — at most one pending per user.
	var evicted []*Session
	for id, ps := range mgr.pending {
		if ps.userID == userID {
			if removed := mgr.removePending(id); removed != nil {
				evicted = append(evicted, removed)
			}
		}
	}
	// Hold the negotiated session as pending (NOT active yet). The current active
	// session keeps running until this one's control channel opens (commit) or it
	// times out (rollback).
	sess.gen = myGen
	mgr.pending[sess.id] = sess
	sess.pendingTimer = time.AfterFunc(pendingTimeout, func() { mgr.expirePending(sess.id) })
	mgr.mu.Unlock()

	for _, ps := range evicted {
		ps.Close()
	}
	return answerSDP, sess.id, nil
}

// removePending removes a pending session by id and stops its expiry timer,
// returning it (nil if not pending). Caller must hold mgr.mu.
func (mgr *Manager) removePending(sessionID string) *Session {
	sess := mgr.pending[sessionID]
	if sess != nil {
		delete(mgr.pending, sessionID)
		if sess.pendingTimer != nil {
			sess.pendingTimer.Stop()
		}
	}
	return sess
}

// expirePending rolls back a pending session that never became ready (its
// control channel never opened), leaving any active session untouched.
func (mgr *Manager) expirePending(sessionID string) {
	mgr.mu.Lock()
	sess := mgr.removePending(sessionID)
	mgr.mu.Unlock()
	if sess != nil {
		sess.log.Info("rtc: pending session timed out before ready; rolling back")
		sess.Close()
	}
}

// Commit promotes a pending session to the user's active session, closing the
// prior active session. It is a no-op if the session was already rolled back
// (CloseSession) or superseded by a newer offer while the answer was in flight —
// in which case the pending session is discarded without disturbing the active
// one. Called by the signaling handler after the SDP answer is delivered.
func (mgr *Manager) Commit(sessionID string) {
	// Peek the pending session without removing it yet.
	mgr.mu.Lock()
	sess := mgr.pending[sessionID]
	mgr.mu.Unlock()
	if sess == nil {
		return // already rolled back, committed, or closed
	}

	// Hold the session's commit lock across the viability check AND the map swap,
	// so a concurrent Close (which also takes commitMu before setting closed)
	// cannot flip this session closed between the two — which would otherwise let
	// us promote a dying session and close the displaced active one, leaving the
	// user with no active call. Lock order is always commitMu -> mgr.mu. commitMu
	// is released BEFORE any Close() (Close also takes it, so calling it here
	// would self-deadlock).
	sess.commitMu.Lock()
	mgr.mu.Lock()
	if mgr.removePending(sessionID) == nil { // re-check under the lock
		mgr.mu.Unlock()
		sess.commitMu.Unlock()
		return
	}
	// Discard (don't promote) if the manager is shutting down, a newer offer
	// superseded us, or the session is no longer viable — it closed, or a
	// datachannel dropped. We must NOT close the displaced active session for a
	// pending one that's already dead.
	discard := mgr.closed || mgr.userGen[sess.userID] != sess.gen || sess.isClosed() || !sess.channelsOpen()
	var old *Session
	if !discard {
		old = mgr.sessions[sess.userID]
		mgr.sessions[sess.userID] = sess
	}
	mgr.mu.Unlock()
	sess.commitMu.Unlock()

	if discard {
		sess.Close()
		return
	}
	// Close the displaced session (a different session, its own commitMu) outside
	// the locks; onSessionClosed's identity check makes its self-removal a no-op
	// now that the map holds the new session.
	if old != nil {
		old.Close()
	}
}

// mergeCancel returns a context derived from parent that is also cancelled when
// trigger is cancelled. The returned cancel stops the watcher goroutine.
func mergeCancel(parent, trigger context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-trigger.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (mgr *Manager) newSession(ctx context.Context, userID, conversationID, offerSDP string) (*Session, string, error) {
	pc, err := mgr.api.NewPeerConnection(webrtc.Configuration{ICEServers: mgr.iceServers})
	if err != nil {
		return nil, "", fmt.Errorf("rtc: new peer connection: %w", err)
	}
	s := newSession(id.New("rtc"), userID, conversationID, pc, mgr.log, mgr.sink, mgr.tts)
	s.onClose = func() { mgr.onSessionClosed(s) }
	s.onReady = func() { mgr.Commit(s.id) } // commit when the control channel opens
	s.wire()

	// cleanup closes the peer connection on any negotiation failure so a half-
	// built session never leaks file descriptors / goroutines.
	fail := func(err error) (*Session, string, error) {
		s.Close()
		return nil, "", err
	}
	// Require EXACTLY one audio track. >1 would leave extra m-lines negotiated but
	// unread; 0 (a datachannel-only offer) would let a session commit and replace
	// a working call without ever capturing mic audio. Fail closed at signaling;
	// handleTrack's claimAudioTrack guard is belt-and-suspenders.
	if n, err := offerSendingAudioCount(offerSDP); err != nil {
		return fail(fmt.Errorf("rtc: parse offer: %w", err))
	} else if n != 1 {
		return fail(fmt.Errorf("rtc: offer must have exactly one sending audio track, has %d", n))
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return fail(fmt.Errorf("rtc: set remote description: %w", err))
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail(fmt.Errorf("rtc: create answer: %w", err))
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail(fmt.Errorf("rtc: set local description: %w", err))
	}
	// Non-trickle: wait for gathering so the returned SDP carries all candidates.
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-time.After(gatherTimeout):
		mgr.log.Warn("rtc: ICE gather timeout; answering with partial candidates", "session", s.id)
	}
	local := pc.LocalDescription()
	if local == nil {
		return fail(errors.New("rtc: no local description after gathering"))
	}
	return s, local.SDP, nil
}

// offerSendingAudioCount returns the number of active (non-rejected) audio
// m-lines in an SDP offer where the offerer is SENDING (sendrecv or sendonly). A
// recvonly/inactive audio section carries no microphone, so it doesn't count —
// otherwise a no-mic offer could commit and replace a working call without ever
// capturing audio. Direction resolves session-level first, then media-level
// override (RFC 3264), so a session-level a=recvonly with no media direction is
// correctly treated as non-sending.
func offerSendingAudioCount(offerSDP string) (int, error) {
	desc := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}
	parsed, err := desc.Unmarshal()
	if err != nil {
		return 0, err
	}
	sessionDir := audioDirection(parsed.Attributes, "sendrecv") // RFC default is sendrecv
	n := 0
	for _, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != "audio" || md.MediaName.Port.Value == 0 {
			continue
		}
		dir := audioDirection(md.Attributes, sessionDir) // media overrides session
		if dir == "sendrecv" || dir == "sendonly" {
			n++
		}
	}
	return n, nil
}

// audioDirection returns the first direction attribute (sendrecv/sendonly/
// recvonly/inactive) in attrs, or fallback if none is present.
func audioDirection(attrs []sdp.Attribute, fallback string) string {
	for _, a := range attrs {
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return a.Key
		}
	}
	return fallback
}

// onSessionClosed removes a session from the map iff it is still the active one
// for its user (identity check), so a replaced session's late close can't evict
// its successor.
func (mgr *Manager) onSessionClosed(s *Session) {
	mgr.mu.Lock()
	if mgr.sessions[s.userID] == s {
		delete(mgr.sessions, s.userID)
	}
	mgr.removePending(s.id) // also drop it (and stop its timer) if it was pending
	mgr.mu.Unlock()
}

// CloseSession closes the session with the given id if it is still active. Used
// to roll back a session whose SDP answer never reached the client (the POST
// failed or was abandoned), so an unreachable session doesn't linger until ICE
// eventually fails.
func (mgr *Manager) CloseSession(sessionID string) {
	mgr.mu.Lock()
	target := mgr.removePending(sessionID) // rolling back a not-yet-committed session
	if target == nil {
		for _, s := range mgr.sessions {
			if s.id == sessionID {
				target = s
				break
			}
		}
	}
	mgr.mu.Unlock()
	if target != nil {
		target.Close()
	}
}

// ErrNoSession is returned by Speak when the user has no active live session.
var ErrNoSession = errors.New("rtc: no active session for user")

// Speak synthesizes text into the user's active live session over the outbound
// audio path (server-driven TTS, e.g. speaking a chat reply aloud). It returns
// ErrNoSession if the user has no active call, or the session's synchronous
// rejection (unavailable / empty / too long / unknown voice / too many sentences).
func (mgr *Manager) Speak(userID, text string, opts tts.Options) error {
	mgr.mu.Lock()
	sess := mgr.sessions[userID]
	mgr.mu.Unlock()
	if sess == nil {
		return ErrNoSession
	}
	return sess.speak(text, opts)
}

// Stats returns a snapshot of every active session's metrics for the admin view.
func (mgr *Manager) Stats() []SessionStats {
	mgr.mu.Lock()
	sessions := make([]*Session, 0, len(mgr.sessions))
	for _, s := range mgr.sessions {
		sessions = append(sessions, s)
	}
	mgr.mu.Unlock()

	out := make([]SessionStats, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.stats())
	}
	return out
}

// Close shuts the manager down and tears down every active session. After Close,
// Answer returns ErrManagerClosed.
func (mgr *Manager) Close() {
	mgr.mu.Lock()
	if mgr.closed {
		mgr.mu.Unlock()
		return
	}
	mgr.closed = true
	sessions := make([]*Session, 0, len(mgr.sessions)+len(mgr.pending))
	for _, s := range mgr.sessions {
		sessions = append(sessions, s)
	}
	for _, s := range mgr.pending { // tear down not-yet-committed sessions too
		sessions = append(sessions, s)
		if s.pendingTimer != nil {
			s.pendingTimer.Stop()
		}
	}
	mgr.sessions = make(map[string]*Session)
	mgr.pending = make(map[string]*Session)
	// Abort any in-flight negotiations so their gather waits unblock promptly.
	for _, cancel := range mgr.negCancel {
		cancel()
	}
	mgr.negCancel = make(map[string]context.CancelFunc)
	mgr.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}
