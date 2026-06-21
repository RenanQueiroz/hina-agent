package rtc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// fakeSink records events the session mirrors to the SSE stream.
type fakeSink struct {
	mu  sync.Mutex
	all []events.Event
}

func (f *fakeSink) PublishEphemeral(e events.Event) {
	f.mu.Lock()
	f.all = append(f.all, e)
	f.mu.Unlock()
}

func (f *fakeSink) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.all))
	for i, e := range f.all {
		out[i] = e.Type
	}
	return out
}

// events returns a snapshot copy of all recorded events (for payload inspection).
func (f *fakeSink) events() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]events.Event(nil), f.all...)
}

func testManager(t *testing.T) (*Manager, *fakeSink) {
	t.Helper()
	sink := &fakeSink{}
	mgr, err := NewManager(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, sink)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)
	return mgr, sink
}

// browserPeer is a test stand-in for the web client: it creates the events +
// audio datachannels and a mic track, then drives the same SDP/control flow the
// real browser does.
type browserPeer struct {
	pc       *webrtc.PeerConnection
	eventsDC *webrtc.DataChannel
	audioDC  *webrtc.DataChannel
	mic      *webrtc.TrackLocalStaticSample

	mu          sync.Mutex
	gotEvents   []events.Event
	gotAudio    [][]byte
	eventsOpen  chan struct{}
	audioFrames chan []byte
}

func newBrowserPeer(t *testing.T) *browserPeer {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("browser RegisterDefaultCodecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("browser NewPeerConnection: %v", err)
	}
	bp := &browserPeer{pc: pc, eventsOpen: make(chan struct{}), audioFrames: make(chan []byte, 256)}

	ordered := true
	bp.eventsDC, err = pc.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("create events DC: %v", err)
	}
	bp.eventsDC.OnOpen(func() { close(bp.eventsOpen) })
	bp.eventsDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		var e events.Event
		if json.Unmarshal(msg.Data, &e) == nil {
			bp.mu.Lock()
			bp.gotEvents = append(bp.gotEvents, e)
			bp.mu.Unlock()
		}
	})

	maxRetransmits := uint16(0)
	unordered := false
	bp.audioDC, err = pc.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{
		Ordered:        &unordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		t.Fatalf("create audio DC: %v", err)
	}
	bp.audioDC.OnMessage(func(msg webrtc.DataChannelMessage) {
		cp := append([]byte(nil), msg.Data...)
		bp.mu.Lock()
		bp.gotAudio = append(bp.gotAudio, cp)
		bp.mu.Unlock()
		select {
		case bp.audioFrames <- cp:
		default:
		}
	})

	bp.mic, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if err != nil {
		t.Fatalf("new mic track: %v", err)
	}
	if _, err := pc.AddTrack(bp.mic); err != nil {
		t.Fatalf("add mic track: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return bp
}

func (bp *browserPeer) offerSDP(t *testing.T) string {
	t.Helper()
	offer, err := bp.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(bp.pc)
	if err := bp.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("browser SetLocalDescription: %v", err)
	}
	<-gather
	return bp.pc.LocalDescription().SDP
}

func (bp *browserPeer) acceptAnswer(t *testing.T, answerSDP string) {
	t.Helper()
	if err := bp.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}
}

func (bp *browserPeer) sendControl(t *testing.T, typ string, payload any) {
	t.Helper()
	e, err := events.New(events.SourceClient, typ, "", "", "", payload)
	if err != nil {
		t.Fatalf("build control event: %v", err)
	}
	data, _ := json.Marshal(e)
	if err := bp.eventsDC.SendText(string(data)); err != nil {
		t.Fatalf("send control %s: %v", typ, err)
	}
}

func (bp *browserPeer) eventTypes() []string {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	out := make([]string, len(bp.gotEvents))
	for i, e := range bp.gotEvents {
		out[i] = e.Type
	}
	return out
}

// connect runs the full negotiation and waits for the events channel to open.
func (bp *browserPeer) connect(t *testing.T, mgr *Manager, userID, convID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	answer, sessionID, err := mgr.Answer(ctx, userID, convID, bp.offerSDP(t))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	bp.acceptAnswer(t, answer)
	// The session commits itself when its control channel opens (no manual
	// Commit) — wait for that so callers see it as active.
	select {
	case <-bp.eventsOpen:
	case <-time.After(15 * time.Second):
		t.Fatal("events datachannel never opened")
	}
	waitFor(t, "session committed", func() bool {
		for _, st := range mgr.Stats() {
			if st.SessionID == sessionID {
				return true
			}
		}
		return false
	})
	return sessionID
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestTonePlaybackAndInterrupt is the core end-to-end transport test: a browser
// peer connects, asks for the tone source, receives framed PCM over the audio
// channel, reports a cursor, then interrupts — all over real Pion peers.
func TestTonePlaybackAndInterrupt(t *testing.T) {
	mgr, _ := testManager(t)
	bp := newBrowserPeer(t)
	bp.connect(t, mgr, "usr_1", "cnv_1")

	bp.sendControl(t, events.TypeModeChanged, map[string]string{"mode": ModeTone})

	// Collect a handful of audio frames and verify framing + that the tone is
	// non-silent and the sequence increments.
	var first []byte
	select {
	case first = <-bp.audioFrames:
	case <-time.After(10 * time.Second):
		t.Fatal("no audio frame received")
	}
	first0, err := audio.DecodeAudioFrame(first)
	if err != nil {
		t.Fatalf("decode audio frame: %v", err)
	}
	if first0.SendMicros <= 0 {
		t.Fatalf("send timestamp not set: %d", first0.SendMicros)
	}
	if first0.Epoch == 0 {
		t.Fatalf("epoch not set on playback frame: %d", first0.Epoch)
	}
	nonzero := false
	got := make([]float32, len(first0.PCM)/2)
	audio.S16LEToFloat32(got, first0.PCM)
	for _, v := range got {
		if v != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Fatal("tone frame is all silence")
	}

	// Server should have acked the mode and announced playback start.
	waitFor(t, "PlaybackStarted ack", func() bool {
		return contains(bp.eventTypes(), events.TypePlaybackStarted)
	})

	// Report a playback cursor echoing the first frame's send timestamp.
	bp.sendControl(t, events.TypePlaybackProgress, map[string]any{
		"epoch":           first0.Epoch,
		"played_samples":  audio.OutputFrameSamples, // 480 samples -> server derives 20 ms
		"ack_send_micros": first0.SendMicros,
	})

	// A later frame's sequence must exceed the first (the pacer advances).
	waitFor(t, "sequence advance", func() bool {
		bp.mu.Lock()
		defer bp.mu.Unlock()
		for _, f := range bp.gotAudio {
			if df, derr := audio.DecodeAudioFrame(f); derr == nil && df.Seq > first0.Seq {
				return true
			}
		}
		return false
	})

	// Interrupt (barge-in): the server stops and reports the truncation cursor
	// the client supplies (here, the 20 ms it had played).
	bp.sendControl(t, events.TypeUserInterrupted, map[string]any{
		"epoch":          first0.Epoch,
		"played_samples": audio.OutputFrameSamples,
	})
	waitFor(t, "PlaybackStopped(truncated)", func() bool {
		bp.mu.Lock()
		defer bp.mu.Unlock()
		for _, e := range bp.gotEvents {
			if e.Type == events.TypePlaybackStopped {
				var p struct {
					Truncated bool  `json:"truncated"`
					CursorMs  int64 `json:"cursor_ms"`
				}
				_ = json.Unmarshal(e.Payload, &p)
				if p.Truncated && p.CursorMs == 20 {
					return true
				}
			}
		}
		return false
	})

	// Metrics should reflect outbound frames, the cursor, and the interrupt.
	waitFor(t, "stats populated", func() bool {
		for _, st := range mgr.Stats() {
			if st.SessionID != "" && st.FramesOut > 0 && st.Interrupts >= 1 && st.PlayedMs == 20 {
				return true
			}
		}
		return false
	})
}

// TestMicTrackIsRead verifies the inbound RTP read loop runs: writing media on
// the browser's mic track increments the server's RTP-in counter (the bytes are
// not real Opus, so they also exercise the decode-error path without crashing).
func TestMicTrackIsRead(t *testing.T) {
	mgr, _ := testManager(t)
	bp := newBrowserPeer(t)
	bp.connect(t, mgr, "usr_mic", "cnv_mic")

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		payload := make([]byte, 80) // non-Opus bytes: read OK, decode fails
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = bp.mic.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond})
			}
		}
	}()

	waitFor(t, "RTP packets read", func() bool {
		for _, st := range mgr.Stats() {
			if st.RTPPacketsIn > 0 {
				return true
			}
		}
		return false
	})
}

// TestReplaceSessionEnforcesOnePerUser checks a second Answer for the same user
// supersedes the first and the manager keeps exactly one session for that user.
func TestReplaceSessionEnforcesOnePerUser(t *testing.T) {
	mgr, _ := testManager(t)

	bp1 := newBrowserPeer(t)
	id1 := bp1.connect(t, mgr, "usr_dup", "cnv_a")

	bp2 := newBrowserPeer(t)
	id2 := bp2.connect(t, mgr, "usr_dup", "cnv_b")

	if id1 == id2 {
		t.Fatal("expected distinct session ids")
	}
	stats := mgr.Stats()
	count := 0
	for _, st := range stats {
		if st.UserID == "usr_dup" {
			count++
			if st.SessionID != id2 {
				t.Fatalf("active session is %s, want the replacement %s", st.SessionID, id2)
			}
		}
	}
	if count != 1 {
		t.Fatalf("user has %d active sessions, want 1", count)
	}
}

// TestConcurrentAnswersKeepOneSession fires several offers for the same user at
// once; the per-user generation logic must let exactly one win (the rest get
// ErrSuperseded) and must not leak negotiation-cancel entries, regardless of
// which offer's negotiation finishes first. Run under -race, this exercises the
// generation path. (The winner stays pending here — no peer connects to commit
// it.)
func TestConcurrentAnswersKeepOneSession(t *testing.T) {
	mgr, _ := testManager(t)
	const n = 4
	offers := make([]string, n)
	for i := range offers {
		offers[i] = newBrowserPeer(t).offerSDP(t)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := mgr.Answer(context.Background(), "usr_race", "cnv", offers[i]); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("%d offers succeeded, want exactly 1 (the latest generation)", successes)
	}
	mgr.mu.Lock()
	negLeaks := len(mgr.negCancel)
	pendingCount := len(mgr.pending)
	mgr.mu.Unlock()
	if negLeaks != 0 {
		t.Fatalf("negCancel leaked %d entries", negLeaks)
	}
	if pendingCount != 1 { // exactly the one winner, awaiting commit/timeout
		t.Fatalf("pending=%d, want 1 (the winning offer)", pendingCount)
	}
}

// TestPendingTimeoutKeepsExistingSession is the commit-on-readiness invariant: a
// replacement that never becomes ready (its control channel never opens) is
// rolled back on timeout, and the existing active session survives.
func TestPendingTimeoutKeepsExistingSession(t *testing.T) {
	mgr, _ := testManager(t)

	bpA := newBrowserPeer(t)
	idA := bpA.connect(t, mgr, "usr_to", "cnv_a") // committed (active)

	bpB := newBrowserPeer(t)
	_, idB, err := mgr.Answer(context.Background(), "usr_to", "cnv_b", bpB.offerSDP(t))
	if err != nil {
		t.Fatalf("Answer B: %v", err)
	}
	mgr.expirePending(idB) // simulate the pending timeout firing (B never connected)

	active := mgr.Stats()
	if len(active) != 1 || active[0].SessionID != idA {
		t.Fatalf("after B timed out, active=%+v, want only A (%s)", active, idA)
	}
}

// TestRollbackKeepsExistingSession is the two-phase invariant: if a replacement
// offer's answer is never delivered (rolled back), the user's existing active
// session must survive.
func TestRollbackKeepsExistingSession(t *testing.T) {
	mgr, _ := testManager(t)

	// Session A: negotiated and committed (active).
	bpA := newBrowserPeer(t)
	idA := bpA.connect(t, mgr, "usr_two", "cnv_a")

	// Replacement B: negotiated but its answer "fails to deliver", so we roll it
	// back instead of committing.
	bpB := newBrowserPeer(t)
	_, idB, err := mgr.Answer(context.Background(), "usr_two", "cnv_b", bpB.offerSDP(t))
	if err != nil {
		t.Fatalf("Answer B: %v", err)
	}
	mgr.CloseSession(idB) // rollback (answer never delivered)

	// A must still be the user's one active session.
	active := mgr.Stats()
	if len(active) != 1 || active[0].SessionID != idA {
		t.Fatalf("after rolling back B, active=%+v, want only A (%s)", active, idA)
	}
}

// TestRejectsReliableAudioChannel verifies the server closes an audio channel
// that is not unordered/unreliable (maxRetransmits:0), so a version-skewed
// client can't get head-of-line-blocking audio.
func TestRejectsReliableAudioChannel(t *testing.T) {
	mgr, _ := testManager(t)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	ordered := true
	if _, err := pc.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("events DC: %v", err)
	}
	// Wrong contract: reliable + ordered audio channel.
	audioDC, err := pc.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	// A mic track is required for the offer to be accepted at all.
	mic, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if _, err := pc.AddTrack(mic); err != nil {
		t.Fatalf("add mic: %v", err)
	}
	closed := make(chan struct{})
	audioDC.OnClose(func() { close(closed) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather
	answer, _, err := mgr.Answer(context.Background(), "usr_bad", "cnv", pc.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("apply answer: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("server did not close the reliable audio channel")
	}
}

// TestVideoOfferGracefullyRejected verifies that the Opus-only media engine
// rejects a video m-line (it negotiates no video codec) while still establishing
// the audio + datachannel session — a version-skewed/hostile client can't push
// media the server would leave unread.
func TestVideoOfferGracefullyRejected(t *testing.T) {
	mgr, _ := testManager(t)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	ordered, unordered, mr := true, false, uint16(0)
	if _, err := pc.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("events DC: %v", err)
	}
	if _, err := pc.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &mr}); err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	video, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "cam")
	if err != nil {
		t.Fatalf("video track: %v", err)
	}
	if _, err := pc.AddTrack(video); err != nil {
		t.Fatalf("add video: %v", err)
	}
	audio, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if err != nil {
		t.Fatalf("audio track: %v", err)
	}
	if _, err := pc.AddTrack(audio); err != nil {
		t.Fatalf("add audio: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather

	// The server negotiates gracefully (no error) and creates the session…
	answer, sessionID, err := mgr.Answer(context.Background(), "usr_vid", "cnv", pc.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if sessionID == "" {
		t.Fatal("no session id")
	}
	// …while refusing to receive video: a video m-line is present only as
	// rejected (port 0). (A real browser simply stops sending video; we assert
	// the server side, since Pion's test peer can't reconcile a sendonly video
	// track against a rejected m-line, so we never drive it to commit.)
	if !strings.Contains(answer, "m=audio ") {
		t.Fatalf("answer missing audio m-line:\n%s", answer)
	}
	if strings.Contains(answer, "m=video") && !strings.Contains(answer, "m=video 0") {
		t.Fatalf("video m-line not rejected in answer:\n%s", answer)
	}
}

// TestRejectsMultipleAudioTracks verifies an offer with more than one audio
// m-line is refused at signaling time, so extra tracks are never negotiated and
// left unread.
func TestRejectsMultipleAudioTracks(t *testing.T) {
	mgr, _ := testManager(t)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	for i := 0; i < 2; i++ {
		track, err := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			"audio"+string(rune('a'+i)), "mic"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("track %d: %v", i, err)
		}
		if _, err := pc.AddTrack(track); err != nil {
			t.Fatalf("add track %d: %v", i, err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather

	if _, _, err := mgr.Answer(context.Background(), "usr_multi", "cnv", pc.LocalDescription().SDP); err == nil {
		t.Fatal("expected an error for an offer with two audio tracks")
	}
	// No session should have been created.
	if len(mgr.Stats()) != 0 {
		t.Fatalf("a session was created for a rejected multi-audio offer: %d", len(mgr.Stats()))
	}
}

// TestNormalDisconnectTearsDown connects a browser peer, starts playback, then
// closes the peer normally (as the web client's "End" does). The server must
// tear the session down — via the control-channel close or the disconnect grace
// — so no dead session or pacer lingers.
func TestNormalDisconnectTearsDown(t *testing.T) {
	mgr, _ := testManager(t)
	bp := newBrowserPeer(t)
	bp.connect(t, mgr, "usr_bye", "cnv")
	bp.sendControl(t, events.TypeModeChanged, map[string]string{"mode": ModeTone}) // start the pacer

	waitFor(t, "session active", func() bool { return len(mgr.Stats()) == 1 })

	if err := bp.pc.Close(); err != nil { // normal client hangup
		t.Fatalf("close browser pc: %v", err)
	}
	waitFor(t, "session removed after hangup", func() bool { return len(mgr.Stats()) == 0 })
}

// TestCloseSessionByID verifies the signaling rollback path: a session can be
// closed by id (used when the SDP answer never reaches the client), and closing
// an unknown id is a harmless no-op.
func TestCloseSessionByID(t *testing.T) {
	mgr, _ := testManager(t)
	bp := newBrowserPeer(t)
	sid := bp.connect(t, mgr, "usr_cs", "cnv")
	waitFor(t, "session active", func() bool { return len(mgr.Stats()) == 1 })

	mgr.CloseSession("rtc_does_not_exist") // no-op
	if len(mgr.Stats()) != 1 {
		t.Fatal("closing an unknown id must not affect active sessions")
	}

	mgr.CloseSession(sid)
	waitFor(t, "session removed", func() bool { return len(mgr.Stats()) == 0 })
}

// TestSequentialOffersDontStockpilePending verifies a client can't accumulate
// pending PeerConnections by POSTing offers without applying answers: a newer
// offer evicts the user's older pending session, so there is at most one.
func TestSequentialOffersDontStockpilePending(t *testing.T) {
	mgr, _ := testManager(t)
	for i := 0; i < 5; i++ {
		bp := newBrowserPeer(t)
		if _, _, err := mgr.Answer(context.Background(), "usr_seq", "cnv", bp.offerSDP(t)); err != nil {
			t.Fatalf("offer %d: %v", i, err)
		}
	}
	mgr.mu.Lock()
	pending := 0
	for _, ps := range mgr.pending {
		if ps.userID == "usr_seq" {
			pending++
		}
	}
	mgr.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending sessions for user = %d, want 1 (older evicted)", pending)
	}
}

// TestRejectsOfferWithoutAudioTrack verifies a datachannel-only offer (no mic
// m-line) is refused, so it can't commit and replace a working call.
func TestRejectsOfferWithoutAudioTrack(t *testing.T) {
	mgr, _ := testManager(t)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ordered, unordered, mr := true, false, uint16(0)
	if _, err := pc.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("events DC: %v", err)
	}
	if _, err := pc.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &mr}); err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	// No mic track added.
	offer, _ := pc.CreateOffer(nil)
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather
	if _, _, err := mgr.Answer(context.Background(), "usr_noaudio", "cnv", pc.LocalDescription().SDP); err == nil {
		t.Fatal("expected an error for an offer with no audio track")
	}
}

// TestRejectsRecvonlyAudioOffer verifies a recvonly audio m-line (an audio
// section with no microphone sender) is refused — it carries no mic, so it must
// not be able to commit/replace a working call.
func TestRejectsRecvonlyAudioOffer(t *testing.T) {
	mgr, _ := testManager(t)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ordered, unordered, mr := true, false, uint16(0)
	if _, err := pc.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("events DC: %v", err)
	}
	if _, err := pc.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{Ordered: &unordered, MaxRetransmits: &mr}); err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	// A recvonly audio transceiver: one audio m-line, but no mic sender.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	offer, _ := pc.CreateOffer(nil)
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather
	if _, _, err := mgr.Answer(context.Background(), "usr_recv", "cnv", pc.LocalDescription().SDP); err == nil {
		t.Fatal("expected an error for a recvonly (no-mic) audio offer")
	}
}

// TestCommitRejectsNotReadyPending verifies Commit refuses a pending session
// whose datachannels aren't open (e.g. it never connected), leaving the existing
// active session untouched.
func TestCommitRejectsNotReadyPending(t *testing.T) {
	mgr, _ := testManager(t)
	bpA := newBrowserPeer(t)
	idA := bpA.connect(t, mgr, "usr_nr", "cnv_a") // active

	bpB := newBrowserPeer(t)
	_, idB, err := mgr.Answer(context.Background(), "usr_nr", "cnv_b", bpB.offerSDP(t))
	if err != nil {
		t.Fatalf("Answer B: %v", err)
	}
	// B never connected (channels not open). A forced Commit must NOT promote it.
	mgr.Commit(idB)

	active := mgr.Stats()
	if len(active) != 1 || active[0].SessionID != idA {
		t.Fatalf("not-ready B was committed: active=%+v, want only A (%s)", active, idA)
	}
}

// TestCommitRejectsClosedPending simulates the commit/close race: a pending
// session is marked closed (as Close would, under commitMu) before Commit runs.
// Commit must refuse it and leave the existing active session untouched.
func TestCommitRejectsClosedPending(t *testing.T) {
	mgr, _ := testManager(t)
	bpA := newBrowserPeer(t)
	idA := bpA.connect(t, mgr, "usr_cc", "cnv_a") // active

	bpB := newBrowserPeer(t)
	_, idB, err := mgr.Answer(context.Background(), "usr_cc", "cnv_b", bpB.offerSDP(t))
	if err != nil {
		t.Fatalf("Answer B: %v", err)
	}
	// Mark B closed while it is still pending (the race window the commitMu guard
	// protects), then attempt to commit it.
	mgr.mu.Lock()
	b := mgr.pending[idB]
	mgr.mu.Unlock()
	if b == nil {
		t.Fatal("B not pending")
	}
	b.closed.Store(true)
	mgr.Commit(idB)

	active := mgr.Stats()
	if len(active) != 1 || active[0].SessionID != idA {
		t.Fatalf("closed pending B was committed: active=%+v, want only A (%s)", active, idA)
	}
}

// TestBadAudioOfferDoesNotReplaceActive verifies a session whose audio channel
// is mis-negotiated (reliable/ordered) never becomes ready, so it can't commit
// and replace the user's working active session.
func TestBadAudioOfferDoesNotReplaceActive(t *testing.T) {
	mgr, _ := testManager(t)
	bpA := newBrowserPeer(t)
	idA := bpA.connect(t, mgr, "usr_ba", "cnv_a") // committed (correct audio)

	// Peer B (same user) with a RELIABLE audio channel — wrong contract.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("codecs: %v", err)
	}
	pcB, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer B: %v", err)
	}
	t.Cleanup(func() { _ = pcB.Close() })
	ordered := true
	evB, err := pcB.CreateDataChannel(channelEvents, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatalf("events DC: %v", err)
	}
	if _, err := pcB.CreateDataChannel(channelAudio, &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	track, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if _, err := pcB.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}
	offerB, _ := pcB.CreateOffer(nil)
	gather := webrtc.GatheringCompletePromise(pcB)
	if err := pcB.SetLocalDescription(offerB); err != nil {
		t.Fatalf("set local B: %v", err)
	}
	<-gather
	answerB, _, err := mgr.Answer(context.Background(), "usr_ba", "cnv_b", pcB.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("Answer B: %v", err)
	}
	if err := pcB.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerB}); err != nil {
		t.Fatalf("apply answer B: %v", err)
	}
	// Wait until B's control channel opens (so it is connected) — yet it must NOT
	// commit, because its audio channel was rejected.
	opened := make(chan struct{})
	evB.OnOpen(func() { close(opened) })
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("B's control channel never opened")
	}
	// Give any erroneous commit a chance to happen, then assert A is still the
	// one active session.
	time.Sleep(300 * time.Millisecond)
	active := mgr.Stats()
	if len(active) != 1 || active[0].SessionID != idA {
		t.Fatalf("bad-audio B replaced A: active=%+v, want only A (%s)", active, idA)
	}
}

func TestAnswerAfterCloseFails(t *testing.T) {
	mgr, _ := testManager(t)
	mgr.Close()
	bp := newBrowserPeer(t)
	if _, _, err := mgr.Answer(context.Background(), "u", "c", bp.offerSDP(t)); err == nil {
		t.Fatal("expected error answering on a closed manager")
	}
}
