package automation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Eligibility carries the runtime facts needed to decide whether an automation may
// be ENABLED (the gate the plan requires: "validation fails on unavailable agent
// auth profiles, missing secret refs, or disallowed agents"). It is filled by the
// HTTP layer from server config + the owner's vault/agent profiles, so the
// automation package stays decoupled from those.
type Eligibility struct {
	AgentsEnabled    bool            // [agents] enabled
	NetworkIsolated  bool            // [sandbox] network_isolated asserted
	SandboxAvailable bool            // a usable sbx runner is present
	AllowedProviders map[string]bool // server provider allow-list (empty/nil => all)
	PiEndpoint       bool            // the Phase 11 local endpoint is configured (Pi reachable)

	Secrets          map[string]bool   // owner's vaulted secret names that exist
	ConfiguredAgents map[string]string // provider -> profile status ("authenticated"/...)
	RunnableAgents   map[string]bool   // provider -> currently runnable (auth + policy ok)
}

// CheckEligibility verifies an automation can actually run on this server for this
// owner: every tool is permitted by the profile, every agent_cli adapter is allowed
// + authenticated + granted, every secret_ref exists, and the fail-closed network/
// sandbox gates hold. It returns a *ValidationErrors (nil when eligible) so the UI
// can surface each blocker before enable.
func (d Definition) CheckEligibility(e Eligibility) *ValidationErrors {
	errs := &ValidationErrors{}

	if !e.SandboxAvailable {
		errs.add("sandbox", "the sandbox runtime is unavailable; automations run inside sbx")
	}

	usesAgents := false
	usesSecrets := len(d.Sandbox.SecretRefs) > 0
	usesNetworkTool := false
	usesLLM := false
	granted := grantedProviders(d.Sandbox.AgentAuthRefs)

	walkSteps(d.Steps, func(s *Step, path string) {
		switch s.Type {
		case StepTool:
			// Mirror the SAME run-time gates the executor enforces, so a schema-eligible
			// automation can't be enabled only to fail (or worse, egress) on its first fire.
			// The network gate applies in EVERY mode (unrestricted still honors network:
			// disabled); the typed-tool + CLI allow-lists are granular-only.
			if ToolNetworked(s.Tool) {
				usesNetworkTool = true
				if !NetworkAllowed(d.Sandbox.Mode, d.Sandbox.Network) {
					errs.add(path+".tool", "tool %q can make network requests but sandbox.network is disabled", s.Tool)
				}
			}
			if d.Sandbox.Mode != ModeUnrestricted {
				if !contains(d.Sandbox.AllowedTools, s.Tool) {
					errs.add(path+".tool", "tool %q is not in the sandbox allowed_tools list", s.Tool)
				}
				for _, cli := range ToolCLIs(s.Tool) {
					if !contains(d.Sandbox.AllowedCLITools, cli) {
						errs.add(path+".tool", "tool %q needs CLI %q in sandbox.allowed_cli_tools", s.Tool, cli)
					}
				}
				// shell.exec's CLI is argv-dependent — mirror the run-time argv[0] gate at
				// enable time so a granular profile can't be enabled and then fail on first run.
				if s.Tool == ToolShellExec {
					if msg := shellExecGranularIssue(s.With, d.Sandbox.AllowedCLITools); msg != "" {
						errs.add(path+".with", "%s", msg)
					}
				}
			}
		case StepAgentCLI:
			usesAgents = true
			d.checkAgentEligible(s, path, e, granted, errs)
		case StepLLM:
			// An llm step streams its prompt + resolved inputs (which may include data from
			// prior steps) to the model provider — a possibly-cloud egress. It needs both the
			// profile's network posture AND the operator's network_isolated assertion, the same
			// fail-closed gate the agent_cli + networked-tool paths get (it is an OUTWARD data
			// flow from an unattended run, even though the provider call is made server-side).
			usesLLM = true
			if !NetworkAllowed(d.Sandbox.Mode, d.Sandbox.Network) {
				errs.add(path+".type", "an llm step reaches the model provider; set sandbox.network=enabled")
			}
		}
	})

	seenEnv := map[string]string{}
	for i, ref := range d.Sandbox.SecretRefs {
		p := fmt.Sprintf("sandbox.secret_refs[%d]", i)
		if !e.Secrets[ref] {
			errs.add(p, "no vaulted secret named %q", ref)
		}
		// The generated env-var name must be unique + not host-dangerous, or one grant
		// would be silently dropped/overwritten at run time (the credential the definition
		// claims to inject would be missing).
		env := EnvVarName(ref)
		if IsDangerousEnvName(env) {
			errs.add(p, "secret %q maps to a host-dangerous env var %q — rename the secret", ref, env)
		}
		if prev, dup := seenEnv[env]; dup {
			errs.add(p, "secrets %q and %q both map to env var %q — rename one", prev, ref, env)
		} else {
			seenEnv[env] = ref
		}
	}

	// Fail closed: an automation that drives callable agents, injects secrets, OR runs a
	// network-capable deterministic tool needs the operator to have asserted the sandbox's
	// egress is controlled — the same gate interactive agent runs require. A networked tool
	// matters because the http.request SSRF guard deliberately does NOT resolve DNS hostnames
	// (a rebinding TOCTOU): the only thing keeping a hostname that resolves to a private/
	// link-local/metadata address contained is a locked-down sbx egress, which network_isolated
	// asserts. Without that assertion, refuse to enable rather than rely on an unverified egress.
	if (usesAgents || usesSecrets || usesNetworkTool || usesLLM) && !e.NetworkIsolated {
		errs.add("sandbox.network", "set [sandbox] network_isolated=true to run an automation that uses callable agents, injects secrets, makes network requests, or calls an llm")
	}
	if usesAgents && !e.AgentsEnabled {
		errs.add("steps", "callable agents are disabled on this server ([agents] enabled=false)")
	}

	if errs.HasIssues() {
		return errs
	}
	return nil
}

func (d Definition) checkAgentEligible(s *Step, path string, e Eligibility, granted map[string]bool, errs *ValidationErrors) {
	prov := s.Adapter
	// An agent run reaches its provider (or, for Pi, the local gateway) — it is inherently
	// networked, so the profile's network posture must allow it (the same gate deterministic
	// networked tools get).
	if !NetworkAllowed(d.Sandbox.Mode, d.Sandbox.Network) {
		errs.add(path+".adapter", "agent runs need network access (set sandbox.network=enabled)")
	}
	if e.AllowedProviders != nil && len(e.AllowedProviders) > 0 && !e.AllowedProviders[prov] {
		errs.add(path+".adapter", "the %s agent is not permitted by server policy", prov)
	}
	if !granted[prov] {
		errs.add(path+".adapter", "the automation does not grant the %s agent (add it to sandbox.agent_auth_refs)", prov)
	}
	if _, ok := e.ConfiguredAgents[prov]; !ok {
		errs.add(path+".adapter", "no configured auth profile for the %s agent", prov)
		return
	}
	if !e.RunnableAgents[prov] {
		errs.add(path+".adapter", "the %s agent is configured but not currently runnable (check its auth/status)", prov)
	}
	if prov == "pi" {
		if !e.PiEndpoint {
			errs.add(path+".adapter", "Pi requires the local llama.cpp endpoint, which is not configured")
		}
		// Pi reaches the host llama.cpp gateway, so the profile must explicitly grant that
		// host service — otherwise the displayed permission profile would be weaker than
		// the runtime boundary.
		if !contains(d.Sandbox.AllowedHostServices, "llamacpp") {
			errs.add(path+".adapter", "Pi needs the \"llamacpp\" host service granted in sandbox.allowed_host_services")
		}
	}
}

// shellExecGranularIssue statically checks a shell.exec `with` against a granular
// profile (the same rules the run-time executor enforces): a command string or a
// shell-interpreter argv[0] requires unrestricted; a dynamic argv[0] (a ${} reference)
// can't be proven; and a literal argv[0] must be in allowed_cli_tools. Returns "" when ok.
func shellExecGranularIssue(with json.RawMessage, allowedCLI []string) string {
	var w struct {
		Argv    []string `json:"argv"`
		Command string   `json:"command"`
	}
	if len(with) > 0 {
		_ = json.Unmarshal(with, &w)
	}
	if len(w.Argv) == 0 {
		if w.Command != "" {
			return "shell.exec with a command string requires an unrestricted sandbox profile"
		}
		return "shell.exec needs an argv array (or a command string under an unrestricted profile)"
	}
	a0 := w.Argv[0]
	if strings.Contains(a0, "${") {
		return "shell.exec argv[0] must be a literal CLI in a granular profile (no ${} reference)"
	}
	if IsShellInterpreter(a0) {
		return "shell.exec via a shell interpreter requires an unrestricted sandbox profile"
	}
	if !contains(allowedCLI, a0) {
		return fmt.Sprintf("shell.exec uses CLI %q, which is not in sandbox.allowed_cli_tools", a0)
	}
	return ""
}

// grantedProviders maps the agent_auth_refs (e.g. "codex_browser", "claude") to the
// set of providers they grant. A ref grants the provider whose name it starts with.
func grantedProviders(refs []string) map[string]bool {
	out := map[string]bool{}
	for _, ref := range refs {
		for prov := range knownAdapters {
			if ref == prov || strings.HasPrefix(ref, prov+"_") {
				out[prov] = true
			}
		}
	}
	return out
}

// walkSteps visits every step in the tree (depth-first), passing its JSON path.
func walkSteps(steps []Step, fn func(s *Step, path string)) {
	var rec func(steps []Step, path string)
	rec = func(steps []Step, path string) {
		for i := range steps {
			s := &steps[i]
			p := fmt.Sprintf("%s[%d]", path, i)
			fn(s, p)
			if len(s.Steps) > 0 {
				rec(s.Steps, p+".steps")
			}
		}
	}
	rec(steps, "steps")
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
