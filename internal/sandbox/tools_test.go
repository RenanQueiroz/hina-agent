package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// --- test doubles ---

type stubRunner struct {
	called   bool
	lastSpec RunSpec
	result   RunResult
	onRun    func() // called inside Run (e.g. to cancel the turn ctx mid-run)
}

func (s *stubRunner) Available() bool { return true }
func (s *stubRunner) Status() Status  { return Status{Available: true} }
func (s *stubRunner) Run(_ context.Context, spec RunSpec) (RunResult, error) {
	s.called = true
	s.lastSpec = spec
	if s.onRun != nil {
		s.onRun()
	}
	return s.result, nil
}

type stubApprover struct {
	approve   bool
	gotReq    ApprovalRequest
	onApprove func() // invoked during Approve, before the decision returns (e.g. to rotate a credential mid-approval)
}

func (s *stubApprover) Approve(_ context.Context, req ApprovalRequest, onRegistered func()) (bool, error) {
	s.gotReq = req
	if onRegistered != nil {
		onRegistered()
	}
	if s.onApprove != nil {
		s.onApprove()
	}
	return s.approve, nil
}

type recordBus struct {
	mu     sync.Mutex
	events []events.Event
}

func (b *recordBus) PublishEphemeral(e events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
}

func (b *recordBus) typeCount(typ string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

type routerKit struct {
	router *Router
	runner *stubRunner
	apr    *stubApprover
	bus    *recordBus
	vault  *vault.Vault
	store  *store.Store
	userID string
	logBuf *bytes.Buffer
}

func newRouterKit(t *testing.T, approval string) *routerKit {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	u := store.User{ID: id.New("usr"), Username: "alice", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	key := make([]byte, platform.MasterKeyLen)
	_, _ = rand.Read(key)
	v, err := vault.New(key, filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "run"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	runner := &stubRunner{result: RunResult{ExitCode: 0, Stdout: "ok", SandboxID: "sbx_1"}}
	apr := &stubApprover{approve: true}
	bus := &recordBus{}
	logBuf := &bytes.Buffer{}
	r := NewRouter(RouterConfig{
		Runner: runner, Secrets: v, Workspaces: ws, Store: st, Bus: bus,
		Approver: apr, Approval: approval, NetworkIsolated: true,
		Log: slog.New(slog.NewTextHandler(logBuf, nil)),
	})
	return &routerKit{router: r, runner: runner, apr: apr, bus: bus, vault: v, store: st, userID: u.ID, logBuf: logBuf}
}

func (k *routerKit) setEnv(t *testing.T, env Environment) {
	t.Helper()
	data, _ := json.Marshal(env)
	if err := k.store.UpsertSandboxState(context.Background(), store.SandboxState{
		ID: id.New("sbx"), UserID: k.userID, Kind: StateKind, Data: string(data),
	}); err != nil {
		t.Fatalf("set env: %v", err)
	}
}

func (k *routerKit) scope() Scope { return Scope{UserID: k.userID, ConversationID: "cnv_1"} }

func shellCall(cmd string) ToolCall {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return ToolCall{ID: "c1", Name: ToolShell, Arguments: args}
}

// --- tests ---

func TestRouterShellRunsAndAudits(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.runner.result = RunResult{ExitCode: 0, Stdout: "hello", SandboxID: "sbx_x"}

	res, err := k.router.Handle(ctx, k.scope(), shellCall("echo hello"))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("unexpected tool error: %s", res.Err)
	}
	if !k.runner.called {
		t.Fatal("runner was not called")
	}
	wantArgv := []string{"/bin/sh", "-lc", "echo hello"}
	if strings.Join(k.runner.lastSpec.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Fatalf("argv = %v, want %v", k.runner.lastSpec.Argv, wantArgv)
	}
	if k.runner.lastSpec.Workspace == "" {
		t.Fatal("spec workspace not set")
	}
	if !strings.Contains(res.Content, "exit 0") || !strings.Contains(res.Content, "hello") {
		t.Fatalf("result content = %q", res.Content)
	}
	// Audit row recorded.
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Tool != ToolShell || runs[0].Decision != "auto" {
		t.Fatalf("audit runs = %+v", runs)
	}
	// Lifecycle events emitted.
	if k.bus.typeCount(events.TypeToolCallRequested) != 1 || k.bus.typeCount(events.TypeToolCallCompleted) != 1 {
		t.Fatalf("events = %+v", k.bus.events)
	}
}

func TestRouterAuditSurvivesCancellation(t *testing.T) {
	k := newRouterKit(t, ApprovalAuto)
	ctx, cancel := context.WithCancel(context.Background())
	// The runner cancels the turn ctx mid-run (a client stop / barge-in), AFTER the
	// command's side effects have happened. The audit row must still persist.
	k.runner.onRun = cancel

	if _, err := k.router.Handle(ctx, k.scope(), shellCall("echo hi")); err != nil && err != context.Canceled {
		t.Fatalf("handle: %v", err)
	}
	runs, _ := k.store.ListSandboxRuns(context.Background(), k.userID, 10)
	if len(runs) != 1 {
		t.Fatalf("a side-effecting run must leave a durable audit row even on cancel; got %d", len(runs))
	}
}

func TestRouterRecordsCaptureFailure(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.runner.result = RunResult{ExitCode: 0, Stdout: "ok", SandboxID: "s", CaptureErr: "disk full"}
	if _, err := k.router.Handle(ctx, k.scope(), shellCall("echo hi")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || !strings.Contains(runs[0].Error, "output capture failed") {
		t.Fatalf("audit row should record the capture failure: %+v", runs)
	}
}

func TestRouterToolNotPermitted(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setEnv(t, Environment{AllowedTools: []string{ToolFSRead}, Network: NetworkPolicy{Default: "deny"}})

	res, _ := k.router.Handle(ctx, k.scope(), shellCall("echo hi"))
	if !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q, want not-permitted", res.Err)
	}
	if k.runner.called {
		t.Fatal("runner must not run a disallowed tool")
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "blocked" {
		t.Fatalf("blocked tool should be audited: %+v", runs)
	}
}

func TestRouterApprovalDenied(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = false

	res, _ := k.router.Handle(ctx, k.scope(), shellCall("echo hi"))
	if !strings.Contains(res.Err, "denied") {
		t.Fatalf("err = %q, want denied", res.Err)
	}
	if k.runner.called {
		t.Fatal("runner must not run a denied tool")
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "denied" {
		t.Fatalf("denied tool should be audited: %+v", runs)
	}
}

// revokeApprover simulates a policy/secret change DURING the approval wait (after
// the request is registered, before the decision returns). deny=true returns a
// denial.
type revokeApprover struct {
	onApprove func()
	deny      bool
}

func (a *revokeApprover) Approve(_ context.Context, _ ApprovalRequest, onRegistered func()) (bool, error) {
	if onRegistered != nil {
		onRegistered()
	}
	if a.onApprove != nil {
		a.onApprove()
	}
	return !a.deny, nil
}

func TestRouterReMaterializesGrantsAfterApproval(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	sec, err := k.vault.Put(ctx, k.userID, "API", "", "sekret")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	granted := Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}, SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "API_KEY"}}}
	k.setEnv(t, granted)

	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	// During the approval wait, the user removes the grant.
	approver := &revokeApprover{onApprove: func() {
		k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	}}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: approver, Approval: ApprovalAlways, NetworkIsolated: true,
	})
	if _, err := router.Handle(ctx, k.scope(), shellCall("printenv API_KEY")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !k.runner.called {
		t.Fatal("the approved run should execute")
	}
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Fatalf("a grant revoked during the approval window must NOT be injected: %v", k.runner.lastSpec.SecretEnv)
	}
}

func TestRouterRedactsUngrantedVaultSecretInOutput(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	// A secret that exists in the vault but is NOT granted (e.g. its grant was
	// removed after a prior run wrote it into the workspace).
	if _, err := k.vault.Put(ctx, k.userID, "TOK", "", "workspace-leaked-value"); err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}}) // no grant
	k.runner.result = RunResult{ExitCode: 0, Stdout: "cat afile -> workspace-leaked-value", SandboxID: "s"}

	res, _ := k.router.Handle(ctx, k.scope(), shellCall("cat afile"))
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Fatalf("an un-granted secret must not be injected: %v", k.runner.lastSpec.SecretEnv)
	}
	// ...but its value must still be redacted from the output (redaction is over ALL
	// vaulted values, not just granted ones).
	if strings.Contains(res.Content, "workspace-leaked-value") {
		t.Fatalf("an un-granted vault secret leaked into the result: %q", res.Content)
	}
}

func TestRouterRedactsRevokedSecretInOutput(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	sec, err := k.vault.Put(ctx, k.userID, "TOK", "", "old-secret-value")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	grant := []SecretGrant{{SecretID: sec.ID, EnvName: "TOK"}}
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}, SecretGrants: grant})
	// The command "echoes" the old secret value (as if it were already in a file).
	k.runner.result = RunResult{ExitCode: 0, Stdout: "leaked old-secret-value here", SandboxID: "s"}

	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	// The grant is removed during the approval wait.
	approver := &revokeApprover{onApprove: func() {
		k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	}}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: approver, Approval: ApprovalAlways, NetworkIsolated: true,
	})
	res, _ := router.Handle(ctx, k.scope(), shellCall("cat afile"))
	// The value is no longer injected (revoked), but it must still be redacted from
	// the output — the run redactor is the union of pre-approval + current grants.
	if strings.Contains(res.Content, "old-secret-value") {
		t.Fatalf("a secret revoked during approval leaked into the result: %q", res.Content)
	}
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Fatalf("a revoked grant must not be injected: %v", k.runner.lastSpec.SecretEnv)
	}
}

func TestRouterReRedactsSummaryForSecretAddedDuringApproval(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	// During approval the user ADDS a secret whose value is present in the command.
	approver := &revokeApprover{onApprove: func() {
		_, _ = k.vault.Put(ctx, k.userID, "NEW", "", "added-secret-xyz")
	}}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: approver, Approval: ApprovalAlways, NetworkIsolated: true,
	})
	if _, err := router.Handle(ctx, k.scope(), shellCall("echo added-secret-xyz")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	found := false
	for _, run := range runs {
		if run.Tool == ToolShell {
			found = true
			if strings.Contains(run.Command, "added-secret-xyz") {
				t.Fatalf("a secret added during approval leaked into the audit command: %q", run.Command)
			}
		}
	}
	if !found {
		t.Fatalf("expected a run audit row: %+v", runs)
	}
}

func TestRouterDeniedApprovalReRedactsSummary(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	// During the wait the user adds a secret whose value is in the command, then DENIES.
	approver := &revokeApprover{deny: true, onApprove: func() {
		_, _ = k.vault.Put(ctx, k.userID, "NEW", "", "denied-secret-abc")
	}}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: approver, Approval: ApprovalAlways, NetworkIsolated: true,
	})
	if _, err := router.Handle(ctx, k.scope(), shellCall("echo denied-secret-abc")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	for _, run := range runs {
		if strings.Contains(run.Command, "denied-secret-abc") {
			t.Fatalf("a secret added before denial leaked into the denied audit row: %q", run.Command)
		}
	}
}

func TestRouterDeniedNetworkNumericPortRedacted(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	sec, err := k.vault.Put(ctx, k.userID, "TOK", "", "31337")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolHTTP},
		Network:      NetworkPolicy{Default: "deny"},
		SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "TOK"}},
	})
	// The secret value is the URL PORT — neither the audit nor the error may leak it.
	args, _ := json.Marshal(map[string]string{"url": "http://example.com:31337/"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if strings.Contains(res.Err, "31337") {
		t.Fatalf("numeric secret port leaked into the denial error: %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || strings.Contains(runs[0].Command, "31337") {
		t.Fatalf("numeric secret port leaked into the audit row: %+v", runs)
	}
}

func TestRouterApprovalApproved(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	k.apr.approve = true

	if _, err := k.router.Handle(ctx, k.scope(), shellCall("echo hi")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !k.runner.called {
		t.Fatal("approved tool should run")
	}
	if k.apr.gotReq.Tool != ToolShell || !strings.Contains(k.apr.gotReq.Summary, "echo hi") {
		t.Fatalf("approver got %+v", k.apr.gotReq)
	}
}

func TestRouterSecretInjectionAndRedaction(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	sec, err := k.vault.Put(ctx, k.userID, "API", "", "sekret-123")
	if err != nil {
		t.Fatalf("put secret: %v", err)
	}
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolShell},
		Network:      NetworkPolicy{Default: "deny"},
		SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "API_KEY"}},
	})
	// Simulate the command echoing the secret to stdout.
	k.runner.result = RunResult{ExitCode: 0, Stdout: "leaked sekret-123", SandboxID: "s"}

	res, _ := k.router.Handle(ctx, k.scope(), shellCall("printenv API_KEY"))
	// The secret is injected as run-scoped SecretEnv (forwarded via the process env,
	// never the argv)...
	foundEnv := false
	for _, e := range k.runner.lastSpec.SecretEnv {
		if e == "API_KEY=sekret-123" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("secret not injected: %v", k.runner.lastSpec.SecretEnv)
	}
	if k.runner.lastSpec.Redactor == nil {
		t.Fatal("the runner should receive a redactor to scrub captured output")
	}
	// ...but never appears in the model-visible result.
	if strings.Contains(res.Content, "sekret-123") {
		t.Fatalf("secret leaked into result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[redacted]") {
		t.Fatalf("expected redaction marker: %q", res.Content)
	}
}

// failInsertStore makes the audit pre-insert fail (other store ops delegate).
type failInsertStore struct{ *store.Store }

func (failInsertStore) InsertSandboxRun(context.Context, store.SandboxRun) error {
	return errors.New("audit db unavailable")
}

func TestRouterFailsClosedOnAuditInsertFailure(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws,
		Store: failInsertStore{k.store}, Bus: k.bus, Approver: k.apr, Approval: ApprovalAuto,
	})
	res, _ := router.Handle(ctx, k.scope(), shellCall("echo hi"))
	if !strings.Contains(res.Err, "refusing to run") {
		t.Fatalf("a run must fail closed when its audit row can't be recorded: %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("a side-effecting run must not execute without a durable audit row")
	}
}

// failUpdateStore lets the pending insert succeed but makes finalize fail.
type failUpdateStore struct{ *store.Store }

func (failUpdateStore) UpdateSandboxRun(context.Context, store.SandboxRun) error {
	return errors.New("audit finalize failed")
}

func TestRouterPendingRowVisibleWhenFinalizeFails(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws,
		Store: failUpdateStore{k.store}, Bus: k.bus, Approver: k.apr, Approval: ApprovalAuto,
	})
	if _, err := router.Handle(ctx, k.scope(), shellCall("echo hi")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// The run happened but couldn't be finalized — the row must remain clearly
	// non-successful, not a zero-output success.
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 {
		t.Fatalf("rows = %+v", runs)
	}
	if runs[0].ExitCode != -1 || !strings.Contains(runs[0].Error, "pending") {
		t.Fatalf("an unfinalized run must look non-successful: %+v", runs[0])
	}
}

func TestRouterNetworkDeniedAudited(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto) // default env: http_fetch allowed, network deny
	args, _ := json.Marshal(map[string]string{"url": "http://example.com/x"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q, want network denied", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "blocked" {
		t.Fatalf("a denied network probe must leave a durable policy-decision row: %+v", runs)
	}
}

func TestRouterNoSecretInjectionWhenNotIsolated(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	sec, err := k.vault.Put(ctx, k.userID, "API", "", "sekret-123")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolShell},
		Network:      NetworkPolicy{Default: "deny"},
		SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "API_KEY"}},
	})
	// A router that has NOT been told the sandbox network is isolated must not place
	// the secret into the run (it could be exfiltrated by a raw shell egress).
	ws, _ := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: k.apr, Approval: ApprovalAuto, NetworkIsolated: false,
	})
	if _, err := router.Handle(ctx, k.scope(), shellCall("printenv API_KEY")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(k.runner.lastSpec.SecretEnv) != 0 {
		t.Fatalf("secrets must NOT be injected when network_isolated is false: %v", k.runner.lastSpec.SecretEnv)
	}
}

func TestRouterBlockedNetworkAuditRedacted(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	sec, err := k.vault.Put(ctx, k.userID, "TOK", "", "supersekret")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolHTTP},
		Network:      NetworkPolicy{Default: "deny"},
		SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "TOK"}},
	})
	// A denied http_fetch whose URL contains a vaulted secret value must NOT persist
	// that value in the audit row.
	args, _ := json.Marshal(map[string]string{"url": "http://example.com/?token=supersekret"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q, want network denied", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 {
		t.Fatalf("rows = %+v", runs)
	}
	if strings.Contains(runs[0].Command, "supersekret") {
		t.Fatalf("secret value leaked into the blocked audit row: %q", runs[0].Command)
	}
	if !strings.Contains(runs[0].Command, "[redacted]") {
		t.Fatalf("expected the secret to be redacted in the audit row: %q", runs[0].Command)
	}
}

func TestRouterBlockedNetworkHostRedacted(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	sec, err := k.vault.Put(ctx, k.userID, "TOK", "", "supersekret")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolHTTP},
		Network:      NetworkPolicy{Default: "deny"},
		SecretGrants: []SecretGrant{{SecretID: sec.ID, EnvName: "TOK"}},
	})
	// The secret value is in the HOSTNAME — neither the audit row nor the model-
	// visible error may contain it.
	args, _ := json.Marshal(map[string]string{"url": "http://supersekret.example.com/"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if strings.Contains(res.Err, "supersekret") {
		t.Fatalf("secret host leaked into the denial error: %q", res.Err)
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || strings.Contains(runs[0].Command, "supersekret") {
		t.Fatalf("secret host leaked into the blocked audit row: %+v", runs)
	}
}

func TestRouterReCheckNetworkHostRedacted(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAlways)
	sec, err := k.vault.Put(ctx, k.userID, "TOK", "", "supersekret")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	grant := []SecretGrant{{SecretID: sec.ID, EnvName: "TOK"}}
	// Host allowed initially; revoked during the approval wait so the post-approval
	// re-check denies it. The re-check audit/error must still redact the host.
	k.setEnv(t, Environment{AllowedTools: []string{ToolHTTP}, Network: NetworkPolicy{Default: "deny", Allow: []NetworkRule{{Host: "supersekret.example.com", Port: 80}}}, SecretGrants: grant})
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	approver := &revokeApprover{onApprove: func() {
		k.setEnv(t, Environment{AllowedTools: []string{ToolHTTP}, Network: NetworkPolicy{Default: "deny"}, SecretGrants: grant})
	}}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: approver, Approval: ApprovalAlways, NetworkIsolated: true,
	})
	args, _ := json.Marshal(map[string]string{"url": "http://supersekret.example.com/"})
	res, _ := router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if !strings.Contains(res.Err, "not permitted") || strings.Contains(res.Err, "supersekret") {
		t.Fatalf("re-check denial leaked the host or didn't deny: %q", res.Err)
	}
	if k.runner.called {
		t.Fatal("a host revoked during approval must not run")
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	blocked := false
	for _, r := range runs {
		if r.Decision == "blocked" {
			blocked = true
			if strings.Contains(r.Command, "supersekret") {
				t.Fatalf("secret host leaked into the re-check audit row: %q", r.Command)
			}
		}
	}
	if !blocked {
		t.Fatalf("expected a blocked audit row: %+v", runs)
	}
}

func TestRouterRefusesSecretInShellArgv(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	// A vaulted secret (un-granted but present) whose value appears in the command.
	if _, err := k.vault.Put(ctx, k.userID, "TOK", "", "argv-secret-value"); err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	res, _ := k.router.Handle(ctx, k.scope(), shellCall("echo argv-secret-value"))
	if !strings.Contains(res.Err, "refusing to run") {
		t.Fatalf("err = %q, want a refusal", res.Err)
	}
	if k.runner.called {
		t.Fatal("a secret-bearing argv must never reach the host sbx command line")
	}
	runs, _ := k.store.ListSandboxRuns(ctx, k.userID, 10)
	if len(runs) != 1 || runs[0].Decision != "blocked" || strings.Contains(runs[0].Command, "argv-secret-value") {
		t.Fatalf("expected a redacted blocked audit row: %+v", runs)
	}
}

func TestRouterRefusesSecretInHTTPURL(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	if _, err := k.vault.Put(ctx, k.userID, "TOK", "", "urlsecret"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Host allowed so the network check passes; the secret is in the URL PATH, so the
	// argv guard must catch it.
	k.setEnv(t, Environment{AllowedTools: []string{ToolHTTP}, Network: NetworkPolicy{Default: "deny", Allow: []NetworkRule{{Host: "example.com", Port: 80}}}})
	args, _ := json.Marshal(map[string]string{"url": "http://example.com/p/urlsecret"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if !strings.Contains(res.Err, "refusing to run") {
		t.Fatalf("err = %q, want a refusal", res.Err)
	}
	if k.runner.called {
		t.Fatal("a secret-bearing URL must never reach the host sbx command line")
	}
}

func TestRouterArgvGuardRedactionMarkerCollision(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	// Pathological: a secret whose value is exactly the redaction marker. A guard that
	// compared "redacted vs original" would miss it; the substring check must catch it.
	if _, err := k.vault.Put(ctx, k.userID, "TOK", "", "[redacted]"); err != nil {
		t.Fatalf("put: %v", err)
	}
	k.setEnv(t, Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}})
	res, _ := k.router.Handle(ctx, k.scope(), shellCall("echo [redacted]"))
	if !strings.Contains(res.Err, "refusing to run") {
		t.Fatalf("err = %q, want a refusal", res.Err)
	}
	if k.runner.called {
		t.Fatal("a secret value equal to the marker must still be caught by the argv guard")
	}
}

// failSecondRedactorVault makes AllValuesRedactor succeed once (pre-approval) then
// fail (post-lock), to exercise the fail-closed redactor-load path.
type failSecondRedactorVault struct {
	*vault.Vault
	calls int
}

func (f *failSecondRedactorVault) AllValuesRedactor(ctx context.Context, userID string) (*vault.Redactor, error) {
	f.calls++
	if f.calls >= 2 {
		return nil, errors.New("vault read failed")
	}
	return f.Vault.AllValuesRedactor(ctx, userID)
}

func TestRouterFailsClosedOnRedactorLoadFailure(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: &failSecondRedactorVault{Vault: k.vault}, Workspaces: ws,
		Store: k.store, Bus: k.bus, Approver: k.apr, Approval: ApprovalAuto, NetworkIsolated: true,
	})
	res, _ := router.Handle(ctx, k.scope(), shellCall("echo hi"))
	if !strings.Contains(res.Err, "refusing to run") {
		t.Fatalf("err = %q, want a fail-closed refusal", res.Err)
	}
	if k.runner.called {
		t.Fatal("must fail closed when the current redactor can't be loaded")
	}
}

func TestRouterNetworkDenied(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	args, _ := json.Marshal(map[string]string{"url": "http://example.com/data"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args})
	if !strings.Contains(res.Err, "not permitted") {
		t.Fatalf("err = %q, want network denied", res.Err)
	}
	if k.runner.called {
		t.Fatal("denied network tool must not run")
	}
}

func TestRouterNetworkAllowed(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	k.setEnv(t, Environment{
		AllowedTools: []string{ToolHTTP},
		Network:      NetworkPolicy{Default: "deny", Allow: []NetworkRule{{Host: "example.com", Port: 80}}},
	})
	args, _ := json.Marshal(map[string]string{"url": "http://example.com/data"})
	if _, err := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolHTTP, Arguments: args}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !k.runner.called {
		t.Fatal("allowed network tool should run")
	}
	if len(k.runner.lastSpec.Network) != 1 || k.runner.lastSpec.Network[0].Port != 80 {
		t.Fatalf("network rule = %+v", k.runner.lastSpec.Network)
	}
}

func TestRouterQuotaExceededDenied(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	ws, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	// Pre-fill the user's durable workspace beyond a tiny quota.
	wsdir, _ := ws.UserWorkspace(k.userID)
	if err := os.WriteFile(filepath.Join(wsdir, "big"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		Runner: k.runner, Secrets: k.vault, Workspaces: ws, Store: k.store, Bus: k.bus,
		Approver: k.apr, Approval: ApprovalAuto, NetworkIsolated: true, QuotaBytes: 1024,
	})
	res, _ := router.Handle(ctx, k.scope(), shellCall("echo hi"))
	if !strings.Contains(res.Err, "quota exceeded") {
		t.Fatalf("err = %q, want quota exceeded", res.Err)
	}
	if k.runner.called {
		t.Fatal("an over-quota run must not execute")
	}
}

func TestRouterFSWrite(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	args, _ := json.Marshal(map[string]string{"path": "out.txt", "content": "data"})
	if _, err := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolFSWrite, Arguments: args}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if strings.Join(k.runner.lastSpec.Argv, " ") != "tee -- out.txt" {
		t.Fatalf("argv = %v", k.runner.lastSpec.Argv)
	}
	if string(k.runner.lastSpec.Stdin) != "data" {
		t.Fatalf("stdin = %q", k.runner.lastSpec.Stdin)
	}
}

func TestRouterFSWritePathEscape(t *testing.T) {
	ctx := context.Background()
	k := newRouterKit(t, ApprovalAuto)
	args, _ := json.Marshal(map[string]string{"path": "../etc/passwd", "content": "x"})
	res, _ := k.router.Handle(ctx, k.scope(), ToolCall{Name: ToolFSWrite, Arguments: args})
	if !strings.Contains(res.Err, "escapes") {
		t.Fatalf("err = %q, want escape rejection", res.Err)
	}
	if k.runner.called {
		t.Fatal("path-escaping write must not run")
	}
}

func TestCleanRelPath(t *testing.T) {
	for _, bad := range []string{"", "/abs", "../up", "a/../../b", `\windows`} {
		if _, err := cleanRelPath(bad); err == nil {
			t.Fatalf("cleanRelPath(%q) should fail", bad)
		}
	}
	for _, ok := range []string{"a.txt", "dir/b.txt", "./c"} {
		if _, err := cleanRelPath(ok); err != nil {
			t.Fatalf("cleanRelPath(%q) = %v", ok, err)
		}
	}
}

func TestParseHTTPTarget(t *testing.T) {
	cases := map[string]struct {
		host string
		port int
	}{
		"http://example.com/x":       {"example.com", 80},
		"https://example.com/x":      {"example.com", 443},
		"http://localhost:8080/v1":   {"localhost", 8080},
		"https://host.docker:1234/y": {"host.docker", 1234},
	}
	for raw, want := range cases {
		host, port, err := parseHTTPTarget(raw)
		if err != nil || host != want.host || port != want.port {
			t.Fatalf("parse(%q) = (%q,%d,%v), want (%q,%d)", raw, host, port, err, want.host, want.port)
		}
	}
	for _, bad := range []string{"ftp://x", "notaurl", "http://"} {
		if _, _, err := parseHTTPTarget(bad); err == nil {
			t.Fatalf("parse(%q) should fail", bad)
		}
	}
}
