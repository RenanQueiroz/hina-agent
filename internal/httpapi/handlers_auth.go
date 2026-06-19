package httpapi

import (
	"errors"
	"net/http"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
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
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(r.Context(), w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(u)})
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
	// Admin-only sample route proving RequireAdmin. A full user-management API
	// arrives with the admin UI in Phase 2.
	admins, err := s.store.CountByRole(r.Context(), "admin")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	users, err := s.store.CountByRole(r.Context(), "user")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"admins": admins, "users": users})
}
