package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// Approval modes (admin policy).
const (
	ApprovalAlways = "always" // every tool call requires an explicit user decision
	ApprovalAuto   = "auto"   // tool calls run without prompting (still audited)
)

// pendingAuditMarker is the Error stamped on a pre-inserted audit row before its
// run is finalized, so a never-finalized row (crash / DB failure) is visibly NOT a
// successful zero-output run. finalizeRun overwrites it with the real outcome.
const pendingAuditMarker = "pending (not finalized)"

// ToolCall is a model-requested tool invocation routed to the sandbox.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is the sandboxed outcome fed back to the model. A non-empty Err is a
// tool-level failure the model should see and recover from (policy denial, a
// non-zero command exit is reported in Content, an execution error here).
type ToolResult struct {
	Content string
	Err     string
}

// Scope binds a tool call to the requesting user and conversation (for workspace
// selection, secret injection, audit, and approval routing).
type Scope struct {
	UserID         string
	ConversationID string
}

// ApprovalRequest is one pending tool call awaiting a user decision. Summary is
// already secret-redacted and safe to show.
type ApprovalRequest struct {
	CallID         string
	UserID         string
	ConversationID string
	Tool           string
	Summary        string
}

// Approver gates a tool call on a user decision (the "approval card" flow). The
// httpapi layer implements it over the event bus + an HTTP decide endpoint; tests
// use a simple allow/deny stub. onRegistered is called AFTER the pending decision
// is registered and BEFORE Approve blocks — the Router uses it to emit the
// ToolCallRequested event, so a fast client decision can never arrive before the
// registry can accept it.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest, onRegistered func()) (bool, error)
}

// SecretSource resolves a user's granted secrets into a run-scoped injection, and
// builds a redactor over ALL the user's vaulted values (redaction is decoupled from
// injection — see AllValuesRedactor). *vault.Vault satisfies it.
type SecretSource interface {
	Materialize(ctx context.Context, userID string, grants []vault.EnvGrant) (*vault.Injection, error)
	AllValuesRedactor(ctx context.Context, userID string) (*vault.Redactor, error)
}

// RouterStore is the persistence surface the Router needs: load a user's policy
// and write/finalize audit rows. *store.Store satisfies it; an interface keeps the
// fail-closed audit path testable.
type RouterStore interface {
	GetSandboxState(ctx context.Context, userID, kind string) (store.SandboxState, error)
	InsertSandboxRun(ctx context.Context, r store.SandboxRun) error
	UpdateSandboxRun(ctx context.Context, r store.SandboxRun) error
}

// EventPublisher fans out a LIVE (ephemeral) tool event. *events.Bus satisfies it.
// Tool-call lifecycle events (request/approval/completion) are live-only — they
// are NOT persisted/replayed, so a server restart can never replay an approval
// card whose in-memory pending decision is gone. The durable record of a tool run
// is the sandbox_runs audit table, not these events.
type EventPublisher interface {
	PublishEphemeral(e events.Event)
}

// RouterConfig wires the Router's dependencies.
type RouterConfig struct {
	Runner     Runner
	Secrets    SecretSource
	Workspaces *WorkspaceManager
	Store      RouterStore
	Bus        EventPublisher
	Approver   Approver
	Approval   string // ApprovalAlways | ApprovalAuto
	Limits     Limits // default per-run limits
	QuotaBytes int64  // per-user durable-workspace quota (0 = unlimited)
	// NetworkIsolated asserts the sbx container's egress is locked down. Granted
	// secrets are injected into a run ONLY when true (Hina can't gate a raw shell
	// command's network, so this fails closed against secret exfiltration).
	NetworkIsolated bool
	Log             *slog.Logger
}

// Router turns model-requested tool calls into audited, policy-checked sandbox
// runs. It is the single execution boundary scope item 4 of Phase 7 describes:
// every shell/file/HTTP tool the model asks for runs inside the calling user's
// `sbx` sandbox with that user's Sandbox Environment policy, granted secrets, an
// approval gate, and an audit-log entry — never on the host.
type Router struct {
	cfg   RouterConfig
	log   *slog.Logger
	locks *UserLocker // shared with the AgentRouter + auth broker so a user's runs serialize
}

// lockUser acquires the per-user run lock, returning the unlock func.
func (r *Router) lockUser(userID string) func() {
	return r.locks.Lock(userID)
}

// Locks returns the shared per-user locker so the auth broker can serialize its
// agent-state/profile writes (SetKey/logout) with agent runs on the same lock.
func (r *Router) Locks() *UserLocker { return r.locks }

// NewRouter builds a Router. Approval defaults to ApprovalAlways (fail safe).
func NewRouter(cfg RouterConfig) *Router {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Approval == "" {
		cfg.Approval = ApprovalAlways
	}
	return &Router{cfg: cfg, log: cfg.Log, locks: &UserLocker{}}
}

// Handle routes one tool call for a user/conversation and returns the result fed
// back to the model. It never returns a Go error for an expected denial/failure —
// those come back in ToolResult.Err so the model can recover; the error return is
// reserved for a context cancellation the loop must treat as an interrupt.
func (r *Router) Handle(ctx context.Context, scope Scope, call ToolCall) (ToolResult, error) {
	env, err := r.environment(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load sandbox policy"}, nil
	}
	if !env.ToolAllowed(call.Name) {
		r.audit(scope, call.Name, "", "blocked", store.SandboxRun{}, "tool not permitted")
		return ToolResult{Err: fmt.Sprintf("tool %q is not permitted by your Sandbox Environment policy", call.Name)}, nil
	}

	op, err := buildOp(call)
	if err != nil {
		return ToolResult{Err: err.Error()}, nil
	}
	// Build a redactor from ALL the user's vaulted secret values (NOT just granted
	// ones — redaction is decoupled from injection so a secret left in the workspace
	// stays scrubbed after its grant is removed) and redact the summary BEFORE any
	// audit row that includes it. The env INJECTION is gated/grant-scoped separately
	// and materialized AFTER approval (see below).
	summaryRedactor, err := r.cfg.Secrets.AllValuesRedactor(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load secrets for redaction"}, nil
	}
	summary := summaryRedactor.Redact(op.summary)

	// Enforce the network allow-list BEFORE touching the runner: a network-explicit
	// tool that targets a host:port the policy doesn't allow is rejected. Both the
	// audited command AND the model-visible error are redacted (the host can carry a
	// secret value too), so a denied probe leaks nothing.
	if denied := r.deniedNetwork(scope, call.Name, op, summary, summaryRedactor, env, "network not permitted"); denied != nil {
		return *denied, nil
	}

	workspace, err := r.cfg.Workspaces.UserWorkspace(scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not prepare workspace"}, nil
	}

	callID := id.New("tcl")
	requested := map[string]any{
		"call_id": callID, "tool": call.Name, "summary": summary,
		"needs_approval": r.cfg.Approval == ApprovalAlways,
	}

	decision := "auto"
	if r.cfg.Approval == ApprovalAlways {
		if r.cfg.Approver == nil {
			return ToolResult{Err: "tool approval is required but no approver is configured"}, nil
		}
		// The approver registers the pending decision, THEN onRegistered emits the
		// request event (so the client can't decide before the registry is ready),
		// THEN it blocks for the decision.
		approved, aerr := r.cfg.Approver.Approve(ctx, ApprovalRequest{
			CallID: callID, UserID: scope.UserID, ConversationID: scope.ConversationID,
			Tool: call.Name, Summary: summary,
		}, func() { r.emit(scope, events.TypeToolCallRequested, requested) })
		if aerr != nil {
			// Emit a terminal event so any live approval card clears.
			r.emit(scope, events.TypeToolCallCompleted, map[string]any{
				"call_id": callID, "tool": call.Name, "ok": false, "decision": "interrupted",
			})
			if ctx.Err() != nil {
				return ToolResult{}, ctx.Err() // interrupt: let the loop preserve partial state
			}
			return ToolResult{Err: "approval failed: " + aerr.Error()}, nil
		}
		if !approved {
			decision = "denied"
			// Re-redact with the current secret set before persisting — a secret may
			// have been added during the wait, and the denied row must not leak it. If
			// the fresh read fails, persist a generic command rather than the raw summary.
			denySummary := "[redaction unavailable]"
			if red, rerr := r.runRedactor(ctx, scope.UserID, summaryRedactor); rerr == nil {
				denySummary = red.Redact(op.summary)
			}
			r.audit(scope, call.Name, "", decision, store.SandboxRun{Command: denySummary}, "")
			r.emit(scope, events.TypeToolCallCompleted, map[string]any{
				"call_id": callID, "tool": call.Name, "ok": false, "decision": decision,
			})
			return ToolResult{Err: "the user denied this tool call"}, nil
		}
		decision = "approved"
	} else {
		// Auto mode: no approval gate, but still surface the call live for observability.
		r.emit(scope, events.TypeToolCallRequested, requested)
	}

	// Serialize a user's tool runs so the workspace-quota preflight is meaningful
	// (concurrent runs can't both pass it and then both write past the limit) and
	// the audit/quota state stays consistent.
	unlock := r.lockUser(scope.UserID)
	defer unlock()

	// Re-load the policy UNDER THE LOCK, so a tool/network/secret change during the
	// approval or queue window is honored — a revoked tool, host, or secret is never
	// run with stale, pre-approval state.
	env, err = r.environment(ctx, scope.UserID)
	if err != nil {
		return ToolResult{Err: "could not load sandbox policy"}, nil
	}
	// Build the run-time redactor over the UNION of all the user's secret values at
	// call time AND at run time (a secret deleted during the window is in the former,
	// one added is in the latter, an un-granted-but-vaulted one is in both), and
	// RE-REDACT the summary with it BEFORE any late audit row — so a secret ADDED
	// during the window whose value sits in op.summary never lands in sandbox_runs.
	redactor, err := r.runRedactor(ctx, scope.UserID, summaryRedactor)
	if err != nil {
		// Fail closed: without a current redactor we can't guarantee a run's output/
		// audit won't leak a secret, so refuse rather than proceed with a stale one.
		return ToolResult{Err: "could not load secrets for redaction; refusing to run the tool"}, nil
	}
	summary = redactor.Redact(op.summary)

	if !env.ToolAllowed(call.Name) {
		r.audit(scope, call.Name, "", "blocked", store.SandboxRun{Command: summary}, "tool no longer permitted")
		return ToolResult{Err: fmt.Sprintf("tool %q is no longer permitted by your Sandbox Environment policy", call.Name)}, nil
	}
	if denied := r.deniedNetwork(scope, call.Name, op, summary, redactor, env, "network no longer permitted"); denied != nil {
		return *denied, nil
	}
	// A model-supplied argument carrying a secret value must NOT be launched on the
	// host sbx process command line (visible via ps/process accounting/logs). Use an
	// explicit substring check (NOT "redaction changed the string" — a secret whose
	// value equals the marker would slip that) and fail closed if any argv element
	// embeds a secret value.
	for _, a := range op.argv {
		if redactor.ContainsSecret(a) {
			r.audit(scope, call.Name, "", "blocked", store.SandboxRun{Command: summary}, "argument contains a secret value")
			return ToolResult{Err: "refusing to run: a tool argument contains a secret value (it would appear on the host command line)"}, nil
		}
	}
	// Materialize the AUTHORITATIVE injection now, from the re-loaded grants — a
	// secret deleted or a grant removed during the window is therefore not injected.
	// The env injection is gated on network_isolated (fail closed against shell
	// egress exfiltration); redaction (above) is decoupled from it.
	injection, err := r.cfg.Secrets.Materialize(ctx, scope.UserID, toEnvGrants(env.SecretGrants))
	if err != nil {
		return ToolResult{Err: "could not resolve granted secrets: " + err.Error()}, nil
	}
	secretEnv := r.injectable(env, injection)

	// Quota preflight under the per-user lock. Fail CLOSED: a scan error (e.g. a
	// root-owned/unreadable entry a prior run created) refuses the run rather than
	// silently allowing unbounded growth. NOTE: this still bounds growth only across
	// runs — it cannot stop a single command from writing past the quota mid-run; a
	// hard limit needs filesystem/volume quotas (deferred to the storage work).
	if r.cfg.QuotaBytes > 0 {
		ok, used, qerr := r.cfg.Workspaces.WithinQuota(scope.UserID, r.cfg.QuotaBytes)
		if qerr != nil {
			r.log.Warn("sandbox: workspace quota scan failed; refusing to run", "user", scope.UserID, "err", qerr)
			return ToolResult{Err: "could not verify your workspace quota; refusing to run the tool"}, nil
		}
		if !ok {
			return ToolResult{Err: fmt.Sprintf("workspace quota exceeded (%d bytes used)", used)}, nil
		}
	}

	// Pre-insert a PENDING audit row BEFORE running. If we can't record the run, we
	// refuse to run it — a side-effecting command must never execute without a
	// durable audit trail (fail closed). The row is stamped with a non-success
	// sentinel (exit -1 + "pending") so a crash/finalize-failure leaves a row that is
	// clearly NOT a successful zero-output run; finalizeRun overwrites both.
	auditID := id.New("sbr")
	if err := r.insertRun(store.SandboxRun{
		ID: auditID, UserID: scope.UserID, ConversationID: scope.ConversationID,
		Tool: call.Name, Decision: decision, Command: summary,
		ExitCode: -1, Error: pendingAuditMarker,
	}); err != nil {
		r.log.Error("sandbox: could not record pending audit row; refusing to run", "err", err)
		return ToolResult{Err: "could not record this tool call for audit; refusing to run it"}, nil
	}

	spec := RunSpec{
		UserID:         scope.UserID,
		ConversationID: scope.ConversationID,
		Tool:           call.Name,
		Argv:           op.argv,
		Stdin:          op.stdin,
		SecretEnv:      secretEnv,
		Redactor:       redactor,
		Workspace:      workspace,
		Network:        op.network,
		Limits:         r.cfg.Limits,
	}
	res, runErr := r.cfg.Runner.Run(ctx, spec)
	// Recheck quota after the run so an over-quota write is at least surfaced (and
	// the next run is denied by the preflight above).
	if ok, used, qerr := r.cfg.Workspaces.WithinQuota(scope.UserID, r.cfg.QuotaBytes); qerr == nil && !ok {
		r.log.Warn("sandbox: workspace over quota after run", "user", scope.UserID, "used", used, "quota", r.cfg.QuotaBytes)
	}

	// Finalize the audit row with the outcome. The command already ran, so a finalize
	// failure is surfaced (logged loudly) rather than dropped — the pending row still
	// records that the run happened.
	auditErr := ""
	if res.Err != nil {
		auditErr = redactor.Redact(res.Err.Error())
	}
	if res.CaptureErr != "" {
		auditErr = joinErr(auditErr, "output capture failed: "+res.CaptureErr)
	}
	if err := r.finalizeRun(store.SandboxRun{
		ID: auditID, SandboxID: res.SandboxID, ExitCode: res.ExitCode,
		DurationMs: res.Duration.Milliseconds(), StdoutPath: res.StdoutPath,
		StderrPath: res.StderrPath, Error: auditErr,
	}); err != nil {
		r.log.Error("sandbox: failed to finalize audit row after a side-effecting run", "id", auditID, "err", err)
	}

	if runErr != nil {
		return ToolResult{Err: redactor.Redact(runErr.Error())}, nil
	}
	r.emit(scope, events.TypeToolCallCompleted, map[string]any{
		"call_id": callID, "tool": call.Name, "ok": res.Err == nil && res.ExitCode == 0,
		"exit_code": res.ExitCode, "decision": decision,
	})

	if res.Err != nil {
		return ToolResult{Err: redactor.Redact(res.Err.Error())}, nil
	}
	return ToolResult{Content: formatOutput(res.ExitCode, redactor.Redact(res.Stdout), redactor.Redact(res.Stderr))}, nil
}

// environment loads the user's Sandbox Environment policy via LoadEnvironment.
func (r *Router) environment(ctx context.Context, userID string) (Environment, error) {
	return LoadEnvironment(ctx, r.cfg.Store, userID)
}

// stateLoader is the subset of RouterStore that LoadEnvironment needs.
type stateLoader interface {
	GetSandboxState(ctx context.Context, userID, kind string) (store.SandboxState, error)
}

// LoadEnvironment returns a user's stored Sandbox Environment policy, falling back
// to the conservative default when none is stored. The HTTP layer and the Router
// both use it so a user's view and the enforced policy never diverge.
func LoadEnvironment(ctx context.Context, st stateLoader, userID string) (Environment, error) {
	row, err := st.GetSandboxState(ctx, userID, StateKind)
	if errors.Is(err, store.ErrNotFound) {
		return DefaultEnvironment(), nil
	}
	if err != nil {
		return Environment{}, err
	}
	var env Environment
	if uerr := json.Unmarshal([]byte(row.Data), &env); uerr != nil {
		return Environment{}, uerr
	}
	return env.Normalize(), nil
}

// auditCtx returns a fresh bounded context decoupled from the (possibly cancelled)
// turn context, so an audit write always completes for a run that already happened.
func auditCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// insertRun inserts an audit row and returns the error (used for the fail-closed
// pre-insert before a side-effecting run).
func (r *Router) insertRun(run store.SandboxRun) error {
	ctx, cancel := auditCtx()
	defer cancel()
	return r.cfg.Store.InsertSandboxRun(ctx, run)
}

// finalizeRun updates a pending audit row with the run outcome and returns the error.
func (r *Router) finalizeRun(run store.SandboxRun) error {
	ctx, cancel := auditCtx()
	defer cancel()
	return r.cfg.Store.UpdateSandboxRun(ctx, run)
}

// joinErr concatenates two error strings with "; " (dropping empties).
func joinErr(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// audit records a no-side-effect decision row (blocked tool / denied approval /
// denied network) best-effort. Once a side-effecting command has run, the
// pre-insert + finalizeRun path is used instead. Secret values are already redacted
// in Command/Error.
func (r *Router) audit(scope Scope, tool, sandboxID, decision string, run store.SandboxRun, runErr string) {
	run.ID = id.New("sbr")
	run.UserID = scope.UserID
	run.ConversationID = scope.ConversationID
	run.Tool = tool
	run.Decision = decision
	if run.SandboxID == "" {
		run.SandboxID = sandboxID
	}
	if runErr != "" {
		run.Error = runErr
	}
	ctx, cancel := auditCtx()
	defer cancel()
	if err := r.cfg.Store.InsertSandboxRun(ctx, run); err != nil {
		r.log.Warn("sandbox: audit insert failed", "err", err)
	}
}

// emit fans out a LIVE tool-call event on the conversation stream (best-effort,
// not persisted/replayed — see EventPublisher).
func (r *Router) emit(scope Scope, typ string, payload map[string]any) {
	if r.cfg.Bus == nil || scope.ConversationID == "" {
		return
	}
	ev, err := events.New(events.SourceSandbox, typ, scope.ConversationID, scope.UserID, "", payload)
	if err != nil {
		return
	}
	r.cfg.Bus.PublishEphemeral(ev)
}

// op is the resolved, validated plan for one tool call.
type op struct {
	argv    []string
	stdin   []byte
	network []NetworkRule
	summary string
}

// buildOp parses a tool call's arguments into a sandbox run plan. It validates
// paths stay inside the workspace and that URLs are well-formed, so a malformed
// model argument is rejected before any sandbox work.
func buildOp(call ToolCall) (op, error) {
	switch call.Name {
	case ToolShell:
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || strings.TrimSpace(a.Command) == "" {
			return op{}, fmt.Errorf("shell: a non-empty 'command' string is required")
		}
		return op{argv: []string{"/bin/sh", "-lc", a.Command}, summary: "shell: " + a.Command}, nil

	case ToolFSRead:
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return op{}, fmt.Errorf("fs_read: invalid arguments")
		}
		rel, err := cleanRelPath(a.Path)
		if err != nil {
			return op{}, fmt.Errorf("fs_read: %w", err)
		}
		return op{argv: []string{"cat", "--", rel}, summary: "read " + rel}, nil

	case ToolFSWrite:
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return op{}, fmt.Errorf("fs_write: invalid arguments")
		}
		rel, err := cleanRelPath(a.Path)
		if err != nil {
			return op{}, fmt.Errorf("fs_write: %w", err)
		}
		// `tee` writes stdin to the file (inside the sandbox); content comes via Stdin
		// so it is never part of the argv/audit summary.
		return op{
			argv:    []string{"tee", "--", rel},
			stdin:   []byte(a.Content),
			summary: fmt.Sprintf("write %s (%d bytes)", rel, len(a.Content)),
		}, nil

	case ToolHTTP:
		var a struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return op{}, fmt.Errorf("http_fetch: invalid arguments")
		}
		host, port, err := parseHTTPTarget(a.URL)
		if err != nil {
			return op{}, fmt.Errorf("http_fetch: %w", err)
		}
		return op{
			argv:    []string{"curl", "-fsS", "--max-time", "30", "--", a.URL},
			network: []NetworkRule{{Host: host, Port: port}},
			summary: "fetch " + a.URL,
		}, nil

	default:
		return op{}, fmt.Errorf("unknown tool %q", call.Name)
	}
}

// cleanRelPath validates a tool-supplied path stays within the workspace: it must
// be relative and must not escape via "..".
func cleanRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", fmt.Errorf("path must be relative to the workspace")
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return clean, nil
}

// parseHTTPTarget extracts the host and port from an http(s) URL for the network
// allow-list check.
func parseHTTPTarget(raw string) (string, int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", 0, fmt.Errorf("url scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("url has no host")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port")
		}
		port = n
	}
	return host, port, nil
}

// formatOutput renders a run's exit + redacted output as the model-visible result.
func formatOutput(exit int, stdout, stderr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit %d", exit)
	if s := strings.TrimRight(stdout, "\n"); s != "" {
		b.WriteString("\nstdout:\n" + s)
	}
	if s := strings.TrimRight(stderr, "\n"); s != "" {
		b.WriteString("\nstderr:\n" + s)
	}
	return b.String()
}

// runRedactor builds the redaction-time redactor: the union of the pre-approval
// values (base) and all the user's CURRENT vaulted values, so output and audit
// strings stay scrubbed across grant/secret changes during the approval window. It
// FAILS CLOSED — a load error is returned, not swallowed, so a caller never proceeds
// with a stale redactor that could miss a secret added during the window.
func (r *Router) runRedactor(ctx context.Context, userID string, base *vault.Redactor) (*vault.Redactor, error) {
	cur, err := r.cfg.Secrets.AllValuesRedactor(ctx, userID)
	if err != nil {
		return nil, err
	}
	return base.Merge(cur), nil
}

// deniedNetwork checks op's network targets against env. If one is not permitted it
// records a "blocked" audit row and returns the denial result, with the FULLY
// composed command + error passed through the redactor — so neither a secret in the
// host NOR a numeric secret in the port can leak. Returns nil when all targets are
// permitted.
func (r *Router) deniedNetwork(scope Scope, tool string, o op, summary string, red *vault.Redactor, env Environment, reason string) *ToolResult {
	for _, rule := range o.network {
		if !env.NetworkAllowed(rule.Host, rule.Port) {
			command := red.Redact(fmt.Sprintf("%s (denied: %s:%d)", summary, rule.Host, rule.Port))
			errMsg := red.Redact(fmt.Sprintf("network access to %s:%d is not permitted (add it to your Sandbox Environment network allow-list)", rule.Host, rule.Port))
			r.audit(scope, tool, "", "blocked", store.SandboxRun{Command: command}, reason)
			res := ToolResult{Err: errMsg}
			return &res
		}
	}
	return nil
}

// injectable returns the env pairs to place into the run: the materialized
// secrets when the operator has asserted the sandbox network is isolated, or NONE
// otherwise (fail closed — a secret is never placed in a sandbox whose egress Hina
// can't gate, even though its value is still known to the redactor for scrubbing).
func (r *Router) injectable(env Environment, inj *vault.Injection) []string {
	if !r.cfg.NetworkIsolated {
		if len(env.SecretGrants) > 0 {
			r.log.Warn("sandbox: not injecting granted secrets ([sandbox] network_isolated is false; can't guarantee no egress)")
		}
		return nil
	}
	return inj.EnvPairs()
}

// toEnvGrants converts policy SecretGrants to vault EnvGrants.
func toEnvGrants(grants []SecretGrant) []vault.EnvGrant {
	out := make([]vault.EnvGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, vault.EnvGrant{SecretID: g.SecretID, EnvName: g.EnvName})
	}
	return out
}
