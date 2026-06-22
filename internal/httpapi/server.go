// Package httpapi is Hina's HTTP surface: versioned JSON routes, auth/admin
// middleware, health/readiness endpoints, and the SSE event stream. The route
// shape and the event envelope are the v0 wire contract that the web client and
// (later) the WebRTC data channel build on.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agent"
	"github.com/RenanQueiroz/hina-agent/internal/agentauth"
	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/autorun"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/RenanQueiroz/hina-agent/internal/vad"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
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
	loop     *agent.Loop // shared agent turn loop (text mode here; voice reuses it)
	rtc      *rtc.Manager
	tts      tts.Engine
	asr      asr.Engine
	vad      vadStatuser
	logs     *logbuf.Buffer
	log      *slog.Logger
	ready    atomic.Bool
	handler  http.Handler

	// Phase 7 sandbox + secret vault (installed via SetSandbox; nil until then).
	vault      *vault.Vault
	runner     sandbox.Runner
	router     *sandbox.Router
	workspaces *sandbox.WorkspaceManager
	approvals  *approvalRegistry

	// Phase 8 callable agents (installed via SetAgents; nil until then).
	broker      *agentauth.Broker
	agentRouter *sandbox.AgentRouter

	// Phase 9 automations (installed via SetAutomations; nil until then).
	automations *autorun.Service

	turnMu      sync.Mutex      // guards activeTurns + pendingInterrupts
	activeTurns map[string]bool // conversationID -> a turn is in flight
	// pendingInterrupts is a per-conversation happens-before fence: an interrupt mark
	// (barge-in / stop / TTS truncation) reserves it SYNCHRONOUSLY before the interrupt
	// is observable, and every turn entry point (RunTurn / handlePostMessage) waits for
	// it to clear before building context — so a next turn (voice OR a concurrent text
	// POST) can never read the pre-interrupt FULL assistant text.
	pendingInterrupts map[string]int
}

// New builds the server and its handler.
func New(cfg config.Config, st *store.Store, bus *events.Bus, am *auth.Manager, provider llm.Provider, logs *logbuf.Buffer, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, store: st, bus: bus, auth: am, provider: provider, logs: logs, log: log, activeTurns: make(map[string]bool), pendingInterrupts: make(map[string]int)}
	// The agent loop is built with the sandbox tool hook (Phase 7); the hook is a
	// no-op until SetSandbox installs the router AND [sandbox] is enabled, so the
	// default build behaves exactly as before (no provider emits tool calls yet).
	s.loop = agent.NewLoop(provider, s.toolHook)
	s.handler = s.withMiddleware(s.routes())
	return s
}

// SetSandbox installs the Phase 7 secret vault + workspace manager + sbx runner
// (post-construction, like SetTTS). The vault + workspaces power the per-user
// secret/environment endpoints regardless of whether tool execution is enabled;
// the tool Router + approval registry are built only when [sandbox] is enabled,
// so a misconfigured/absent sbx never affects secret/env management. v/ws/runner
// may be nil (those features then report unavailable).
func (s *Server) SetSandbox(v *vault.Vault, ws *sandbox.WorkspaceManager, runner sandbox.Runner) {
	s.vault = v
	s.workspaces = ws
	s.runner = runner
	if !s.cfg.Sandbox.Enabled || runner == nil || ws == nil || v == nil {
		return
	}
	s.approvals = newApprovalRegistry(s.cfg.Sandbox.ApprovalTimeoutOr(5 * time.Minute))
	s.router = sandbox.NewRouter(sandbox.RouterConfig{
		Runner:     runner,
		Secrets:    v,
		Workspaces: ws,
		Store:      s.store,
		Bus:        s.bus,
		Approver:   s.approvals,
		Approval:   s.cfg.Sandbox.ApprovalMode(),
		Limits: sandbox.Limits{
			CPUs:    s.cfg.Sandbox.CPUs,
			Memory:  s.cfg.Sandbox.Memory,
			PIDs:    s.cfg.Sandbox.PIDs,
			Timeout: s.cfg.Sandbox.TimeoutOr(5 * time.Minute),
		},
		QuotaBytes:      s.cfg.Sandbox.WorkspaceQuotaBytes(),
		NetworkIsolated: s.cfg.Sandbox.NetworkIsolated,
		Log:             s.log,
	})
	s.setupAgents(v, ws, runner)
}

// setupAgents builds the Phase 8 callable-agent broker + run router on top of the
// sandbox Router (so an agent run shares the same per-user lock, approval, secret
// redaction, quota, and audit boundary). Only built when [agents] is enabled and the
// sandbox stack is present; otherwise the agent endpoints report unavailable. The
// vault gating (Windows / master-key failure) flows through automatically: when v is
// nil this is never reached.
func (s *Server) setupAgents(v *vault.Vault, ws *sandbox.WorkspaceManager, runner sandbox.Runner) {
	if !s.cfg.Agents.Enabled || s.router == nil {
		return
	}
	// A SHORT per-user credential lock shared by the broker and the AgentRouter's
	// refreshed-state persist. It is deliberately NOT the long run lock, so a logout /
	// key-rotation is prompt (not blocked for an in-flight run's whole duration) while
	// the persist-vs-delete race stays closed.
	credLocks := &sandbox.UserLocker{}
	// In-flight run registry shared with the broker so a logout cancels a run holding
	// (or about to launch with) the revoked credential.
	runs := &sandbox.RunRegistry{}
	s.agentRouter = s.router.NewAgentRouter(sandbox.AgentRouterConfig{
		State:            v,
		Profiles:         s.store,
		LocalEndpoint:    s.cfg.Agents.LocalEndpoint,
		AllowedProviders: s.cfg.Agents.Providers,
		CredLocks:        credLocks,
		Runs:             runs,
		Limits: sandbox.Limits{
			CPUs:    s.cfg.Sandbox.CPUs,
			Memory:  s.cfg.Sandbox.Memory,
			PIDs:    s.cfg.Sandbox.PIDs,
			Timeout: s.cfg.Agents.TimeoutOr(10 * time.Minute),
		},
	})
	s.broker = agentauth.New(agentauth.Config{
		Runner: runner,
		// The auth container gets the same kit + CPU/memory/PID caps as a normal run.
		Factory: agentauth.NewCLIFactory(runner, s.cfg.Sandbox.Kit, sandbox.Limits{
			CPUs:   s.cfg.Sandbox.CPUs,
			Memory: s.cfg.Sandbox.Memory,
			PIDs:   s.cfg.Sandbox.PIDs,
		}),
		Scratch:         ws,
		State:           v,
		Profiles:        s.store,
		NetworkIsolated: s.cfg.Sandbox.NetworkIsolated,
		// The SHORT cred lock (not the run lock): logout/SetKey/login serialize with a
		// run's refreshed-state persist + launch fence without blocking on the run itself.
		Locks: credLocks,
		Runs:  runs,
		Log:   s.log,
	})
}

// beginTurn marks a conversation as having an in-flight turn, returning false if
// one is already active. This serializes turns per conversation so concurrent or
// retried POSTs (multiple tabs, API clients, retries) can't interleave and
// corrupt the durable turn order or the context BuildContext sees. It is
// in-memory (single-process, matching the current deployment); a DB-backed lease
// is the upgrade path if the control plane ever runs multi-process.
func (s *Server) beginTurn(conversationID string) bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.activeTurns[conversationID] {
		return false
	}
	s.activeTurns[conversationID] = true
	return true
}

// endTurn clears the in-flight marker for a conversation.
func (s *Server) endTurn(conversationID string) {
	s.turnMu.Lock()
	delete(s.activeTurns, conversationID)
	s.turnMu.Unlock()
}

// reserveInterrupt registers an in-flight interrupt mark for a conversation and returns
// a release. The caller (a barge-in / stop / TTS-truncation path) reserves SYNCHRONOUSLY
// before the interrupt is observable, performs the durable mark, then releases — so a
// turn entry point that awaitInterrupts can't read context until the mark commits/fails.
func (s *Server) reserveInterrupt(conversationID string) func() {
	s.turnMu.Lock()
	s.pendingInterrupts[conversationID]++
	s.turnMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.turnMu.Lock()
			if s.pendingInterrupts[conversationID] <= 1 {
				delete(s.pendingInterrupts, conversationID)
			} else {
				s.pendingInterrupts[conversationID]--
			}
			s.turnMu.Unlock()
		})
	}
}

// BeginInterrupt implements rtc.AgentService: reserve the interrupt fence (see
// reserveInterrupt). The live loop calls this synchronously, BEFORE a barge-in/stop is
// observable, then MarkTurnInterrupted, then the returned release.
func (s *Server) BeginInterrupt(conversationID string) func() {
	return s.reserveInterrupt(conversationID)
}

// awaitInterrupts blocks until no interrupt mark is in flight for the conversation,
// returning true when the fence actually CLEARED. It returns FALSE if ctx ended first
// (client disconnect / a stuck mark past the deadline): the caller must then abort —
// NOT build context or persist a turn — since the pre-interrupt full reply might still
// be the durable state. Turn entry points call it after claiming the turn lock and
// BEFORE building context (the happens-before edge against a stale next turn).
func (s *Server) awaitInterrupts(ctx context.Context, conversationID string) bool {
	for {
		s.turnMu.Lock()
		n := s.pendingInterrupts[conversationID]
		s.turnMu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// SetAutomations installs the Phase 9 automation service (post-construction). May be
// nil ([automations] off); the automation endpoints then report unavailable. The
// service owns the durable scheduler — the caller Starts/Stops it for lifecycle.
func (s *Server) SetAutomations(svc *autorun.Service) { s.automations = svc }

// Automations returns the installed automation service (nil when off), so the server
// owner can Start/Stop the scheduler around the HTTP lifecycle.
func (s *Server) Automations() *autorun.Service { return s.automations }

// AgentRouter exposes the Phase 8 callable-agent router (nil when agents are off), so
// the automation service can run agent_cli steps through the same hardened boundary.
func (s *Server) AgentRouter() *sandbox.AgentRouter { return s.agentRouter }

// AutomationEligibility assembles the runtime facts that decide whether an automation
// may be enabled for a user (server gates + the owner's secrets/agents). Exposed so
// the automation service can run the enable gate without duplicating catalog logic.
func (s *Server) AutomationEligibility(ctx context.Context, userID string) (automation.Eligibility, error) {
	return s.automationEligibility(ctx, userID)
}

// SetReady marks readiness (migrations done, config valid).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// SetRealtime installs the WebRTC manager (post-construction, like SetReady).
// When unset, the realtime routes respond 503.
func (s *Server) SetRealtime(m *rtc.Manager) { s.rtc = m }

// SetTTS installs the local TTS engine (post-construction). May be nil (TTS off);
// the admin runtime view reports it as disabled/unavailable accordingly.
func (s *Server) SetTTS(e tts.Engine) { s.tts = e }

// SetASR installs the local ASR engine (post-construction). May be nil (ASR off);
// the admin runtime view reports it as disabled/unavailable accordingly.
func (s *Server) SetASR(e asr.Engine) { s.asr = e }

// vadStatuser is the subset of the VAD engine the admin runtime view needs. An
// interface keeps httpapi decoupled from the concrete engine and testable.
type vadStatuser interface{ Status() vad.Status }

// SetVAD installs the local VAD engine (post-construction). May be nil (live voice
// off); the admin runtime view reports it as disabled/unavailable accordingly.
func (s *Server) SetVAD(e vadStatuser) {
	if e == nil {
		return
	}
	s.vad = e
}

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

	// WebRTC signaling (mirrors OpenAI's application/sdp /realtime/calls).
	mux.Handle("POST /api/v1/realtime/calls", s.requireUser(s.handleRealtimeCall))
	// Speak text into the caller's active live session (server-driven TTS).
	mux.Handle("POST /api/v1/realtime/speak", s.requireUser(s.handleRealtimeSpeak))

	// Tool-call approval decisions (per conversation; owner only).
	mux.Handle("POST /api/v1/conversations/{id}/tool-approvals/{callID}", s.requireUser(s.handleToolApproval))

	// Sandbox Environment + secret vault (per user).
	mux.Handle("GET /api/v1/sandbox/environment", s.requireUser(s.handleGetSandboxEnvironment))
	mux.Handle("PUT /api/v1/sandbox/environment", s.requireUser(s.handlePutSandboxEnvironment))
	mux.Handle("GET /api/v1/sandbox/secrets", s.requireUser(s.handleListSecrets))
	mux.Handle("POST /api/v1/sandbox/secrets", s.requireUser(s.handleCreateSecret))
	mux.Handle("PUT /api/v1/sandbox/secrets/{id}", s.requireUser(s.handleUpdateSecret))
	mux.Handle("DELETE /api/v1/sandbox/secrets/{id}", s.requireUser(s.handleDeleteSecret))

	// Callable agents (Phase 8): catalog, per-provider profile (key/logout), and the
	// interactive login broker (start/stream/input/cancel).
	mux.Handle("GET /api/v1/agents", s.requireUser(s.handleAgentCatalog))
	mux.Handle("POST /api/v1/agents/{provider}/key", s.requireUser(s.handleSetAgentKey))
	mux.Handle("DELETE /api/v1/agents/{provider}", s.requireUser(s.handleAgentLogout))
	mux.Handle("POST /api/v1/agents/{provider}/login", s.requireUser(s.handleStartAgentLogin))
	mux.Handle("GET /api/v1/agents/login/{sessionID}/events", s.requireUser(s.handleAgentLoginEvents))
	mux.Handle("POST /api/v1/agents/login/{sessionID}/input", s.requireUser(s.handleAgentLoginInput))
	mux.Handle("POST /api/v1/agents/login/{sessionID}/cancel", s.requireUser(s.handleAgentLoginCancel))

	// Automations (Phase 9): per-user CRUD, validate, enable, manual run, run history,
	// artifacts, the builder catalog, and LLM-assisted drafting.
	mux.Handle("GET /api/v1/automations", s.requireUser(s.handleListAutomations))
	mux.Handle("POST /api/v1/automations", s.requireUser(s.handleCreateAutomation))
	mux.Handle("GET /api/v1/automations/meta", s.requireUser(s.handleAutomationMeta))
	mux.Handle("POST /api/v1/automations/validate", s.requireUser(s.handleValidateAutomation))
	mux.Handle("POST /api/v1/automations/assist", s.requireUser(s.handleAssistAutomation))
	mux.Handle("GET /api/v1/automations/{id}", s.requireUser(s.handleGetAutomation))
	mux.Handle("PUT /api/v1/automations/{id}", s.requireUser(s.handleUpdateAutomation))
	mux.Handle("DELETE /api/v1/automations/{id}", s.requireUser(s.handleDeleteAutomation))
	mux.Handle("GET /api/v1/automations/{id}/export", s.requireUser(s.handleExportAutomation))
	mux.Handle("POST /api/v1/automations/{id}/enabled", s.requireUser(s.handleSetAutomationEnabled))
	mux.Handle("POST /api/v1/automations/{id}/run", s.requireUser(s.handleRunAutomation))
	mux.Handle("GET /api/v1/automations/{id}/runs", s.requireUser(s.handleListAutomationRuns))
	// Run + artifact lookups are top-level paths so they don't collide with the
	// /automations/{id}/... space (Go's ServeMux rejects ambiguous wildcard overlaps).
	mux.Handle("GET /api/v1/automation-runs/{runID}", s.requireUser(s.handleGetAutomationRun))
	mux.Handle("GET /api/v1/automation-artifacts/{artifactID}", s.requireUser(s.handleDownloadArtifact))

	// Admin routes.
	mux.Handle("GET /api/v1/admin/users", s.requireAdmin(s.handleListUsers))
	mux.Handle("GET /api/v1/admin/llm", s.requireAdmin(s.handleAdminLLM))
	mux.Handle("GET /api/v1/admin/runtime", s.requireAdmin(s.handleAdminRuntime))
	mux.Handle("GET /api/v1/admin/logs", s.requireAdmin(s.handleAdminLogs))
	mux.Handle("GET /api/v1/admin/rtc", s.requireAdmin(s.handleAdminRTC))
	mux.Handle("GET /api/v1/admin/sandbox", s.requireAdmin(s.handleAdminSandbox))
	mux.Handle("GET /api/v1/admin/agents", s.requireAdmin(s.handleAdminAgents))

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
	return s.recoverMW(s.logMW(s.csrfMW(next)))
}

// csrfMW protects cookie-authenticated state changes: for unsafe methods it
// requires the Origin (or Referer) to be same-origin. Browsers always send
// Origin on cross-origin unsafe requests, so a forged cross-site POST that
// rides the session cookie is rejected; requests with neither header (non-browser
// API clients, which don't carry the cookie automatically) are allowed.
func (s *Server) csrfMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r) {
				writeErr(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true // no browser origin header -> not a cookie-CSRF vector
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
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
