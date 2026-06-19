package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	u, err := auth.Authenticate(r.Context(), s.store, body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeErr(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		s.log.Error("authenticate", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.auth.Login(r.Context(), w, u.ID); err != nil {
		s.log.Error("login", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, wire.LoginResponse{User: toUserView(u)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(r.Context(), w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	writeJSON(w, http.StatusOK, wire.LoginResponse{User: toUserView(u)})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(body.NewPassword) < 8 {
		writeErr(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if _, err := auth.Authenticate(r.Context(), s.store, u.Username, body.CurrentPassword); err != nil {
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := auth.ChangePassword(r.Context(), s.store, u.ID, body.NewPassword); err != nil {
		s.log.Error("change password", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]wire.AdminUser, 0, len(users))
	for _, u := range users {
		out = append(out, wire.AdminUser{
			ID: u.ID, Username: u.Username, Role: u.Role, Status: u.Status,
			MustChangePassword: u.MustChangePassword, CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, wire.AdminUserList{Users: out})
}

func (s *Server) handleAdminLLM(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, wire.AdminLLMInfo{
		Provider: s.provider.Name(),
		Model:    s.cfg.LLM.Model,
		BaseURL:  s.cfg.LLM.BaseURL,
	})
}

func (s *Server) handleAdminRuntime(w http.ResponseWriter, _ *http.Request) {
	// Stub: runtime/backend status (local STT/LLM/TTS, sandbox) fills in across
	// later phases.
	writeJSON(w, http.StatusOK, map[string]any{
		"llm_provider": s.provider.Name(),
		"note":         "runtime status expands in later phases",
	})
}

// handleAdminLogs streams server logs (recent + live) over SSE from the
// in-memory ring buffer. Per-backend log views fill in with those backends.
func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.logs.Subscribe()
	defer cancel()
	for _, line := range s.logs.Recent() {
		fmt.Fprintf(w, "data: %s\n\n", sseSafe(line))
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case line, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", sseSafe(line))
			flusher.Flush()
		}
	}
}

// sseSafe collapses newlines so a log line stays a single SSE data frame.
func sseSafe(s string) string { return strings.ReplaceAll(s, "\n", " ") }
