package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/pion/webrtc/v4"
)

// rtcTestServer builds a server wired with a real WebRTC manager, plus a logged-
// in admin client. It returns the test server, the bus (for ownership setup),
// and the authenticated http client.
func rtcTestServer(t *testing.T) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := New(
		config.Default(), st, events.NewBus(st), auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(100),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	mgr, err := rtc.NewManager(rtc.Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, events.NewBus(st))
	if err != nil {
		t.Fatalf("rtc manager: %v", err)
	}
	t.Cleanup(mgr.Close)
	srv.SetRealtime(mgr)
	srv.SetReady(true)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	boot, err := auth.EnsureAdmin(ctx, st)
	if err != nil || !boot.Created {
		t.Fatalf("bootstrap admin: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	postJSON(t, client, ts.URL+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": boot.Password}, nil)
	return ts, st, client
}

// newBrowserOffer builds a pion peer mimicking the web client (events + audio
// datachannels, a mic track) and returns the gathered offer SDP plus the peer so
// the test can apply the server's answer.
func newBrowserOffer(t *testing.T) (string, *webrtc.PeerConnection) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("RegisterDefaultCodecs: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	ordered := true
	if _, err := pc.CreateDataChannel("events", &webrtc.DataChannelInit{Ordered: &ordered}); err != nil {
		t.Fatalf("events DC: %v", err)
	}
	// Audio must be unordered + unreliable to match the server's contract.
	unordered := false
	maxRetransmits := uint16(0)
	if _, err := pc.CreateDataChannel("audio", &webrtc.DataChannelInit{
		Ordered:        &unordered,
		MaxRetransmits: &maxRetransmits,
	}); err != nil {
		t.Fatalf("audio DC: %v", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if err != nil {
		t.Fatalf("mic track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gather
	return pc.LocalDescription().SDP, pc
}

func postSDP(t *testing.T, c *http.Client, url, sdp string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(sdp))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("post sdp: %v", err)
	}
	return resp
}

func TestRealtimeCallEstablishes(t *testing.T) {
	ts, _, client := rtcTestServer(t)
	offer, pc := newBrowserOffer(t)
	t.Cleanup(func() { _ = pc.Close() })

	resp := postSDP(t, client, ts.URL+"/api/v1/realtime/calls", offer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, want 201; body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("content-type=%q, want application/sdp", ct)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/api/v1/realtime/calls/rtc_") {
		t.Fatalf("location=%q, want /api/v1/realtime/calls/rtc_…", loc)
	}
	answer, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(answer), "v=0") {
		t.Fatalf("answer is not SDP: %q", string(answer))
	}

	// The answer must actually drive the peer to connected.
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatalf("apply answer: %v", err)
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer never reached connected state")
	}

	// The session commits when its control channel opens (shortly after the peer
	// connects), so poll the admin stats until it appears.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var stats struct {
			Sessions []struct {
				SessionID string `json:"session_id"`
			} `json:"sessions"`
		}
		getInto(t, client, ts.URL+"/api/v1/admin/rtc", &stats)
		if len(stats.Sessions) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("admin sessions never reached 1 (got %d)", len(stats.Sessions))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRealtimeCallRejectsEmptyOffer(t *testing.T) {
	ts, _, client := rtcTestServer(t)
	resp := postSDP(t, client, ts.URL+"/api/v1/realtime/calls", "   ")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestRealtimeCallRequiresAuth(t *testing.T) {
	ts, _, _ := rtcTestServer(t)
	resp := postSDP(t, &http.Client{}, ts.URL+"/api/v1/realtime/calls", "v=0")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}

func TestRealtimeCallUnknownConversation404(t *testing.T) {
	ts, _, client := rtcTestServer(t)
	offer, pc := newBrowserOffer(t)
	t.Cleanup(func() { _ = pc.Close() })
	resp := postSDP(t, client, ts.URL+"/api/v1/realtime/calls?conversation_id=cnv_missing", offer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestRealtimeCallForeignConversation403(t *testing.T) {
	ts, st, client := rtcTestServer(t)
	// A conversation owned by a different (real) user.
	if err := st.CreateUser(context.Background(), store.User{
		ID: "usr_other", Username: "other", Role: "user", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	other := store.Conversation{ID: id.New("cnv"), OwnerUserID: "usr_other", Title: "x"}
	evt := events.Event{Source: events.SourceServer, Type: events.TypeSessionCreated, ConversationID: other.ID}
	if err := st.CreateConversationWithEvent(context.Background(), other,
		&store.Event{EventID: id.New("evt"), ConversationID: other.ID, Source: evt.Source, Type: evt.Type, Payload: "{}"}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	offer, pc := newBrowserOffer(t)
	t.Cleanup(func() { _ = pc.Close() })
	resp := postSDP(t, client, ts.URL+"/api/v1/realtime/calls?conversation_id="+other.ID, offer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
}

func TestAdminRTCRequiresAdmin(t *testing.T) {
	ts, _, _ := rtcTestServer(t)
	resp, err := (&http.Client{}).Get(ts.URL + "/api/v1/admin/rtc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}
