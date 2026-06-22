package automation

import (
	"strings"
	"testing"
)

func eligibleCtx() Eligibility {
	return Eligibility{
		AgentsEnabled:    true,
		NetworkIsolated:  true,
		SandboxAvailable: true,
		Secrets:          map[string]bool{"github_token": true},
		ConfiguredAgents: map[string]string{"codex": "authenticated", "claude": "authenticated"},
		RunnableAgents:   map[string]bool{"codex": true, "claude": true},
	}
}

func TestEligibilityPasses(t *testing.T) {
	def := mustDef(t)
	if verrs := def.CheckEligibility(eligibleCtx()); verrs != nil {
		t.Fatalf("expected eligible: %v", verrs)
	}
}

func TestEligibilityMissingSecret(t *testing.T) {
	def := mustDef(t)
	e := eligibleCtx()
	e.Secrets = map[string]bool{} // github_token missing
	verrs := def.CheckEligibility(e)
	if verrs == nil || !strings.Contains(verrs.Error(), "github_token") {
		t.Fatalf("missing secret should be flagged: %v", verrs)
	}
}

func TestEligibilityUnauthenticatedAgent(t *testing.T) {
	def := mustDef(t)
	e := eligibleCtx()
	delete(e.ConfiguredAgents, "codex")
	if def.CheckEligibility(e) == nil {
		t.Fatal("an unconfigured agent must fail eligibility")
	}
}

func TestEligibilityRequiresNetworkIsolation(t *testing.T) {
	def := mustDef(t)
	e := eligibleCtx()
	e.NetworkIsolated = false
	verrs := def.CheckEligibility(e)
	if verrs == nil || !strings.Contains(verrs.Error(), "network_isolated") {
		t.Fatalf("agents/secrets require network_isolated: %v", verrs)
	}
}

func TestEligibilityDisallowedProvider(t *testing.T) {
	def := mustDef(t)
	e := eligibleCtx()
	e.AllowedProviders = map[string]bool{"claude": true} // codex not allowed
	if def.CheckEligibility(e) == nil {
		t.Fatal("a disallowed provider must fail eligibility")
	}
}

func TestEligibilityOmittedModeStillGatesTools(t *testing.T) {
	// mode omitted ("") must be treated as granular — a tool not in allowed_tools fails.
	js := `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"allowed_cli_tools":["gh"]},
		"steps":[{"id":"n","type":"tool","tool":"github.notifications","with":{}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true})
	if verrs == nil || !strings.Contains(verrs.Error(), "allowed_tools") {
		t.Fatalf("an omitted mode must still gate allowed_tools: %v", verrs)
	}
}

func TestEligibilityPiRequiresHostService(t *testing.T) {
	mk := func(hostServices string) Definition {
		js := `{"schema_version":"automation.v1","name":"p","trigger":{"type":"manual"},
			"sandbox":{"mode":"granular","network":"enabled","agent_auth_refs":["pi"]` + hostServices + `},
			"steps":[{"id":"r","type":"agent_cli","adapter":"pi","prompt_template":"x"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		if v := def.Validate(); v != nil {
			t.Fatalf("validate: %v", v)
		}
		return def
	}
	e := Eligibility{
		AgentsEnabled: true, NetworkIsolated: true, SandboxAvailable: true, PiEndpoint: true,
		ConfiguredAgents: map[string]string{"pi": "authenticated"}, RunnableAgents: map[string]bool{"pi": true},
	}
	// Without the llamacpp host-service grant, Pi must be ineligible.
	if verrs := mk("").CheckEligibility(e); verrs == nil || !strings.Contains(verrs.Error(), "llamacpp") {
		t.Fatalf("Pi without allowed_host_services must fail: %v", verrs)
	}
	// With it granted, Pi is eligible.
	if verrs := mk(`,"allowed_host_services":["llamacpp"]`).CheckEligibility(e); verrs != nil {
		t.Fatalf("Pi with the host-service grant should be eligible: %v", verrs)
	}
}

func TestEligibilityToolRequiresCLIsAndNetwork(t *testing.T) {
	// github.pr_checkout needs sh+gh+git; omitting sh must fail at enable, not first run.
	missingCLI := `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","network":"enabled","allowed_tools":["github.pr_checkout"],"allowed_cli_tools":["gh","git"]},
		"steps":[{"id":"c","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":1}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(missingCLI))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "sh") {
		t.Fatalf("a missing required CLI must fail eligibility: %v", verrs)
	}

	// A networked tool under network:disabled must fail at enable.
	noNet := `{"schema_version":"automation.v1","name":"y","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","network":"disabled","allowed_tools":["github.notifications"],"allowed_cli_tools":["gh"]},
		"steps":[{"id":"n","type":"tool","tool":"github.notifications","with":{}},{"id":"f","type":"finish","status":"success"}]}`
	def2, _ := Parse([]byte(noNet))
	if verrs := def2.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "network") {
		t.Fatalf("a networked tool under network:disabled must fail eligibility: %v", verrs)
	}
}

func TestEligibilityShellExecMirrorsRuntime(t *testing.T) {
	mk := func(allowedCLI, withJSON string) Definition {
		js := `{"schema_version":"automation.v1","name":"s","trigger":{"type":"manual"},
			"sandbox":{"mode":"granular","network":"enabled","allowed_tools":["shell.exec"],"allowed_cli_tools":` + allowedCLI + `},
			"steps":[{"id":"x","type":"tool","tool":"shell.exec","with":` + withJSON + `},{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if v := def.Validate(); v != nil {
			t.Fatalf("validate: %v", v)
		}
		return def
	}
	e := Eligibility{SandboxAvailable: true, NetworkIsolated: true}
	// A command string under granular must fail (it needs unrestricted).
	if mk(`["sh"]`, `{"command":"ls"}`).CheckEligibility(e) == nil {
		t.Error("shell.exec command-string under granular must fail eligibility")
	}
	// A shell-interpreter argv must fail.
	if mk(`["sh"]`, `{"argv":["sh","-c","x"]}`).CheckEligibility(e) == nil {
		t.Error("shell.exec via a shell interpreter under granular must fail eligibility")
	}
	// A literal argv[0] not in allowed_cli_tools must fail.
	if mk(`["git"]`, `{"argv":["curl","http://x"]}`).CheckEligibility(e) == nil {
		t.Error("shell.exec argv[0] not in allowed_cli_tools must fail eligibility")
	}
	// A dynamic ${} argv element is now rejected at VALIDATION (even earlier than the eligibility
	// CLI check): resolveWith never expands array elements, so a per-element template would run
	// literally — a dynamic argv must use argv_from (round-84).
	dyn, _ := Parse([]byte(`{"schema_version":"automation.v1","name":"s","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","network":"enabled","allowed_tools":["shell.exec"],"allowed_cli_tools":["git"]},
		"steps":[{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":["${y.bin}","z"]}},{"id":"f","type":"finish","status":"success"}]}`))
	if dyn.Validate() == nil {
		t.Error("a dynamic ${} argv element must be rejected at validation")
	}
	// A literal, granted CLI passes.
	if verrs := mk(`["git"]`, `{"argv":["git","status"]}`).CheckEligibility(e); verrs != nil {
		t.Errorf("shell.exec with a granted literal CLI should be eligible: %v", verrs)
	}
}

func TestEligibilityUnrestrictedHonorsNetworkDisabled(t *testing.T) {
	// unrestricted + network:disabled + a networked tool must FAIL eligibility.
	blocked := `{"schema_version":"automation.v1","name":"u","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"disabled"},
		"steps":[{"id":"h","type":"tool","tool":"http.request","with":{"url":"https://x"}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(blocked))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "network") {
		t.Fatalf("unrestricted + network:disabled must fail a networked tool: %v", verrs)
	}
	// unrestricted with network omitted (default-allow) is eligible.
	ok := `{"schema_version":"automation.v1","name":"u2","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"h","type":"tool","tool":"http.request","with":{"url":"https://x"}},{"id":"f","type":"finish","status":"success"}]}`
	def2, _ := Parse([]byte(ok))
	if verrs := def2.CheckEligibility(Eligibility{SandboxAvailable: true, NetworkIsolated: true}); verrs != nil {
		t.Fatalf("unrestricted with default network should be eligible: %v", verrs)
	}
}

// A network-capable deterministic tool requires [sandbox] network_isolated, since the
// http.request SSRF guard doesn't resolve DNS and relies on a locked-down sbx egress
// (round-46) — the same fail-closed gate agents + secret-bearing runs get.
func TestEligibilityNetworkedToolRequiresIsolation(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"n","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"h","type":"tool","tool":"http.request","with":{"url":"https://x"}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	// network_isolated NOT asserted -> ineligible.
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "network_isolated") {
		t.Fatalf("a networked tool must require network_isolated, got %v", verrs)
	}
	// Asserted -> eligible.
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true, NetworkIsolated: true}); verrs != nil {
		t.Fatalf("a networked tool with network_isolated should be eligible: %v", verrs)
	}
}

// An llm step reaches the (possibly cloud) model provider, an outward data flow from an
// unattended run — it requires [sandbox] network_isolated, like networked tools/agents (round-50).
func TestEligibilityLLMRequiresIsolation(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"n","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"sum","type":"llm","prompt_template":"summarize"},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "network_isolated") {
		t.Fatalf("an llm step must require network_isolated, got %v", verrs)
	}
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true, NetworkIsolated: true}); verrs != nil {
		t.Fatalf("an llm step with network_isolated should be eligible: %v", verrs)
	}
}

// A vaulted secret whose name maps to a gh routing env var (gh_host -> GH_HOST) must be
// rejected as a dangerous grant at enable time — it would reroute the github.* tools off
// github.com despite the repo pin (round-56).
func TestEligibilityRejectsGHRoutingSecret(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"n","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled","secret_refs":["gh_host"]},
		"steps":[{"id":"x","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	elig := Eligibility{SandboxAvailable: true, NetworkIsolated: true, Secrets: map[string]bool{"gh_host": true}}
	if verrs := def.CheckEligibility(elig); verrs == nil || !strings.Contains(verrs.Error(), "GH_HOST") {
		t.Fatalf("a gh_host secret grant must be rejected as host-dangerous, got %v", verrs)
	}
}

// XDG_CONFIG_HOME is gh's config-dir fallback when GH_CONFIG_DIR is unset, so a secret named
// xdg_config_home would also steer gh to an attacker-controlled hosts.yml — reject it too (round-57).
func TestEligibilityRejectsXDGConfigSecret(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"n","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled","secret_refs":["xdg_config_home"]},
		"steps":[{"id":"x","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	elig := Eligibility{SandboxAvailable: true, NetworkIsolated: true, Secrets: map[string]bool{"xdg_config_home": true}}
	if verrs := def.CheckEligibility(elig); verrs == nil || !strings.Contains(verrs.Error(), "XDG_CONFIG_HOME") {
		t.Fatalf("an xdg_config_home secret grant must be rejected as host-dangerous, got %v", verrs)
	}
}

func TestEligibilitySecretEnvMapping(t *testing.T) {
	mk := func(refs string) Definition {
		js := `{"schema_version":"automation.v1","name":"s","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"disabled","secret_refs":` + refs + `},
			"steps":[{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return def
	}
	// All declared secrets exist, network_isolated asserted.
	e := Eligibility{SandboxAvailable: true, NetworkIsolated: true, Secrets: map[string]bool{
		"foo-bar": true, "foo_bar": true, "path": true, "github_token": true,
	}}
	// Two refs collide on the same generated env var (FOO_BAR) -> fail.
	if mk(`["foo-bar","foo_bar"]`).CheckEligibility(e) == nil {
		t.Error("colliding secret env names must fail eligibility")
	}
	// A host-dangerous generated env var (PATH) -> fail.
	if mk(`["path"]`).CheckEligibility(e) == nil {
		t.Error("a host-dangerous secret env name must fail eligibility")
	}
	// A clean ref is eligible.
	if verrs := mk(`["github_token"]`).CheckEligibility(e); verrs != nil {
		t.Errorf("a clean secret_ref should be eligible: %v", verrs)
	}
}

// An agent_cli step under a network-disabled profile must fail eligibility — an agent
// run reaches its provider, so a "network disabled" profile must not appear to permit it
// (round-17 finding).
func TestEligibilityAgentRequiresNetwork(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"a","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","network":"disabled","agent_auth_refs":["codex"]},
		"steps":[{"id":"r","type":"agent_cli","adapter":"codex","prompt_template":"x"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	e := Eligibility{
		AgentsEnabled: true, NetworkIsolated: true, SandboxAvailable: true,
		ConfiguredAgents: map[string]string{"codex": "authenticated"}, RunnableAgents: map[string]bool{"codex": true},
	}
	if verrs := def.CheckEligibility(e); verrs == nil || !strings.Contains(verrs.Error(), "network") {
		t.Fatalf("agent_cli under network:disabled must fail eligibility: %v", verrs)
	}
}

// An llm step under a network-disabled profile must fail eligibility — it streams its
// prompt + inputs to the model provider (round-18 finding).
func TestEligibilityLLMRequiresNetwork(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"l","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","network":"disabled"},
		"steps":[{"id":"agg","type":"llm","prompt_template":"summarize"},{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	if verrs := def.CheckEligibility(Eligibility{SandboxAvailable: true}); verrs == nil || !strings.Contains(verrs.Error(), "network") {
		t.Fatalf("an llm step under network:disabled must fail eligibility: %v", verrs)
	}
}

func TestEligibilityToolNotAllowed(t *testing.T) {
	def := mustDef(t)
	// Remove a tool the steps use from the allow-list.
	def.Sandbox.AllowedTools = []string{"github.notifications"}
	verrs := def.CheckEligibility(eligibleCtx())
	if verrs == nil || !strings.Contains(verrs.Error(), "allowed_tools") {
		t.Fatalf("a used tool missing from allowed_tools must fail: %v", verrs)
	}
}
