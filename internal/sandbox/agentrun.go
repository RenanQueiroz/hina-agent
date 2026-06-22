package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// AgentStateStore is the vault surface the AgentRouter needs: read/write a user's
// per-provider encrypted credential material. *vault.Vault satisfies it.
type AgentStateStore interface {
	GetAgentState(userID, provider string) ([]byte, error)
	// GetAgentStateVersioned returns the blob AND a WRITE-UNIQUE version (so a same-value
	// delete+recreate yields a new version), from a single read.
	GetAgentStateVersioned(userID, provider string) ([]byte, string, error)
	// AgentStateVersion returns the current write-unique version ("" if none).
	AgentStateVersion(userID, provider string) string
	PutAgentState(userID, provider string, data []byte) error
	HasAgentState(userID, provider string) bool
}

// AgentProfileStore is the store surface for a user's configured agent profiles.
// *store.Store satisfies it.
type AgentProfileStore interface {
	GetAgentProfile(ctx context.Context, userID, provider string) (store.AgentProfile, error)
	UpsertAgentProfile(ctx context.Context, p store.AgentProfile) error
}

// AgentRouterConfig wires the AgentRouter's agent-specific dependencies. The shared
// security infrastructure (runner, secrets, workspaces, audit store, bus, approver,
// approval mode, per-user lock, network-isolation gate, quota) is inherited from the
// Router it is built from, so an agent run gets the exact same boundary as a raw
// tool call.
type AgentRouterConfig struct {
	State    AgentStateStore
	Profiles AgentProfileStore
	// LocalEndpoint is the host-inference proxy base URL Pi targets (the Phase 11
	// gateway). Empty until Phase 11 lands it — Pi then reports unavailable.
	LocalEndpoint string
	// AllowedProviders is the admin allow-list ([agents] providers). Empty permits
	// every built-in provider. Enforced at RUN time (not just in the UI) so removing
	// a provider from policy makes an existing profile non-runnable immediately.
	AllowedProviders []string
	// CredLocks is a SHORT per-user lock shared with the auth broker, taken only around
	// credential-state writes (the run's refreshed-store persist, and the broker's
	// SetKey/logout/login). It is NOT the long run lock, so a logout/key-rotation is
	// PROMPT (not blocked for the whole run) while the persist-vs-delete race stays
	// closed. New() defaults a private one if nil (fine for isolated tests).
	CredLocks *UserLocker
	// Runs is the in-flight run registry shared with the broker so a logout can cancel a
	// run that holds (or is about to launch with) the revoked credential. New() defaults
	// a private one if nil.
	Runs *RunRegistry
	// Limits bounds an agent run (typically a longer timeout than a shell call).
	Limits Limits
}

// providerAllowed reports whether provider is permitted by the admin allow-list.
func (ar *AgentRouter) providerAllowed(provider agentcli.Provider) bool {
	if len(ar.cfg.AllowedProviders) == 0 {
		return true
	}
	for _, p := range ar.cfg.AllowedProviders {
		if p == string(provider) {
			return true
		}
	}
	return false
}

// AgentRouter turns a model-requested `agent.<provider>.run` tool call into an
// audited, policy-checked, sandboxed run of a callable coding-agent CLI. It reuses
// the Router's per-user lock, approval gate, secret redaction, workspace quota, and
// audit log; on top it resolves the user's encrypted agent-state (credential store
// or API key), mounts/injects it for exactly one run, parses the CLI's output into a
// normalized AgentRunResult, and re-encrypts a refreshed credential store afterward.
type AgentRouter struct {
	base *Router
	cfg  AgentRouterConfig
	log  *slog.Logger
}

// NewAgentRouter builds an AgentRouter that shares this Router's infrastructure
// (lock, runner, secrets, workspaces, audit, approver, network-isolation gate).
func (r *Router) NewAgentRouter(cfg AgentRouterConfig) *AgentRouter {
	if cfg.CredLocks == nil {
		// A private locker is fine for isolated tests; production shares one with the
		// broker (setupAgents) so logout/SetKey serialize with the run's persist.
		cfg.CredLocks = &UserLocker{}
	}
	if cfg.Runs == nil {
		cfg.Runs = &RunRegistry{}
	}
	return &AgentRouter{base: r, cfg: cfg, log: r.log}
}

// Handles reports whether name is an agent-run tool this router owns.
func Handles(name string) bool {
	_, ok := agentcli.ProviderFromToolName(name)
	return ok
}

// agentArgs is the typed argument surface a model supplies for an agent run — never
// argv/flags, so command injection is not reachable from a model response.
type agentArgs struct {
	Prompt     string          `json:"prompt"`
	Model      string          `json:"model"`
	MaxTurns   int             `json:"max_turns"`
	Structured bool            `json:"structured"`
	Schema     json.RawMessage `json:"schema"`
}

// AgentRunOptions lets a TRUSTED caller (the Phase 9 automation runtime) override how
// an agent run is workspaced + approved. The interactive path passes the zero value.
//   - Workspace: a SERVER-CREATED host dir mounted read-write at /workspace instead of
//     the user's durable workspace (e.g. the automation run's ephemeral scratch). It
//     must be a path the server owns — NEVER a model/definition-controlled value.
//   - Workdir: the working directory inside the container (e.g. "/workspace/pr-42");
//     the caller is responsible for keeping it under /workspace.
//   - AutoApprove: skip the interactive approval gate (an unattended automation run has
//     no human to prompt). The run is still fully audited.
type AgentRunOptions struct {
	Workspace   string
	Workdir     string
	AutoApprove bool
	// Limits, when non-zero, caps this run's resources (the automation's sandbox.resources)
	// instead of the AgentRouter's global limits.
	Limits Limits
	// StateRoot, when set, roots the run's credential-store + staging scratch dirs under THIS
	// directory instead of a fresh sibling scratch. The automation runtime passes a per-run
	// dir it ALSO watches (but does NOT mount at /workspace), so the agent's RW credential dir
	// counts toward the per-run disk cap yet stays invisible to sibling/parallel steps.
	StateRoot string
}

// agentRunOptions is the internal form threaded through the shared run core.
type agentRunOptions struct {
	workspace   string
	workdir     string
	autoApprove bool
	limits      Limits
	hasLimits   bool
	stateRoot   string // roots the credential/staging scratch (watched, not /workspace-mounted)
	// automation marks the unattended automation path. It is bound by the automation's OWN
	// sandbox profile, so it does NOT consult the user's INTERACTIVE Sandbox Environment
	// tool allow-list (every other gate — provider allow-list, profile/auth, network
	// isolation, redaction, lock, quota, audit — still applies identically).
	automation bool
}

// Handle routes one agent-run tool call (the interactive model-driven path) and
// returns the normalized result fed back to the model. Like Router.Handle it returns a
// Go error ONLY for a context cancellation the loop must treat as an interrupt; every
// expected failure (not configured, denied, runtime unavailable) comes back in
// ToolResult.Err.
func (ar *AgentRouter) Handle(ctx context.Context, scope Scope, call ToolCall) (ToolResult, error) {
	return ar.handle(ctx, scope, call, agentRunOptions{})
}

// HandleAutomation runs an agent for the unattended automation runtime: it reuses the
// EXACT same credential resolution / mount / redaction / audit / launch-fence boundary
// as Handle, but mounts the run's own (server-created) workspace, runs in the given
// workdir, and auto-approves (no human is present). opts.Workspace must be a path the
// server controls.
func (ar *AgentRouter) HandleAutomation(ctx context.Context, scope Scope, call ToolCall, opts AgentRunOptions) (ToolResult, error) {
	// Fail closed: the automation path MUST supply its own (server-created) workspace —
	// an empty value would fall back to the user's durable workspace, which an unattended
	// auto-approved run must never touch.
	if opts.Workspace == "" {
		return ToolResult{Err: "automation agent run requires an explicit run workspace"}, nil
	}
	zeroLimits := Limits{}
	return ar.handle(ctx, scope, call, agentRunOptions{
		workspace: opts.Workspace, workdir: opts.Workdir, autoApprove: opts.AutoApprove,
		limits: opts.Limits, hasLimits: opts.Limits != zeroLimits, automation: true,
		stateRoot: opts.StateRoot,
	})
}

// handle is the shared agent-run core. opts is the zero value for the interactive
// path; the automation path supplies a workspace/workdir override + auto-approve.
func (ar *AgentRouter) handle(ctx context.Context, scope Scope, call ToolCall, opts agentRunOptions) (ToolResult, error) {
	provider, ok := agentcli.ProviderFromToolName(call.Name)
	if !ok {
		return ToolResult{Err: fmt.Sprintf("%q is not a callable-agent tool", call.Name)}, nil
	}
	adapter, ok := agentcli.Get(provider)
	if !ok {
		return ToolResult{Err: fmt.Sprintf("agent %q is unknown", provider)}, nil
	}
	// Enforce the admin allow-list at run time (not just in the catalog UI), so a
	// provider removed from [agents] providers stops running even with a live profile.
	if !ar.providerAllowed(provider) {
		return ToolResult{Err: fmt.Sprintf("the %s agent is not permitted by server policy", provider)}, nil
	}

	// The sandbox runtime must be present — agent CLIs only ever run inside `sbx`.
	if ar.base.cfg.Runner == nil || !ar.base.cfg.Runner.Available() {
		return ToolResult{Err: "the sandbox runtime is unavailable; agent runs require a working, pinned sbx install"}, nil
	}
	// Fail closed: an agent run carries powerful provider credentials AND needs
	// network egress to its provider (or, for Pi, the local proxy), which Hina cannot
	// gate per-container yet. Only run when the operator has asserted the sbx
	// container's egress is controlled.
	if !ar.base.cfg.NetworkIsolated {
		return ToolResult{Err: "agent runs are disabled unless [sandbox] network_isolated=true (the operator must assert the sandbox's network egress is controlled — a coding-agent run sends data to its provider and Hina can't gate that egress itself)"}, nil
	}

	// Enforce the user's INTERACTIVE Sandbox Environment tool allow-list — but ONLY for the
	// interactive (model-driven) path. A user who removes agent.<provider>.run from their
	// policy blocks the model from invoking it. An AUTOMATION is bound by its own sandbox
	// profile + agent_auth_refs instead, so it does NOT consult this interactive gate
	// (otherwise running a scheduled agent would force widening the unrelated chat trust
	// boundary). Loaded here pre-approval and re-checked under the lock below.
	env, err := ar.base.environment(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load sandbox policy"}, nil
	}
	if !opts.automation && !env.ToolAllowed(call.Name) {
		ar.base.audit(scope, call.Name, "", "blocked", store.SandboxRun{}, "tool not permitted")
		return ToolResult{Err: fmt.Sprintf("tool %q is not permitted by your Sandbox Environment policy", call.Name)}, nil
	}

	// Eligibility: the user must have configured a profile for this provider.
	profile, err := ar.cfg.Profiles.GetAgentProfile(ctx, scope.UserID, string(provider))
	if errors.Is(err, store.ErrNotFound) {
		return ToolResult{Err: fmt.Sprintf("%s is not configured — authenticate it in your Sandbox Environment first", adapter.Capability().DisplayName)}, nil
	}
	if err != nil {
		return ToolResult{Err: "could not load the agent profile"}, nil
	}
	authType := agentcli.AuthType(profile.AuthType)

	req, err := parseAgentArgs(call.Arguments)
	if err != nil {
		return ToolResult{Err: err.Error()}, nil
	}
	req.AuthType = authType
	if provider == agentcli.ProviderPi {
		if strings.TrimSpace(ar.cfg.LocalEndpoint) == "" {
			return ToolResult{Err: "Pi is unavailable: it needs the managed local LLM backend (Phase 11), which is not configured"}, nil
		}
		req.LocalEndpoint = ar.cfg.LocalEndpoint
	}

	// Redact the model-visible summary over ALL the user's vaulted secret values
	// before any audit row. For a key/token profile, ALSO fold the stored credential
	// into the summary redactor up front (read-only — not injected, not run) so a
	// prompt that embeds the credential can't leak it into a DENIED-approval audit row,
	// which is written before the run-time credential redactor exists.
	baseRed, err := ar.base.cfg.Secrets.AllValuesRedactor(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load secrets for redaction"}, nil
	}
	// preCredRed snapshots the agent credential as it is BEFORE approval. It is carried
	// into the denial AND run-time redactors so that if a key/store is rotated or
	// deleted during the approval window, an OLD credential value the prompt still
	// embeds is redacted from the audit and refused on the argv (not just the new one).
	// If the configured credential can't be read/decoded, we can't prove the prompt is
	// scrubbed — fall back to a prompt-free summary (fail closed).
	preCredRed, credErr := ar.storedCredRedactor(scope.UserID, provider, authType)
	summaryRed := baseRed
	if preCredRed != nil {
		summaryRed = baseRed.Merge(preCredRed)
	}
	summary := genericAgentSummary(provider, profile.AuthType)
	if credErr == nil {
		summary = agentSummary(provider, profile.AuthType, summaryRed.RedactText(req.Prompt))
	}

	callID := id.New("tcl")
	requested := map[string]any{
		"call_id": callID, "tool": call.Name, "summary": summary,
		"needs_approval": ar.base.cfg.Approval == ApprovalAlways,
	}

	decision := "auto"
	if ar.base.cfg.Approval == ApprovalAlways && !opts.autoApprove {
		if ar.base.cfg.Approver == nil {
			return ToolResult{Err: "tool approval is required but no approver is configured"}, nil
		}
		approved, aerr := ar.base.cfg.Approver.Approve(ctx, ApprovalRequest{
			CallID: callID, UserID: scope.UserID, ConversationID: scope.ConversationID,
			Tool: call.Name, Summary: summary,
		}, func() { ar.base.emit(scope, events.TypeToolCallRequested, requested) })
		if aerr != nil {
			ar.base.emit(scope, events.TypeToolCallCompleted, map[string]any{
				"call_id": callID, "tool": call.Name, "ok": false, "decision": "interrupted",
			})
			if ctx.Err() != nil {
				return ToolResult{}, ctx.Err()
			}
			return ToolResult{Err: "approval failed: " + aerr.Error()}, nil
		}
		if !approved {
			// Re-redact with the CURRENT secret + agent-credential set before persisting:
			// a SetKey or a browser re-auth during the approval window may have stored a
			// new credential, and a denied row must not record it. If the fresh read
			// fails, persist a prompt-free generic summary rather than the stale one.
			denySummary := genericAgentSummary(provider, profile.AuthType)
			if cur, derr := ar.deniedRedactor(ctx, scope.UserID, provider, preCredRed); derr == nil {
				// Redact the FULL prompt over {current vaulted + current cred + pre-approval
				// cred}, THEN truncate — so neither an old nor a new credential, nor one
				// straddling the length cap, survives in the denied audit row.
				denySummary = agentSummary(provider, profile.AuthType, cur.RedactText(req.Prompt))
			}
			ar.base.audit(scope, call.Name, "", "denied", store.SandboxRun{Command: denySummary}, "")
			ar.base.emit(scope, events.TypeToolCallCompleted, map[string]any{
				"call_id": callID, "tool": call.Name, "ok": false, "decision": "denied",
			})
			return ToolResult{Err: "the user denied this agent run"}, nil
		}
		decision = "approved"
	} else {
		ar.base.emit(scope, events.TypeToolCallRequested, requested)
	}

	// Serialize the user's sandbox activity (shared lock with raw tool runs) so the
	// quota preflight is meaningful and agent-state writes don't race.
	unlock := ar.base.lockUser(scope.UserID)
	defer unlock()

	// Re-load the profile UNDER THE LOCK so a logout during the approval/queue window
	// is honored (the agent-state may be gone).
	profile, err = ar.cfg.Profiles.GetAgentProfile(ctx, scope.UserID, string(provider))
	if errors.Is(err, store.ErrNotFound) {
		return ToolResult{Err: fmt.Sprintf("%s is no longer configured", adapter.Capability().DisplayName)}, nil
	}
	if err != nil {
		return ToolResult{Err: "could not load the agent profile"}, nil
	}
	req.AuthType = agentcli.AuthType(profile.AuthType)

	// Read the credential blob ONCE and bind the materialized credential, its mount, and
	// the launch-fence version to THIS exact blob. (Reading it separately for the version,
	// the redactor/env, and the mount would let an A->B->A rotation materialize B yet pass
	// a fence that only compares the current blob to a pre-materialization snapshot.) The
	// per-auth-type missing/error handling stays in materializeCredential.
	credBlob, credVersion, credBlobErr := ar.loadCredBlob(scope.UserID, provider)

	// Build the run-time redactor over the CURRENT secret set; fail closed.
	redactor, err := ar.base.runRedactor(ctx, scope.UserID, baseRed)
	if err != nil {
		return ToolResult{Err: "could not load secrets for redaction; refusing to run the agent"}, nil
	}

	plan, err := adapter.BuildRun(req)
	if err != nil {
		return ToolResult{Err: err.Error()}, nil
	}

	// Resolve the agent credential and fold it into the redactor BEFORE the run, so a
	// key value can never reach output/audit. An API key/token is injected as an env
	// var; a browser/subscription store contributes a token redactor (no injected env).
	secretEnv, credRedactor, materr := ar.materializeCredential(credBlob, credBlobErr, provider, req.AuthType, plan)
	if materr != nil {
		return ToolResult{Err: materr.Error()}, nil
	}
	if credRedactor != nil {
		redactor = redactor.Merge(credRedactor)
	}
	// Also carry the PRE-approval credential so the argv guard refuses (and the audit
	// scrubs) an OLD credential value the prompt embeds even if it was rotated away
	// while approval was pending — the old value may still be live until the provider
	// revokes it, and it must never reach the host argv or an audit row.
	if preCredRed != nil {
		redactor = redactor.Merge(preCredRed)
	}
	// Recompute the summary over the FULL current set (vaulted secrets + the current
	// agent credential) before ANY blocked/pending audit uses it — so a credential a
	// prompt embeds can't land in an audit row, even one rotated during the approval.
	// Redact the FULL prompt BEFORE truncating (agentSummary truncates), so a credential
	// straddling the summary length cap can't leave an unredactable prefix. RedactText scrubs
	// the JSON-escaped form too, so an escaped credential the argv guard blocks (or that reaches
	// the run-time audit) is never persisted unredacted in sandbox_runs.command.
	summary = agentSummary(provider, profile.AuthType, redactor.RedactText(req.Prompt))

	// Re-check the interactive Sandbox Environment tool allow-list UNDER THE LOCK (a tool
	// removed during the window is honored), auditing the now-current summary. Skipped for
	// the automation path, which is not bound by the interactive policy.
	env, err = ar.base.environment(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load sandbox policy"}, nil
	}
	if !opts.automation && !env.ToolAllowed(call.Name) {
		ar.base.audit(scope, call.Name, "", "blocked", store.SandboxRun{Command: summary}, "tool no longer permitted")
		return ToolResult{Err: fmt.Sprintf("tool %q is no longer permitted by your Sandbox Environment policy", call.Name)}, nil
	}

	// Guard: no argv element (notably the prompt) may carry a secret value — it would land on
	// the host `sbx` command line AND be sent to the agent provider. ContainsSecretText also
	// catches a secret in its JSON-escaped form, so a `\n`/`\"`/`\\`-escaped credential that a
	// template rendered into the prompt (via a json.Marshal'd object value) can't slip past a
	// plaintext-only check. Fail closed (exact substring, so a value equal to the marker can't slip).
	for _, a := range plan.Argv {
		if redactor.ContainsSecretText(a) {
			ar.base.audit(scope, call.Name, "", "blocked", store.SandboxRun{Command: summary}, "argument contains a secret value")
			return ToolResult{Err: "refusing to run: an argument (e.g. the prompt) contains a secret value (it would appear on the host command line)"}, nil
		}
	}
	// Same guard for adapter-staged files (e.g. a model-supplied output schema): they
	// are handed to the CLI, so a secret in a staged file's path or content is refused.
	// Staged files are JSON, so a secret with a quote/backslash/newline could appear
	// under ANY valid escaping — DECODE the JSON and check the plaintext string values.
	for _, f := range plan.Files {
		if redactor.ContainsSecret(f.RelPath) || redactor.JSONContainsSecret(f.Content) {
			ar.base.audit(scope, call.Name, "", "blocked", store.SandboxRun{Command: summary}, "staged file contains a secret value")
			return ToolResult{Err: "refusing to run: a staged file (e.g. the output schema) contains a secret value"}, nil
		}
	}

	// Workspace mounted read-write at /workspace: the automation runtime supplies its
	// own (server-created) run scratch so an unattended agent run NEVER reads/writes the
	// user's durable workspace; the interactive path uses the durable workspace.
	workspace := opts.workspace
	if workspace == "" {
		workspace, err = ar.base.cfg.Workspaces.UserWorkspace(scope.UserID)
		if err != nil {
			return ToolResult{Err: "could not prepare workspace"}, nil
		}
	}

	// Materialize the credential-store mount (always present so the CLI has a writable
	// home): the decrypted browser/subscription store, or a fresh empty dir.
	stateScratch, mount, err := ar.prepareStateMount(credBlob, provider, req.AuthType, adapter, opts.stateRoot)
	if err != nil {
		return ToolResult{Err: "could not prepare the agent credential store"}, nil
	}
	// When the caller supplied a StateRoot (the automation runtime), the scratch lives under
	// that caller-owned, watchdog-counted dir — DON'T remove it here, or a fast over-cap write
	// to the credential mount would be cleaned up before the run's final disk check sees it.
	// The automation runtime removes the whole StateRoot after that check. The interactive path
	// (no StateRoot) owns a sibling scratch and must clean it up itself.
	if opts.stateRoot == "" {
		defer stateScratch.Remove()
	}
	// The browser-state store's token redactor is already folded into `redactor` via
	// materializeCredential (built from the authoritative blob, before the argv guard).

	// Quota preflight under the lock; fail closed on a scan error.
	if q := ar.base.cfg.QuotaBytes; q > 0 {
		ok, used, qerr := ar.base.cfg.Workspaces.WithinQuota(scope.UserID, q)
		if qerr != nil {
			ar.log.Warn("agent: workspace quota scan failed; refusing to run", "user", scope.UserID, "err", qerr)
			return ToolResult{Err: "could not verify your workspace quota; refusing to run the agent"}, nil
		}
		if !ok {
			return ToolResult{Err: fmt.Sprintf("workspace quota exceeded (%d bytes used)", used)}, nil
		}
	}

	// Pre-insert a PENDING audit row before the side-effecting run; fail closed.
	auditID := id.New("sbr")
	if err := ar.base.insertRun(store.SandboxRun{
		ID: auditID, UserID: scope.UserID, ConversationID: scope.ConversationID,
		Tool: call.Name, Decision: decision, Command: summary,
		ExitCode: -1, Error: pendingAuditMarker,
	}); err != nil {
		ar.log.Error("agent: could not record pending audit row; refusing to run", "err", err)
		return ToolResult{Err: "could not record this agent run for audit; refusing to run it"}, nil
	}

	// Stage adapter files (output schema / Pi models.json) into a FRESH per-run scratch
	// mounted read-only at the container's staging dir — NOT the durable workspace. A
	// fresh dir has no pre-existing symlinks, so a model-controlled path can't make the
	// host follow a symlink and overwrite a file outside sbx. Done AFTER quota + the
	// pending audit so no host-side mutation precedes the audited gate.
	mounts := []Mount{mount}
	stagingScratch, stagingMount, hasStaging, serr := ar.prepareStaging(plan.Files, opts.stateRoot)
	if serr != nil {
		ar.log.Warn("agent: stage files failed", "err", serr)
		_ = ar.base.finalizeRun(store.SandboxRun{ID: auditID, ExitCode: -1, Error: "could not stage agent run files"})
		return ToolResult{Err: "could not stage agent run files"}, nil
	}
	if hasStaging {
		mounts = append(mounts, stagingMount)
		// Same as the state scratch: leave a StateRoot-rooted staging dir for the automation
		// runtime's final disk check + cleanup; only the interactive path removes its own.
		if opts.stateRoot == "" {
			defer stagingScratch.Remove()
		}
	}

	runLimits := ar.cfg.Limits
	if opts.hasLimits {
		runLimits = opts.limits // the automation's sandbox.resources cap this run
	}
	spec := RunSpec{
		UserID:         scope.UserID,
		ConversationID: scope.ConversationID,
		Tool:           call.Name,
		Argv:           plan.Argv,
		Env:            plan.Env,
		SecretEnv:      secretEnv,
		Redactor:       redactor,
		Workspace:      workspace,
		Workdir:        opts.workdir,
		Mounts:         mounts,
		Limits:         runLimits,
	}

	// Final launch fence. The credential was materialized earlier; a logout could have
	// landed since. Under the SHORT cred lock (the same one logout takes): re-check the
	// profile and, if still present, register this launch in the run registry with a
	// cancellable context. A concurrent logout therefore either runs first — the re-check
	// sees the profile gone and we refuse — or second — it cancels this context, so
	// Runner.Run starts already-cancelled and launches nothing with the revoked credential.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	fenceUnlock := ar.cfg.CredLocks.Lock(scope.UserID)
	_, perr := ar.cfg.Profiles.GetAgentProfile(ctx, scope.UserID, string(provider))
	if errors.Is(perr, store.ErrNotFound) || ar.agentStateVersion(scope.UserID, provider) != credVersion {
		// The credential was DELETED (logout) or REPLACED (SetKey / browser re-auth)
		// after we materialized it — refuse the stale launch rather than run with the old
		// key/store.
		fenceUnlock()
		_ = ar.base.finalizeRun(store.SandboxRun{ID: auditID, ExitCode: -1, Error: "credential changed before launch"})
		return ToolResult{Err: "the agent credential was changed or removed before the run could start"}, nil
	}
	releaseRun := ar.cfg.Runs.Add(scope.UserID, string(provider), cancelRun)
	defer releaseRun()
	fenceUnlock()

	res, runErr := ar.base.cfg.Runner.Run(runCtx, spec)

	// EVERY auth type gets a writable credential-store mount, so a CLI of ANY type can
	// MINT a derived bearer/session token into it mid-run (a browser run rotates its
	// store; an api_key/oauth run can cache an exchanged token) and then echo it. That
	// freshly-minted token wasn't in the pre-run redactor, so the runner couldn't scrub
	// it from the captured files or the inline output. Build a redactor over the
	// now-current store, fold it in, and re-scrub before anything is exposed — so a
	// minted token can't leak through output/audit/result for any auth type.
	storeScanFailed := false
	{
		postRed, scanErr := CredStoreRedactor(filepath.Join(stateScratch.Dir, "state"))
		if scanErr != nil {
			storeScanFailed = true
			// Can't fully scan the (possibly token-bearing) store, so a token minted into an
			// unreadable/oversized file can't be proven scrubbed — withhold ALL output:
			// drop the inline result and delete the capture files.
			ar.log.Warn("agent: could not scan credential store; withholding output", "err", scanErr)
			res.Stdout, res.Stderr = "", ""
			if res.StdoutPath != "" {
				_ = os.Remove(res.StdoutPath)
				res.StdoutPath = ""
			}
			if res.StderrPath != "" {
				_ = os.Remove(res.StderrPath)
				res.StderrPath = ""
			}
			res.CaptureErr = joinErr(res.CaptureErr, "credential store not fully scannable; output withheld")
		} else if postRed.MaxValueLen() > 0 {
			redactor = redactor.Merge(postRed)
			// Re-scrub with the truncation-boundary MARGIN re-applied: the runner already
			// margin-dropped using the PRE-run redactor, but a minted token is longer
			// than that margin assumed, so a straddling-token prefix can survive at the cap
			// — drop a fresh margin sized to the merged set. FAIL CLOSED: if a capture
			// file can't be re-scrubbed it is deleted and its path cleared, so a result
			// never references a file that still holds a minted token.
			if !ar.rescrubCaptureFile(res.StdoutPath, redactor, res.StdoutTruncated) {
				res.StdoutPath = ""
				res.CaptureErr = joinErr(res.CaptureErr, "stdout capture removed: token scrub failed")
			}
			if !ar.rescrubCaptureFile(res.StderrPath, redactor, res.StderrTruncated) {
				res.StderrPath = ""
				res.CaptureErr = joinErr(res.CaptureErr, "stderr capture removed: token scrub failed")
			}
			res.Stdout = safePostRedactInline(redactor, res.Stdout)
			res.Stderr = safePostRedactInline(redactor, res.Stderr)
		}
	}

	// Persist a refreshed browser/subscription credential store (tokens may have
	// rotated). Only when the agent actually executed (no spawn/timeout error): a run
	// that never touched the store must not overwrite the good login. Best-effort —
	// a re-archive failure is logged, not fatal to the run. Skip if the run was CANCELLED
	// (a logout/re-auth revoked it): a kill is often a non-timeout exit with res.Err==nil,
	// and persisting this run's stale scratch would overwrite the replacement credential.
	// Skip too if the store scan FAILED (unscannable/poisoned): don't save a store we
	// couldn't bound-scan, which would only fail-closed on the next run's load.
	if req.AuthType == agentcli.AuthBrowserState && runErr == nil && res.Err == nil && runCtx.Err() == nil && !storeScanFailed {
		ar.persistRefreshedState(scope.UserID, provider, stateScratch.Dir, credVersion)
	}

	// Finalize the audit row.
	auditErr := ""
	if res.Err != nil {
		auditErr = redactor.Redact(res.Err.Error())
	}
	if res.CaptureErr != "" {
		auditErr = joinErr(auditErr, "output capture failed: "+res.CaptureErr)
	}
	if err := ar.base.finalizeRun(store.SandboxRun{
		ID: auditID, SandboxID: res.SandboxID, ExitCode: res.ExitCode,
		DurationMs: res.Duration.Milliseconds(), StdoutPath: res.StdoutPath,
		StderrPath: res.StderrPath, Error: auditErr,
	}); err != nil {
		ar.log.Error("agent: failed to finalize audit row after a run", "id", auditID, "err", err)
	}

	if runErr != nil {
		return ToolResult{Err: redactor.Redact(runErr.Error())}, nil
	}

	// For an UNATTENDED automation run, the parsed result feeds later steps that auto-act on
	// it — so it must reflect the COMPLETE output, not the 64 KiB model-display inline stream.
	// Fail closed if the output couldn't be captured or exceeded the (1 MiB) capture cap, and
	// otherwise parse the full redacted capture FILE (same redaction as the inline stream).
	// The interactive path keeps the inline stream (the model sees that anyway).
	stdout := res.Stdout
	if opts.automation {
		if res.CaptureErr != "" {
			return ToolResult{Err: "agent output could not be captured; refusing to act on incomplete output"}, nil
		}
		if res.StdoutTruncated {
			return ToolResult{Err: "agent output exceeded the capture limit; refusing to act on truncated output"}, nil
		}
		if res.StdoutPath != "" {
			full, rerr := os.ReadFile(res.StdoutPath)
			if rerr != nil {
				return ToolResult{Err: "could not read the captured agent output; refusing to act on incomplete output"}, nil
			}
			stdout = string(full)
		}
	}

	// Normalize the captured output into an AgentRunResult, then redact every
	// model-visible field over the run-time redactor.
	parsed := adapter.Parse(provider, agentcli.RawResult{
		Stdout: stdout, Stderr: res.Stderr, ExitCode: res.ExitCode,
		TimedOut: res.TimedOut, Duration: res.Duration,
		StdoutPath: res.StdoutPath, StderrPath: res.StderrPath,
	})
	if res.Err != nil && parsed.Err == "" {
		parsed.Err = redactor.Redact(res.Err.Error())
	}
	redactAgentResult(&parsed, redactor)

	ar.base.emit(scope, events.TypeToolCallCompleted, map[string]any{
		"call_id": callID, "tool": call.Name,
		"ok": parsed.Status == agentcli.StatusOK, "decision": decision, "exit_code": res.ExitCode,
	})

	content, _ := json.Marshal(parsed)
	return ToolResult{Content: string(content)}, nil
}

// materializeCredential resolves the run's injected secret env and a redactor over
// its value. For an API key / OAuth token it returns the env pair(s) for the names
// the adapter declared; for a browser/subscription profile it returns no env (the
// credential is the mounted store). Injection is already gated on NetworkIsolated by
// the caller.
func (ar *AgentRouter) materializeCredential(blob []byte, blobErr error, provider agentcli.Provider, authType agentcli.AuthType, plan agentcli.RunPlan) ([]string, *vault.Redactor, error) {
	switch authType {
	case agentcli.AuthAPIKey, agentcli.AuthOAuthToken:
		if len(plan.SecretNames) == 0 {
			return nil, nil, nil
		}
		if errors.Is(blobErr, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("the %s credential is missing — re-authenticate it", provider)
		}
		if blobErr != nil {
			return nil, nil, fmt.Errorf("could not load the %s credential", provider)
		}
		kind, data, derr := DecodeCredState(blob)
		// Fail closed on a profile/blob mismatch — never inject a tar credential store
		// as if it were a key.
		if derr != nil || kind != CredKindKey {
			return nil, nil, fmt.Errorf("the %s credential is in an unexpected form — re-authenticate it", provider)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return nil, nil, fmt.Errorf("the stored %s credential is empty — re-authenticate it", provider)
		}
		var env []string
		for _, name := range plan.SecretNames {
			env = append(env, name+"="+key)
		}
		return env, vault.NewRedactor([]string{key}), nil
	case agentcli.AuthBrowserState:
		// No injected env (the credential is the mounted store), but DO return a
		// redactor over the store's token-shaped values so the argv guard, summary, and
		// run output all scrub a browser/subscription token — built from the SAME blob
		// the mount untars, BEFORE the argv check.
		if errors.Is(blobErr, store.ErrNotFound) {
			// A configured browser_state profile with NO stored credential store (e.g.
			// after a partial logout) must FAIL CLOSED — never run the agent with provider
			// egress and an empty store. Re-authenticating recreates it.
			return nil, nil, fmt.Errorf("the %s credential store is missing — re-authenticate it", provider)
		}
		if blobErr != nil {
			return nil, nil, fmt.Errorf("could not load the %s credential store", provider)
		}
		kind, data, derr := DecodeCredState(blob)
		if derr != nil || kind != CredKindTar {
			return nil, nil, fmt.Errorf("the %s credential store is in an unexpected form — re-authenticate it", provider)
		}
		red, terr := tokenRedactor(data)
		if terr != nil {
			return nil, nil, fmt.Errorf("the %s credential store is too large to scan for redaction — re-authenticate it", provider)
		}
		return nil, red, nil
	default:
		return nil, nil, nil
	}
}

// deniedRedactor builds a redactor over the user's CURRENT vaulted secrets, the
// CURRENT stored agent credential (reloading the profile for its possibly-rotated
// auth type), AND the pre-approval credential (preCredRed) — so a denied summary
// scrubs both an old credential the prompt still embeds and a newly-rotated one.
// Returns an error if the secret set can't be loaded — the caller then uses a
// prompt-free generic summary.
func (ar *AgentRouter) deniedRedactor(ctx context.Context, userID string, provider agentcli.Provider, preCredRed *vault.Redactor) (*vault.Redactor, error) {
	red, err := ar.base.cfg.Secrets.AllValuesRedactor(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Current credential, under its CURRENT auth type (a rotation may have changed
	// kind). Fail closed if the profile can't be read, or it is configured but the
	// credential is unreadable/corrupt — the caller then uses a prompt-free summary
	// rather than persisting a possibly-unredacted prompt.
	p, perr := ar.cfg.Profiles.GetAgentProfile(ctx, userID, string(provider))
	if perr != nil && !errors.Is(perr, store.ErrNotFound) {
		return nil, fmt.Errorf("could not load the agent profile for redaction")
	}
	if perr == nil {
		cr, cerr := ar.storedCredRedactor(userID, provider, agentcli.AuthType(p.AuthType))
		if cerr != nil {
			return nil, cerr
		}
		if cr != nil {
			red = red.Merge(cr)
		}
	}
	if preCredRed != nil {
		red = red.Merge(preCredRed)
	}
	return red, nil
}

// genericAgentSummary is a prompt-free audit summary used when the current redactor
// can't be loaded — it records the provider + auth type but never the prompt.
func genericAgentSummary(provider agentcli.Provider, authType string) string {
	return fmt.Sprintf("%s run [auth:%s]: [redaction unavailable]", provider, authType)
}

// storedCredRedactor returns a read-only redactor over a configured credential (an
// API key/OAuth token, or a browser/subscription store's token-shaped values) for use
// in approval/denied summaries BEFORE the run-time credential is materialized — so a
// prompt embedding the credential can't leak into a denied audit row. The credential
// is never injected or run here.
//
// It FAILS CLOSED: a credential that the profile says exists but is unreadable,
// corrupt, or whose kind mismatches the profile returns an error (so the caller uses a
// prompt-free summary rather than persisting the prompt unredacted). A genuinely
// absent credential (no stored state) returns (nil, nil).
// loadCredBlob reads the user's encrypted agent-state blob ONCE and returns it with a
// WRITE-UNIQUE version, so the materialized credential, its mount, and the launch fence
// all bind to the SAME write of the blob (closing a different-value AND a same-value ABA
// rotation race). version is "" when err != nil.
func (ar *AgentRouter) loadCredBlob(userID string, provider agentcli.Provider) ([]byte, string, error) {
	return ar.cfg.State.GetAgentStateVersioned(userID, string(provider))
}

// agentStateVersion returns the current WRITE-UNIQUE version of the user's agent-state
// blob, or "" if there is none/unreadable. The launch + persist fences compare it to the
// version of the blob the run actually materialized, so a credential REPLACEMENT, a
// DELETION, or a same-value delete+recreate after materialization is detected and the
// stale launch/persist refused.
func (ar *AgentRouter) agentStateVersion(userID string, provider agentcli.Provider) string {
	return ar.cfg.State.AgentStateVersion(userID, string(provider))
}

func (ar *AgentRouter) storedCredRedactor(userID string, provider agentcli.Provider, authType agentcli.AuthType) (*vault.Redactor, error) {
	blob, err := ar.cfg.State.GetAgentState(userID, string(provider))
	if err != nil {
		// The caller only invokes this for a CONFIGURED profile, so a missing/unreadable
		// blob is an inconsistency, not "no credential": fail closed (the caller then uses
		// a prompt-free summary) rather than admit a prompt that may embed the credential.
		return nil, fmt.Errorf("could not read the %s credential", provider)
	}
	kind, data, derr := DecodeCredState(blob)
	if derr != nil {
		return nil, fmt.Errorf("the %s credential is corrupt", provider)
	}
	switch authType {
	case agentcli.AuthAPIKey, agentcli.AuthOAuthToken:
		if kind != CredKindKey {
			return nil, fmt.Errorf("the %s credential kind mismatches the profile", provider)
		}
		if key := strings.TrimSpace(string(data)); key != "" {
			return vault.NewRedactor([]string{key}), nil
		}
		return nil, nil // an empty key has nothing to redact (the run fails closed separately)
	case agentcli.AuthBrowserState:
		if kind != CredKindTar {
			return nil, fmt.Errorf("the %s credential kind mismatches the profile", provider)
		}
		return tokenRedactor(data)
	default:
		return nil, nil
	}
}

// tokenRedactor builds a redactor over the token-shaped runs in raw credential bytes
// (a key, or the bytes of a credential-store tar). It is BOUNDED + fails closed: the
// count-limited FindAll caps the match slice itself, and the unique-token count/byte
// caps cap the redactor — so an archive-cap-compliant store packed with millions of
// unique short token-shaped values (a pre-run/summary DoS) returns an error rather than
// allocating an unbounded match slice/redactor.
func tokenRedactor(data []byte) (*vault.Redactor, error) {
	matches := credStoreSecretRe.FindAll(data, maxScanTokens+1)
	if len(matches) > maxScanTokens {
		return nil, fmt.Errorf("credential has too many token-shaped values to scan for redaction")
	}
	seen := map[string]struct{}{}
	var vals []string
	var tokenBytes int64
	for _, m := range matches {
		s := string(m)
		if _, dup := seen[s]; dup {
			continue
		}
		if tokenBytes += int64(len(s)); tokenBytes > maxScanTokenBytes {
			return nil, fmt.Errorf("credential has too many token bytes to scan for redaction")
		}
		seen[s] = struct{}{}
		vals = append(vals, s)
	}
	return vault.NewRedactor(vals), nil
}

// prepareStateMount returns the credential-store mount for the run (always present so
// the CLI has a writable HOME/config dir): for a browser/subscription profile the
// decrypted store unpacked into a fresh owner-private scratch dir, otherwise an empty
// scratch dir. The scratch is mounted read-write at the adapter's container path.
func (ar *AgentRouter) prepareStateMount(blob []byte, provider agentcli.Provider, authType agentcli.AuthType, adapter agentcli.Adapter, stateRoot string) (Scratch, Mount, error) {
	cs := adapter.CredStore()
	scratch, err := ar.base.cfg.Workspaces.NewScratchUnder(stateRoot)
	if err != nil {
		return Scratch{}, Mount{}, err
	}
	stateDir := filepath.Join(scratch.Dir, "state")
	if err := platform.EnsurePrivateDir(stateDir); err != nil {
		scratch.Remove()
		return Scratch{}, Mount{}, err
	}
	// Untar from the SAME blob materializeCredential built its redactor from (passed in),
	// not a fresh read — so the mounted store and the launch-fence version can't diverge
	// across a mid-setup rotation.
	if authType == agentcli.AuthBrowserState && len(blob) > 0 {
		kind, data, derr := DecodeCredState(blob)
		// Fail closed on a profile/blob mismatch — never untar a key blob.
		if derr != nil || kind != CredKindTar {
			scratch.Remove()
			return Scratch{}, Mount{}, fmt.Errorf("the stored credential store is in an unexpected form")
		}
		if err := UntarToDir(data, stateDir); err != nil {
			scratch.Remove()
			return Scratch{}, Mount{}, err
		}
	}
	return scratch, Mount{Host: stateDir, Container: cs.ContainerDir, ReadOnly: false}, nil
}

// persistRefreshedState re-archives the (possibly token-refreshed) browser/
// subscription credential store and re-encrypts it. Best-effort: failures are logged
// so a transient re-archive error never fails the run nor corrupts the stored state.
// It refuses to overwrite a stored login with an EMPTY store (a container that wiped
// its HOME would otherwise wipe the user's credentials). Under the cred lock it fences
// against a logout OR a credential REPLACEMENT that landed during the run: it persists
// only if the profile still exists, is still browser_state, AND the stored blob version
// still matches the launch version — so a successful re-auth (or a switch to api_key)
// during the run can't be clobbered by this run's stale scratch store.
func (ar *AgentRouter) persistRefreshedState(userID string, provider agentcli.Provider, scratchRoot, launchVersion string) {
	stateDir := filepath.Join(scratchRoot, "state")
	if !dirHasFiles(stateDir) {
		ar.log.Warn("agent: refreshed credential store is empty; keeping the prior login", "provider", provider)
		return
	}
	data, err := TarDir(stateDir)
	if err != nil {
		ar.log.Warn("agent: could not re-archive refreshed credential store", "provider", provider, "err", err)
		return
	}
	// Serialize the profile/version re-check + write with the broker's logout/SetKey on
	// the SHORT cred lock (NOT the long run lock — so a logout during the run is prompt).
	unlock := ar.cfg.CredLocks.Lock(userID)
	defer unlock()
	ctx, cancel := auditCtx()
	defer cancel()
	p, perr := ar.cfg.Profiles.GetAgentProfile(ctx, userID, string(provider))
	if errors.Is(perr, store.ErrNotFound) {
		ar.log.Info("agent: profile removed during run; not re-persisting credential store", "provider", provider)
		return
	}
	if perr != nil {
		ar.log.Warn("agent: could not re-check the profile before persisting; keeping the stored login", "provider", provider, "err", perr)
		return
	}
	// A credential REPLACEMENT during the run (re-auth, or a switch to api_key) bumped the
	// stored version / changed the auth type — do not overwrite the new login with this
	// run's stale store.
	if agentcli.AuthType(p.AuthType) != agentcli.AuthBrowserState || ar.agentStateVersion(userID, provider) != launchVersion {
		ar.log.Info("agent: credential replaced during run; not re-persisting the stale store", "provider", provider)
		return
	}
	if err := ar.cfg.State.PutAgentState(userID, string(provider), EncodeCredState(CredKindTar, data)); err != nil {
		ar.log.Warn("agent: could not persist refreshed credential store", "provider", provider, "err", err)
	}
}

// credStoreSecretRe matches token-shaped strings (JWTs, OAuth/API tokens, session
// cookies) — long runs of credential characters. 20+ chars avoids redacting ordinary
// words/paths while catching real bearer tokens.
var credStoreSecretRe = regexp.MustCompile(`[A-Za-z0-9_\-./+=]{20,}`)

// Caps on the EXTRACTED token set (vars so tests can shrink them): a store packed with
// many short unique token-shaped matches must not blow heap/CPU even while its file
// bytes stay under maxAgentStateTotal.
var (
	maxScanTokens           = 65536
	maxScanTokenBytes int64 = 16 << 20
)

// CredStoreRedactor builds a redactor over the token-shaped values found in a
// decrypted browser/subscription credential store directory, so a token an agent (or
// the auth status command) echoes to stdout/stderr can't reach captured output, the
// audit log, or a model-visible result. It is heuristic (it can't know which fields
// are secret), so it scrubs every long token-shaped run — over-redaction of output is
// acceptable; a leaked bearer token is not. Best-effort: an unreadable store yields an
// empty redactor.
// It FAILS CLOSED: a walk error, an unreadable file, a file too large to scan, or a
// store whose AGGREGATE entry count / total bytes exceeds the archive caps returns an
// error, so a caller can't keep output that may contain a token hidden in a part of the
// store it could not read — and a hostile agent can't force an unbounded synchronous
// scan of arbitrary total data (a host DoS). Over the caps, the fail-closed caller
// withholds output, exactly as for an unreadable file.
func CredStoreRedactor(dir string) (*vault.Redactor, error) {
	seen := map[string]struct{}{}
	var values []string
	var entries int
	var total, tokenBytes int64
	werr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Path-free: a raw *os.PathError embeds the (untrusted, possibly token-shaped)
			// name, and the caller logs this scan error when withholding output.
			return fmt.Errorf("a credential-store entry could not be scanned")
		}
		// Count EVERY visited entry (directories + symlinks + files) BEFORE the regular-
		// file check, so a deep tree of empty dirs/symlinks can't force an unbounded walk
		// with zero "files".
		if entries++; entries > maxAgentStateFiles {
			return fmt.Errorf("credential store has too many entries to scan for redaction (> %d)", maxAgentStateFiles)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > maxAgentStateFile {
			// Path-free: a credential-store filename is untrusted (could be token-shaped)
			// and this error is logged.
			return fmt.Errorf("a credential-store file is too large to scan for redaction")
		}
		if total += info.Size(); total > maxAgentStateTotal {
			return fmt.Errorf("credential store is too large in aggregate to scan for redaction")
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			// Path-free: the *os.PathError embeds the untrusted filename, and this is logged.
			return fmt.Errorf("a credential-store file could not be read for redaction")
		}
		for _, m := range credStoreSecretRe.FindAllString(string(data), -1) {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			values = append(values, m)
			// Bound the extracted token set: many short unique matches must not blow
			// heap/CPU even while file bytes stay under the aggregate cap.
			if tokenBytes += int64(len(m)); len(values) > maxScanTokens || tokenBytes > maxScanTokenBytes {
				return fmt.Errorf("credential store has too many token-shaped values to scan for redaction")
			}
		}
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return vault.NewRedactor(values), nil
}

// rescrubCaptureFile re-reads an already-written capture file and atomically rewrites
// it with the redactor applied — to scrub a token minted DURING the run (so absent
// from the pre-run redactor) from the on-disk capture before it is exposed. When the
// capture was truncated at the cap it also drops a fresh trailing margin so a token
// straddling the boundary leaves no unredactable prefix. It is FAIL-CLOSED: it returns
// false and DELETES the file if it can't be read or rewritten, so the caller can clear
// the path rather than reference a capture that may still hold a refreshed token.
func (ar *AgentRouter) rescrubCaptureFile(path string, red *vault.Redactor, truncated bool) bool {
	if path == "" {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		_ = os.Remove(path)
		ar.log.Warn("agent: could not read capture to re-scrub a refreshed token; removed it", "path", path, "err", err)
		return false
	}
	scrubbed := marginRedact(red, red.RedactBytes(data), truncated)
	if len(scrubbed) == len(data) && string(scrubbed) == string(data) {
		return true // nothing minted during the run; the pre-run capture is already clean
	}
	// Atomic replace (temp + rename in the same dir): a partial write can't leave a
	// half-scrubbed file, and any failure deletes the original (which still holds the token).
	tmp := path + ".scrub"
	if err := os.WriteFile(tmp, scrubbed, 0o600); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(path)
		ar.log.Warn("agent: could not write scrubbed capture; removed the file", "path", path, "err", err)
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(path)
		ar.log.Warn("agent: could not publish scrubbed capture; removed the file", "path", path, "err", err)
		return false
	}
	return true
}

// marginRedact drops a trailing (MaxValueLen-1) margin from already-redacted bytes
// when they were truncated, so a value straddling the truncation cap leaves no
// matchable prefix (mirrors the runner's safeRedact).
func marginRedact(red *vault.Redactor, scrubbed []byte, truncated bool) []byte {
	if !truncated {
		return scrubbed
	}
	if margin := red.MaxValueLen() - 1; margin > 0 {
		if margin >= len(scrubbed) {
			return nil
		}
		return scrubbed[:len(scrubbed)-margin]
	}
	return scrubbed
}

// inlineTruncSuffix is the marker the runner appends to inline output it truncated.
const inlineTruncSuffix = "\n…[output truncated]"

// safePostRedactInline re-redacts the model-visible inline output with the merged
// post-run redactor, re-applying the truncation margin (around the runner's truncation
// marker) so a refreshed token straddling the inline cap can't survive.
func safePostRedactInline(red *vault.Redactor, s string) string {
	trunc := strings.HasSuffix(s, inlineTruncSuffix)
	if trunc {
		s = strings.TrimSuffix(s, inlineTruncSuffix)
	}
	s = red.Redact(s)
	if trunc {
		if margin := red.MaxValueLen() - 1; margin > 0 {
			if margin >= len(s) {
				s = ""
			} else {
				s = s[:len(s)-margin]
			}
		}
		s += inlineTruncSuffix
	}
	return s
}

// dirHasFiles reports whether dir contains at least one regular file (anywhere
// under it). Used to avoid persisting an emptied credential store over a good login.
func dirHasFiles(dir string) bool {
	found := false
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// parseAgentArgs decodes a model's agent-run arguments into a RunRequest.
func parseAgentArgs(raw json.RawMessage) (agentcli.RunRequest, error) {
	var a agentArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return agentcli.RunRequest{}, fmt.Errorf("invalid agent arguments: a JSON object with a 'prompt' is required")
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return agentcli.RunRequest{}, fmt.Errorf("a non-empty 'prompt' string is required")
	}
	if a.MaxTurns < 0 {
		return agentcli.RunRequest{}, fmt.Errorf("'max_turns' must be >= 0")
	}
	return agentcli.RunRequest{
		Prompt:     a.Prompt,
		Model:      a.Model,
		MaxTurns:   a.MaxTurns,
		Structured: a.Structured && len(a.Schema) > 0,
		SchemaJSON: a.Schema,
	}, nil
}

// prepareStaging writes an adapter's files into a FRESH per-run scratch dir and
// returns a read-only mount of it at the container's staging path. Returns hasStaging
// false (and no scratch) when there are no files. Writing into a fresh dir (whose every
// component we create) means no pre-existing symlink can redirect a host write outside
// the sandbox.
func (ar *AgentRouter) prepareStaging(files []agentcli.StagedFile, stateRoot string) (Scratch, Mount, bool, error) {
	if len(files) == 0 {
		return Scratch{}, Mount{}, false, nil
	}
	scratch, err := ar.base.cfg.Workspaces.NewScratchUnder(stateRoot)
	if err != nil {
		return Scratch{}, Mount{}, false, err
	}
	stageDir := filepath.Join(scratch.Dir, "staging")
	if err := platform.EnsurePrivateDir(stageDir); err != nil {
		scratch.Remove()
		return Scratch{}, Mount{}, false, err
	}
	if err := stageFiles(stageDir, files); err != nil {
		scratch.Remove()
		return Scratch{}, Mount{}, false, err
	}
	return scratch, Mount{Host: stageDir, Container: agentcli.StagingDir, ReadOnly: true}, true, nil
}

// stageFiles writes an adapter's staged files under a FRESH staging dir (created by
// prepareStaging), rejecting any path that escapes it. Because the dir is fresh and we
// create every directory component, no symlink can exist to redirect a write.
func stageFiles(stageDir string, files []agentcli.StagedFile) error {
	for _, f := range files {
		dst, err := safeJoin(stageDir, f.RelPath)
		if err != nil {
			return err
		}
		if err := platform.EnsurePrivateDir(filepath.Dir(dst)); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if f.Mode != 0 {
			mode = os.FileMode(f.Mode)
		}
		if err := os.WriteFile(dst, f.Content, mode); err != nil {
			return err
		}
	}
	return nil
}

// agentRunResultSummaryMax bounds the prompt text echoed into an audit summary.
const agentRunResultSummaryMax = 200

// agentSummaryRaw is the un-redacted audit/UI summary for a run; agentSummary is the
// caller-redacted form. It records the auth-profile TYPE (never the credential) per
// the Phase 8 exit criterion.
func agentSummaryRaw(provider agentcli.Provider, authType, prompt string) string {
	p := strings.TrimSpace(prompt)
	// Truncate on a rune boundary so the audit summary is always valid UTF-8.
	if r := []rune(p); len(r) > agentRunResultSummaryMax {
		p = string(r[:agentRunResultSummaryMax]) + "…"
	}
	return fmt.Sprintf("%s run [auth:%s]: %s", provider, authType, p)
}

func agentSummary(provider agentcli.Provider, authType, redactedPrompt string) string {
	return agentSummaryRaw(provider, authType, redactedPrompt)
}

// redactAgentResult scrubs every model-visible field of a parsed result over the
// run-time redactor (the CLI could echo a secret into its final text, structured
// output, an error, or a changed-file path).
func redactAgentResult(res *agentcli.AgentRunResult, red *vault.Redactor) {
	// RedactText (not Redact) on the text fields: an agent that echoed json.Marshal of a
	// credential would otherwise persist the escaped form into the result/run record.
	res.FinalText = red.RedactText(res.FinalText)
	res.Err = red.RedactText(res.Err)
	if len(res.Structured) > 0 {
		// The structured field is raw JSON — DECODE + redact string values so a secret
		// under any valid escaping is caught (not just the plaintext substring).
		res.Structured = json.RawMessage(red.RedactJSON(res.Structured))
	}
	for i, f := range res.ChangedFiles {
		res.ChangedFiles[i] = red.RedactText(f)
	}
	for i, tc := range res.ToolCalls {
		res.ToolCalls[i].Name = red.RedactText(tc.Name)
		res.ToolCalls[i].Input = red.RedactText(tc.Input)
	}
}
