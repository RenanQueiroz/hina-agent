package agentcli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	all := All()
	if len(all) != 4 {
		t.Fatalf("All() = %d adapters, want 4", len(all))
	}
	for _, p := range []Provider{ProviderCodex, ProviderClaude, ProviderCursor, ProviderPi} {
		a, ok := Get(p)
		if !ok {
			t.Fatalf("Get(%q) not found", p)
		}
		if a.Provider() != p {
			t.Fatalf("Get(%q).Provider() = %q", p, a.Provider())
		}
		cap := a.Capability()
		if cap.Provider != p || cap.DisplayName == "" || cap.ToolName != ToolName(p) {
			t.Fatalf("%q capability malformed: %+v", p, cap)
		}
		if len(cap.AuthTypes) == 0 {
			t.Fatalf("%q has no auth types", p)
		}
	}
	if _, ok := Get("bogus"); ok {
		t.Fatal("Get(bogus) should be false")
	}
	if len(Capabilities()) != 4 {
		t.Fatalf("Capabilities() = %d", len(Capabilities()))
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	for _, p := range []Provider{ProviderCodex, ProviderClaude, ProviderCursor, ProviderPi} {
		name := ToolName(p)
		got, ok := ProviderFromToolName(name)
		if !ok || got != p {
			t.Fatalf("ProviderFromToolName(%q) = %q,%v", name, got, ok)
		}
	}
	for _, bad := range []string{"", "agent.run", "shell", "agent.bogus.run", "agent..run", "codex.run", "agent.codex.exec"} {
		if _, ok := ProviderFromToolName(bad); ok {
			t.Fatalf("ProviderFromToolName(%q) should be false", bad)
		}
	}
}

func TestProviderAndAuthTypeValid(t *testing.T) {
	if Provider("nope").Valid() || !ProviderCodex.Valid() {
		t.Fatal("Provider.Valid wrong")
	}
	if AuthType("nope").Valid() || !AuthBrowserState.Valid() {
		t.Fatal("AuthType.Valid wrong")
	}
}

// --- argv construction ---

func TestCodexBuildRun(t *testing.T) {
	a, _ := Get(ProviderCodex)
	plan, err := a.BuildRun(RunRequest{Prompt: "-rm -rf danger", Model: "gpt-5.4", AuthType: AuthAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"codex exec --json", "--cd /workspace", "--skip-git-repo-check", "--sandbox workspace-write", "--ask-for-approval never", "-m gpt-5.4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("codex argv missing %q: %s", want, joined)
		}
	}
	// The prompt must be a trailing positional after "--" so a leading-dash prompt
	// can't be parsed as a flag.
	if plan.Argv[len(plan.Argv)-1] != "-rm -rf danger" || plan.Argv[len(plan.Argv)-2] != "--" {
		t.Errorf("prompt not the guarded trailing positional: %v", plan.Argv)
	}
	if strings.Contains(joined, "--full-auto") {
		t.Error("codex must not use the deprecated --full-auto (B7)")
	}
	mustEnv(t, plan.Env, "CODEX_HOME=/agent/codex")
	if len(plan.SecretNames) != 1 || plan.SecretNames[0] != "OPENAI_API_KEY" {
		t.Errorf("codex api-key secret = %v, want [OPENAI_API_KEY] (never CODEX_API_KEY)", plan.SecretNames)
	}
	for _, n := range plan.SecretNames {
		if n == "CODEX_API_KEY" {
			t.Error("CODEX_API_KEY was dropped per B7 correction 1")
		}
	}
}

func TestCodexBrowserStateInjectsNoSecret(t *testing.T) {
	a, _ := Get(ProviderCodex)
	plan, _ := a.BuildRun(RunRequest{Prompt: "hi", AuthType: AuthBrowserState})
	if len(plan.SecretNames) != 0 {
		t.Errorf("browser-state codex must inject no secret, got %v", plan.SecretNames)
	}
}

func TestCodexStructuredStagesSchema(t *testing.T) {
	a, _ := Get(ProviderCodex)
	schema := json.RawMessage(`{"type":"object"}`)
	plan, _ := a.BuildRun(RunRequest{Prompt: "hi", Structured: true, SchemaJSON: schema})
	if !strings.Contains(strings.Join(plan.Argv, " "), "--output-schema /hina/output-schema.json") {
		t.Errorf("codex schema flag missing/not in the staging mount: %v", plan.Argv)
	}
	if len(plan.Files) != 1 || plan.Files[0].RelPath != "output-schema.json" || string(plan.Files[0].Content) != string(schema) {
		t.Errorf("codex schema not staged: %+v", plan.Files)
	}
}

func TestClaudeBuildRun(t *testing.T) {
	a, _ := Get(ProviderClaude)
	plan, _ := a.BuildRun(RunRequest{Prompt: "do x", MaxTurns: 5, AuthType: AuthBrowserState})
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"claude -p do x", "--output-format json", "--dangerously-skip-permissions", "--max-turns 5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude argv missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--bare") {
		t.Error("claude must never use --bare for subscription (B7 correction 5)")
	}
	mustEnv(t, plan.Env, "CLAUDE_CONFIG_DIR=/agent/claude")
	// Browser-state must keep ANTHROPIC_API_KEY unset (it would override subscription).
	if len(plan.SecretNames) != 0 {
		t.Errorf("browser-state claude must inject no secret, got %v", plan.SecretNames)
	}
}

func TestClaudeAuthTypeSecrets(t *testing.T) {
	a, _ := Get(ProviderClaude)
	cases := map[AuthType]string{AuthAPIKey: "ANTHROPIC_API_KEY", AuthOAuthToken: "CLAUDE_CODE_OAUTH_TOKEN"}
	for at, want := range cases {
		plan, _ := a.BuildRun(RunRequest{Prompt: "x", AuthType: at})
		if len(plan.SecretNames) != 1 || plan.SecretNames[0] != want {
			t.Errorf("claude %s secret = %v, want [%s]", at, plan.SecretNames, want)
		}
	}
}

func TestCursorBuildRun(t *testing.T) {
	a, _ := Get(ProviderCursor)
	plan, _ := a.BuildRun(RunRequest{Prompt: "fix it", Model: "auto", AuthType: AuthAPIKey})
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"agent -p fix it", "--output-format json", "--force", "--model auto"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cursor argv missing %q: %s", want, joined)
		}
	}
	mustEnv(t, plan.Env, "HOME=/agent/cursor")
	if len(plan.SecretNames) != 1 || plan.SecretNames[0] != "CURSOR_API_KEY" {
		t.Errorf("cursor secret = %v", plan.SecretNames)
	}
	if env := a.LoginEnv(); len(env) != 1 || env[0] != "NO_OPEN_BROWSER=1" {
		t.Errorf("cursor LoginEnv = %v, want [NO_OPEN_BROWSER=1]", env)
	}
}

func TestPiFailsClosedWithoutEndpoint(t *testing.T) {
	a, _ := Get(ProviderPi)
	if _, err := a.BuildRun(RunRequest{Prompt: "hi"}); err == nil {
		t.Fatal("pi must fail closed without a local endpoint (no cloud fallback)")
	}
}

func TestPiRejectsRemoteEndpoint(t *testing.T) {
	a, _ := Get(ProviderPi)
	for _, bad := range []string{"http://evil.com/v1", "https://api.openai.com/v1", "http://10.0.0.5:8080/v1", "ftp://localhost/v1"} {
		if _, err := a.BuildRun(RunRequest{Prompt: "hi", LocalEndpoint: bad}); err == nil {
			t.Errorf("Pi must reject the non-local endpoint %q (local-only)", bad)
		}
	}
	for _, ok := range []string{"http://host.docker.internal:8081/v1", "http://127.0.0.1:8080/v1", "http://localhost:8080/v1"} {
		if _, err := a.BuildRun(RunRequest{Prompt: "hi", LocalEndpoint: ok}); err != nil {
			t.Errorf("Pi must accept the local endpoint %q: %v", ok, err)
		}
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	for _, ok := range []string{"http://host.docker.internal:8081/v1", "http://127.0.0.1:8080/v1", "http://localhost/v1", "http://[::1]:8080/v1"} {
		if !IsLocalEndpoint(ok) {
			t.Errorf("%q should be local", ok)
		}
	}
	for _, bad := range []string{"http://evil.com/v1", "https://api.openai.com", "http://10.1.2.3/v1", "ftp://localhost/v1", "", "not a url", "http:///v1"} {
		if IsLocalEndpoint(bad) {
			t.Errorf("%q should NOT be local", bad)
		}
	}
}

func TestPiBuildRunWithEndpoint(t *testing.T) {
	a, _ := Get(ProviderPi)
	plan, err := a.BuildRun(RunRequest{Prompt: "hi", LocalEndpoint: "http://host.docker.internal:8081/v1", Model: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	mustEnv(t, plan.Env, "PI_OFFLINE=1")
	joined := strings.Join(plan.Argv, " ")
	for _, want := range []string{"--no-extensions", "--no-skills", "--no-context-files", "--no-tools"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pi lockdown flag missing %q: %s", want, joined)
		}
	}
	if len(plan.Files) != 1 || plan.Files[0].RelPath != piModelsFile {
		t.Fatalf("pi models.json not staged: %+v", plan.Files)
	}
	var cfg map[string]any
	if err := json.Unmarshal(plan.Files[0].Content, &cfg); err != nil {
		t.Fatalf("pi models.json invalid: %v", err)
	}
	prov := cfg["providers"].(map[string]any)["local"].(map[string]any)
	if prov["baseUrl"] != "http://host.docker.internal:8081/v1" || prov["api"] != "openai-completions" {
		t.Errorf("pi provider config wrong: %+v", prov)
	}
}

func TestBuildRunRejectsEmptyPrompt(t *testing.T) {
	for _, a := range All() {
		if _, err := a.BuildRun(RunRequest{Prompt: "   ", LocalEndpoint: "http://x/v1"}); err == nil {
			t.Errorf("%s BuildRun should reject an empty prompt", a.Provider())
		}
	}
}

func mustEnv(t *testing.T, env []string, want string) {
	t.Helper()
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("env %v missing %q", env, want)
}
