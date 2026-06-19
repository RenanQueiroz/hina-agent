// Package httpapi is Hina's HTTP surface: versioned JSON routes, auth/admin
// middleware, health/readiness endpoints, and the SSE event stream. The route
// shape and the event envelope are the v0 wire contract that the web client and
// (later) the WebRTC data channel build on.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
	webui "github.com/RenanQueiroz/hina-agent/web"
)

// Server holds the HTTP dependencies and the built handler.
type Server struct {
	cfg      config.Config
	store    *store.Store
	bus      *events.Bus
	auth     *auth.Manager
	provider llm.Provider
	log      *slog.Logger
	ready    atomic.Bool
	handler  http.Handler
}

// New builds the server and its handler.
func New(cfg config.Config, st *store.Store, bus *events.Bus, am *auth.Manager, provider llm.Provider, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, store: st, bus: bus, auth: am, provider: provider, log: log}
	s.handler = s.withMiddleware(s.routes())
	return s
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// SetReady marks readiness (migrations done, config valid).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	// Authenticated user routes.
	mux.Handle("POST /api/v1/auth/logout", s.requireUser(s.handleLogout))
	mux.Handle("GET /api/v1/auth/me", s.requireUser(s.handleMe))
	mux.Handle("POST /api/v1/auth/change-password", s.requireUser(s.handleChangePassword))
	mux.Handle("GET /api/v1/conversations", s.requireUser(s.handleListConversations))
	mux.Handle("POST /api/v1/conversations", s.requireUser(s.handleCreateConversation))
	mux.Handle("GET /api/v1/conversations/{id}", s.requireUser(s.handleGetConversation))
	mux.Handle("GET /api/v1/conversations/{id}/turns", s.requireUser(s.handleListTurns))
	mux.Handle("POST /api/v1/conversations/{id}/messages", s.requireUser(s.handlePostMessage))
	mux.Handle("GET /api/v1/conversations/{id}/events", s.requireUser(s.handleEvents))
	mux.Handle("GET /api/v1/config", s.requireUser(s.handleConfig))

	// Admin routes.
	mux.Handle("GET /api/v1/admin/users", s.requireAdmin(s.handleListUsers))
	mux.Handle("GET /api/v1/admin/llm", s.requireAdmin(s.handleAdminLLM))
	mux.Handle("GET /api/v1/admin/runtime", s.requireAdmin(s.handleAdminRuntime))

	// SPA: the embedded web client serves all remaining paths (more specific
	// /healthz, /readyz, /api/v1/* patterns take precedence).
	mux.Handle("/", webui.Handler())

	return mux
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, wire.ConfigInfo{
		AgentName:   s.cfg.Agent.Name,
		LLMProvider: s.provider.Name(),
	})
}

func (s *Server) requireUser(h http.HandlerFunc) http.Handler  { return s.auth.RequireUser(h) }
func (s *Server) requireAdmin(h http.HandlerFunc) http.Handler { return s.auth.RequireAdmin(h) }

// --- health ---

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- middleware ---

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.recoverMW(s.logMW(next))
}

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "err", rec, "path", r.URL.Path)
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := id.New("req")
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.log.Info("http",
			"req_id", reqID, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "dur_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush exposes the underlying flusher for SSE.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB cap
	return json.NewDecoder(r.Body).Decode(v)
}

func toUserView(u store.User) wire.User {
	return wire.User{ID: u.ID, Username: u.Username, Role: u.Role, MustChangePassword: u.MustChangePassword}
}
