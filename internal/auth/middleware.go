package auth

import (
	"context"
	"net/http"

	"github.com/RenanQueiroz/hina-agent/internal/store"
)

type ctxKey int

const userKey ctxKey = iota

// UserFrom returns the authenticated user injected by RequireUser, if present.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

// RequireUser rejects unauthenticated requests and injects the user into ctx.
func (m *Manager) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := m.userFromRequest(r.Context(), r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// RequireAdmin rejects non-admins (and unauthenticated requests).
func (m *Manager) RequireAdmin(next http.Handler) http.Handler {
	return m.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := UserFrom(r.Context()); !ok || !u.IsAdmin() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}
