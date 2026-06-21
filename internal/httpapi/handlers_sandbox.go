package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

// --- Sandbox Environment settings (per user) ---

func (s *Server) handleGetSandboxEnvironment(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	env, err := sandbox.LoadEnvironment(r.Context(), s.store, u.ID)
	if err != nil {
		s.log.Error("load sandbox environment", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toWireEnvironment(env))
}

func (s *Server) handlePutSandboxEnvironment(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var body wire.SandboxEnvironment
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	env := fromWireEnvironment(body).Normalize()
	if err := env.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	data := string(raw)
	if err := s.store.UpsertSandboxState(r.Context(), store.SandboxState{
		ID: id.New("sbx"), UserID: u.ID, Kind: sandbox.StateKind, Data: data,
	}); err != nil {
		s.log.Error("save sandbox environment", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toWireEnvironment(env))
}

// --- Secret vault (per user) ---

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	metas, err := s.vault.List(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]wire.Secret, 0, len(metas))
	for _, m := range metas {
		out = append(out, toWireSecret(m))
	}
	writeJSON(w, http.StatusOK, wire.SecretList{Secrets: out})
}

func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Value       string `json:"value"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Name == "" || body.Value == "" {
		writeErr(w, http.StatusBadRequest, "name and value are required")
		return
	}
	meta, err := s.vault.Put(r.Context(), u.ID, body.Name, body.Description, body.Value)
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "a secret with that name already exists")
		return
	}
	if err != nil {
		s.log.Error("create secret", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toWireSecret(meta))
}

func (s *Server) handleUpdateSecret(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	var body struct {
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	err := s.vault.UpdateDescription(r.Context(), u.ID, r.PathValue("id"), body.Description)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, http.StatusServiceUnavailable, "secret vault unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	err := s.vault.Delete(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Tool-call approval decisions ---

func (s *Server) handleToolApproval(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		writeErr(w, http.StatusServiceUnavailable, "tool approvals unavailable")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	convID := r.PathValue("id")
	// Only the conversation's owner may decide its tool approvals.
	conv, err := s.store.GetConversation(r.Context(), convID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && conv.OwnerUserID != u.ID) {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	var body struct {
		Approve bool `json:"approve"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !s.approvals.Decide(r.PathValue("callID"), convID, body.Approve) {
		writeErr(w, http.StatusNotFound, "no pending tool call with that id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Admin sandbox view ---

func (s *Server) handleAdminSandbox(w http.ResponseWriter, r *http.Request) {
	out := wire.AdminSandbox{Runtime: wire.SandboxRuntime{
		Enabled:  s.cfg.Sandbox.Enabled,
		Pinned:   sandbox.PinnedVersion,
		Approval: s.cfg.Sandbox.ApprovalMode(),
	}}
	if s.runner != nil {
		st := s.runner.Status()
		out.Runtime.Available = st.Available
		out.Runtime.Version = st.Version
		out.Runtime.Path = st.Path
		out.Runtime.Reason = st.Reason
	}

	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, u := range users {
		usage := wire.SandboxUserUsage{UserID: u.ID, Username: u.Username}
		if s.workspaces != nil {
			if b, err := s.workspaces.UserUsageBytes(u.ID); err == nil {
				usage.WorkspaceBytes = b
			}
		}
		if n, err := s.store.CountSandboxRunsByUser(r.Context(), u.ID); err == nil {
			usage.RunCount = n
		}
		out.Users = append(out.Users, usage)
	}

	runs, err := s.store.ListSandboxRuns(r.Context(), "", 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, run := range runs {
		out.Runs = append(out.Runs, wire.SandboxRunInfo{
			ID: run.ID, UserID: run.UserID, ConversationID: run.ConversationID,
			Tool: run.Tool, Decision: run.Decision, ExitCode: run.ExitCode,
			DurationMs: run.DurationMs, Command: run.Command, Error: run.Error,
			CreatedAt: run.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- conversions ---

func toWireEnvironment(env sandbox.Environment) wire.SandboxEnvironment {
	out := wire.SandboxEnvironment{
		AllowedTools:   env.AllowedTools,
		AvailableTools: sandbox.BuiltinTools,
		WritableMounts: env.WritableMounts,
		Network:        wire.SandboxNetworkPolicy{Default: env.Network.Default},
	}
	for _, m := range env.MCPServers {
		out.MCPServers = append(out.MCPServers, wire.SandboxMCPServer{Name: m.Name, URL: m.URL})
	}
	for _, rule := range env.Network.Allow {
		out.Network.Allow = append(out.Network.Allow, wire.SandboxNetworkRule{Host: rule.Host, Port: rule.Port})
	}
	for _, g := range env.SecretGrants {
		out.SecretGrants = append(out.SecretGrants, wire.SandboxSecretGrant{SecretID: g.SecretID, EnvName: g.EnvName})
	}
	return out
}

func fromWireEnvironment(in wire.SandboxEnvironment) sandbox.Environment {
	env := sandbox.Environment{
		AllowedTools:   in.AllowedTools,
		WritableMounts: in.WritableMounts,
		Network:        sandbox.NetworkPolicy{Default: in.Network.Default},
	}
	for _, m := range in.MCPServers {
		env.MCPServers = append(env.MCPServers, sandbox.MCPServer{Name: m.Name, URL: m.URL})
	}
	for _, rule := range in.Network.Allow {
		env.Network.Allow = append(env.Network.Allow, sandbox.NetworkRule{Host: rule.Host, Port: rule.Port})
	}
	for _, g := range in.SecretGrants {
		env.SecretGrants = append(env.SecretGrants, sandbox.SecretGrant{SecretID: g.SecretID, EnvName: g.EnvName})
	}
	return env
}

func toWireSecret(m store.SecretMeta) wire.Secret {
	return wire.Secret{ID: m.ID, Name: m.Name, Description: m.Description, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
}
