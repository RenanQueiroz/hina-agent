package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/RenanQueiroz/hina-agent/internal/agentauth"
	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

// --- Callable-agent catalog + profiles (per user) ---

func (s *Server) handleAgentCatalog(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	profiles, err := s.store.ListAgentProfilesByUser(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	byProvider := make(map[string]string, len(profiles)) // provider -> status
	authTypeOf := make(map[string]string, len(profiles))
	for _, p := range profiles {
		byProvider[p.Provider] = p.Status
		authTypeOf[p.Provider] = p.AuthType
	}

	// Load the user's Sandbox Environment so eligibility reflects the SAME per-user
	// tool allow-list the AgentRouter enforces (a removed agent tool is not runnable).
	env, err := sandbox.LoadEnvironment(r.Context(), s.store, u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	cat := wire.AgentCatalog{
		Enabled:         s.cfg.Agents.Enabled && s.cfg.Sandbox.Enabled,
		NetworkIsolated: s.cfg.Sandbox.NetworkIsolated,
		BrowserAuth:     s.broker != nil && s.broker.BrowserAuthAvailable(),
	}
	for _, capability := range agentcli.Capabilities() {
		provider := string(capability.Provider)
		if !s.cfg.Agents.ProviderAllowed(provider) {
			continue
		}
		status := byProvider[provider]
		configured := status == "authenticated"
		info := wire.AgentInfo{
			Provider:           provider,
			DisplayName:        capability.DisplayName,
			AuthTypes:          authTypeStrings(capability.AuthTypes),
			BrowserAuth:        capability.BrowserAuth,
			LocalOnly:          capability.LocalOnly,
			ToolName:           capability.ToolName,
			Configured:         configured,
			ConfiguredAuthType: authTypeOf[provider],
			Status:             status,
		}
		info.Runnable, info.Reason = s.agentRunnable(capability.Provider, configured, env.ToolAllowed(capability.ToolName))
		cat.Agents = append(cat.Agents, info)
	}
	writeJSON(w, http.StatusOK, cat)
}

// agentRunnable reports whether a provider is eligible to be invoked right now, with
// the reason for the first failing gate (so the UI can explain why it's disabled).
// envAllowed is whether the user's Sandbox Environment permits this agent's tool.
func (s *Server) agentRunnable(provider agentcli.Provider, configured, envAllowed bool) (bool, string) {
	if !s.cfg.Agents.Enabled || !s.cfg.Sandbox.Enabled {
		return false, "callable agents are disabled on this server"
	}
	if s.agentRouter == nil || s.runner == nil || !s.runner.Available() {
		return false, "the sandbox runtime is unavailable"
	}
	if !s.cfg.Sandbox.NetworkIsolated {
		return false, "agent runs require [sandbox] network_isolated=true (controlled egress)"
	}
	if provider == agentcli.ProviderPi && s.cfg.Agents.LocalEndpoint == "" {
		return false, "Pi needs the managed local LLM backend (Phase 11)"
	}
	if !configured {
		return false, "not configured — authenticate this agent first"
	}
	if !envAllowed {
		return false, "disabled in your Sandbox Environment tool policy"
	}
	return true, ""
}

func (s *Server) handleSetAgentKey(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	provider, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	if !s.cfg.Agents.ProviderAllowed(provider) {
		writeErr(w, http.StatusForbidden, "this agent is not permitted by server policy")
		return
	}
	var body wire.SetAgentKey
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	at := agentcli.AuthType(body.AuthType)
	if at != agentcli.AuthAPIKey && at != agentcli.AuthOAuthToken {
		writeErr(w, http.StatusBadRequest, "auth_type must be api_key or oauth_token")
		return
	}
	if body.Value == "" {
		writeErr(w, http.StatusBadRequest, "value is required")
		return
	}
	if err := s.broker.SetKey(r.Context(), u.ID, provider, at, body.Value); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentLogout(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	provider, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	if err := s.broker.Logout(r.Context(), u.ID, provider); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Interactive login broker ---

func (s *Server) handleStartAgentLogin(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	provider, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	if !s.cfg.Agents.ProviderAllowed(provider) {
		writeErr(w, http.StatusForbidden, "this agent is not permitted by server policy")
		return
	}
	var body wire.StartAgentLogin
	_ = decodeJSON(w, r, &body) // body optional; default browser flow
	sessionID, err := s.broker.StartLogin(u.ID, provider, body.DeviceAuth)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wire.AgentLoginStarted{SessionID: sessionID})
}

func (s *Server) handleAgentLoginEvents(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, unsub, err := s.broker.Subscribe(r.PathValue("sessionID"), u.ID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such login session")
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case f, open := <-ch:
			if !open {
				return
			}
			writeLoginFrame(w, flusher, f)
		}
	}
}

func (s *Server) handleAgentLoginInput(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	var body wire.AgentLoginInput
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.broker.WriteInput(r.PathValue("sessionID"), u.ID, body.Data); err != nil {
		writeErr(w, http.StatusNotFound, "no such login session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentLoginCancel(w http.ResponseWriter, r *http.Request) {
	if s.broker == nil {
		writeErr(w, http.StatusServiceUnavailable, "callable agents are unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	if err := s.broker.Cancel(r.PathValue("sessionID"), u.ID); err != nil {
		writeErr(w, http.StatusNotFound, "no such login session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Admin coarse status ---

func (s *Server) handleAdminAgents(w http.ResponseWriter, r *http.Request) {
	out := wire.AdminAgents{
		Available:   s.cfg.Agents.Enabled && s.cfg.Sandbox.Enabled && s.runner != nil && s.runner.Available(),
		BrowserAuth: s.broker != nil && s.broker.BrowserAuthAvailable(),
	}
	if !s.cfg.Agents.Enabled {
		out.Reason = "callable agents are disabled ([agents] enabled=false)"
	} else if !s.cfg.Sandbox.Enabled {
		out.Reason = "callable agents need [sandbox] enabled"
	}
	profiles, err := s.store.ListAllAgentProfiles(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Username
	}
	for _, p := range profiles {
		out.Profiles = append(out.Profiles, wire.AdminAgentProfile{
			UserID: p.UserID, Username: names[p.UserID], Provider: p.Provider,
			AuthType: p.AuthType, Status: p.Status, UpdatedAt: p.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// agentProvider extracts + validates the {provider} path value.
func (s *Server) agentProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	provider := r.PathValue("provider")
	if _, ok := agentcli.Get(agentcli.Provider(provider)); !ok {
		writeErr(w, http.StatusNotFound, "unknown agent")
		return "", false
	}
	return provider, true
}

func writeLoginFrame(w http.ResponseWriter, flusher http.Flusher, f agentauth.Frame) {
	wf := wire.AgentLoginFrame{Type: f.Type, Text: f.Text, OK: f.OK, Error: f.Error}
	if f.Hint != nil {
		wf.Hint = &wire.AgentLoginHint{Kind: string(f.Hint.Kind), Value: f.Hint.Value}
	}
	b, err := json.Marshal(wf)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func authTypeStrings(types []agentcli.AuthType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, string(t))
	}
	return out
}
