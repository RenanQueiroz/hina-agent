package httpapi

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

func newAgentServer(t *testing.T) *sandboxKit {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Approval = "auto"
	cfg.Sandbox.NetworkIsolated = true
	cfg.Agents.Enabled = true

	bus := events.NewBus(st)
	srv := New(cfg, st, bus, auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(100), slog.New(slog.NewTextHandler(io.Discard, nil)))

	key := make([]byte, platform.MasterKeyLen)
	_, _ = rand.Read(key)
	v, err := vault.New(key, filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	ws, err := sandbox.NewWorkspaceManager(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "run"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	runner := &stubToolRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "ok", SandboxID: "sbx_1"}}
	srv.SetSandbox(v, ws, runner)
	srv.SetReady(true)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	boot, err := auth.EnsureAdmin(ctx, st)
	if err != nil || !boot.Created {
		t.Fatalf("bootstrap admin: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	postJSON(t, client, ts.URL+"/api/v1/auth/login", map[string]string{"username": "admin", "password": boot.Password}, nil)
	return &sandboxKit{ts: ts, client: client, store: st, bus: bus, runner: runner}
}

func TestAgentCatalog(t *testing.T) {
	k := newAgentServer(t)
	var cat wire.AgentCatalog
	getInto(t, k.client, k.ts.URL+"/api/v1/agents", &cat)
	if !cat.Enabled || !cat.NetworkIsolated {
		t.Fatalf("catalog gates = %+v", cat)
	}
	if len(cat.Agents) != 4 {
		t.Fatalf("expected 4 agents, got %d", len(cat.Agents))
	}
	codex := findAgent(cat.Agents, "codex")
	if codex == nil || codex.Configured || codex.Runnable {
		t.Fatalf("codex should be unconfigured + not runnable: %+v", codex)
	}
	if !strings.Contains(codex.Reason, "not configured") {
		t.Errorf("codex reason = %q", codex.Reason)
	}
	// Pi is offered but never runnable without the Phase 11 endpoint.
	pi := findAgent(cat.Agents, "pi")
	if pi == nil || pi.Runnable || !strings.Contains(pi.Reason, "Phase 11") {
		t.Fatalf("pi should be gated on Phase 11: %+v", pi)
	}
}

func TestAgentSetKeyMakesRunnable(t *testing.T) {
	k := newAgentServer(t)
	// Configure an API-key profile.
	resp := postRaw(t, k.client, k.ts.URL+"/api/v1/agents/codex/key", `{"auth_type":"api_key","value":"sk-123"}`)
	if resp != http.StatusNoContent {
		t.Fatalf("set key = %d", resp)
	}
	var cat wire.AgentCatalog
	getInto(t, k.client, k.ts.URL+"/api/v1/agents", &cat)
	codex := findAgent(cat.Agents, "codex")
	if codex == nil || !codex.Configured || !codex.Runnable {
		t.Fatalf("codex should be configured + runnable after key: %+v", codex)
	}
	if codex.ConfiguredAuthType != "api_key" {
		t.Errorf("auth type = %q", codex.ConfiguredAuthType)
	}

	// Logout clears it.
	req, _ := http.NewRequest(http.MethodDelete, k.ts.URL+"/api/v1/agents/codex", nil)
	r, _ := k.client.Do(req)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d", r.StatusCode)
	}
	getInto(t, k.client, k.ts.URL+"/api/v1/agents", &cat)
	if c := findAgent(cat.Agents, "codex"); c.Configured {
		t.Fatal("codex should be unconfigured after logout")
	}
}

func TestAgentCatalogReflectsEnvironmentPolicy(t *testing.T) {
	k := newAgentServer(t)
	// Configure codex so the only remaining gate is the Environment tool policy.
	postRaw(t, k.client, k.ts.URL+"/api/v1/agents/codex/key", `{"auth_type":"api_key","value":"sk-1"}`)
	// Remove agent.codex.run from the user's policy (PUT keeps only shell).
	put := `{"allowed_tools":["shell"],"network":{"default":"deny"}}`
	req, _ := http.NewRequest(http.MethodPut, k.ts.URL+"/api/v1/sandbox/environment", strings.NewReader(put))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := k.client.Do(req)
	_ = resp.Body.Close()

	var cat wire.AgentCatalog
	getInto(t, k.client, k.ts.URL+"/api/v1/agents", &cat)
	codex := findAgent(cat.Agents, "codex")
	if codex == nil || !codex.Configured || codex.Runnable {
		t.Fatalf("codex should be configured but not runnable (policy-disabled): %+v", codex)
	}
	if !strings.Contains(codex.Reason, "Sandbox Environment") {
		t.Errorf("reason should cite the Environment policy, got %q", codex.Reason)
	}
}

func TestAgentSetKeyValidation(t *testing.T) {
	k := newAgentServer(t)
	// Unknown provider.
	if got := postRaw(t, k.client, k.ts.URL+"/api/v1/agents/bogus/key", `{"auth_type":"api_key","value":"x"}`); got != http.StatusNotFound {
		t.Fatalf("unknown provider = %d, want 404", got)
	}
	// Bad auth type.
	if got := postRaw(t, k.client, k.ts.URL+"/api/v1/agents/codex/key", `{"auth_type":"browser_state","value":"x"}`); got != http.StatusBadRequest {
		t.Fatalf("bad auth type = %d, want 400", got)
	}
	// Pi rejects an api_key profile.
	if got := postRaw(t, k.client, k.ts.URL+"/api/v1/agents/pi/key", `{"auth_type":"api_key","value":"x"}`); got != http.StatusBadRequest {
		t.Fatalf("pi api_key = %d, want 400", got)
	}
}

func TestAdminAgents(t *testing.T) {
	k := newAgentServer(t)
	postRaw(t, k.client, k.ts.URL+"/api/v1/agents/claude/key", `{"auth_type":"api_key","value":"sk-abc"}`)
	var admin wire.AdminAgents
	getInto(t, k.client, k.ts.URL+"/api/v1/admin/agents", &admin)
	if !admin.Available {
		t.Fatalf("admin agents should be available: %+v", admin)
	}
	found := false
	for _, p := range admin.Profiles {
		if p.Provider == "claude" {
			found = true
			if p.AuthType != "api_key" || p.Status != "authenticated" || p.Username != "admin" {
				t.Errorf("admin profile = %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("claude profile missing from admin view: %+v", admin.Profiles)
	}
}

func findAgent(agents []wire.AgentInfo, provider string) *wire.AgentInfo {
	for i := range agents {
		if agents[i].Provider == provider {
			return &agents[i]
		}
	}
	return nil
}

func postRaw(t *testing.T, client *http.Client, url, body string) int {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
