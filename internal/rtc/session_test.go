package rtc

import (
	"encoding/json"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/pion/webrtc/v4"
)

// TestDisconnectGraceGeneration verifies the disconnect-grace timer is tagged
// with an episode generation: a stale episode (after a recovery/re-disconnect)
// must not tear the session down, so ICE flapping can't drop a recoverable call.
func TestDisconnectGraceGeneration(t *testing.T) {
	var s Session
	gen := s.disconnectGen.Add(1) // disconnect episode 1
	if !s.shouldCloseAfterGrace(gen, webrtc.PeerConnectionStateDisconnected) {
		t.Fatal("same-episode, still-disconnected should close after grace")
	}
	s.disconnectGen.Add(1) // recovery (Connected) bumps the generation
	if s.shouldCloseAfterGrace(gen, webrtc.PeerConnectionStateDisconnected) {
		t.Fatal("a stale disconnect episode must not close after recovery/re-disconnect")
	}
	cur := s.disconnectGen.Load()
	if s.shouldCloseAfterGrace(cur, webrtc.PeerConnectionStateConnected) {
		t.Fatal("a recovered (connected) connection must not close")
	}
}

// Only the first audio track is claimed; extras are refused so no second
// readLoop ever races the shared inbound DSP state.
func TestClaimAudioTrackOnlyOnce(t *testing.T) {
	s, _ := newDSPSession()
	if !s.claimAudioTrack() {
		t.Fatal("first audio track should be claimed")
	}
	if s.claimAudioTrack() {
		t.Fatal("second audio track must be refused")
	}
}

// An oversized control message is dropped before json.Unmarshal — no panic, no
// state change — so an authenticated peer can't force large allocations.
func TestOversizedControlMessageDropped(t *testing.T) {
	s, _ := newDSPSession()
	before := s.out.mode()
	s.handleControlMessage(webrtc.DataChannelMessage{
		IsString: true,
		Data:     make([]byte, maxControlBytes+1),
	})
	if s.out.mode() != before {
		t.Fatal("oversized control message must not change state")
	}
}

// A valid ModeChanged control message selects the tone source.
func TestControlMessageSelectsMode(t *testing.T) {
	s, _ := newDSPSession()
	e, err := events.New(events.SourceClient, events.TypeModeChanged, "", "", "", map[string]string{"mode": ModeTone})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s.handleControlMessage(webrtc.DataChannelMessage{IsString: true, Data: raw})
	if got := s.out.mode(); got != ModeTone {
		t.Fatalf("mode=%q, want tone", got)
	}
	s.out.stop() // stop the pacer goroutine started by start()
}
