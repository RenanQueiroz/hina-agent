package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

// automationsReady guards every automation endpoint: the service must be installed
// ([automations] enabled + the sandbox stack present).
func (s *Server) automationsReady(w http.ResponseWriter) bool {
	if s.automations == nil {
		writeErr(w, http.StatusServiceUnavailable, "automations are not enabled on this server")
		return false
	}
	return true
}

func (s *Server) handleListAutomations(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	rows, err := s.automations.List(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := wire.AutomationList{Automations: make([]wire.AutomationSummary, 0, len(rows))}
	for _, row := range rows {
		out.Automations = append(out.Automations, toAutomationSummary(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	def, ok := s.decodeDefinition(w, r)
	if !ok {
		return
	}
	row, err := s.automations.Create(r.Context(), u.ID, def)
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toAutomationDetail(row))
}

func (s *Server) handleGetAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	row, err := s.automations.Get(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAutomationDetail(row))
}

func (s *Server) handleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	def, ok := s.decodeDefinition(w, r)
	if !ok {
		return
	}
	row, err := s.automations.Update(r.Context(), u.ID, r.PathValue("id"), def)
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAutomationDetail(row))
}

func (s *Server) handleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	if err := s.automations.Delete(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExportAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	row, err := s.automations.Get(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	def, perr := automation.Parse([]byte(row.Definition))
	if perr != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	body, err := def.Export()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"automation.json\"")
	_, _ = w.Write(body)
}

func (s *Server) handleSetAutomationEnabled(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	var body wire.SetAutomationEnabled
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	row, err := s.automations.SetEnabled(r.Context(), u.ID, r.PathValue("id"), body.Enabled)
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAutomationDetail(row))
}

func (s *Server) handleRunAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	runID, err := s.automations.TriggerNow(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, wire.TriggerAutomationResponse{RunID: runID})
}

func (s *Server) handleListAutomationRuns(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	runs, err := s.automations.ListRuns(r.Context(), u.ID, r.PathValue("id"), 100)
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	out := wire.AutomationRunList{Runs: make([]wire.AutomationRunSummary, 0, len(runs))}
	for _, run := range runs {
		out.Runs = append(out.Runs, toRunSummary(run))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetAutomationRun(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	run, err := s.automations.GetRun(r.Context(), u.ID, r.PathValue("runID"))
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	arts, _ := s.automations.ListArtifacts(r.Context(), u.ID, run.ID)
	detail := wire.AutomationRunDetail{
		ID: run.ID, AutomationID: run.AutomationID, Status: run.Status, Trigger: run.Trigger,
		Error: run.Error, StartedAt: timePtr(run.StartedAt), FinishedAt: timePtr(run.FinishedAt),
		DurationMs: durationMs(run.StartedAt, run.FinishedAt), Record: json.RawMessage(run.Record),
		Artifacts: make([]wire.AutomationArtifactInfo, 0, len(arts)),
	}
	for _, a := range arts {
		detail.Artifacts = append(detail.Artifacts, wire.AutomationArtifactInfo{
			ID: a.ID, Name: a.Name, StepID: a.StepID, Size: a.Size, CreatedAt: a.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	art, data, err := s.automations.ReadArtifact(r.Context(), u.ID, r.PathValue("artifactID"))
	if err != nil {
		s.writeAutomationErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeHeader(art.Name)+"\"")
	_, _ = w.Write(data)
}

func (s *Server) handleValidateAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	def, ok := s.decodeDefinition(w, r)
	if !ok {
		return
	}
	res := wire.AutomationValidation{Valid: true, Eligible: true}
	if verrs := def.Validate(); verrs != nil {
		res.Valid = false
		res.Issues = toWireIssues(verrs)
	}
	// Eligibility is informational here (only an enable hard-blocks on it).
	if elig, err := s.automationEligibility(r.Context(), u.ID); err == nil {
		if everrs := def.CheckEligibility(elig); everrs != nil {
			res.Eligible = false
			res.EligibilityIssues = toWireIssues(everrs)
		}
	} else {
		res.Eligible = false
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAssistAutomation(w http.ResponseWriter, r *http.Request) {
	if !s.automationsReady(w) {
		return
	}
	var body wire.AssistAutomationRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	res, err := s.automations.Assist(r.Context(), body.Prompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := wire.AssistAutomationResponse{Definition: res.Definition, RawText: res.RawText, Valid: res.Valid, Attempts: res.Attempts}
	if res.Issues != nil {
		out.Issues = toWireIssues(res.Issues)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAutomationMeta(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	meta := wire.AutomationMeta{
		Enabled:         s.cfg.Automations.Enabled,
		SchemaVersion:   automation.SchemaVersion,
		Tools:           automation.KnownTools(),
		Adapters:        automation.KnownAdapters(),
		HostServices:    []string{"llamacpp"},
		NetworkIsolated: s.cfg.Sandbox.NetworkIsolated,
		AgentsEnabled:   s.cfg.Agents.Enabled && s.cfg.Sandbox.Enabled,
		Available:       s.automations != nil,
	}
	if s.automations == nil {
		meta.Reason = "automations are not enabled on this server"
	}
	// Owner's secret names (for the secret_refs picker).
	if s.vault != nil {
		if secrets, err := s.vault.List(r.Context(), u.ID); err == nil {
			for _, sec := range secrets {
				meta.Secrets = append(meta.Secrets, sec.Name)
			}
		}
	}
	// Owner's agent availability (for the agent_auth_refs picker).
	elig, _ := s.automationEligibility(r.Context(), u.ID)
	for _, prov := range automation.KnownAdapters() {
		_, configured := elig.ConfiguredAgents[prov]
		meta.Agents = append(meta.Agents, wire.AutomationAgentOption{
			Provider: prov, Configured: configured, Runnable: elig.RunnableAgents[prov],
		})
	}
	writeJSON(w, http.StatusOK, meta)
}

// automationEligibility assembles the runtime facts the automation eligibility check
// needs from server config + the owner's vault + agent profiles. It mirrors the
// agent catalog's runnable logic so the two views never diverge.
func (s *Server) automationEligibility(ctx context.Context, userID string) (automation.Eligibility, error) {
	elig := automation.Eligibility{
		AgentsEnabled:    s.cfg.Agents.Enabled && s.cfg.Sandbox.Enabled,
		NetworkIsolated:  s.cfg.Sandbox.NetworkIsolated,
		SandboxAvailable: s.runner != nil && s.runner.Available(),
		PiEndpoint:       s.cfg.Agents.LocalEndpoint != "",
		Secrets:          map[string]bool{},
		ConfiguredAgents: map[string]string{},
		RunnableAgents:   map[string]bool{},
	}
	if len(s.cfg.Agents.Providers) > 0 {
		elig.AllowedProviders = map[string]bool{}
		for _, p := range s.cfg.Agents.Providers {
			elig.AllowedProviders[p] = true
		}
	}
	if s.vault != nil {
		if secrets, err := s.vault.List(ctx, userID); err == nil {
			for _, sec := range secrets {
				elig.Secrets[sec.Name] = true
			}
		}
	}
	profiles, err := s.store.ListAgentProfilesByUser(ctx, userID)
	if err != nil {
		return elig, err
	}
	for _, p := range profiles {
		if p.Status == "authenticated" {
			elig.ConfiguredAgents[p.Provider] = p.Status
		}
		// An automation is bound by its OWN sandbox profile + agent_auth_refs, NOT the user's
		// interactive Sandbox Environment tool policy — so runnability for automations passes
		// envAllowed=true (HandleAutomation likewise skips env.ToolAllowed). This keeps a
		// scheduled agent automation from requiring the user to widen their chat trust boundary.
		runnable, _ := s.agentRunnable(agentcli.Provider(p.Provider), p.Status == "authenticated", true)
		elig.RunnableAgents[p.Provider] = runnable
	}
	return elig, nil
}

// decodeDefinition reads the automation.v1 document from a create/update/validate body.
func (s *Server) decodeDefinition(w http.ResponseWriter, r *http.Request) (automation.Definition, bool) {
	var in wire.AutomationInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return automation.Definition{}, false
	}
	if len(in.Definition) == 0 {
		writeErr(w, http.StatusBadRequest, "definition is required")
		return automation.Definition{}, false
	}
	def, err := automation.Parse(in.Definition)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "definition is not valid automation.v1 JSON: "+err.Error())
		return automation.Definition{}, false
	}
	return def, true
}

// writeAutomationErr maps service errors to HTTP: validation -> 400 with issues,
// not-found -> 404, everything else -> 500/400.
func (s *Server) writeAutomationErr(w http.ResponseWriter, err error) {
	var verrs *automation.ValidationErrors
	if errors.As(err, &verrs) {
		writeJSON(w, http.StatusUnprocessableEntity, wire.AutomationValidation{
			Valid: false, Issues: toWireIssues(verrs),
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

// --- conversions ---

func toAutomationSummary(row store.Automation) wire.AutomationSummary {
	// Trigger comes from the scalar column (the list path omits the definition JSON).
	return wire.AutomationSummary{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Trigger: row.Trigger,
		NextRun: timePtr(row.NextRunAt), LastRun: timePtr(row.LastRunAt),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func (s *Server) toAutomationDetail(row store.Automation) wire.AutomationDetail {
	d := wire.AutomationDetail{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled,
		NextRun: timePtr(row.NextRunAt), LastRun: timePtr(row.LastRunAt),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Definition: json.RawMessage(row.Definition),
	}
	if def, err := automation.Parse([]byte(row.Definition)); err == nil {
		d.Trigger = def.Trigger.Type
	}
	return d
}

func toRunSummary(run store.AutomationRun) wire.AutomationRunSummary {
	return wire.AutomationRunSummary{
		ID: run.ID, AutomationID: run.AutomationID, Status: run.Status, Trigger: run.Trigger,
		Error: run.Error, StartedAt: timePtr(run.StartedAt), FinishedAt: timePtr(run.FinishedAt),
		DurationMs: durationMs(run.StartedAt, run.FinishedAt),
	}
}

func toWireIssues(verrs *automation.ValidationErrors) []wire.AutomationIssue {
	if verrs == nil {
		return nil
	}
	out := make([]wire.AutomationIssue, 0, len(verrs.Issues))
	for _, is := range verrs.Issues {
		out = append(out, wire.AutomationIssue{Path: is.Path, Message: is.Message})
	}
	return out
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}

func durationMs(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

// sanitizeHeader strips characters unsafe for a Content-Disposition filename.
func sanitizeHeader(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '"' || r == '\n' || r == '\r' || r < 0x20 {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "artifact"
	}
	return string(out)
}
