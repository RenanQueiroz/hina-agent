package autorun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// secretSource is the vault surface the executor needs: a redactor over the owner's
// vaulted secret values, a run-scoped injection of granted secrets, and the list to
// map secret_ref names to ids/env vars. *vault.Vault satisfies it.
type secretSource interface {
	AllValuesRedactor(ctx context.Context, userID string) (*vault.Redactor, error)
	Materialize(ctx context.Context, userID string, grants []vault.EnvGrant) (*vault.Injection, error)
	List(ctx context.Context, userID string) ([]store.SecretMeta, error)
}

// agentInvoker is the Phase 8 AgentRouter surface the executor reuses for agent_cli
// steps, so an automation's callable-agent runs go through the exact same hardened
// credential/redaction/audit boundary as an interactive agent call — but workspaced to
// the automation's own ephemeral run scratch and auto-approved (unattended).
// *sandbox.AgentRouter satisfies it.
type agentInvoker interface {
	HandleAutomation(ctx context.Context, scope sandbox.Scope, call sandbox.ToolCall, opts sandbox.AgentRunOptions) (sandbox.ToolResult, error)
}

// runAuditor is the sandbox_runs audit surface for deterministic tool steps (which run
// directly via the sbx Runner, not the interactive Router). A pending row is written
// BEFORE the side-effecting command and finalized after — so a crash mid-command leaves a
// durable record, and that audit survives the automation's deletion (it's keyed by user,
// not automation). *store.Store satisfies it.
type runAuditor interface {
	InsertSandboxRun(ctx context.Context, r store.SandboxRun) error
	UpdateSandboxRun(ctx context.Context, r store.SandboxRun) error
}

// ExecConfig wires the executor's shared dependencies (set once on the Service).
type ExecConfig struct {
	Runner          sandbox.Runner
	Secrets         secretSource
	Agents          agentInvoker
	Provider        llm.Provider
	Audit           runAuditor
	NetworkIsolated bool
	Limits          sandbox.Limits
	Log             *slog.Logger
}

// runExecutor implements automation.Executor for ONE run: it is bound to that run's
// ephemeral workspace so deterministic tool steps (and agent_cli steps) share a
// checkout across steps and never touch the owner's durable workspace.
type runExecutor struct {
	cfg       ExecConfig
	workspace string // the run's ephemeral scratch dir (host path mounted at /workspace)
	// agentStateRoot roots an agent_cli run's credential/staging scratch. It's a separate
	// per-run dir the disk watchdog ALSO counts but that is NOT mounted at /workspace, so an
	// agent's RW credential store is bounded by the per-run cap yet hidden from sibling steps.
	agentStateRoot string
}

// Tool runs a deterministic tool step argv-first inside the run's sandbox workspace,
// with the owner's secrets redacted from output and (when network is isolated)
// granted secrets injected. It returns the tool's parsed structured output.
func (e *runExecutor) Tool(ctx context.Context, req automation.ToolStep) (automation.StepResult, error) {
	op, err := buildToolOp(req.Tool, req.With)
	if err != nil {
		return automation.StepResult{Failed: true, Err: err.Error()}, nil
	}
	// A shell-string shell.exec is gated on an unrestricted profile.
	if req.Tool == automation.ToolShellExec && shellExecNeedsUnrestricted(req.With) && req.Run.Profile.Mode != automation.ModeUnrestricted {
		return automation.StepResult{Failed: true, Err: "shell.exec with a command string requires an unrestricted sandbox profile (use argv)"}, nil
	}
	// Enforce the automation's permission profile at RUN time, not just at enable: the
	// tool must be in allowed_tools (granular), a networked op needs network=enabled, and
	// (granular) every CLI binary the op invokes must be in allowed_cli_tools. A denied
	// action never reaches the Runner.
	if gate := enforceProfile(req.Run.Profile, req.Tool, op); gate != "" {
		return automation.StepResult{Failed: true, Err: gate}, nil
	}
	// A network-capable deterministic tool egresses from the sbx container; require the
	// operator to have asserted that egress is controlled (network_isolated) — the same
	// fail-closed gate agents + secret-bearing runs get, and what the http.request SSRF
	// guard's DNS-hostname backstop depends on. Mirrors the enable-time eligibility check.
	if (len(op.network) > 0 || op.networkCapable) && !e.cfg.NetworkIsolated {
		return automation.StepResult{Failed: true, Err: "this tool makes network requests; set [sandbox] network_isolated=true to run it"}, nil
	}

	redactor, err := e.cfg.Secrets.AllValuesRedactor(ctx, req.Run.UserID)
	if err != nil {
		return automation.StepResult{}, fmt.Errorf("could not load secrets for redaction")
	}
	// Materialize the granted secrets FIRST and fold the injection's own redactor (built
	// from the SAME read as the injected values) into the run redactor BEFORE the argv
	// guard and the run — so a secret rotated (delete+recreate) between the all-values
	// read above and this materialization is still redacted from output/artifacts and
	// still refused on the argv. (Closes the redactor-snapshot race.)
	secretEnv, injRedactor, err := e.injectableSecrets(ctx, req.Run)
	if err != nil {
		return automation.StepResult{}, err
	}
	if injRedactor != nil {
		redactor = redactor.Merge(injRedactor)
	}
	// A secret value in the argv would land on the host command line — fail closed, checked
	// against the MERGED redactor (covers a just-injected rotated value too). ContainsSecretText
	// also catches a secret in its JSON-escaped form (a PEM/token a template rendered into the
	// argv via a json.Marshal'd value), which a plaintext-only check would miss.
	for _, a := range op.argv {
		if redactor.ContainsSecretText(a) {
			return automation.StepResult{Failed: true, Err: "refusing to run: a tool argument contains a secret value"}, nil
		}
	}
	// The body (op.stdin: an http.request body, a github.pr_comment body) is sent to the
	// target too — guard it with the SAME merged redactor before audit/launch. ContainsSecretText
	// catches plaintext + a JSON-escaped value; JSONContainsSecret additionally decodes a JSON
	// body and matches a secret under any encoding. Fail closed so a secret never egresses.
	if len(op.stdin) > 0 && (redactor.ContainsSecretText(string(op.stdin)) || redactor.JSONContainsSecret(op.stdin)) {
		return automation.StepResult{Failed: true, Err: "refusing to run: a tool body contains a secret value"}, nil
	}

	// Honor a tool step's workspace_from (the engine resolved it into req.Workspace, e.g. a
	// prior github.pr_checkout's "/workspace/pr-42"): run the command in that subdir, validated
	// to stay under the run's /workspace mount. Empty -> the runner's default (/workspace). This
	// closes the bug where a resolved workspace_from was accepted but silently ignored, running
	// shell.exec against the automation root instead of the checkout.
	workdir, werr := containerWorkdir(req.Workspace)
	if werr != nil {
		return automation.StepResult{Failed: true, Err: werr.Error()}, nil
	}
	spec := sandbox.RunSpec{
		UserID:    req.Run.UserID,
		Tool:      req.Tool,
		Argv:      op.argv,
		Stdin:     op.stdin,
		SecretEnv: secretEnv,
		Redactor:  redactor,
		Workspace: e.workspace,
		Workdir:   workdir,
		Network:   op.network,
		Limits:    e.limitsFor(req.Run.Profile),
	}
	// Write a PENDING sandbox_runs audit row before the side-effecting command (fail closed
	// if it can't be recorded), mirroring the interactive Router. This makes a deterministic
	// tool's side effect durably auditable even on a crash mid-command, and the audit lives
	// in sandbox_runs (keyed by user) so it survives the automation's deletion.
	auditID, aerr := e.beginToolAudit(req.Run.UserID, req.Tool, redactor.RedactText(req.Tool+" "+strings.Join(op.argv, " ")))
	if aerr != nil {
		return automation.StepResult{Failed: true, Err: "could not record the tool audit row; refusing to run"}, nil
	}

	res, runErr := e.cfg.Runner.Run(ctx, spec)
	e.finalizeToolAudit(auditID, res, runErr, redactor)
	if runErr != nil {
		if ctx.Err() != nil {
			return automation.StepResult{}, ctx.Err()
		}
		return automation.StepResult{Failed: true, Err: redactor.Redact(runErr.Error())}, nil
	}
	logLine := fmt.Sprintf("%s exit=%d", req.Tool, res.ExitCode)
	// A sandbox-LAYER failure (sbx unavailable, spawn error) is reported in RunResult.Err
	// with a nil Go error and often ExitCode 0 — fail the step BEFORE parsing stdout, so a
	// tool whose parser accepts empty output can't be recorded as a successful side effect.
	if res.Err != nil {
		return automation.StepResult{Failed: true, Err: redactor.Redact(res.Err.Error()), Log: logLine}, nil
	}
	// A capture failure means the run's output is incomplete/unreliable — a deterministic
	// decision (e.g. "notifications empty") must not be made on truncated output.
	if res.CaptureErr != "" {
		return automation.StepResult{Failed: true, Err: "tool output could not be captured: " + redactor.Redact(res.CaptureErr), Log: logLine}, nil
	}
	if res.ExitCode != 0 {
		return automation.StepResult{
			Failed: true,
			Err:    redactor.Redact(fmt.Sprintf("%s exited %d: %s", req.Tool, res.ExitCode, firstLine(res.Stderr))),
			Log:    logLine,
		}, nil
	}
	// Parse the COMPLETE output, not res.Stdout — that is the 64 KiB model-display inline
	// stream (with a truncation suffix), so parsing it could feed a later step a partial body
	// and let it make an irreversible decision on incomplete data. Fail closed if even the
	// (≤1 MiB) capture was truncated; otherwise read the full redacted capture file, which the
	// runner wrote with the SAME redaction as the inline stream.
	if res.StdoutTruncated {
		return automation.StepResult{Failed: true, Err: "tool output exceeded the capture limit; refusing to act on truncated output", Log: logLine}, nil
	}
	stdout := res.Stdout
	if res.StdoutPath != "" {
		full, rerr := os.ReadFile(res.StdoutPath)
		if rerr != nil {
			return automation.StepResult{Failed: true, Err: "could not read the captured tool output; refusing to act on incomplete output", Log: logLine}, nil
		}
		stdout = string(full)
	}
	out, perr := op.parse(stdout)
	if perr != nil {
		return automation.StepResult{Failed: true, Err: perr.Error(), Log: logLine}, nil
	}
	return automation.StepResult{Output: out, Log: logLine}, nil
}

// Agent runs an agent_cli step through the Phase 8 AgentRouter (owner-scoped), so it
// inherits the full credential/redaction/audit/fail-closed boundary. The run is
// scoped to the owner and the run id (audit correlation); the AgentRouter resolves
// and mounts the owner's encrypted agent credential for exactly one run.
//
// The run executes in the automation's OWN ephemeral run scratch (e.workspace),
// never the user's durable workspace, and auto-approves (unattended). workspace_from
// (resolved into req.Workspace as a container path produced by a prior checkout) is
// mapped to the agent's workdir, validated to stay under /workspace.
func (e *runExecutor) Agent(ctx context.Context, req automation.AgentStep) (automation.StepResult, error) {
	if e.cfg.Agents == nil {
		return automation.StepResult{Failed: true, Err: "callable agents are not enabled on this server"}, nil
	}
	// An agent run reaches its provider — honor the profile's network posture (unrestricted
	// still honors network:disabled). Fail closed before launching.
	if !automation.NetworkAllowed(req.Run.Profile.Mode, req.Run.Profile.Network) {
		return automation.StepResult{Failed: true, Err: "agent runs need network access but the automation's sandbox network is not enabled"}, nil
	}
	workdir, werr := containerWorkdir(req.Workspace)
	if werr != nil {
		return automation.StepResult{Failed: true, Err: werr.Error()}, nil
	}
	args := map[string]any{"prompt": req.Prompt}
	if req.Model != "" {
		args["model"] = req.Model
	}
	if req.MaxTurns > 0 {
		args["max_turns"] = req.MaxTurns
	}
	if len(req.Schema) > 0 {
		args["structured"] = true
		args["schema"] = json.RawMessage(req.Schema)
	}
	raw, _ := json.Marshal(args)
	scope := sandbox.Scope{UserID: req.Run.UserID, ConversationID: req.Run.RunID}
	call := sandbox.ToolCall{ID: id.New("acl"), Name: automation.AgentToolName(req.Adapter), Arguments: raw}
	// The automation's sandbox.resources cap the agent run too (not just the global
	// server agent limits).
	opts := sandbox.AgentRunOptions{Workspace: e.workspace, Workdir: workdir, AutoApprove: true, Limits: e.limitsFor(req.Run.Profile), StateRoot: e.agentStateRoot}
	res, err := e.cfg.Agents.HandleAutomation(ctx, scope, call, opts)
	if err != nil {
		if ctx.Err() != nil {
			return automation.StepResult{}, ctx.Err()
		}
		return automation.StepResult{Failed: true, Err: err.Error()}, nil
	}
	if res.Err != "" {
		return automation.StepResult{Failed: true, Err: res.Err, Log: "agent " + req.Adapter + " failed"}, nil
	}
	// The AgentRouter returns the normalized result as its Content (already redacted).
	// Decode it as JSON when possible so selectors can reach structured fields; else
	// keep it as final_text.
	out := decodeAgentContent(res.Content)
	return automation.StepResult{Output: out, Log: "agent " + req.Adapter + " ok"}, nil
}

// LLM runs an aggregation/verification model call: it builds a context from the
// prompt + the resolved inputs and streams a reply. Structured output is best-effort
// (the provider abstraction has no strict-schema mode): when a schema is set the
// prompt asks for matching JSON and a parseable reply's fields are merged in.
func (e *runExecutor) LLM(ctx context.Context, req automation.LLMStep) (automation.StepResult, error) {
	if e.cfg.Provider == nil {
		return automation.StepResult{Failed: true, Err: "no LLM provider is configured"}, nil
	}
	// An llm step reaches the model provider (possibly a cloud backend) — honor the
	// profile's network posture AND the operator's network_isolated assertion, failing closed
	// BEFORE assembling/sending the prompt. It is an outward data flow from an unattended run,
	// so it gets the same fail-closed gate as networked tools + agent runs.
	if !automation.NetworkAllowed(req.Run.Profile.Mode, req.Run.Profile.Network) {
		return automation.StepResult{Failed: true, Err: "llm steps need network access but the automation's sandbox network is not enabled"}, nil
	}
	if !e.cfg.NetworkIsolated {
		return automation.StepResult{Failed: true, Err: "llm steps reach the model provider; set [sandbox] network_isolated=true to run them"}, nil
	}
	// Load the owner's all-values redactor up front to GUARD the outbound payload: an llm step
	// sends its prompt + inputs to the (possibly cloud) provider via a server-side call that
	// the sandbox network isolation can't gate, so a vaulted secret value reaching the prompt
	// (a hardcoded prompt_template, or a prior step's output) must not be exfiltrated.
	var redactor *vault.Redactor
	if e.cfg.Secrets != nil {
		r, rerr := e.cfg.Secrets.AllValuesRedactor(ctx, req.Run.UserID)
		if rerr != nil {
			return automation.StepResult{Failed: true, Err: "could not load secrets to guard the llm prompt; refusing to run"}, nil
		}
		redactor = r
	}
	var sys strings.Builder
	sys.WriteString(req.Prompt)
	if len(req.Schema) > 0 {
		sys.WriteString("\n\nReturn ONLY a JSON object matching this schema:\n")
		sys.Write(req.Schema)
	}
	var inputsJSON string
	if len(req.Inputs) > 0 {
		// Bound the assembled inputs BEFORE marshaling the whole payload, so a definition
		// that repeats a reference to a large prior output thousands of times can't allocate
		// a huge prompt or run up model cost — fail the step instead.
		j, ierr := boundedInputsJSON(req.Inputs, req.MaxOutputBytes)
		if ierr != nil {
			return automation.StepResult{Failed: true, Err: ierr.Error()}, nil
		}
		inputsJSON = j
	}
	// Fail closed BEFORE contacting the provider if the prompt, schema, or inputs carry a
	// secret value (the same refusal a tool/agent gets when an argument carries one). The schema
	// + inputs are JSON, so use JSONContainsSecret — it decodes them and matches a secret under
	// ANY valid JSON escaping (a PEM key's `\n`, a credential's `\"`/`\\`), which a plaintext
	// search over the assembled bytes would miss. The prompt text is checked plaintext.
	if redactor != nil && (redactor.ContainsSecretText(req.Prompt) ||
		redactor.JSONContainsSecret(req.Schema) ||
		(inputsJSON != "" && redactor.JSONContainsSecret([]byte(inputsJSON)))) {
		return automation.StepResult{Failed: true, Err: "refusing to run: the llm prompt or inputs contain a secret value", Log: "llm blocked: secret in payload"}, nil
	}
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: sys.String()}}
	if inputsJSON != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "Inputs:\n" + inputsJSON})
	} else {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: "Proceed."})
	}
	text, truncated, err := streamText(ctx, e.cfg.Provider, msgs, req.MaxOutputBytes)
	if err != nil {
		if ctx.Err() != nil {
			return automation.StepResult{}, ctx.Err()
		}
		return automation.StepResult{Failed: true, Err: err.Error()}, nil
	}
	// Fail closed on truncation: an unattended automation must not expose a partial model
	// response to later steps (a body_from/comment/decision would act on incomplete output) —
	// the same boundary the tool + agent_cli capture paths enforce.
	if truncated {
		return automation.StepResult{Failed: true, Err: "the model response exceeded the output cap; refusing to act on truncated output", Log: "llm truncated"}, nil
	}
	out := map[string]any{"text": text, "markdown": text}
	if len(req.Schema) > 0 {
		if parsed := tryParseJSONObject(text); parsed != nil {
			for k, v := range parsed {
				out[k] = v
			}
		}
	}
	return automation.StepResult{Output: out, Log: "llm merged"}, nil
}

// injectableSecrets resolves the automation's granted secret_refs into env pairs AND
// the redactor over exactly those materialized values (so the caller can merge it
// into the run redactor from the same read — closing the rotation race). Injection is
// gated on network isolation (fail closed — a secret is never placed in a sandbox
// whose egress Hina can't gate). Returns nil,nil when not isolated or no grants.
func (e *runExecutor) injectableSecrets(ctx context.Context, run automation.RunInfo) ([]string, *vault.Redactor, error) {
	if len(run.Profile.SecretRefs) == 0 {
		return nil, nil, nil
	}
	// Fail closed: a run that DECLARES secret_refs must not proceed without controlled
	// egress (the eligibility check normally blocks this; this is the authoritative gate).
	if !e.cfg.NetworkIsolated {
		return nil, nil, fmt.Errorf("granted secrets require [sandbox] network_isolated=true; refusing to run")
	}
	grants, err := e.secretGrants(ctx, run.UserID, run.Profile.SecretRefs)
	if err != nil {
		return nil, nil, err
	}
	// secretGrants is strict: one grant per ref, unique safe env names. (Defensive.)
	if len(grants) != len(run.Profile.SecretRefs) {
		return nil, nil, fmt.Errorf("granted secrets could not all be resolved")
	}
	inj, err := e.cfg.Secrets.Materialize(ctx, run.UserID, grants)
	if err != nil || inj == nil {
		return nil, nil, fmt.Errorf("could not resolve granted secrets")
	}
	pairs := inj.EnvPairs()
	// STRICT: every declared secret_ref must produce EXACTLY one injected env pair, with
	// the expected name — so a secret deleted between secretGrants and Materialize (a
	// TOCTOU that silently drops a grant) fails the run rather than running with a
	// missing credential.
	if len(pairs) != len(grants) {
		return nil, nil, fmt.Errorf("a granted secret changed during materialization (expected %d env vars, got %d)", len(grants), len(pairs))
	}
	want := map[string]bool{}
	for _, g := range grants {
		want[g.EnvName] = true
	}
	for _, p := range pairs {
		name := p
		if i := strings.IndexByte(p, '='); i >= 0 {
			name = p[:i]
		}
		if !want[name] {
			return nil, nil, fmt.Errorf("an injected secret env var did not match the granted set")
		}
	}
	return pairs, inj.Redactor(), nil
}

// enforceProfile applies the automation's sandbox profile to a resolved tool op at
// run time. It returns a non-empty error message when the op is not permitted (so it
// never reaches the Runner). Any mode that is not explicitly "unrestricted" is treated
// as granular: the tool must be in allowed_tools, and every CLI binary the op invokes
// must be in allowed_cli_tools. A networked op always requires network=enabled.
func enforceProfile(p automation.SandboxProfile, tool string, op toolOp) string {
	// The network gate applies in EVERY mode (unrestricted still honors network:disabled),
	// so a network-capable op can't egress when the declared posture forbids it.
	if (len(op.network) > 0 || op.networkCapable) && !automation.NetworkAllowed(p.Mode, p.Network) {
		return "this tool can make network requests but the automation's sandbox network is not enabled"
	}
	// SSRF guard (applies in EVERY mode, even unrestricted): a typed network op must never
	// target a loopback/link-local/cloud-metadata/private address — an unattended automation
	// can't be used to probe internal services. (A raw shell string's egress isn't parseable
	// here; that's bounded by the sbx container's own egress policy.)
	for _, rule := range op.network {
		if isInternalHostTarget(rule.Host) {
			return fmt.Sprintf("network target %q is a loopback/link-local/private address an automation may not reach", rule.Host)
		}
	}
	// The typed-tool + CLI allow-lists are granular-only (unrestricted permits any CLI).
	if p.Mode != automation.ModeUnrestricted {
		if !containsStr(p.AllowedTools, tool) {
			return fmt.Sprintf("tool %q is not in the automation's allowed_tools", tool)
		}
		for _, cli := range op.clis {
			if !containsStr(p.AllowedCLITools, cli) {
				return fmt.Sprintf("CLI %q is not in the automation's allowed_cli_tools", cli)
			}
		}
	}
	return ""
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// containerWorkdir maps a resolved workspace_from value (a container path a prior
// github.pr_checkout produced, e.g. "/workspace/pr-42") to the agent's workdir. It
// MUST stay under /workspace (the run scratch mount) — a reference resolving to any
// other path is rejected (fail closed) so an agent can't be pointed outside the run's
// own workspace. Empty means the workspace root.
func containerWorkdir(ws string) (string, error) {
	if ws == "" {
		return "", nil
	}
	clean := path.Clean(ws)
	if clean != "/workspace" && !strings.HasPrefix(clean, "/workspace/") {
		return "", fmt.Errorf("workspace_from %q must resolve to a path under /workspace (a checkout produced earlier in this run)", ws)
	}
	return clean, nil
}

// secretGrants maps the automation's secret_ref NAMES to EnvGrants (secret id +
// injected env-var name), resolving names against the owner's vaulted secrets.
func (e *runExecutor) secretGrants(ctx context.Context, userID string, refs []string) ([]vault.EnvGrant, error) {
	metas, err := e.cfg.Secrets.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("could not list secrets")
	}
	byName := map[string]string{}
	for _, m := range metas {
		byName[m.Name] = m.ID
	}
	// Strict mapping: every declared secret_ref must produce exactly one injected env
	// pair, under a unique, non-host-dangerous name — else a run would proceed with the
	// credential silently omitted or overwritten. Fail closed (the enable-time eligibility
	// check normally catches these; this closes the TOCTOU + is the authoritative gate).
	seenEnv := map[string]string{}
	var grants []vault.EnvGrant
	for _, ref := range refs {
		id, ok := byName[ref]
		if !ok {
			return nil, fmt.Errorf("granted secret %q no longer exists", ref)
		}
		env := automation.EnvVarName(ref)
		if sandbox.DangerousEnvName(env) {
			return nil, fmt.Errorf("granted secret %q maps to a host-dangerous env var %q", ref, env)
		}
		if prev, dup := seenEnv[env]; dup {
			return nil, fmt.Errorf("granted secrets %q and %q both map to env var %q", prev, ref, env)
		}
		seenEnv[env] = ref
		grants = append(grants, vault.EnvGrant{SecretID: id, EnvName: env})
	}
	return grants, nil
}

// limitsFor maps the automation profile's resources onto the runner limits. An
// automation may only LOWER the operator's per-run limits, never raise them: a requested
// value above the server-configured ceiling is CLAMPED to that ceiling, so an automation
// author can't ask sbx for arbitrarily large CPU/memory/PIDs and exhaust the host. When
// the operator left a limit unset (0/empty = unlimited), the automation's value is honored
// (it can only restrict itself).
func (e *runExecutor) limitsFor(p automation.SandboxProfile) sandbox.Limits {
	lim := e.cfg.Limits
	if p.Resources.CPUs > 0 {
		if maxCPU := parseCPUs(lim.CPUs); maxCPU <= 0 || float64(p.Resources.CPUs) <= maxCPU {
			lim.CPUs = strconv.Itoa(p.Resources.CPUs)
		} // else: requested > operator ceiling -> keep the ceiling (lim.CPUs)
	}
	if p.Resources.MemoryMB > 0 {
		if maxMB := parseMemMB(lim.Memory); maxMB <= 0 || p.Resources.MemoryMB <= maxMB {
			lim.Memory = strconv.Itoa(p.Resources.MemoryMB) + "m"
		}
	}
	if p.Resources.PIDs > 0 {
		if lim.PIDs <= 0 || p.Resources.PIDs <= lim.PIDs {
			lim.PIDs = p.Resources.PIDs
		}
	}
	return lim
}

// parseCPUs parses a Docker-style CPU limit ("2", "1.5") to a float; 0 on empty/invalid
// (treated as "no operator ceiling").
func parseCPUs(s string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return n
}

// parseMemMB parses a Docker-style memory limit ("2g", "512m", "1048576k", "N" bytes) to
// megabytes; 0 on empty/invalid (treated as "no operator ceiling").
func parseMemMB(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	mult := 1.0 / (1024 * 1024) // bare number = bytes
	num := s
	switch s[len(s)-1] {
	case 'g':
		mult, num = 1024, s[:len(s)-1]
	case 'm':
		mult, num = 1, s[:len(s)-1]
	case 'k':
		mult, num = 1.0/1024, s[:len(s)-1]
	case 'b':
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0
	}
	return int(n * mult)
}

// beginToolAudit inserts a pending sandbox_runs row before a deterministic tool's side
// effect (fail-closed). Returns "" when no auditor is configured (audit then skipped).
func (e *runExecutor) beginToolAudit(userID, tool, command string) (string, error) {
	if e.cfg.Audit == nil {
		return "", nil
	}
	id := id.New("sbr")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.cfg.Audit.InsertSandboxRun(ctx, store.SandboxRun{
		// Sentinel (exit -1 + "pending") so a crash/finalize-failure leaves a row that reads
		// as an unfinished side effect, NOT a clean success — mirrors the interactive Router.
		ID: id, UserID: userID, Tool: tool, Command: command, Decision: "auto",
		ExitCode: -1, Error: pendingAuditMarker, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	return id, nil
}

// pendingAuditMarker matches internal/sandbox: the Error/exit stamped on a pre-inserted
// row until finalized, so an interrupted deterministic tool step is visibly unfinished.
const pendingAuditMarker = "pending (not finalized)"

// finalizeToolAudit updates the pending audit row with the full command outcome — exit,
// duration, capture paths, and any error (best-effort; a crash before this leaves the
// pending sentinel row, which is itself the durable evidence of an unfinished side effect).
func (e *runExecutor) finalizeToolAudit(auditID string, res sandbox.RunResult, runErr error, redactor *vault.Redactor) {
	if auditID == "" || e.cfg.Audit == nil {
		return
	}
	fin := store.SandboxRun{
		ID: auditID, SandboxID: res.SandboxID, ExitCode: res.ExitCode,
		DurationMs: res.Duration.Milliseconds(), StdoutPath: res.StdoutPath, StderrPath: res.StderrPath,
	}
	switch {
	case runErr != nil:
		fin.Error = redactor.Redact(runErr.Error())
	case res.Err != nil:
		fin.Error = redactor.Redact(res.Err.Error())
	case res.CaptureErr != "":
		fin.Error = "capture: " + redactor.Redact(res.CaptureErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.cfg.Audit.UpdateSandboxRun(ctx, fin); err != nil {
		e.cfg.Log.Warn("autorun: could not finalize tool audit row", "err", err)
	}
}

// envVarName delegates to the shared automation classifier so enable-time eligibility
// and run-time injection generate identical env names.
func envVarName(name string) string { return automation.EnvVarName(name) }

// decodeAgentContent decodes an agent run's normalized content as JSON when possible.
func decodeAgentContent(content string) any {
	t := strings.TrimSpace(content)
	if strings.HasPrefix(t, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			return m
		}
	}
	return map[string]any{"final_text": content}
}

// tryParseJSONObject extracts the first JSON object from text (the model may wrap it
// in prose); returns nil if none parses.
func tryParseJSONObject(text string) map[string]any {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text[start:end+1]), &m) == nil {
		return m
	}
	return nil
}

// streamText drains a provider stream into the accumulated reply, BOUNDED by maxBytes
// (the run's output budget). A bad backend / prompt-induced runaway response can't
// exhaust memory or bloat the run record: once the cap is hit the stream context is
// cancelled and the remaining deltas dropped. It returns truncated=true when the cap was
// hit, so the caller can FAIL CLOSED rather than expose a partial reply to later steps.
func streamText(ctx context.Context, p llm.Provider, msgs []llm.Message, maxBytes int64) (text string, truncated bool, err error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 1 MiB safety floor
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, serr := p.Stream(sctx, llm.Request{Messages: msgs})
	if serr != nil {
		return "", false, serr
	}
	var b strings.Builder
	for {
		// Select on ctx.Done() as well as the provider channel: a run timeout / shutdown /
		// explicit cancel must unwedge this unattended run promptly, even if the provider
		// stalls and never closes its channel (which would otherwise hold a run slot and
		// hang Stop()). We enforce cancellation here rather than trusting the provider to.
		select {
		case <-ctx.Done():
			return b.String(), false, ctx.Err()
		case d, ok := <-stream:
			if !ok {
				return b.String(), false, nil // provider closed the stream
			}
			if d.Err != nil {
				return b.String(), false, d.Err
			}
			if d.Done {
				return b.String(), false, nil
			}
			if int64(b.Len())+int64(len(d.Text)) > maxBytes {
				remaining := maxBytes - int64(b.Len())
				if remaining > 0 {
					b.WriteString(d.Text[:remaining])
				}
				b.WriteString("…[truncated]")
				cancel() // stop the provider; return the truncated text WITHOUT unbounded draining
				return b.String(), true, nil
			}
			b.WriteString(d.Text)
		}
	}
}

// boundedInputsJSON encodes the llm inputs as a JSON array, failing if the assembled
// size would exceed maxBytes (the run's output budget). It marshals one input at a time
// (each bounded by the run's captured-output caps) and accumulates up to the limit, so
// a repeated huge reference can't allocate an unbounded payload.
func boundedInputsJSON(inputs []any, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, in := range inputs {
		if i > 0 {
			b.WriteByte(',')
		}
		m, err := json.Marshal(in)
		if err != nil {
			return "", fmt.Errorf("could not encode llm input %d", i)
		}
		if int64(b.Len())+int64(len(m))+1 > maxBytes {
			return "", fmt.Errorf("llm inputs exceed the %d-byte limit", maxBytes)
		}
		b.Write(m)
	}
	b.WriteByte(']')
	return b.String(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
