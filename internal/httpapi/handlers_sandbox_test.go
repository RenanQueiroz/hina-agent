package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// stubToolRunner is a sandbox.Runner that records the last spec and returns a
// canned result, so the HTTP + router + loop path is exercised without a real sbx.
type stubToolRunner struct {
	called   bool
	lastSpec sandbox.RunSpec
	result   sandbox.RunResult
}

func (s *stubToolRunner) Available() bool { return true }
func (s *stubToolRunner) Status() sandbox.Status {
	return sandbox.Status{Available: true, Version: "0.33.0", Pinned: sandbox.PinnedVersion}
}
func (s *stubToolRunner) Run(_ context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	s.called = true
	s.lastSpec = spec
	return s.result, nil
}

type sandboxKit struct {
	ts     *httptest.Server
	client *http.Client
	store  *store.Store
	bus    *events.Bus
	runner *stubToolRunner
}

func newSandboxServer(t *testing.T, approval string) *sandboxKit {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Approval = approval

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
	runner := &stubToolRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "tool-output", SandboxID: "sbx_1"}}
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

func TestSecretsCRUD(t *testing.T) {
	k := newSandboxServer(t, "auto")
	base := k.ts.URL + "/api/v1/sandbox/secrets"

	// Create.
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	postJSONInto(t, k.client, base, map[string]string{"name": "OPENAI_KEY", "description": "d", "value": "sk-secret"}, &created)
	if created.ID == "" || created.Name != "OPENAI_KEY" {
		t.Fatalf("created = %+v", created)
	}

	// List shows metadata but never the value.
	resp, _ := k.client.Get(base)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "sk-secret") {
		t.Fatalf("secret value leaked into list response: %s", body)
	}
	if !strings.Contains(string(body), "OPENAI_KEY") {
		t.Fatalf("secret name missing from list: %s", body)
	}

	// Update description.
	req, _ := http.NewRequest(http.MethodPut, base+"/"+created.ID, strings.NewReader(`{"description":"new"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = k.client.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("update = %d", resp.StatusCode)
	}

	// Delete.
	req, _ = http.NewRequest(http.MethodDelete, base+"/"+created.ID, nil)
	resp, _ = k.client.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
}

func TestSecretDuplicateNameConflict(t *testing.T) {
	k := newSandboxServer(t, "auto")
	base := k.ts.URL + "/api/v1/sandbox/secrets"
	postJSON(t, k.client, base, map[string]string{"name": "DUP", "value": "a"}, nil)
	resp, _ := k.client.Post(base, "application/json", strings.NewReader(`{"name":"DUP","value":"b"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409", resp.StatusCode)
	}
}

func TestSandboxEnvironmentGetPut(t *testing.T) {
	k := newSandboxServer(t, "auto")
	base := k.ts.URL + "/api/v1/sandbox/environment"

	// Default env: all built-in tools, network deny.
	var env struct {
		AllowedTools   []string `json:"allowed_tools"`
		AvailableTools []string `json:"available_tools"`
		Network        struct {
			Default string `json:"default"`
		} `json:"network"`
	}
	getInto(t, k.client, base, &env)
	if len(env.AllowedTools) == 0 || len(env.AvailableTools) == 0 || env.Network.Default != "deny" {
		t.Fatalf("default env = %+v", env)
	}

	// PUT a narrowed policy.
	put := map[string]any{
		"allowed_tools": []string{"shell"},
		"network":       map[string]any{"default": "deny", "allow": []map[string]any{{"host": "localhost", "port": 8080}}},
	}
	req := putJSONReq(t, base, put)
	resp, _ := k.client.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put env = %d", resp.StatusCode)
	}
	getInto(t, k.client, base, &env)
	if len(env.AllowedTools) != 1 || env.AllowedTools[0] != "shell" {
		t.Fatalf("env not persisted: %+v", env)
	}

	// Invalid policy (unknown tool) -> 400.
	req = putJSONReq(t, base, map[string]any{"allowed_tools": []string{"rm-rf"}})
	resp, _ = k.client.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid env = %d, want 400", resp.StatusCode)
	}
}

func TestSandboxToolFlowEndToEnd(t *testing.T) {
	k := newSandboxServer(t, "auto") // auto-approve so no manual decision is needed
	k.runner.result = sandbox.RunResult{ExitCode: 0, Stdout: "hello-from-sandbox", SandboxID: "sbx_e2e"}

	var conv struct {
		ID string `json:"id"`
	}
	postJSONInto(t, k.client, k.ts.URL+"/api/v1/conversations", map[string]string{"title": "t"}, &conv)

	var msg struct {
		Text string `json:"text"`
	}
	postJSONInto(t, k.client, k.ts.URL+"/api/v1/conversations/"+conv.ID+"/messages",
		map[string]string{"text": "/sh echo hi"}, &msg)

	if !k.runner.called {
		t.Fatal("the sandbox runner was not invoked for a /sh tool call")
	}
	if strings.Join(k.runner.lastSpec.Argv, " ") != "/bin/sh -lc echo hi" {
		t.Fatalf("argv = %v", k.runner.lastSpec.Argv)
	}
	if !strings.Contains(msg.Text, "sandbox") {
		t.Fatalf("assistant reply did not summarize the tool run: %q", msg.Text)
	}
	// Audit row recorded.
	runs, _ := k.store.ListSandboxRuns(context.Background(), "", 10)
	if len(runs) != 1 || runs[0].Tool != "shell" {
		t.Fatalf("audit runs = %+v", runs)
	}
}

func TestAdminSandboxView(t *testing.T) {
	k := newSandboxServer(t, "auto")
	var out struct {
		Runtime struct {
			Enabled   bool   `json:"enabled"`
			Available bool   `json:"available"`
			Pinned    string `json:"pinned"`
		} `json:"runtime"`
		Users []struct {
			Username string `json:"username"`
		} `json:"users"`
	}
	getInto(t, k.client, k.ts.URL+"/api/v1/admin/sandbox", &out)
	if !out.Runtime.Enabled || !out.Runtime.Available || out.Runtime.Pinned != sandbox.PinnedVersion {
		t.Fatalf("runtime = %+v", out.Runtime)
	}
	if len(out.Users) == 0 {
		t.Fatal("admin sandbox view should list users")
	}
}

func TestToolApprovalApproveFlow(t *testing.T) {
	k := newSandboxServer(t, "always") // default policy: the user must approve
	k.runner.result = sandbox.RunResult{ExitCode: 0, Stdout: "approved-run", SandboxID: "s"}

	var conv struct {
		ID string `json:"id"`
	}
	postJSONInto(t, k.client, k.ts.URL+"/api/v1/conversations", map[string]string{"title": "t"}, &conv)

	// Tool-call events are live-only (ephemeral) — subscribe before posting to catch
	// the ToolCallRequested and learn its call id.
	sub := k.bus.Subscribe(conv.ID)
	defer sub.Cancel()

	// The POST blocks while the tool call waits for approval — run it concurrently.
	done := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		b, _ := json.Marshal(map[string]string{"text": "/sh echo hi"})
		resp, err := k.client.Post(k.ts.URL+"/api/v1/conversations/"+conv.ID+"/messages", "application/json", strings.NewReader(string(b)))
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		var m struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		done <- m.Text
	}()

	callID := ""
	deadline := time.After(10 * time.Second)
	for callID == "" {
		select {
		case e := <-sub.Events:
			if e.Type == events.TypeToolCallRequested {
				var p struct {
					CallID string `json:"call_id"`
				}
				_ = json.Unmarshal([]byte(e.Payload), &p)
				callID = p.CallID
			}
		case <-deadline:
			t.Fatal("no ToolCallRequested event was raised")
		}
	}

	// Approve it; the blocked turn should then run the tool and complete.
	resp, err := k.client.Post(k.ts.URL+"/api/v1/conversations/"+conv.ID+"/tool-approvals/"+callID,
		"application/json", strings.NewReader(`{"approve":true}`))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d", resp.StatusCode)
	}

	select {
	case text := <-done:
		if !strings.Contains(text, "sandbox") {
			t.Fatalf("reply did not reflect the approved run: %q", text)
		}
	case err := <-errc:
		t.Fatalf("message POST: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the turn did not complete after approval")
	}
	if !k.runner.called {
		t.Fatal("an approved tool call should execute")
	}
}

func TestToolApprovalUnknownCall(t *testing.T) {
	k := newSandboxServer(t, "always")
	var conv struct {
		ID string `json:"id"`
	}
	postJSONInto(t, k.client, k.ts.URL+"/api/v1/conversations", map[string]string{"title": "t"}, &conv)
	// No pending call with this id -> 404.
	resp, _ := k.client.Post(k.ts.URL+"/api/v1/conversations/"+conv.ID+"/tool-approvals/tcl_missing",
		"application/json", strings.NewReader(`{"approve":true}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown approval = %d, want 404", resp.StatusCode)
	}
}

func putJSONReq(t *testing.T, url string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	return req
}
