package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

func agentCall(provider, prompt string) ToolCall {
	args, _ := json.Marshal(map[string]any{"prompt": prompt})
	return ToolCall{ID: "ac1", Name: agentcli.ToolName(agentcli.Provider(provider)), Arguments: args}
}

func (k *routerKit) agentRouter(endpoint string) *AgentRouter {
	return k.router.NewAgentRouter(AgentRouterConfig{
		State: k.vault, Profiles: k.store, LocalEndpoint: endpoint, Limits: Limits{},
	})
}

func (k *routerKit) setProfile(t *testing.T, provider, authType string) {
	t.Helper()
	err := k.store.UpsertAgentProfile(context.Background(), store.AgentProfile{
		ID: id.New("agp"), UserID: k.userID, Provider: provider, AuthType: authType, Status: "authenticated",
	})
	if err != nil {
		t.Fatalf("set profile: %v", err)
	}
}

func TestAgentRunAPIKeyHappyPath(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	if err := k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-secret-xyz"))); err != nil {
		t.Fatal(err)
	}
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx_a",
		Stdout: `{"type":"agent_message","message":"key sk-secret-xyz used"}`}

	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("unexpected tool err: %s", res.Err)
	}
	var parsed agentcli.AgentRunResult
	if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
		t.Fatalf("content not AgentRunResult JSON: %v (%s)", err, res.Content)
	}
	if parsed.Status != agentcli.StatusOK {
		t.Errorf("status = %q", parsed.Status)
	}
	// The credential echoed in output must be redacted from the model-visible result.
	if strings.Contains(parsed.FinalText, "sk-secret-xyz") {
		t.Errorf("credential leaked into final text: %q", parsed.FinalText)
	}
	if !strings.Contains(parsed.FinalText, "[redacted]") {
		t.Errorf("expected redaction marker, got %q", parsed.FinalText)
	}
	// The key is injected via the process env (never argv), and CODEX_HOME + the
	// credential-store mount are set.
	if !hasEnv(k.runner.lastSpec.SecretEnv, "OPENAI_API_KEY=sk-secret-xyz") {
		t.Errorf("OPENAI_API_KEY not injected: %v", k.runner.lastSpec.SecretEnv)
	}
	if !hasEnv(k.runner.lastSpec.Env, "CODEX_HOME=/agent/codex") {
		t.Errorf("CODEX_HOME not set: %v", k.runner.lastSpec.Env)
	}
	if len(k.runner.lastSpec.Mounts) != 1 || k.runner.lastSpec.Mounts[0].Container != "/agent/codex" || k.runner.lastSpec.Mounts[0].ReadOnly {
		t.Errorf("cred-store mount wrong: %+v", k.runner.lastSpec.Mounts)
	}
	// The key must never appear on the argv.
	if strings.Contains(strings.Join(k.runner.lastSpec.Argv, " "), "sk-secret-xyz") {
		t.Error("credential leaked onto the argv")
	}
	// Audit row records the run with the auth-profile TYPE and no credential.
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Tool != "agent.codex.run" || runs[0].ExitCode != 0 {
		t.Fatalf("audit row wrong: %+v", runs)
	}
	if !strings.Contains(runs[0].Command, "auth:api_key") || strings.Contains(runs[0].Command, "sk-secret") {
		t.Errorf("audit command leaked or missing auth type: %q", runs[0].Command)
	}
}

func TestAgentRunNotConfigured(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	ar := k.agentRouter("")
	res, _ := ar.Handle(context.Background(), k.scope(), agentCall("claude", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "not configured") {
		t.Fatalf("expected not-configured error, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("runner must not run for an unconfigured agent")
	}
}

func TestAgentRunRequiresNetworkIsolation(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk")))
	// Build a router with network isolation OFF.
	r := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: k.router.cfg.Workspaces, Store: k.store,
		Bus: k.bus, Approver: k.apr, Approval: ApprovalAuto, NetworkIsolated: false,
	})
	ar := r.NewAgentRouter(AgentRouterConfig{State: k.vault, Profiles: k.store})
	res, _ := ar.Handle(context.Background(), k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "network_isolated") {
		t.Fatalf("expected fail-closed on network isolation, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("runner must not run when network isolation is unasserted")
	}
}

func TestAgentRunPiUnavailableWithoutEndpoint(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "pi", "local_llamacpp")
	ar := k.agentRouter("") // no Phase 11 endpoint
	res, _ := ar.Handle(context.Background(), k.scope(), agentCall("pi", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "Phase 11") {
		t.Fatalf("expected Pi-unavailable error, got %q", res.Err)
	}
}

func TestAgentPersistRefreshedStateSerializesOnCredLock(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	credLocks := &UserLocker{}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: k.vault, Profiles: k.store, CredLocks: credLocks})

	scratch, err := k.router.cfg.Workspaces.NewScratch()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(scratch.Dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "auth.json"), []byte(`{"token":"refreshedvalue123456789"}`), 0o600)

	// Hold the cred lock (as a concurrent logout/SetKey would): the persist must block
	// on it — that serializes the persist-vs-delete race WITHOUT the long run lock.
	unlock := credLocks.Lock(k.userID)
	done := make(chan struct{})
	// launchVersion "" matches the (unseeded) stored version, so the persist proceeds.
	go func() { ar.persistRefreshedState(k.userID, "codex", scratch.Dir, ""); close(done) }()
	select {
	case <-done:
		unlock()
		t.Fatal("persistRefreshedState must block on the shared cred lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("persistRefreshedState did not complete after the cred lock released")
	}
	if !k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("expected the refreshed state to be persisted")
	}
}

func TestPersistRefreshedStateSkipsAfterReplacement(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: k.vault, Profiles: k.store})

	// Launch-time credential + its version.
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, []byte("original-tar-bytes")))
	launchVersion := ar.agentStateVersion(k.userID, "codex")
	// A successful browser re-auth REPLACES the credential DURING the run.
	replacement := EncodeCredState(CredKindTar, []byte("replacement-tar-bytes"))
	_ = k.vault.PutAgentState(k.userID, "codex", replacement)

	// The cancelled/stale run then tries to persist its scratch store.
	scratch, err := k.router.cfg.Workspaces.NewScratch()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(scratch.Dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "auth.json"), []byte(`{"token":"stale"}`), 0o600)
	ar.persistRefreshedState(k.userID, "codex", scratch.Dir, launchVersion)

	// The re-authed credential must survive — not be overwritten by the stale run.
	got, _ := k.vault.GetAgentState(k.userID, "codex")
	if !bytes.Equal(got, replacement) {
		t.Fatal("a stale in-flight run overwrote the re-authed credential store")
	}
}

func TestAgentRunPiWithEndpointRuns(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "pi", "local_llamacpp")
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"result","content":"local answer"}`}
	// The staging scratch is removed after the run, so verify the staged models.json
	// DURING the run (in onRun) from the staging mount (mounted at /hina, not the
	// durable workspace).
	var modelsExists, stagingMounted bool
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == agentcli.StagingDir {
				stagingMounted = true
				if _, err := os.Stat(filepath.Join(m.Host, "agent", "models.json")); err == nil {
					modelsExists = true
				}
			}
		}
	}
	ar := k.agentRouter("http://host.docker.internal:8081/v1")
	res, err := ar.Handle(ctx, k.scope(), agentCall("pi", "hi"))
	if err != nil || res.Err != "" {
		t.Fatalf("pi run failed: err=%v toolErr=%q", err, res.Err)
	}
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Errorf("pi must inject no secret env, got %v", k.runner.lastSpec.SecretEnv)
	}
	// The staged file is in a fresh scratch mount, NOT the durable workspace.
	if !stagingMounted || !modelsExists {
		t.Fatalf("pi models.json not staged into the staging mount (mounted=%v exists=%v)", stagingMounted, modelsExists)
	}
	ws, _ := k.router.cfg.Workspaces.UserWorkspace(k.userID)
	if _, err := os.Stat(filepath.Join(ws, ".pi", "agent", "models.json")); err == nil {
		t.Error("models.json must NOT be written into the durable workspace")
	}
}

func TestAgentRunDeniedApproval(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk")))
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("runner must not run a denied agent call")
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "denied" {
		t.Fatalf("denied run not audited: %+v", runs)
	}
}

func TestAgentRunRefusesSecretInPrompt(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk")))
	// A vaulted secret whose value appears in the prompt would land on the host argv.
	if _, err := k.vault.Put(ctx, k.userID, "DEPLOY_TOKEN", "", "tok-abc-123"); err != nil {
		t.Fatal(err)
	}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use tok-abc-123 to deploy"))
	if res.Err == "" || !strings.Contains(res.Err, "secret value") {
		t.Fatalf("expected refusal for a secret in the prompt, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("runner must not run when the prompt embeds a secret value")
	}
}

func TestAgentRunBrowserStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	// Seed an encrypted credential store (a tar of a dir holding a token file).
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), "subscription-token")
	tarBytes, err := TarDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes)); err != nil {
		t.Fatal(err)
	}
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"ok"}`}

	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil || res.Err != "" {
		t.Fatalf("browser-state run failed: err=%v toolErr=%q", err, res.Err)
	}
	// Browser-state injects no secret env (the credential is the mounted store).
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Errorf("browser-state must inject no secret env, got %v", k.runner.lastSpec.SecretEnv)
	}
	// The credential store is re-archivable after the run (tokens may refresh).
	got, err := k.vault.GetAgentState(k.userID, "codex")
	if err != nil {
		t.Fatalf("agent state gone after run: %v", err)
	}
	out := filepath.Join(t.TempDir(), "rt")
	kind, data, derr := DecodeCredState(got)
	if derr != nil || kind != CredKindTar {
		t.Fatalf("stored state not a tagged tar: kind=%c err=%v", kind, derr)
	}
	if err := UntarToDir(data, out); err != nil {
		t.Fatalf("re-archived state not extractable: %v", err)
	}
	if readFile(t, filepath.Join(out, "auth.json")) != "subscription-token" {
		t.Error("credential store content lost across the run round-trip")
	}
}

func TestAgentRunFailedExecutionKeepsCredentialStore(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), "original-login")
	tarBytes, err := TarDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes)); err != nil {
		t.Fatal(err)
	}
	// A spawn/execution-layer failure (res.Err set) must NOT re-archive — the prior
	// login has to survive a failed run rather than being clobbered.
	k.runner.result = RunResult{Err: errors.New("sbx spawn failed"), SandboxID: "sbx_x"}

	ar := k.agentRouter("")
	if _, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	got, err := k.vault.GetAgentState(k.userID, "codex")
	if err != nil {
		t.Fatalf("credential store gone after a failed run: %v", err)
	}
	out := filepath.Join(t.TempDir(), "rt")
	kind, data, derr := DecodeCredState(got)
	if derr != nil || kind != CredKindTar {
		t.Fatalf("stored state not a tagged tar: kind=%c err=%v", kind, derr)
	}
	if err := UntarToDir(data, out); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(out, "auth.json")) != "original-login" {
		t.Error("a failed run must not overwrite the stored login")
	}
}

func TestAgentRunProviderNotAllowed(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk")))
	// Admin allow-list excludes codex — an existing profile must NOT run (enforced at
	// the run path, not just hidden in the UI).
	ar := k.router.NewAgentRouter(AgentRouterConfig{
		State: k.vault, Profiles: k.store, AllowedProviders: []string{"claude"},
	})
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if res.Err == "" || !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("expected policy refusal, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("a policy-disallowed agent must not run")
	}
}

// An AUTOMATION agent run is bound by its OWN sandbox profile, NOT the user's interactive
// Sandbox Environment tool policy — so HandleAutomation runs even when agent.<provider>.run
// is removed from that interactive policy, while interactive Handle is still blocked (round-29).
func TestAgentRunAutomationIgnoresInteractiveEnvPolicy(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-x")))
	// The user's interactive policy does NOT permit agent.codex.run (shell only).
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx_x", Stdout: `{"type":"agent_message","message":"ok"}`}
	ar := k.agentRouter("")

	// Interactive Handle is still blocked by the env policy.
	if res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it")); res.Err == "" || !strings.Contains(res.Err, "Sandbox Environment policy") {
		t.Fatalf("interactive Handle must still honor the env policy, got %q", res.Err)
	}
	k.runner.called = false // reset

	// The automation path runs regardless of the interactive tool policy.
	res, err := ar.HandleAutomation(ctx, k.scope(), agentCall("codex", "do it"), AgentRunOptions{
		Workspace: "/run/scratch", Workdir: "/workspace/pr", AutoApprove: true,
	})
	if err != nil {
		t.Fatalf("HandleAutomation: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("an automation agent run must NOT be blocked by the interactive env policy: %s", res.Err)
	}
	if !k.runner.called {
		t.Error("the automation agent run should have executed")
	}
}

// When the automation runtime supplies a StateRoot, the agent's credential scratch must NOT
// be removed when HandleAutomation returns — it lives under the caller-owned, watchdog-counted
// dir, and removing it here would hide a fast over-cap credential-mount write from the run's
// final disk check (round-61). The interactive path (no StateRoot) still cleans its own.
func TestAgentRunAutomationLeavesStateScratchForCaller(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-x")))
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx_x", Stdout: `{"type":"agent_message","message":"ok"}`}
	ar := k.agentRouter("")
	stateRoot := t.TempDir()
	res, err := ar.HandleAutomation(ctx, k.scope(), agentCall("codex", "do it"), AgentRunOptions{
		Workspace: "/run/scratch", Workdir: "/workspace", AutoApprove: true, StateRoot: stateRoot,
	})
	if err != nil || res.Err != "" {
		t.Fatalf("HandleAutomation: err=%v res.Err=%q", err, res.Err)
	}
	// The credential scratch must still be present under StateRoot (the caller cleans the whole
	// root AFTER its final disk check) — proving it wasn't removed before that check.
	entries, derr := os.ReadDir(stateRoot)
	if derr != nil || len(entries) == 0 {
		t.Fatalf("the automation agent state scratch must linger under StateRoot for the caller's disk check (entries=%d err=%v)", len(entries), derr)
	}
}

// An UNATTENDED automation agent run must decide on the COMPLETE output: it parses the full
// redacted capture file (not the 64 KiB inline stream) and fails closed if the capture was
// truncated or couldn't be persisted — a later step must never act on a partial review (round-62).
func TestAgentRunAutomationParsesFullCaptureFailsClosed(t *testing.T) {
	ctx := context.Background()
	setup := func() (*routerKit, *AgentRouter) {
		k := newRouterKit(t, ApprovalAuto)
		k.setProfile(t, "codex", "api_key")
		_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-x")))
		return k, k.agentRouter("")
	}
	opts := func() AgentRunOptions {
		return AgentRunOptions{Workspace: "/run", Workdir: "/workspace", AutoApprove: true, StateRoot: t.TempDir()}
	}

	// (a) inline stream is partial but the capture FILE holds the full output -> parse the file.
	k, ar := setup()
	full := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(full, []byte(`{"type":"agent_message","message":"FULLMSG"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx", Stdout: `{"type":"agent_mess`, StdoutPath: full}
	res, err := ar.HandleAutomation(ctx, k.scope(), agentCall("codex", "x"), opts())
	if err != nil || res.Err != "" || !strings.Contains(res.Content, "FULLMSG") {
		t.Fatalf("automation agent run must parse the FULL capture, got err=%v res.Err=%q content=%q", err, res.Err, res.Content)
	}

	// (b) the capture was truncated (output > capture cap) -> fail closed.
	k2, ar2 := setup()
	k2.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx", Stdout: "{}", StdoutTruncated: true}
	if res2, _ := ar2.HandleAutomation(ctx, k2.scope(), agentCall("codex", "x"), opts()); res2.Err == "" {
		t.Fatal("a truncated agent capture must fail closed")
	}

	// (c) the output couldn't be captured at all -> fail closed.
	k3, ar3 := setup()
	k3.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx", Stdout: "{}", CaptureErr: "disk full"}
	if res3, _ := ar3.HandleAutomation(ctx, k3.scope(), agentCall("codex", "x"), opts()); res3.Err == "" {
		t.Fatal("an agent capture error must fail closed")
	}
}

// An agent_cli prompt carrying a vaulted secret in its JSON-ESCAPED form (a PEM with \n/\"/\\
// that a template rendered into the prompt argv via a json.Marshal'd object) must be refused
// before launch — the plaintext-only argv guard would miss it and put it on the sbx command
// line + send it to the provider (round-69).
func TestAgentRunRefusesJSONEscapedSecretInPrompt(t *testing.T) {
	ctx := context.Background()
	pem := "-----BEGIN KEY-----\nmid\"q\\b\n-----END KEY-----"
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	// The injected credential IS the PEM, so the run redactor knows it.
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(pem)))
	ar := k.agentRouter("")
	// Build a prompt that embeds the secret in its json.Marshal'd (escaped) form.
	obj, _ := json.Marshal(map[string]any{"key": pem})
	res, err := ar.HandleAutomation(ctx, k.scope(), agentCall("codex", "use "+string(obj)), AgentRunOptions{
		Workspace: "/run", Workdir: "/workspace", AutoApprove: true, StateRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("HandleAutomation: %v", err)
	}
	if res.Err == "" || !strings.Contains(res.Err, "secret") {
		t.Fatalf("an escaped secret in the agent prompt must be refused, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the runner must NOT be invoked when the prompt carries an (escaped) secret")
	}
	// The blocked-run audit summary must NOT persist the escaped (or plaintext) secret either.
	pemEsc, _ := json.Marshal(pem)
	escBody := string(pemEsc[1 : len(pemEsc)-1]) // the escaped PEM body that appeared in the prompt
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	for _, r := range runs {
		if strings.Contains(r.Command, pem) || strings.Contains(r.Command, escBody) {
			t.Fatalf("the (escaped) secret leaked into the audit command: %q", r.Command)
		}
	}
}

func TestAgentRunBlockedByEnvironment(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk")))
	// Remove agent.codex.run from the user's Sandbox Environment (allow only shell):
	// a configured agent must still be blocked by the per-user tool allow-list.
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if res.Err == "" || !strings.Contains(res.Err, "Sandbox Environment policy") {
		t.Fatalf("expected an Environment-policy block, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("an agent removed from the user's policy must not run")
	}
}

func TestAgentRunCredKindMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	// The profile says api_key but the blob is a tar (a drifted/partial write) — the
	// run must refuse rather than inject the tar bytes as the key.
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, []byte("not-a-key")))
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "unexpected form") {
		t.Fatalf("expected cred-kind mismatch refusal, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("a cred/profile mismatch must not run")
	}
}

func TestAgentRunLogoutDuringRunDoesNotResurrect(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), "token")
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"ok"}`}
	// Simulate a logout landing DURING the run (delete the profile while the runner
	// "executes"); the post-run re-persist must not resurrect the credential store.
	k.runner.onRun = func() {
		_ = k.store.DeleteAgentProfile(ctx, k.userID, "codex")
		_ = k.vault.DeleteAgentState(k.userID, "codex")
	}
	ar := k.agentRouter("")
	if _, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("a logout during the run must not be undone by re-persisting the store")
	}
}

func TestAgentRunBrowserStateRedactsStoreTokens(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	token := "ya29_verylongbearertokenvalue1234567890"
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"`+token+`"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))
	// The agent echoes a bearer token from the mounted credential store.
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"using ` + token + ` now"}`}

	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	if strings.Contains(res.Content, token) {
		t.Fatalf("browser-state token leaked into the model-visible result: %s", res.Content)
	}
	// The run-time redactor must carry the token so captured output is scrubbed too.
	if !k.runner.lastSpec.Redactor.(*vault.Redactor).ContainsSecret(token) {
		t.Error("run redactor does not cover the credential-store token")
	}
}

func TestAgentRunDeniedRedactsRotatedKey(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.setProfile(t, "codex", "api_key")
	oldKey := "sk-oldkeyvalue1234567890"
	newKey := "sk-newrotatedkeyvalue0987654321"
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(oldKey)))
	// The approval is DENIED, but the credential is rotated DURING the approval window
	// (as a concurrent SetKey would). The prompt embeds the NEW key.
	k.apr.approve = false
	k.apr.onApprove = func() {
		_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(newKey)))
	}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+newKey+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "denied" {
		t.Fatalf("denied run not audited: %+v", runs)
	}
	// The denied summary must be redacted over the CURRENT (rotated) credential.
	if strings.Contains(runs[0].Command, newKey) {
		t.Fatalf("a credential rotated during approval leaked into the denied audit: %q", runs[0].Command)
	}
}

func TestAgentRunApprovedRefusesOldRotatedKeyInPrompt(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = true
	k.setProfile(t, "codex", "api_key")
	oldKey := "sk-oldkeyvalue1234567890"
	newKey := "sk-newrotatedkeyvalue0987654321"
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(oldKey)))
	// The key is rotated DURING approval; the prompt embeds the OLD (now-rotated) key,
	// which may still be live. The argv guard must refuse it (the pre-approval credential
	// is carried into the run-time redactor), not launch it on the host argv.
	k.apr.onApprove = func() {
		_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(newKey)))
	}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+oldKey+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "secret value") {
		t.Fatalf("expected refusal for a rotated-away credential in the prompt, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("must not run with an old (rotated-away) credential on the argv")
	}
}

func TestAgentRunDeniedRedactsOldRotatedKey(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	oldKey := "sk-oldkeyvalue1234567890"
	newKey := "sk-newrotatedkeyvalue0987654321"
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(oldKey)))
	k.apr.onApprove = func() {
		_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(newKey)))
	}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+oldKey+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if strings.Contains(runs[0].Command, oldKey) {
		t.Fatalf("an old (rotated-away) credential leaked into the denied audit: %q", runs[0].Command)
	}
}

func TestAgentRunSummaryRedactsCredentialAtBoundary(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	key := "sk-BOUNDARYMARKER" + strings.Repeat("z", 30) // 47 chars
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(key)))
	// Position the key so it straddles the 200-rune summary cutoff: redaction must run
	// over the FULL prompt before truncation, or a credential prefix survives.
	prompt := strings.Repeat("a", 190) + key + " tail"
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", prompt))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if strings.Contains(runs[0].Command, "sk-BOUNDARYMARKER") {
		t.Fatalf("a credential straddling the summary length cap survived: %q", runs[0].Command)
	}
}

// failGetProfiles wraps an AgentProfileStore to fail GetAgentProfile once armed.
type failGetProfiles struct {
	AgentProfileStore
	fail bool
}

func (f *failGetProfiles) GetAgentProfile(ctx context.Context, userID, provider string) (store.AgentProfile, error) {
	if f.fail {
		return store.AgentProfile{}, errors.New("profile store unavailable")
	}
	return f.AgentProfileStore.GetAgentProfile(ctx, userID, provider)
}

// hideProfileAfterN returns the real profile for the first `after` GetAgentProfile
// calls, then ErrNotFound — to simulate a logout landing after the eligibility load but
// before the launch fence.
type hideProfileAfterN struct {
	AgentProfileStore
	calls int
	after int
}

func (h *hideProfileAfterN) GetAgentProfile(ctx context.Context, userID, provider string) (store.AgentProfile, error) {
	h.calls++
	if h.calls > h.after {
		return store.AgentProfile{}, store.ErrNotFound
	}
	return h.AgentProfileStore.GetAgentProfile(ctx, userID, provider)
}

func TestAgentRunLaunchFenceRefusesAfterLogout(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-realkeyvalue99887766")))
	// Profile present at eligibility + the post-approval re-check, gone at the launch
	// fence (a logout landing after the credential was materialized).
	hp := &hideProfileAfterN{AgentProfileStore: k.store, after: 2}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: k.vault, Profiles: hp})
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "removed before the run") {
		t.Fatalf("expected the launch fence to refuse, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the run launched despite the credential being revoked before launch")
	}
}

// testContentVer is a write-unique-enough version for fakes that rotate by CONTENT.
func testContentVer(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// swapStateAfterN returns the real blob for the first `after` reads, then `newBlob` — to
// simulate a credential REPLACEMENT (SetKey / re-auth) landing after the run materialized
// the credential but before the launch fence re-checks its version. All read methods
// (GetAgentState, GetAgentStateVersioned, AgentStateVersion) share the read counter.
type swapStateAfterN struct {
	AgentStateStore
	reads   int
	after   int
	newBlob []byte
}

func (s *swapStateAfterN) pick(userID, provider string) ([]byte, error) {
	s.reads++
	if s.reads > s.after {
		return s.newBlob, nil
	}
	return s.AgentStateStore.GetAgentState(userID, provider)
}

func (s *swapStateAfterN) GetAgentState(userID, provider string) ([]byte, error) {
	return s.pick(userID, provider)
}

func (s *swapStateAfterN) GetAgentStateVersioned(userID, provider string) ([]byte, string, error) {
	b, err := s.pick(userID, provider)
	if err != nil {
		return nil, "", err
	}
	return b, testContentVer(b), nil
}

func (s *swapStateAfterN) AgentStateVersion(userID, provider string) string {
	b, err := s.pick(userID, provider)
	if err != nil {
		return ""
	}
	return testContentVer(b)
}

func TestAgentRunLaunchFenceRefusesOnCredentialReplacement(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-originalkey1234567890")))
	// loadCredBlob reads the original key once (read #2, after the early redactor #1) and
	// binds the version to it; the launch fence's re-read (#3) sees a rotated blob → refuse.
	sw := &swapStateAfterN{AgentStateStore: k.vault, after: 2,
		newBlob: EncodeCredState(CredKindKey, []byte("sk-rotatedkey0987654321"))}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: sw, Profiles: k.store})
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "changed or removed before the run") {
		t.Fatalf("expected the version fence to refuse a replaced credential, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the run launched with a stale credential after replacement")
	}
}

// seqState returns a scripted blob per read (reads past the end repeat the last), to
// model an A->B->A rotation across a run's reads. All read methods share the counter.
type seqState struct {
	AgentStateStore
	blobs [][]byte
	n     int
}

func (s *seqState) next() []byte {
	i := s.n
	s.n++
	if i >= len(s.blobs) {
		i = len(s.blobs) - 1
	}
	return s.blobs[i]
}

func (s *seqState) GetAgentState(userID, provider string) ([]byte, error) { return s.next(), nil }

func (s *seqState) GetAgentStateVersioned(userID, provider string) ([]byte, string, error) {
	b := s.next()
	return b, testContentVer(b), nil
}

func (s *seqState) AgentStateVersion(userID, provider string) string {
	return testContentVer(s.next())
}

// recreateState returns the SAME blob content but different versions for the materialize
// read vs the fence read — modeling a logout + same-key re-add (re-encrypted, so the
// write-unique version changes while the plaintext is identical).
type recreateState struct {
	AgentStateStore
	blob           []byte
	materializeVer string
	currentVer     string
}

func (s *recreateState) GetAgentState(userID, provider string) ([]byte, error) {
	return s.blob, nil
}

func (s *recreateState) GetAgentStateVersioned(userID, provider string) ([]byte, string, error) {
	return s.blob, s.materializeVer, nil
}

func (s *recreateState) AgentStateVersion(userID, provider string) string { return s.currentVer }

func TestAgentRunLaunchFenceRefusesABARotation(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	a := EncodeCredState(CredKindKey, []byte("sk-oldkeyvalue1234567890"))
	b := EncodeCredState(CredKindKey, []byte("sk-newkeyvalue0987654321"))
	// Reads: early redactor (A) -> loadCredBlob materializes (B) -> fence (A). The run
	// materialized B; a snapshot-before-materialize fence would wrongly pass (current==A),
	// but binding the version to the materialized blob (B) refuses (current A != B).
	seq := &seqState{AgentStateStore: k.vault, blobs: [][]byte{a, b, a}}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: seq, Profiles: k.store})
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "changed or removed before the run") {
		t.Fatalf("an A->B->A rotation must refuse the run, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the run launched with an ABA-rotated credential")
	}
}

func TestAgentRunLaunchFenceRefusesSameValueRecreate(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	key := EncodeCredState(CredKindKey, []byte("sk-samekeyvalue1234567890"))
	// Same plaintext at materialize and fence, but the WRITE-UNIQUE version differs — a
	// logout + same-key re-add re-encrypts the blob (a plaintext content hash would miss
	// this; the encrypted-envelope version catches it).
	rs := &recreateState{blob: key, materializeVer: "v1", currentVer: "v2"}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: rs, Profiles: k.store})
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "changed or removed before the run") {
		t.Fatalf("a same-value delete+recreate must refuse the run, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the run launched after a same-value credential recreate")
	}
}

func TestRunRegistryCancel(t *testing.T) {
	reg := &RunRegistry{}
	ctx, cancel := context.WithCancel(context.Background())
	release := reg.Add("u1", "codex", cancel)
	// A different provider isn't cancelled.
	reg.Cancel("u1", "claude")
	select {
	case <-ctx.Done():
		t.Fatal("a different provider's run was cancelled")
	default:
	}
	// The matching (user, provider) is cancelled.
	reg.Cancel("u1", "codex")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the matching run was not cancelled")
	}
	release()
	reg.Cancel("u1", "codex") // no-op after release; must not panic
}

func TestAgentRunDeniedRedactorProfileReadFailureFailsSafe(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	oldKey := "sk-oldkeyvalue1234567890"
	newKey := "sk-newrotatedkeyvalue0987654321"
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(oldKey)))

	fp := &failGetProfiles{AgentProfileStore: k.store}
	ar := k.router.NewAgentRouter(AgentRouterConfig{State: k.vault, Profiles: fp})
	// During approval, rotate the key AND arm a profile-read failure, so deniedRedactor's
	// current-credential reload fails — it must fall back to the generic prompt-free
	// summary, not persist the prompt (which embeds the NEW key) unredacted.
	k.apr.onApprove = func() {
		_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(newKey)))
		fp.fail = true
	}
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+newKey+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if strings.Contains(runs[0].Command, newKey) {
		t.Fatalf("rotated key leaked when the denial-time profile read failed: %q", runs[0].Command)
	}
	if !strings.Contains(runs[0].Command, "redaction unavailable") {
		t.Fatalf("expected the generic prompt-free summary, got %q", runs[0].Command)
	}
}

func TestAgentRunStoreScanFileCountCapWithholdsOutput(t *testing.T) {
	ctx := context.Background()
	old := maxAgentStateFiles
	defer func() { maxAgentStateFiles = old }()
	maxAgentStateFiles = 3
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-key1234567890abcd")))
	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte("agent output"), 0o600)
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile,
		Stdout: `{"type":"agent_message","message":"the answer"}`}
	// The CLI floods its writable store with many under-per-file-cap files.
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == "/agent/codex" {
				for i := 0; i < 5; i++ {
					_ = os.WriteFile(filepath.Join(m.Host, "f"+strconv.Itoa(i)+".json"), []byte("x"), 0o600)
				}
			}
		}
	}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	var parsed agentcli.AgentRunResult
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	if parsed.FinalText != "" || parsed.StdoutPath != "" {
		t.Fatalf("output must be withheld when the store exceeds the scan cap: %+v", parsed)
	}
	if _, statErr := os.Stat(capFile); statErr == nil {
		t.Error("the capture file should be deleted when the store can't be bounded-scanned")
	}
}

func TestAgentRunBrowserStateTokenFloodRefused(t *testing.T) {
	ctx := context.Background()
	old := maxScanTokens
	defer func() { maxScanTokens = old }()
	maxScanTokens = 5
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	// A stored credential store packed with many UNIQUE token-shaped values: the pre-run
	// bounded scanner must refuse the run (fail closed), not allocate an unbounded set.
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("uniquetokenvaluexxxxxx" + strconv.Itoa(i) + "\n")
	}
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, []byte(b.String())))
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if res.Err == "" || !strings.Contains(res.Err, "too large to scan") {
		t.Fatalf("expected the run refused on a token-flooded store, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("the run launched despite an unscannable credential store")
	}
}

func TestCredStoreRedactorErrorIsPathFree(t *testing.T) {
	old := maxAgentStateFile
	defer func() { maxAgentStateFile = old }()
	maxAgentStateFile = 4
	dir := t.TempDir()
	tokenName := "bearer-token-abcdef1234567890.json"
	_ = os.WriteFile(filepath.Join(dir, tokenName), []byte("oversized"), 0o600)
	_, err := CredStoreRedactor(dir)
	if err == nil {
		t.Fatal("expected an oversize scan error")
	}
	if strings.Contains(err.Error(), "bearer-token") {
		t.Fatalf("scan error leaked a credential-store filename: %v", err)
	}
}

func TestTarDirErrorIsPathFree(t *testing.T) {
	old := maxAgentStateFile
	defer func() { maxAgentStateFile = old }()
	maxAgentStateFile = 4
	dir := t.TempDir()
	tokenName := "device-code-XYZ12345678901234.json"
	_ = os.WriteFile(filepath.Join(dir, tokenName), []byte("oversized"), 0o600)
	if _, err := TarDir(dir); err == nil || strings.Contains(err.Error(), "device-code") {
		t.Fatalf("TarDir error leaked a credential-store filename: %v", err)
	}
}

func TestCredStoreScanUnreadableEntryErrorsArePathFree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	tokenSub := filepath.Join(dir, "ghp-bearertoken9988776655.d")
	if err := os.MkdirAll(tokenSub, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tokenSub, "f"), []byte("x"), 0o600)
	if err := os.Chmod(tokenSub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tokenSub, 0o700) })
	// A raw os.PathError would embed the unreadable token-named entry; both helpers must
	// return a path-free error instead.
	if _, err := CredStoreRedactor(dir); err == nil {
		t.Skip("walk did not error on the unreadable dir")
	} else if strings.Contains(err.Error(), "ghp-bearertoken") {
		t.Fatalf("CredStoreRedactor leaked the unreadable entry name: %v", err)
	}
	if _, err := TarDir(dir); err == nil {
		t.Skip("walk did not error on the unreadable dir")
	} else if strings.Contains(err.Error(), "ghp-bearertoken") {
		t.Fatalf("TarDir leaked the unreadable entry name: %v", err)
	}
}

func TestUntarToDirWriteFailureErrorIsPathFree(t *testing.T) {
	src := t.TempDir()
	entry := "ghp-tarentrytoken99887766.txt"
	_ = os.WriteFile(filepath.Join(src, entry), []byte("x"), 0o600)
	tarBytes, err := TarDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	// Pre-create a DIRECTORY where the regular entry must be written, so the file create
	// fails deterministically (EISDIR) — the error must not carry the token-shaped name.
	if err := os.Mkdir(filepath.Join(dst, entry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := UntarToDir(tarBytes, dst); err == nil {
		t.Skip("extraction did not fail on this platform") // the error hygiene is moot
	} else if strings.Contains(err.Error(), "ghp-tarentrytoken") {
		t.Fatalf("UntarToDir leaked a token-named entry on a write failure: %v", err)
	}
}

func TestAgentRunOversizeStoreFileNameNotLogged(t *testing.T) {
	ctx := context.Background()
	old := maxAgentStateFile
	defer func() { maxAgentStateFile = old }()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	store0 := t.TempDir()
	writeFile(t, filepath.Join(store0, "auth.json"), `{"access_token":"oldtokenvalue1234567890abc"}`)
	tarBytes, _ := TarDir(store0)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"ok"}`}
	// During the run the CLI writes an oversized file whose NAME is token-shaped.
	tokenName := "ghp-secretbearertoken9988776655.json"
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == "/agent/codex" {
				_ = os.WriteFile(filepath.Join(m.Host, tokenName), []byte("oversized-content"), 0o600)
			}
		}
		maxAgentStateFile = 5 // make that file oversized for the post-run scan
	}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if strings.Contains(res.Content, "ghp-secretbearertoken") {
		t.Fatalf("token-shaped filename leaked into the result: %s", res.Content)
	}
	if strings.Contains(k.logBuf.String(), "ghp-secretbearertoken") {
		t.Fatalf("token-shaped filename leaked into the logs: %s", k.logBuf.String())
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 5)
	if len(runs) > 0 && strings.Contains(runs[0].Command, "ghp-secretbearertoken") {
		t.Fatalf("token-shaped filename leaked into the audit row: %s", runs[0].Command)
	}
}

func TestAgentRunStoreScanDirFloodWithholdsOutput(t *testing.T) {
	ctx := context.Background()
	old := maxAgentStateFiles
	defer func() { maxAgentStateFiles = old }()
	maxAgentStateFiles = 3
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-key1234567890abcd")))
	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte("agent output"), 0o600)
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile,
		Stdout: `{"type":"agent_message","message":"the answer"}`}
	// A flood of EMPTY dirs (zero regular files/bytes) must still trip the entry cap.
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == "/agent/codex" {
				for i := 0; i < 6; i++ {
					_ = os.MkdirAll(filepath.Join(m.Host, "d"+strconv.Itoa(i)), 0o700)
				}
			}
		}
	}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	var parsed agentcli.AgentRunResult
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	if parsed.FinalText != "" || parsed.StdoutPath != "" {
		t.Fatalf("output must be withheld when the store has too many entries: %+v", parsed)
	}
}

func TestAgentRunStoreScanTokenFloodWithholdsOutput(t *testing.T) {
	ctx := context.Background()
	old := maxScanTokens
	defer func() { maxScanTokens = old }()
	maxScanTokens = 5
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-key1234567890abcd")))
	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte("agent output"), 0o600)
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile,
		Stdout: `{"type":"agent_message","message":"the answer"}`}
	// A single small file packed with many UNIQUE token-shaped values (under the byte
	// cap) must still trip the extracted-token cap.
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == "/agent/codex" {
				var b strings.Builder
				for i := 0; i < 20; i++ {
					b.WriteString("uniquetokenvaluexxxxxx" + strconv.Itoa(i) + "\n")
				}
				_ = os.WriteFile(filepath.Join(m.Host, "tokens.json"), []byte(b.String()), 0o600)
			}
		}
	}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	var parsed agentcli.AgentRunResult
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	if parsed.FinalText != "" || parsed.StdoutPath != "" {
		t.Fatalf("output must be withheld when the store has too many token-shaped values: %+v", parsed)
	}
}

func TestAgentRunApiKeyScrubsMintedStoreToken(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-injectedkey1234567890")))
	minted := "derived-bearer-token-abcdef1234567890"
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"exchanged for ` + minted + `"}`}
	// The api_key CLI mints a derived token into its writable store mount and echoes it.
	k.runner.onRun = func() {
		for _, m := range k.runner.lastSpec.Mounts {
			if m.Container == "/agent/codex" {
				_ = os.WriteFile(filepath.Join(m.Host, "cached.json"), []byte(`{"bearer":"`+minted+`"}`), 0o600)
			}
		}
	}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "hi"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	if strings.Contains(res.Content, minted) {
		t.Fatalf("a token minted into the api_key store leaked into the result: %s", res.Content)
	}
}

func TestAgentRunDeniedMissingBlobFailsSafe(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	// Configured profile but NO stored credential blob: the denied summary must be the
	// prompt-free generic (can't prove the credential is redacted), not the prompt.
	secret := "sk-tokenshapedvalue1234567890"
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+secret+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if strings.Contains(runs[0].Command, secret) {
		t.Fatalf("prompt persisted in the denied audit despite a missing credential blob: %q", runs[0].Command)
	}
	if !strings.Contains(runs[0].Command, "redaction unavailable") {
		t.Fatalf("expected the prompt-free generic summary, got %q", runs[0].Command)
	}
}

func TestAgentRunBrowserStateUnscannableStateWithholdsOutput(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"oldtokenvalue1234567890abc"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))

	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte("agent output line"), 0o600)
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile,
		Stdout: `{"type":"agent_message","message":"the answer"}`}
	// Make the mounted store unscannable AFTER the mount is prepared (shrink the per-file
	// cap mid-run so the post-run scan sees the store file as oversized → fail closed).
	old := maxAgentStateFile
	defer func() { maxAgentStateFile = old }()
	k.runner.onRun = func() { maxAgentStateFile = 5 }

	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	var parsed agentcli.AgentRunResult
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	if parsed.FinalText != "" || parsed.StdoutPath != "" {
		t.Fatalf("output must be withheld when the refreshed store can't be fully scanned: %+v", parsed)
	}
	if _, statErr := os.Stat(capFile); statErr == nil {
		t.Error("the capture file should be deleted when the store can't be scanned")
	}
}

func TestAgentRunBrowserStateMissingBlobFailsClosed(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	// A configured browser_state profile but NO stored credential store (partial
	// logout) must fail closed — never run with provider egress and an empty store.
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if res.Err == "" || !strings.Contains(res.Err, "missing") {
		t.Fatalf("expected a missing-credential-store refusal, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("must not run a browser_state agent with no stored credential store")
	}
}

func TestAgentRunCaptureScrubFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"sometokenvalue1234567890abc"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))
	// Point the capture path at an (empty) directory so the post-run re-scrub read
	// fails: the file must be removed and its path cleared rather than referenced.
	badCapture := t.TempDir()
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: badCapture, StdoutTruncated: true,
		Stdout: `{"type":"agent_message","message":"ok"}`}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	var parsed agentcli.AgentRunResult
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	if parsed.StdoutPath != "" {
		t.Fatalf("an unscrubbable capture path must be cleared, got %q", parsed.StdoutPath)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].StdoutPath != "" || !strings.Contains(runs[0].Error, "scrub failed") {
		t.Fatalf("audit row should record the cleared capture + the scrub gap: %+v", runs)
	}
}

func TestTarDirArchivesManyFilesWithoutHoldingDescriptors(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(src, "f"+strconv.Itoa(i)), "data"+strconv.Itoa(i))
	}
	data, err := TarDir(src)
	if err != nil {
		t.Fatalf("archiving many files failed (descriptors not released?): %v", err)
	}
	out := filepath.Join(t.TempDir(), "rt")
	if err := UntarToDir(data, out); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(out, "f150")) != "data150" {
		t.Error("round-trip of many files lost content")
	}
}

func TestAgentRunStagingDoesNotFollowWorkspaceSymlink(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-realkeyvalue99887766")))

	// Plant a symlink in the durable workspace at the OLD staging path, pointing at a
	// victim file OUTSIDE the workspace. Staging must NOT follow it (it now writes into
	// a fresh scratch mount, never the durable workspace).
	ws, _ := k.router.cfg.Workspaces.UserWorkspace(k.userID)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(ws, ".hina"), 0o700)
	if err := os.Symlink(victim, filepath.Join(ws, ".hina", "output-schema.json")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	schema := json.RawMessage(`{"type":"object"}`)
	args, _ := json.Marshal(map[string]any{"prompt": "do x", "structured": true, "schema": schema})
	call := ToolCall{ID: "x", Name: agentcli.ToolName(agentcli.ProviderCodex), Arguments: args}
	k.runner.result = RunResult{ExitCode: 0, Stdout: `{"type":"agent_message","message":"ok"}`}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), call)
	if res.Err != "" {
		t.Fatalf("run failed: %q", res.Err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "ORIGINAL" {
		t.Fatal("staging followed a workspace symlink and overwrote a file outside the sandbox")
	}
}

func TestRedactAgentResultJSONEscaped(t *testing.T) {
	secret := `mytok"secret12345678`
	red := vault.NewRedactor([]string{secret})
	// The structured field embeds the secret under a NON-canonical encoding (escaped
	// solidus + a unicode escape) that plaintext substring matching can't catch.
	structured := []byte(`{"a":"mytok\"secret12345678","b":"x\/y"}`)
	res := &agentcli.AgentRunResult{
		FinalText:  "plain " + secret,
		Structured: json.RawMessage(structured),
	}
	redactAgentResult(res, red)
	if strings.Contains(res.FinalText, secret) {
		t.Fatalf("plaintext secret survived in FinalText: %q", res.FinalText)
	}
	// After decode+redact+re-marshal, the secret (in any encoding) must be gone.
	if strings.Contains(string(res.Structured), "secret12345678") {
		t.Fatalf("a non-canonically-encoded secret survived in Structured: %s", res.Structured)
	}
}

func TestAgentRunRefusesJSONEscapedSecretInSchema(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-key1234567890abcd")))
	secret := `vaultsecret"withquote12345`
	if _, err := k.vault.Put(ctx, k.userID, "MY_SECRET", "", secret); err != nil {
		t.Fatal(err)
	}
	// A structured run whose schema embeds the vaulted secret — JSON-marshaling escapes
	// the quote, so the plaintext redactor would miss it; the JSON-variant guard catches it.
	schemaJSON, _ := json.Marshal(map[string]any{"type": "object", "description": "use " + secret})
	args, _ := json.Marshal(map[string]any{"prompt": "hi", "structured": true, "schema": json.RawMessage(schemaJSON)})
	call := ToolCall{ID: "x", Name: agentcli.ToolName(agentcli.ProviderCodex), Arguments: args}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), call)
	if res.Err == "" || !strings.Contains(res.Err, "contains a secret value") {
		t.Fatalf("expected the staged-schema guard to catch the JSON-escaped secret, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("a secret-bearing schema was handed to the agent CLI")
	}
}

func TestAgentRunRefusesNumericSecretInSchema(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-key1234567890abcd")))
	// A vaulted secret that is numeric: embedded in a schema as a JSON NUMBER it would
	// bypass string-only traversal, but the raw-byte backstop catches it.
	if _, err := k.vault.Put(ctx, k.userID, "PIN", "", "31337424242"); err != nil {
		t.Fatal(err)
	}
	schemaJSON := []byte(`{"type":"object","const":31337424242}`)
	args, _ := json.Marshal(map[string]any{"prompt": "hi", "structured": true, "schema": json.RawMessage(schemaJSON)})
	call := ToolCall{ID: "x", Name: agentcli.ToolName(agentcli.ProviderCodex), Arguments: args}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), call)
	if res.Err == "" || !strings.Contains(res.Err, "contains a secret value") {
		t.Fatalf("expected the staged-schema guard to catch the numeric secret, got %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("a numeric-secret-bearing schema was handed to the agent CLI")
	}
}

func TestAgentRunRefusesSecretInStagedSchema(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-realkeyvalue99887766")))
	if _, err := k.vault.Put(ctx, k.userID, "DEPLOY_TOKEN", "", "tok-secret-1234567890"); err != nil {
		t.Fatal(err)
	}
	// A structured run whose output schema embeds a vaulted secret: the staged file is
	// written to the workspace + handed to the CLI, so it must be refused like the argv.
	schema := `{"type":"object","description":"send to tok-secret-1234567890"}`
	args, _ := json.Marshal(map[string]any{"prompt": "do x", "structured": true, "schema": json.RawMessage(schema)})
	call := ToolCall{ID: "x", Name: agentcli.ToolName(agentcli.ProviderCodex), Arguments: args}
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), call)
	if res.Err == "" || !strings.Contains(res.Err, "staged file") {
		t.Fatalf("expected refusal for a secret in the staged schema, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("must not run when a staged file embeds a secret")
	}
}

func TestAgentRunBrowserStateRefreshedTokenAtCapBoundary(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"oldtokenvalue1234567890abc"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))

	refreshed := "refreshedtokenvalue9876543210xyzabc" // 35 chars; minted mid-run
	prefix := refreshed[:20]                           // the prefix retained at a truncation cap
	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte(strings.Repeat("p", 100)+prefix), 0o600) // ends with the straddling prefix
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile, StdoutTruncated: true,
		Stdout: strings.Repeat("p", 100) + prefix + inlineTruncSuffix}
	k.runner.onRun = func() {
		dir := k.runner.lastSpec.Mounts[0].Host
		_ = os.WriteFile(filepath.Join(dir, "refreshed.json"), []byte(`{"t":"`+refreshed+`"}`), 0o600)
	}
	ar := k.agentRouter("")
	if _, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it")); err != nil {
		t.Fatal(err)
	}
	capData, _ := os.ReadFile(capFile)
	if strings.Contains(string(capData), prefix) {
		t.Fatalf("a refreshed token's prefix survived at the capture cap: %q", capData)
	}
}

func TestAgentRunDeniedCorruptCredFailsSafe(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	secret := "sk-secretkeyvalue1234567890"
	// A corrupt (un-decodable) agent-state blob with an EXISTING profile: the router
	// can't prove credential redaction, so the denied summary must be the prompt-free
	// generic — not the prompt (which embeds a credential).
	_ = k.vault.PutAgentState(k.userID, "codex", []byte("not-a-valid-kind-tagged-blob"))
	ar := k.agentRouter("")
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+secret+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if strings.Contains(runs[0].Command, secret) {
		t.Fatalf("prompt persisted despite unprovable credential redaction: %q", runs[0].Command)
	}
	if !strings.Contains(runs[0].Command, "redaction unavailable") {
		t.Fatalf("expected the prompt-free generic summary, got %q", runs[0].Command)
	}
}

func TestAgentRunBrowserStateRefusesTokenInPrompt(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	token := "ya29_verylongbearertokenvalue1234567890"
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"`+token+`"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))
	ar := k.agentRouter("")
	// A store token embedded in the prompt would land on the host argv — the guard
	// (now seeded with the store tokens BEFORE the argv check) must refuse.
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+token+" now"))
	if res.Err == "" || !strings.Contains(res.Err, "secret value") {
		t.Fatalf("expected refusal for a store token in the prompt, got %q", res.Err)
	}
	if k.runner.called {
		t.Error("must not run when the prompt embeds a credential-store token")
	}
}

func TestAgentRunBrowserStateScrubsRefreshedToken(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "browser_state")
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"oldtokenvalue1234567890abc"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))

	refreshed := "refreshedtokenvalue9876543210xyz"
	capFile := filepath.Join(t.TempDir(), "out.log")
	_ = os.WriteFile(capFile, []byte("agent printed "+refreshed+"\n"), 0o600)
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "x", StdoutPath: capFile,
		Stdout: `{"type":"agent_message","message":"token is ` + refreshed + `"}`}
	// Simulate the CLI minting a NEW token into the mounted store mid-run.
	k.runner.onRun = func() {
		dir := k.runner.lastSpec.Mounts[0].Host
		_ = os.WriteFile(filepath.Join(dir, "refreshed.json"), []byte(`{"t":"`+refreshed+`"}`), 0o600)
	}
	ar := k.agentRouter("")
	res, err := ar.Handle(ctx, k.scope(), agentCall("codex", "do it"))
	if err != nil || res.Err != "" {
		t.Fatalf("run failed: err=%v toolErr=%q", err, res.Err)
	}
	if strings.Contains(res.Content, refreshed) {
		t.Fatalf("a token minted during the run leaked into the result: %s", res.Content)
	}
	capData, _ := os.ReadFile(capFile)
	if strings.Contains(string(capData), refreshed) {
		t.Fatalf("a refreshed token was left in the capture file: %s", capData)
	}
}

func TestAgentRunBrowserStateDeniedRedactsToken(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "browser_state")
	token := "ya29_verylongbearertokenvalue1234567890"
	storeDir := t.TempDir()
	writeFile(t, filepath.Join(storeDir, "auth.json"), `{"access_token":"`+token+`"}`)
	tarBytes, _ := TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindTar, tarBytes))

	ar := k.agentRouter("")
	// A store token in the prompt must be redacted from the denied audit row (the
	// pre-approval summary now folds the browser-state store tokens in).
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+token+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "denied" {
		t.Fatalf("denied run not audited: %+v", runs)
	}
	if strings.Contains(runs[0].Command, token) {
		t.Fatalf("browser-state token leaked into the denied audit summary: %q", runs[0].Command)
	}
}

func TestAgentRunDeniedRedactsKeyInPrompt(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false
	k.setProfile(t, "codex", "api_key")
	key := "sk-verysecretkeyvalue1234567890"
	_ = k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte(key)))

	ar := k.agentRouter("")
	// The prompt embeds the stored API key; denial must not leave it in the audit row.
	res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "use "+key+" please"))
	if res.Err == "" || !strings.Contains(res.Err, "denied") {
		t.Fatalf("expected denial, got %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "denied" {
		t.Fatalf("denied run not audited: %+v", runs)
	}
	if strings.Contains(runs[0].Command, key) {
		t.Fatalf("API key leaked into the denied audit summary: %q", runs[0].Command)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestAgentRunAutomationOverride is the Phase 9 plumbing: HandleAutomation mounts the
// automation's OWN run scratch (not the durable workspace), sets the workdir from a
// per-run checkout, and AUTO-APPROVES — while the interactive Handle on the same kit
// is denied. It reuses the full credential/redaction/audit path unchanged.
func TestAgentRunAutomationOverride(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false // the interactive path would be DENIED here
	k.setProfile(t, "codex", "api_key")
	if err := k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-x"))); err != nil {
		t.Fatal(err)
	}
	k.runner.result = RunResult{ExitCode: 0, SandboxID: "sbx_x", Stdout: `{"type":"agent_message","message":"ok"}`}
	ar := k.agentRouter("")

	// Sanity: interactive Handle is denied (ApprovalAlways + the approver denies).
	if res, _ := ar.Handle(ctx, k.scope(), agentCall("codex", "do it")); res.Err == "" {
		t.Fatal("interactive Handle should be denied here")
	}
	k.apr.gotReq = ApprovalRequest{} // reset so we can prove the approver isn't consulted

	res, err := ar.HandleAutomation(ctx, k.scope(), agentCall("codex", "do it"), AgentRunOptions{
		Workspace: "/run/scratch", Workdir: "/workspace/pr-42", AutoApprove: true,
		Limits: Limits{CPUs: "1", Memory: "512m", PIDs: 64},
	})
	if err != nil {
		t.Fatalf("HandleAutomation: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("automation run refused: %s", res.Err)
	}
	if k.apr.gotReq.Tool != "" {
		t.Error("auto-approve must not consult the approver")
	}
	if k.runner.lastSpec.Workspace != "/run/scratch" {
		t.Errorf("workspace = %q, want the run scratch (not the durable workspace)", k.runner.lastSpec.Workspace)
	}
	if k.runner.lastSpec.Workdir != "/workspace/pr-42" {
		t.Errorf("workdir = %q, want /workspace/pr-42", k.runner.lastSpec.Workdir)
	}
	if k.runner.lastSpec.Limits.CPUs != "1" || k.runner.lastSpec.Limits.Memory != "512m" || k.runner.lastSpec.Limits.PIDs != 64 {
		t.Errorf("agent run limits = %+v, want the per-automation caps", k.runner.lastSpec.Limits)
	}
}

// HandleAutomation must REFUSE an empty workspace rather than falling back to the
// user's durable workspace (round-3 finding).
func TestAgentRunAutomationRejectsEmptyWorkspace(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	k.setProfile(t, "codex", "api_key")
	if err := k.vault.PutAgentState(k.userID, "codex", EncodeCredState(CredKindKey, []byte("sk-x"))); err != nil {
		t.Fatal(err)
	}
	ar := k.agentRouter("")
	res, _ := ar.HandleAutomation(context.Background(), k.scope(), agentCall("codex", "do it"), AgentRunOptions{AutoApprove: true})
	if res.Err == "" {
		t.Fatal("an empty workspace override must be refused (no durable-workspace fallback)")
	}
}
