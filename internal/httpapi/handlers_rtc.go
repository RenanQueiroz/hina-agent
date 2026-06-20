package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

// maxOfferBytes caps the SDP offer body. Real offers are a few KB; this bounds a
// hostile upload without rejecting legitimate candidates.
const maxOfferBytes = 256 * 1024

// handleRealtimeCall is the WebRTC signaling endpoint, mirroring OpenAI's
// /realtime/calls application/sdp contract: the browser POSTs its SDP offer and
// gets the Pion answer back as plain text. Authenticated; one active talk
// session per user (a new call supersedes the old). An optional ?conversation_id
// ties the session to a conversation the caller owns.
func (s *Server) handleRealtimeCall(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if s.rtc == nil {
		writeErr(w, http.StatusServiceUnavailable, "realtime is not enabled")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOfferBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "sdp offer too large")
		return
	}
	offer := string(body)
	if strings.TrimSpace(offer) == "" {
		writeErr(w, http.StatusBadRequest, "empty SDP offer")
		return
	}

	// Optionally bind the session to a conversation the caller owns.
	convID := r.URL.Query().Get("conversation_id")
	if convID != "" {
		conv, err := s.store.GetConversation(r.Context(), convID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "conversation not found")
			} else {
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
			return
		}
		if conv.OwnerUserID != u.ID {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	answer, sessionID, err := s.rtc.Answer(r.Context(), u.ID, convID, offer)
	if err != nil {
		if errors.Is(err, rtc.ErrManagerClosed) {
			writeErr(w, http.StatusServiceUnavailable, "server shutting down")
			return
		}
		s.log.Error("realtime answer", "user", u.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "failed to establish call")
		return
	}

	// Mirror OpenAI: the call id is discoverable via the Location header.
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/v1/realtime/calls/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	// The session is negotiated but PENDING. It commits itself only when its
	// control channel opens (proof the client applied the answer and connected),
	// or it times out and rolls back — so a buffered/lost answer can never close
	// the user's existing active session. If the write fails outright we don't
	// wait for the timeout; roll it back now.
	if _, werr := io.WriteString(w, answer); werr != nil {
		s.log.Warn("realtime answer not delivered; rolling back session", "session", sessionID, "err", werr)
		s.rtc.CloseSession(sessionID)
	}
}

// handleAdminRTC reports active live sessions and their loss/jitter/RTT metrics.
func (s *Server) handleAdminRTC(w http.ResponseWriter, _ *http.Request) {
	out := wire.RTCStats{Sessions: []wire.RTCSessionStats{}}
	if s.rtc != nil {
		for _, st := range s.rtc.Stats() {
			out.Sessions = append(out.Sessions, rtcStatsView(st))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func rtcStatsView(st rtc.SessionStats) wire.RTCSessionStats {
	return wire.RTCSessionStats{
		SessionID:       st.SessionID,
		UserID:          st.UserID,
		ConversationID:  st.ConversationID,
		Mode:            st.Mode,
		UptimeMs:        st.UptimeMs,
		RTPPacketsIn:    st.RTPPacketsIn,
		DecodeErrors:    st.DecodeErrors,
		FramesOut:       st.FramesOut,
		BytesOut:        st.BytesOut,
		FramesDropped:   st.FramesDropped,
		Interrupts:      st.Interrupts,
		PlayedMs:        st.PlayedMs,
		CaptureMs:       st.CaptureMs,
		AppRTTMicros:    st.AppRTTMicros,
		PacketsReceived: st.PacketsReceived,
		PacketsLost:     st.PacketsLost,
		JitterSeconds:   st.JitterSeconds,
	}
}
