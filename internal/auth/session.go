package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

const (
	cookieName = "hina_session"
	sessionTTL = 7 * 24 * time.Hour
)

// Manager issues and resolves login sessions backed by the store.
type Manager struct {
	store  *store.Store
	secure bool // set the Secure cookie flag (true when serving over TLS)
}

// NewManager builds a session manager. Pass secure=true under TLS.
func NewManager(st *store.Store, secure bool) *Manager {
	return &Manager{store: st, secure: secure}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Login creates a session for userID and writes the session cookie.
func (m *Manager) Login(ctx context.Context, w http.ResponseWriter, userID string) error {
	token := id.Token(32)
	now := time.Now().UTC()
	sess := store.AuthSession{
		ID:        id.New("ses"),
		UserID:    userID,
		TokenHash: hashToken(token),
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
	}
	if err := m.store.CreateAuthSession(ctx, sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
	return nil
}

// Logout deletes the current session (if any) and clears the cookie.
func (m *Manager) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if sess, err := m.store.GetAuthSessionByTokenHash(ctx, hashToken(c.Value)); err == nil {
			_ = m.store.DeleteAuthSession(ctx, sess.ID)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	})
}

// userFromRequest resolves the authenticated user from the session cookie.
func (m *Manager) userFromRequest(ctx context.Context, r *http.Request) (store.User, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return store.User{}, store.ErrNotFound
	}
	sess, err := m.store.GetAuthSessionByTokenHash(ctx, hashToken(c.Value))
	if err != nil {
		return store.User{}, err
	}
	return m.store.GetUserByID(ctx, sess.UserID)
}
