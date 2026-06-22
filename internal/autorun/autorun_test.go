package autorun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// --- fakes ---

type fakeRunner struct {
	mu     sync.Mutex
	specs  []sandbox.RunSpec
	result sandbox.RunResult
	block  chan struct{} // when set, Run blocks until closed or ctx cancelled
}

func (f *fakeRunner) Available() bool        { return true }
func (f *fakeRunner) Status() sandbox.Status { return sandbox.Status{Available: true} }
func (f *fakeRunner) Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return sandbox.RunResult{}, ctx.Err()
		}
	}
	return f.result, nil
}

type fakeSecrets struct {
	materializeCalls int
	secretValues     []string // values the all-values redactor knows (for the secret-in-payload guard)
}

func (f *fakeSecrets) AllValuesRedactor(context.Context, string) (*vault.Redactor, error) {
	return vault.NewRedactor(f.secretValues), nil
}
func (f *fakeSecrets) Materialize(context.Context, string, []vault.EnvGrant) (*vault.Injection, error) {
	f.materializeCalls++
	return nil, nil
}
func (f *fakeSecrets) List(context.Context, string) ([]store.SecretMeta, error) { return nil, nil }

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu              sync.Mutex
	autos           map[string]store.Automation
	deleted         map[string]bool
	runs            map[string]store.AutomationRun
	artifacts       map[string][]store.AutomationArtifact
	failInsert      bool   // when set, InsertAutomationRun errors (simulates a locked/full DB or deleted FK)
	failPendingFire bool   // when set, SetAutomationPendingFire errors (simulates a durable-write failure)
	failGetByID     bool   // when set, GetAutomationByID returns a TRANSIENT error (not ErrNotFound)
	afterGet        func() // when set, called inside GetAutomation (to interpose a concurrent edit in a test)
}

func newFakeStore() *fakeStore {
	return &fakeStore{autos: map[string]store.Automation{}, deleted: map[string]bool{}, runs: map[string]store.AutomationRun{}, artifacts: map[string][]store.AutomationArtifact{}}
}

func (s *fakeStore) CreateAutomation(_ context.Context, a store.Automation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Gen == 0 {
		a.Gen = 1 // mirror the real store: a created automation starts at generation 1
	}
	s.autos[a.ID] = a
	return nil
}
func (s *fakeStore) UpdateAutomation(_ context.Context, a store.Automation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.autos[a.ID]
	if !ok {
		return store.ErrNotFound
	}
	a.PendingFire = ""   // mirror the real store: a definition/enable change clears the queue
	a.Gen = prev.Gen + 1 // bump the generation on every user-visible transition
	s.autos[a.ID] = a
	return nil
}
func (s *fakeStore) GetAutomation(_ context.Context, id, owner string) (store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok || a.OwnerUserID != owner || s.deleted[id] {
		return store.Automation{}, store.ErrNotFound
	}
	if s.afterGet != nil {
		s.afterGet() // test hook: simulate a concurrent edit landing between a caller's read + write
	}
	return a, nil
}
func (s *fakeStore) GetAutomationByID(_ context.Context, id string) (store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGetByID {
		return store.Automation{}, errors.New("transient db read error")
	}
	a, ok := s.autos[id]
	if !ok {
		return store.Automation{}, store.ErrNotFound
	}
	return a, nil
}
func (s *fakeStore) ListAutomationsByUser(_ context.Context, owner string) ([]store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Automation
	for _, a := range s.autos {
		if a.OwnerUserID == owner && !s.deleted[a.ID] {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *fakeStore) ListSchedulableAutomations(_ context.Context) ([]store.Automation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Automation
	for _, a := range s.autos {
		// Mirror the SQL: enabled, non-deleted, and either a pending fire or a non-manual trigger.
		if a.Enabled && !s.deleted[a.ID] && (a.PendingFire != "" || a.Trigger != "manual") {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) CountEnabledByUser(_ context.Context, userID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.autos {
		if a.OwnerUserID == userID && a.Enabled && !s.deleted[a.ID] {
			n++
		}
	}
	return n, nil
}
func (s *fakeStore) SoftDeleteAutomation(_ context.Context, id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok || a.OwnerUserID != owner || s.deleted[id] {
		return store.ErrNotFound
	}
	s.deleted[id] = true
	a.Enabled = false
	a.PendingFire = ""
	a.Gen++         // bump the generation so an in-flight claimed fire is detected as stale
	s.autos[id] = a // runs/artifacts retained (the audit survives)
	return nil
}
func (s *fakeStore) SetAutomationEnabled(_ context.Context, id, owner string, enabled bool, next time.Time, expectedGen int64, maxEnabled int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok || a.OwnerUserID != owner || s.deleted[id] || a.Gen != expectedGen {
		return false, nil // generation moved / not owner / deleted -> conflict
	}
	if enabled && maxEnabled > 0 { // atomic cap: count OTHER enabled rows for this owner
		others := 0
		for oid, o := range s.autos {
			if oid != id && o.OwnerUserID == owner && o.Enabled && !s.deleted[oid] {
				others++
			}
		}
		if others >= maxEnabled {
			return false, nil
		}
	}
	a.Enabled = enabled
	a.NextRunAt = next
	a.PendingFire = ""
	a.Gen++
	s.autos[id] = a
	return true, nil
}
func (s *fakeStore) SetAutomationSchedule(_ context.Context, id string, next, last time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.autos[id]
	a.NextRunAt = next
	a.LastRunAt = last
	s.autos[id] = a
	return nil
}
func (s *fakeStore) SetAutomationPendingFire(_ context.Context, id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPendingFire {
		return errors.New("pending_fire write failed")
	}
	// Mirror the real store: an UPDATE that matches no row is NOT an error.
	if a, ok := s.autos[id]; ok {
		a.PendingFire = token
		s.autos[id] = a
	}
	return nil
}
func (s *fakeStore) SetPendingFireIfCurrent(_ context.Context, id, token string, gen int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPendingFire {
		return false, errors.New("pending_fire write failed")
	}
	a, ok := s.autos[id]
	if !ok || !a.Enabled || s.deleted[id] {
		return false, nil
	}
	if gen > 0 && a.Gen != gen { // generation changed since the fire was claimed
		return false, nil
	}
	a.PendingFire = token
	s.autos[id] = a
	return true, nil
}
func (s *fakeStore) ClaimPendingFire(_ context.Context, id, token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPendingFire {
		return false, errors.New("pending_fire claim failed")
	}
	a, ok := s.autos[id]
	if token == "" || !ok || a.PendingFire != token { // compare-and-clear by EXACT token
		return false, nil
	}
	a.PendingFire = ""
	s.autos[id] = a
	return true, nil
}
func (s *fakeStore) pendingFireDurable(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autos[id].PendingFire != ""
}
func (s *fakeStore) ClaimDueRun(_ context.Context, id string, expected, next, last time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.autos[id]
	if !ok || !a.Enabled || !a.NextRunAt.Equal(expected) {
		return false, nil
	}
	a.NextRunAt = next
	a.LastRunAt = last
	s.autos[id] = a
	return true, nil
}
func (s *fakeStore) InsertAutomationRun(_ context.Context, r store.AutomationRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failInsert {
		return errors.New("insert failed")
	}
	s.runs[r.ID] = r
	return nil
}
func (s *fakeStore) FinalizeAutomationRun(_ context.Context, r store.AutomationRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.runs[r.ID]
	if !ok {
		return store.ErrNotFound
	}
	cur.Status = r.Status
	cur.Error = r.Error
	cur.Record = r.Record
	cur.FinishedAt = r.FinishedAt
	s.runs[r.ID] = cur
	return nil
}
func (s *fakeStore) GetAutomationRun(_ context.Context, id, owner string) (store.AutomationRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.OwnerUserID != owner {
		return store.AutomationRun{}, store.ErrNotFound
	}
	return r, nil
}
func (s *fakeStore) ListAutomationRuns(_ context.Context, autoID, owner string, _ int) ([]store.AutomationRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AutomationRun
	for _, r := range s.runs {
		if r.AutomationID == autoID && r.OwnerUserID == owner {
			out = append(out, r)
		}
	}
	return out, nil
}
func (s *fakeStore) MarkRunningRunsInterrupted(_ context.Context, status, msg string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if r.Status == automation.RunRunning {
			r.Status = status
			r.Error = msg
			s.runs[id] = r
			n++
		}
	}
	return n, nil
}
func (s *fakeStore) InsertAutomationArtifact(_ context.Context, a store.AutomationArtifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[a.RunID] = append(s.artifacts[a.RunID], a)
	return nil
}
func (s *fakeStore) ListAutomationArtifacts(_ context.Context, runID string) ([]store.AutomationArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.artifacts[runID], nil
}
func (s *fakeStore) GetAutomationArtifact(_ context.Context, id, owner string) (store.AutomationArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, arts := range s.artifacts {
		for _, a := range arts {
			if a.ID == id {
				return a, nil
			}
		}
	}
	return store.AutomationArtifact{}, store.ErrNotFound
}

func (s *fakeStore) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *fakeStore) runStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id].Status
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAuditor records sandbox_runs audit writes.
type fakeAuditor struct {
	mu         sync.Mutex
	inserted   []store.SandboxRun
	updated    []store.SandboxRun
	failInsert bool
}

func (a *fakeAuditor) InsertSandboxRun(_ context.Context, r store.SandboxRun) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failInsert {
		return errors.New("audit insert failed")
	}
	a.inserted = append(a.inserted, r)
	return nil
}
func (a *fakeAuditor) UpdateSandboxRun(_ context.Context, r store.SandboxRun) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updated = append(a.updated, r)
	return nil
}

// A deterministic tool step must write a durable sandbox_runs audit row (pending before the
// side effect, finalized after) and fail closed if that record can't be written (round-38).
func TestExecutorToolWritesAuditRow(t *testing.T) {
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}
	step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://x"}}

	// Happy path: a pending row is inserted (non-success SENTINEL), then finalized with the
	// real outcome + capture metadata.
	aud := &fakeAuditor{}
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}", SandboxID: "sbx_1", Duration: 7 * time.Millisecond, StdoutPath: "/tmp/o", StderrPath: "/tmp/e"}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Audit: aud, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	if _, err := ex.Tool(context.Background(), step); err != nil {
		t.Fatalf("tool: %v", err)
	}
	if len(aud.inserted) != 1 || aud.inserted[0].UserID != "u1" || aud.inserted[0].Tool != automation.ToolHTTPRequest {
		t.Fatalf("a pending audit row must be inserted before the run, got %+v", aud.inserted)
	}
	// The pre-insert must look UNFINISHED (so a crash before finalize isn't a phantom success).
	if aud.inserted[0].ExitCode != -1 || aud.inserted[0].Error != pendingAuditMarker {
		t.Fatalf("the pending audit row must use the non-success sentinel, got exit=%d err=%q", aud.inserted[0].ExitCode, aud.inserted[0].Error)
	}
	if len(aud.updated) != 1 || aud.updated[0].ID != aud.inserted[0].ID {
		t.Fatalf("the audit row must be finalized with the outcome, got %+v", aud.updated)
	}
	// Finalization records the real exit, clears the pending error, and captures metadata.
	fin := aud.updated[0]
	if fin.ExitCode != 0 || fin.Error != "" || fin.DurationMs != 7 || fin.StdoutPath != "/tmp/o" || fin.StderrPath != "/tmp/e" {
		t.Fatalf("finalize must record exit/duration/capture paths, got %+v", fin)
	}

	// Fail-closed: if the audit row can't be inserted, the tool must NOT run.
	aud2 := &fakeAuditor{failInsert: true}
	fr2 := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
	ex2 := &runExecutor{cfg: ExecConfig{Runner: fr2, Secrets: &fakeSecrets{}, Audit: aud2, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex2.Tool(context.Background(), step)
	if !res.Failed {
		t.Fatal("a tool whose audit row can't be written must fail closed")
	}
	if len(fr2.specs) != 0 {
		t.Fatal("the runner must not be invoked when the audit pre-insert failed")
	}
}

// A tool step's workspace_from must be HONORED: the command runs in that checkout subdir
// (validated under /workspace), not silently in the automation root; an out-of-/workspace
// reference is rejected (round-75).
func TestExecutorToolHonorsWorkspaceFrom(t *testing.T) {
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted}
	// (a) a valid workspace_from (a prior checkout's path) -> the command's workdir.
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/run/ws"}
	step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"ls"}}, Workspace: "/workspace/pr-42"}
	if res, _ := ex.Tool(context.Background(), step); res.Failed {
		t.Fatalf("a valid workspace_from should run, got %q", res.Err)
	}
	if len(fr.specs) != 1 || fr.specs[0].Workdir != "/workspace/pr-42" {
		t.Fatalf("the tool must run in the workspace_from checkout dir, got %+v", fr.specs)
	}
	// (b) a workspace_from OUTSIDE /workspace -> rejected, runner not invoked.
	fr2 := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex2 := &runExecutor{cfg: ExecConfig{Runner: fr2, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/run/ws"}
	bad := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"ls"}}, Workspace: "/etc/evil"}
	if res, _ := ex2.Tool(context.Background(), bad); !res.Failed {
		t.Fatal("a workspace_from outside /workspace must be rejected")
	}
	if len(fr2.specs) != 0 {
		t.Fatal("the runner must not run with an out-of-workspace workdir")
	}
}

// A deterministic tool BODY (op.stdin: an http.request body, a github.pr_comment body) carrying
// a vaulted secret — plaintext OR JSON-escaped — must be refused before the run; it would be
// sent to GitHub / the HTTP target while the argv-only audit looked clean (round-72).
func TestExecutorToolRefusesSecretInBody(t *testing.T) {
	pem := "-----BEGIN-----\nmid\"q\\b\n-----END-----"
	escObj, _ := json.Marshal(map[string]any{"k": pem}) // pem JSON-escaped inside a body
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}
	cases := []struct {
		name string
		tool string
		with map[string]any
	}{
		{"http plaintext", automation.ToolHTTPRequest, map[string]any{"url": "https://api.github.com/x", "method": "POST", "body": "key=" + pem}},
		{"http json-escaped", automation.ToolHTTPRequest, map[string]any{"url": "https://api.github.com/x", "method": "POST", "body": string(escObj)}},
		{"pr_comment plaintext", automation.ToolGithubPRComment, map[string]any{"repo": "o/r", "pr": float64(7), "body": "see " + pem}},
	}
	for _, c := range cases {
		fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
		ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
		step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: c.tool, With: c.with}
		res, _ := ex.Tool(context.Background(), step)
		if !res.Failed || !strings.Contains(res.Err, "secret") {
			t.Fatalf("%s: a secret in the body must be refused, got %+v", c.name, res)
		}
		if len(fr.specs) != 0 {
			t.Fatalf("%s: the runner must not be invoked", c.name)
		}
	}
}

// A deterministic tool argv carrying a vaulted secret in its JSON-escaped form (a PEM/token a
// template rendered via a json.Marshal'd value) must be refused before the run — the plaintext
// guard would miss it, putting it on the host sbx command line + the audit (round-71).
func TestExecutorToolRefusesEscapedSecretInArgv(t *testing.T) {
	pem := "-----BEGIN-----\nmid\"q\\b\n-----END-----"
	pemEsc, _ := json.Marshal(pem)
	escBody := string(pemEsc[1 : len(pemEsc)-1]) // the JSON-escaped PEM body
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	step := automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"echo", escBody}},
	}
	res, _ := ex.Tool(context.Background(), step)
	if !res.Failed || !strings.Contains(res.Err, "secret") {
		t.Fatalf("an escaped secret in a tool argv must be refused, got %+v", res)
	}
	if len(fr.specs) != 0 {
		t.Fatal("the runner must not be invoked when an (escaped) secret is in the argv")
	}
}

// An automation tool step must decide on the COMPLETE output, not the 64 KiB model-display
// inline stream: it parses the full redacted capture file, and fails closed if even the
// capture was truncated — never feeding a later step a partial body (round-61).
func TestExecutorToolParsesFullCaptureNotInline(t *testing.T) {
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}
	step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://api.github.com/x"}}

	// (a) the capture itself was truncated -> fail closed, never parse partial output.
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}", StdoutTruncated: true}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	if res, _ := ex.Tool(context.Background(), step); !res.Failed {
		t.Fatal("a truncated tool capture must fail closed")
	}

	// (b) the inline stream is partial but the capture FILE holds the full output -> parse the file.
	full := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(full, []byte(`{"complete":true,"items":[1,2,3]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fr2 := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: `{"complete":fa`, StdoutPath: full}}
	ex2 := &runExecutor{cfg: ExecConfig{Runner: fr2, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	res2, _ := ex2.Tool(context.Background(), step)
	if res2.Failed {
		t.Fatalf("should parse the full capture, got %q", res2.Err)
	}
	if body, _ := res2.Output.(map[string]any)["body"].(string); body != `{"complete":true,"items":[1,2,3]}` {
		t.Fatalf("must parse the FULL capture, not the truncated inline stream, got %q", body)
	}
}

// A network-enabled automation must NOT be usable to probe loopback/link-local/cloud-metadata/
// private services via the typed http.request tool — even under an unrestricted profile (round-44).
func TestExecutorToolBlocksInternalNetworkTargets(t *testing.T) {
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}
	blocked := []string{
		"http://127.0.0.1/", "http://localhost/admin", "http://[::1]/",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/", "http://0.0.0.0/",
		// Legacy inet_aton forms curl/getaddrinfo resolve to loopback/metadata (round-44/45):
		"http://2130706433/",          // decimal 127.0.0.1
		"http://127.1/",               // shortened loopback
		"http://0177.0.0.1/",          // octal loopback
		"http://2852039166/",          // decimal 169.254.169.254 (metadata)
		"http://0xa9fea9fe/",          // hex metadata
		"http://0251.0376.0251.0376/", // octal metadata
		// Normalized forms that must not dodge the block (round-51):
		"http://localhost./",            // trailing FQDN-root dot
		"http://127.0.0.1./",            // trailing dot on a loopback literal
		"http://[::1%25lo0]/",           // scoped IPv6 loopback (RFC6874 %25 zone)
		"http://[fe80::1%25eth0]/admin", // scoped IPv6 link-local
		// Percent-encoded literals: curl would decode %2e to "." (round-52). url.Parse rejects
		// these before they reach curl, but assert the runner is never invoked either way.
		"http://127%2e0%2e0%2e1/",       // encoded 127.0.0.1
		"http://127%2e1/",               // encoded shortened loopback
		"http://169%2e254%2e169%2e254/", // encoded metadata
		// IDN confusable-dot hosts curl canonicalizes to loopback/metadata (round-53):
		"http://127。0。0。1/",
		"http://169。254。169。254/meta",
	}
	for _, u := range blocked {
		fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
		// NetworkIsolated so the SSRF guard (not the network_isolated gate) is what refuses these.
		ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
		step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolHTTPRequest, With: map[string]any{"url": u}}
		res, _ := ex.Tool(context.Background(), step)
		if !res.Failed {
			t.Fatalf("http.request to internal target %q must be refused", u)
		}
		if len(fr.specs) != 0 {
			t.Fatalf("the runner must not be invoked for blocked target %q", u)
		}
	}
	// A public host is NOT blocked by the SSRF guard (it reaches the runner).
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	step := automation.ToolStep{Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://api.github.com/x"}}
	if res, _ := ex.Tool(context.Background(), step); res.Failed {
		t.Fatalf("a public host must not be blocked by the SSRF guard, got %q", res.Err)
	}
}

// A network-capable deterministic tool must be REFUSED at run time unless network_isolated
// is asserted — even with a valid public URL (round-46). The SSRF guard's DNS backstop
// depends on a locked-down sbx egress, which only network_isolated promises.
func TestExecutorNetworkedToolRequiresIsolation(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: false, Log: testLogger()}, workspace: "/ws"}
	step := automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}},
		Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://api.github.com/x"},
	}
	res, _ := ex.Tool(context.Background(), step)
	if !res.Failed || !strings.Contains(res.Err, "network_isolated") {
		t.Fatalf("a networked tool must be refused without network_isolated, got %+v", res)
	}
	if len(fr.specs) != 0 {
		t.Fatal("a network-gated tool must not reach the runner")
	}
}

// isInternalHostTarget must classify normalized internal hosts directly — covering the
// percent-decode + IPv6-zone strip + trailing-dot paths the SSRF guard canonicalizes (so a
// host reaching the guard by any route is judged the way curl would resolve it).
func TestIsInternalHostTarget(t *testing.T) {
	internal := []string{
		"127.0.0.1", "localhost", "LOCALHOST", "localhost.", "127.0.0.1.", "::1",
		"fe80::1%eth0", "::1%lo0", "169.254.169.254", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"0.0.0.0", "2130706433", "0xa9fea9fe", "127.1", "0177.0.0.1",
		"127%2e0%2e0%2e1", "169%2e254%2e169%2e254", "127%2e1", // percent-encoded literals
		// IDN confusable dots (U+3002/U+FF0E/U+FF61) + fullwidth digits curl canonicalizes:
		"127。0。0。1", "127．0．0．1", "127｡0｡0｡1",
		"169。254。169。254", "１２７．0．0．1",
	}
	for _, h := range internal {
		if !isInternalHostTarget(h) {
			t.Errorf("%q should be classified internal", h)
		}
	}
	public := []string{"api.github.com", "example.com", "github.com.", "8.8.8.8", "1.1.1.1", "93.184.216.34"}
	for _, h := range public {
		if isInternalHostTarget(h) {
			t.Errorf("%q should NOT be classified internal", h)
		}
	}
}

// The github.* PR tools must reject a repo value that isn't a bare github.com owner/repo —
// a host-qualified / URL / internal-address repo would route gh+git off the declared GitHub
// network target, invisible to the op.network SSRF guard (round-55).
func TestExecutorGitHubToolsRejectNonGitHubRepo(t *testing.T) {
	prof := automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}
	bad := []string{
		"ghes.corp/owner/repo",          // GHES host/owner/repo (3 segments)
		"https://github.com/owner/repo", // URL form
		"https://10.0.0.5/owner/repo",   // URL to a private host
		"169.254.169.254/owner/repo",    // metadata-address host
		"127.0.0.1/owner/repo",          // loopback host
		"-malicious/repo",               // leading-dash flag injection
		"owner/repo/extra",              // extra path
	}
	tools := []string{automation.ToolGithubPRCheckout, automation.ToolGithubPRComment}
	for _, tool := range tools {
		for _, repo := range bad {
			fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
			ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
			step := automation.ToolStep{
				Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: tool,
				With: map[string]any{"repo": repo, "pr": float64(7), "body": "hi"},
			}
			res, _ := ex.Tool(context.Background(), step)
			if !res.Failed {
				t.Fatalf("%s with repo %q must be refused", tool, repo)
			}
			if len(fr.specs) != 0 {
				t.Fatalf("%s with repo %q must not reach the runner", tool, repo)
			}
		}
		// A bare owner/repo IS accepted (reaches the runner).
		fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "https://github.com/o/r/pull/7#c"}}
		ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
		step := automation.ToolStep{
			Run: automation.RunInfo{UserID: "u1", Profile: prof}, Tool: tool,
			With: map[string]any{"repo": "my-org/my.repo", "pr": float64(7), "body": "hi"},
		}
		if res, _ := ex.Tool(context.Background(), step); res.Failed {
			t.Fatalf("%s with a bare owner/repo should be accepted, got %q", tool, res.Err)
		}
	}
}

// nonJSONProvider always returns non-JSON text (the assist failure mode).
type nonJSONProvider struct{}

func (nonJSONProvider) Name() string { return "nonjson" }
func (nonJSONProvider) Stream(context.Context, llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta, 2)
	ch <- llm.Delta{Text: "sorry, I can't help with that"}
	ch <- llm.Delta{Done: true}
	close(ch)
	return ch, nil
}

// When every assist attempt returns non-JSON, the response must still be VALID JSON: a
// "null" definition + the raw text + a parse issue, never invalid bytes in the RawMessage
// (round-37 finding).
func TestAssistNonJSONReturnsValidResponse(t *testing.T) {
	svc := New(ServiceConfig{Exec: ExecConfig{Provider: nonJSONProvider{}, Log: testLogger()}, Log: testLogger()})
	res, err := svc.Assist(context.Background(), "do something useful")
	if err != nil {
		t.Fatalf("assist: %v", err)
	}
	if !json.Valid(res.Definition) {
		t.Fatalf("Definition must be valid JSON, got %q", res.Definition)
	}
	if res.Valid {
		t.Fatal("an unparseable draft must not be marked Valid")
	}
	if res.RawText == "" {
		t.Fatal("the raw unparsed model text should be surfaced in RawText")
	}
	if res.Issues == nil || len(res.Issues.Issues) == 0 {
		t.Fatal("a parse issue should be reported")
	}
}

// writeHeavyRunner writes a large file into the run workspace then blocks until cancelled
// — simulating a tool that fills the scratch, to exercise the disk watchdog.
type writeHeavyRunner struct{ bytes int }

func (writeHeavyRunner) Available() bool        { return true }
func (writeHeavyRunner) Status() sandbox.Status { return sandbox.Status{Available: true} }
func (w writeHeavyRunner) Run(ctx context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	if spec.Workspace != "" {
		_ = os.WriteFile(filepath.Join(spec.Workspace, "big"), make([]byte, w.bytes), 0o600)
	}
	<-ctx.Done() // hold the run open so the watchdog can observe the oversized scratch
	return sandbox.RunResult{}, ctx.Err()
}

// An agent_cli run's credential/staging scratch lives OUTSIDE /workspace (so a sibling step
// can't read it) but must still count toward the per-run disk cap — the watchdog sums every
// watched scratch, so growth in the agent-state dir trips the cap, not just MinFreeBytes (round-58).
func TestWatchWorkspaceCountsAgentStateScratch(t *testing.T) {
	ws, err := sandbox.NewWorkspaceManager(t.TempDir(), t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	run, err := ws.NewScratch()
	if err != nil {
		t.Fatal(err)
	}
	defer run.Remove()
	agent, err := ws.NewScratch()
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Remove()
	// The RUN scratch stays empty; the AGENT-STATE scratch alone exceeds the cap.
	if err := os.WriteFile(filepath.Join(agent.Dir, "big"), make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := New(ServiceConfig{Store: newFakeStore(), MaxWorkspaceBytes: 1024, WorkspaceWatchInterval: 10 * time.Millisecond, Log: testLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var exceeded atomic.Bool
	done := make(chan struct{})
	go func() { svc.watchWorkspace(ctx, []sandbox.Scratch{run, agent}, cancel, &exceeded); close(done) }()
	waitFor(t, func() bool { return exceeded.Load() })
	<-done
	if !exceeded.Load() {
		t.Fatal("agent-state scratch growth must count toward the per-run cap")
	}
}

// fastWriteRunner writes a large file into the run workspace and returns IMMEDIATELY (no
// block) — so the step can finish before the watchdog's first poll, exercising the final
// synchronous disk check that a ticker-only watchdog would miss under bursty writes.
type fastWriteRunner struct{ bytes int }

func (fastWriteRunner) Available() bool        { return true }
func (fastWriteRunner) Status() sandbox.Status { return sandbox.Status{Available: true} }
func (w fastWriteRunner) Run(_ context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	if spec.Workspace != "" {
		_ = os.WriteFile(filepath.Join(spec.Workspace, "big"), make([]byte, w.bytes), 0o600)
	}
	return sandbox.RunResult{ExitCode: 0, Stdout: "{}"}, nil
}

// A step that writes past the cap and EXITS before any watchdog poll must still fail the run:
// the per-run cap can't be advisory under bursty writes (round-59).
func TestWorkspaceFinalCheckCatchesFastWrite(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"w","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo","hi"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body})
	ws, err := sandbox.NewWorkspaceManager(t.TempDir(), t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(ServiceConfig{
		Store:      fs,
		Exec:       ExecConfig{Runner: fastWriteRunner{bytes: 8192}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		Workspaces: ws, ArtifactDir: t.TempDir(),
		MaxWorkspaceBytes:      1024,
		WorkspaceWatchInterval: time.Hour, // the ticker can't fire in-test; the final check must catch it
		Log:                    testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()
	runID, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitFor(t, func() bool {
		st := fs.runStatus(runID)
		return st == automation.RunFailed || st == automation.RunSuccess
	})
	if fs.runStatus(runID) != automation.RunFailed {
		t.Fatalf("a fast over-cap write must fail the run, got %q", fs.runStatus(runID))
	}
}

// scratchOverLimit must FAIL CLOSED when a watched scratch can't be measured — a sandboxed
// process making a subtree unreadable to the Hina process must not let it contribute 0 bytes
// and dodge the cap (round-59). (A vanished/cleaned scratch is NOT an error — du returns 0.)
func TestScratchOverLimitFailsClosedOnScanError(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0o600)
	_ = os.Chmod(sub, 0o000)
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) }) // so t.TempDir's cleanup can recurse
	if _, err := os.ReadDir(sub); err == nil {
		t.Skip("cannot make a directory unreadable on this platform/uid (root or Windows)")
	}
	svc := New(ServiceConfig{Store: newFakeStore(), MaxWorkspaceBytes: 1024, Log: testLogger()})
	if over, _ := svc.scratchOverLimit([]sandbox.Scratch{{Dir: dir}}); !over {
		t.Fatal("an unreadable scratch subtree must be treated as over-limit (fail closed)")
	}
}

// The free-space guard must kill a run when the scratch FILESYSTEM drops below the floor
// — the path that catches open-but-unlinked files a directory walk can't see (round-31).
func TestWorkspaceWatchdogFreeSpaceGuardKillsRun(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"w","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","100"]}}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body})
	scratchRoot := t.TempDir()
	ws, err := sandbox.NewWorkspaceManager(scratchRoot, t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	free, ferr := platform.FreeBytes(scratchRoot)
	if ferr != nil || free <= 0 {
		t.Skip("statfs unavailable on this platform")
	}
	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Runner: blocked, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		Workspaces:  ws,
		ArtifactDir: t.TempDir(),
		// Floor ABOVE the actual free space -> the guard trips on the first poll, simulating
		// the host disk being driven low (as an open-unlink fill would).
		MinFreeBytes:           free + (1 << 30),
		WorkspaceWatchInterval: 25 * time.Millisecond,
		Log:                    testLogger(),
	})
	svc.Start(context.Background())
	defer func() { close(blocked.block); svc.Stop() }()

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunFailed })
	run, _ := fs.GetAutomationRun(context.Background(), r1, "u1")
	if !strings.Contains(run.Error, "space") {
		t.Fatalf("a free-space-guard kill must report low disk, got %q", run.Error)
	}
}

// A run whose scratch exceeds the per-run workspace cap must be KILLED by the watchdog and
// finalized failed — an unattended run can't fill the host disk (round-30 finding).
func TestWorkspaceWatchdogKillsOverrunningRun(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"w","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","100"]}}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body})
	ws, err := sandbox.NewWorkspaceManager(t.TempDir(), t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(ServiceConfig{
		Store:                  fs,
		Exec:                   ExecConfig{Runner: writeHeavyRunner{bytes: 8192}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		Workspaces:             ws,
		ArtifactDir:            t.TempDir(),
		MaxWorkspaceBytes:      1024, // 1 KiB cap, but the run writes 8 KiB
		WorkspaceWatchInterval: 25 * time.Millisecond,
		Log:                    testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunFailed })
	run, _ := fs.GetAutomationRun(context.Background(), r1, "u1")
	if !strings.Contains(run.Error, "workspace") {
		t.Fatalf("a watchdog-killed run must report the workspace cap, got %q", run.Error)
	}
}

// --- executor tests ---

func TestExecutorToolRunsArgv(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: `[{"id":"1","reason":"review_requested","subject":{"title":"x","type":"PullRequest","url":"https://api.github.com/repos/o/r/pulls/3"},"repository":{"full_name":"o/r"}}]`}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	res, err := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeGranular, Network: "enabled", AllowedTools: []string{"github.notifications"}, AllowedCLITools: []string{"gh"}}},
		Tool: automation.ToolGithubNotifications, With: map[string]any{"reasons": []any{"review_requested"}},
	})
	if err != nil || res.Failed {
		t.Fatalf("tool failed: %v %+v", err, res)
	}
	out := res.Output.(map[string]any)
	items := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	if fr.specs[0].Workspace != "/ws" {
		t.Errorf("workspace not passed: %q", fr.specs[0].Workspace)
	}
}

func TestExecutorToolNonZeroExitFails(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 1, Stderr: "boom"}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run: automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"false"}},
	})
	if !res.Failed {
		t.Fatal("non-zero exit should fail the step")
	}
}

func TestExecutorSecretInjectionGatedOnIsolation(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	fs := &fakeSecrets{}
	// network_isolated=false: granted secrets must NOT be materialized/injected.
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: fs, NetworkIsolated: false, Log: testLogger()}, workspace: "/ws"}
	_, _ = ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, SecretRefs: []string{"github_token"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"true"}},
	})
	if fs.materializeCalls != 0 {
		t.Fatal("secrets must not be materialized when network is not isolated (fail closed)")
	}
}

func TestExecutorProfileNetworkGate(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	// A networked tool with network NOT enabled is refused before the runner.
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeGranular, Network: "disabled", AllowedTools: []string{"github.notifications"}, AllowedCLITools: []string{"gh"}}},
		Tool: automation.ToolGithubNotifications, With: map[string]any{"reasons": []any{"review_requested"}},
	})
	if !res.Failed {
		t.Fatal("a networked tool must be refused when the profile network is disabled")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a profile-denied tool must never reach the runner")
	}
}

func TestExecutorProfileCLIGate(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	// shell.exec argv whose binary is NOT in allowed_cli_tools (granular) is refused.
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeGranular, AllowedTools: []string{"shell.exec"}, AllowedCLITools: []string{"echo"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"curl", "http://x"}},
	})
	if !res.Failed {
		t.Fatal("a CLI not in allowed_cli_tools must be refused")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a CLI-denied tool must never reach the runner")
	}
}

// TestExecutorMergesInjectionRedactor is the finding-1 regression: the redactor
// passed to the runner must cover the value materialized from the granted secret_refs
// (built from the same read as the injection), so a rotated secret can't be injected
// yet survive unredacted in output.
func TestExecutorMergesInjectionRedactor(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(context.Background(), store.User{ID: "u1", Username: "u", Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, platform.MasterKeyLen)
	v, err := vault.New(key, filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Put(context.Background(), "u1", "github_token", "", "supersecretvalue"); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: v, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	_, err = ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, SecretRefs: []string{"github_token"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"true"}},
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if len(fr.specs) != 1 {
		t.Fatalf("runner specs = %d", len(fr.specs))
	}
	red, ok := fr.specs[0].Redactor.(*vault.Redactor)
	if !ok || !red.ContainsSecret("supersecretvalue") {
		t.Fatal("the run redactor must cover the injected secret value (merge from the same read)")
	}
	// And the secret is actually injected (network isolated).
	var injected bool
	for _, kv := range fr.specs[0].SecretEnv {
		if kv == "GITHUB_TOKEN=supersecretvalue" {
			injected = true
		}
	}
	if !injected {
		t.Fatalf("granted secret not injected: %v", fr.specs[0].SecretEnv)
	}
}

// A granted secret_ref that no longer resolves must fail the run, never run with the
// credential silently omitted (round-4 finding, defense in depth under the re-check).
func TestExecutorMissingGrantedSecretFailsClosed(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	// fakeSecrets.List returns no secrets, so "missing" can't be resolved.
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	_, err := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, SecretRefs: []string{"missing"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"true"}},
	})
	if err == nil {
		t.Fatal("a missing granted secret must fail the run (not run with it omitted)")
	}
	if len(fr.specs) != 0 {
		t.Fatal("the runner must not be invoked when a granted secret can't be resolved")
	}
}

// A sandbox-LAYER failure (RunResult.Err with a nil Go error and ExitCode 0) must fail
// the step, never be parsed as a successful empty result (round-7 finding).
func TestExecutorSandboxLayerErrorFails(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Err: errors.New("sbx unavailable")}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Tool: automation.ToolGithubNotifications, With: map[string]any{},
	})
	if !res.Failed {
		t.Fatal("a RunResult.Err must fail the step even with ExitCode 0 (not parse empty output as success)")
	}
	if res.Output != nil {
		t.Fatal("a sandbox-layer failure must not produce parsed output")
	}
}

// shell.exec must be blocked under a granular network:disabled profile — it can egress
// (e.g. curl) and would otherwise exfiltrate a granted secret (round-7 finding).
func TestExecutorShellExecBlockedWhenNetworkDisabled(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run: automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{
			Mode: automation.ModeGranular, Network: "disabled",
			AllowedTools: []string{"shell.exec"}, AllowedCLITools: []string{"curl"},
		}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"curl", "http://x"}},
	})
	if !res.Failed {
		t.Fatal("shell.exec under a network:disabled profile must be blocked")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a network-blocked op must not reach the runner")
	}
}

// A granular shell.exec whose argv[0] is a shell interpreter (["sh","-c","curl ..."])
// must be refused — it would run arbitrary commands behind one approved interpreter,
// bypassing allowed_cli_tools (round-8 finding).
func TestExecutorShellInterpreterArgvNeedsUnrestricted(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run: automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{
			Mode: automation.ModeGranular, Network: "enabled",
			AllowedTools: []string{"shell.exec"}, AllowedCLITools: []string{"sh"},
		}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"sh", "-c", "curl http://x"}},
	})
	if !res.Failed {
		t.Fatal("a shell-interpreter argv must require an unrestricted profile")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a shell-interpreter argv must not reach the runner under a granular profile")
	}
}

// Even an UNRESTRICTED profile must honor network:"disabled" for network-capable
// tools (round-10 finding) — otherwise an unattended unrestricted shell could
// exfiltrate a granted secret despite a network-denied profile.
func TestExecutorUnrestrictedHonorsNetworkDisabled(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	// http.request under unrestricted + network:disabled -> blocked.
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "disabled"}},
		Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://example.com"},
	})
	if !res.Failed {
		t.Fatal("http.request under unrestricted + network:disabled must be blocked")
	}
	// shell.exec command string under unrestricted + network:disabled -> blocked (egress).
	res2, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "disabled"}},
		Tool: automation.ToolShellExec, With: map[string]any{"command": "curl http://x"},
	})
	if !res2.Failed {
		t.Fatal("shell.exec under unrestricted + network:disabled must be blocked")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a network-blocked op must not reach the runner")
	}
	// Sanity: unrestricted with network NOT disabled (omitted) allows http.request to run.
	fr2 := &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "{}"}}
	ex2 := &runExecutor{cfg: ExecConfig{Runner: fr2, Secrets: &fakeSecrets{}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	if res3, _ := ex2.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Tool: automation.ToolHTTPRequest, With: map[string]any{"url": "https://example.com"},
	}); res3.Failed {
		t.Fatalf("unrestricted with default network should permit http.request: %+v", res3)
	}
}

// Two granted secrets whose names collide on one env var must fail the run, not
// silently inject only one (round-13 finding).
func TestExecutorRejectsCollidingSecretEnv(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = st.CreateUser(context.Background(), store.User{ID: "u1", Username: "u", Role: "user", PasswordHash: "x"})
	v, err := vault.New(make([]byte, platform.MasterKeyLen), filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = v.Put(context.Background(), "u1", "foo-bar", "", "v1")
	_, _ = v.Put(context.Background(), "u1", "foo_bar", "", "v2") // both -> FOO_BAR

	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: v, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	_, terr := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, SecretRefs: []string{"foo-bar", "foo_bar"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"true"}},
	})
	if terr == nil {
		t.Fatal("colliding secret env names must fail the run")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a colliding-secret run must not reach the runner")
	}
}

// racingSecrets deletes the granted secret during Materialize, simulating a TOCTOU
// that would otherwise inject fewer env pairs than declared.
type racingSecrets struct {
	v   *vault.Vault
	uid string
}

func (r *racingSecrets) AllValuesRedactor(ctx context.Context, uid string) (*vault.Redactor, error) {
	return r.v.AllValuesRedactor(ctx, uid)
}
func (r *racingSecrets) List(ctx context.Context, uid string) ([]store.SecretMeta, error) {
	return r.v.List(ctx, uid)
}
func (r *racingSecrets) Materialize(ctx context.Context, uid string, grants []vault.EnvGrant) (*vault.Injection, error) {
	// Delete every secret before materializing -> Materialize skips them -> fewer pairs.
	metas, _ := r.v.List(ctx, uid)
	for _, m := range metas {
		_ = r.v.Delete(ctx, uid, m.ID)
	}
	return r.v.Materialize(ctx, uid, grants)
}

// A secret deleted between secretGrants and Materialize (a TOCTOU) must fail the run,
// not run with a missing injected env pair (round-14 finding).
func TestExecutorSecretTOCTOUFailsClosed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = st.CreateUser(context.Background(), store.User{ID: "u1", Username: "u", Role: "user", PasswordHash: "x"})
	v, err := vault.New(make([]byte, platform.MasterKeyLen), filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = v.Put(context.Background(), "u1", "tok", "", "v1")

	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &racingSecrets{v: v, uid: "u1"}, NetworkIsolated: true, Log: testLogger()}, workspace: "/ws"}
	_, terr := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, SecretRefs: []string{"tok"}}},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"true"}},
	})
	if terr == nil {
		t.Fatal("a secret deleted during materialization must fail the run (strict cardinality)")
	}
	if len(fr.specs) != 0 {
		t.Fatal("the runner must not be invoked when an injected secret went missing")
	}
}

// floodProvider streams more bytes than the LLM output budget.
// stallProvider returns a channel that never sends and never closes — a stalled/regressed
// provider. streamText must still return on ctx cancellation (round-51).
type stallProvider struct{}

func (stallProvider) Name() string { return "stall" }
func (stallProvider) Stream(context.Context, llm.Request) (<-chan llm.Delta, error) {
	return make(chan llm.Delta), nil
}

// A stalled provider must NOT wedge the run: streamText returns ctx.Err() on cancellation
// rather than blocking forever (which would hold a run slot + hang Stop()).
func TestStreamTextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var gotErr error
	go func() {
		_, _, gotErr = streamText(ctx, stallProvider{}, []llm.Message{{Role: llm.RoleSystem, Content: "x"}}, 1000)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamText wedged on a stalled provider after ctx cancellation")
	}
	if gotErr == nil {
		t.Fatal("streamText should return ctx.Err() on cancellation")
	}
}

type floodProvider struct{ n int }

func (floodProvider) Name() string { return "flood" }
func (f floodProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta)
	go func() {
		defer close(ch)
		for i := 0; i < f.n; i++ {
			select {
			case ch <- llm.Delta{Text: strings.Repeat("x", 1000)}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- llm.Delta{Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// An LLM step's streamed output must be bounded by the run budget (round-14 finding).
// An llm step must FAIL CLOSED before contacting the provider if its prompt or inputs carry a
// vaulted secret value — a server-side provider call isn't gated by sandbox network isolation,
// so a credential must not be exfiltrated to a (possibly cloud) model (round-67).
func TestExecutorLLMRefusesSecretInPayload(t *testing.T) {
	prov := &countingProvider{n: new(int)}
	ex := &runExecutor{cfg: ExecConfig{Provider: prov, Secrets: &fakeSecrets{secretValues: []string{"topsecret"}}, NetworkIsolated: true, Log: testLogger()}}
	// (a) the secret is in the prompt_template.
	res, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "use the key topsecret to do the thing",
	})
	if !res.Failed || !strings.Contains(res.Err, "secret") {
		t.Fatalf("an llm prompt carrying a secret must be refused, got %+v", res)
	}
	// (b) the secret arrives via a resolved input.
	res2, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "summarize", Inputs: []any{"prior output: topsecret"},
	})
	if !res2.Failed || !strings.Contains(res2.Err, "secret") {
		t.Fatalf("an llm input carrying a secret must be refused, got %+v", res2)
	}
	if *prov.n != 0 {
		t.Fatal("the provider must NOT be contacted when the payload carries a secret")
	}
}

// The llm secret guard must catch a vaulted secret even when it appears JSON-ESCAPED in the
// schema or inputs (a PEM key's newlines, a JSON credential's quotes/backslashes) — a plaintext
// search over the assembled bytes would miss the escaped form (round-68).
func TestExecutorLLMRefusesJSONEscapedSecret(t *testing.T) {
	const pem = "-----BEGIN KEY-----\nline2\"quote\\back\n-----END KEY-----"
	// (a) the secret appears JSON-escaped inside an output_schema.
	prov := &countingProvider{n: new(int)}
	ex := &runExecutor{cfg: ExecConfig{Provider: prov, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}}
	schema, _ := json.Marshal(map[string]any{"example": pem}) // pem is now \n/\"/\\-escaped in the bytes
	res, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "produce output", Schema: schema,
	})
	if !res.Failed || !strings.Contains(res.Err, "secret") {
		t.Fatalf("a JSON-escaped secret in the schema must be refused, got %+v", res)
	}
	// (b) the secret appears JSON-escaped inside a resolved input.
	prov2 := &countingProvider{n: new(int)}
	ex2 := &runExecutor{cfg: ExecConfig{Provider: prov2, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}}
	res2, _ := ex2.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "summarize", Inputs: []any{map[string]any{"key": pem}},
	})
	if !res2.Failed || !strings.Contains(res2.Err, "secret") {
		t.Fatalf("a JSON-escaped secret in an input must be refused, got %+v", res2)
	}
	// (c) the secret appears JSON-escaped inside the PROMPT (a json.Marshal'd object value a
	// prompt_template expanded) — the plaintext prompt check alone would miss it (round-69).
	prov3 := &countingProvider{n: new(int)}
	ex3 := &runExecutor{cfg: ExecConfig{Provider: prov3, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}}
	obj, _ := json.Marshal(map[string]any{"key": pem})
	res3, _ := ex3.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "use this config: " + string(obj),
	})
	if !res3.Failed || !strings.Contains(res3.Err, "secret") {
		t.Fatalf("a JSON-escaped secret in the prompt must be refused, got %+v", res3)
	}
	if *prov.n != 0 || *prov2.n != 0 || *prov3.n != 0 {
		t.Fatal("the provider must NOT be contacted when an escaped secret is present")
	}
}

// An llm step must refuse a secret that arrives JSON-escaped INSIDE a string input (a prior
// step that emitted json.Marshal of a credential), before contacting the provider (round-73).
func TestExecutorLLMRefusesEscapedSecretInsideStringInput(t *testing.T) {
	pem := "-----BEGIN-----\nmid\"q\\b\n-----END-----"
	escapedStr, _ := json.Marshal(pem) // the json.Marshal'd form a prior tool might have produced
	prov := &countingProvider{n: new(int)}
	ex := &runExecutor{cfg: ExecConfig{Provider: prov, Secrets: &fakeSecrets{secretValues: []string{pem}}, NetworkIsolated: true, Log: testLogger()}}
	res, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}},
		Prompt: "summarize", Inputs: []any{string(escapedStr)},
	})
	if !res.Failed || !strings.Contains(res.Err, "secret") {
		t.Fatalf("a secret escaped inside a string input must be refused, got %+v", res)
	}
	if *prov.n != 0 {
		t.Fatal("the provider must NOT be contacted")
	}
}

// An llm step whose response exceeds the output cap must FAIL CLOSED — never expose a partial
// reply to later steps (body_from/comment/decision) — mirroring the tool + agent_cli paths (round-63).
func TestExecutorLLMFailsClosedOnTruncation(t *testing.T) {
	ex := &runExecutor{cfg: ExecConfig{Provider: floodProvider{n: 1000}, NetworkIsolated: true, Log: testLogger()}}
	res, err := ex.LLM(context.Background(), automation.LLMStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Prompt: "x", MaxOutputBytes: 5000})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Err, "truncated") {
		t.Fatalf("an llm step exceeding the output cap must fail closed, got %+v", res)
	}
	if res.Output != nil {
		t.Fatalf("a truncated llm step must expose no output to later steps, got %+v", res.Output)
	}
}

// An llm step must be REFUSED at run time unless network_isolated is asserted — it streams
// the prompt + resolved inputs to a possibly-cloud provider (round-50).
func TestExecutorLLMRequiresIsolation(t *testing.T) {
	ex := &runExecutor{cfg: ExecConfig{Provider: llm.NewMockProvider(), NetworkIsolated: false, Log: testLogger()}}
	res, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "enabled"}}, Prompt: "x",
	})
	if !res.Failed || !strings.Contains(res.Err, "network_isolated") {
		t.Fatalf("an llm step must be refused without network_isolated, got %+v", res)
	}
}

func TestExecutorLLMStep(t *testing.T) {
	ex := &runExecutor{cfg: ExecConfig{Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()}}
	res, err := ex.LLM(context.Background(), automation.LLMStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Prompt: "merge these", Inputs: []any{"a", "b"}})
	if err != nil || res.Failed {
		t.Fatalf("llm: %v %+v", err, res)
	}
	out := res.Output.(map[string]any)
	if out["markdown"] == "" || out["text"] == "" {
		t.Fatalf("llm output = %+v", out)
	}
}

type fakeAgent struct {
	result sandbox.ToolResult
	gotOpt sandbox.AgentRunOptions
}

func (f *fakeAgent) HandleAutomation(_ context.Context, _ sandbox.Scope, _ sandbox.ToolCall, opts sandbox.AgentRunOptions) (sandbox.ToolResult, error) {
	f.gotOpt = opts
	return f.result, nil
}

func TestExecutorAgentStep(t *testing.T) {
	fa := &fakeAgent{result: sandbox.ToolResult{Content: `{"status":"success","final_text":"ok"}`}}
	ex := &runExecutor{cfg: ExecConfig{Agents: fa, Log: testLogger()}, workspace: "/scratch"}
	res, err := ex.Agent(context.Background(), automation.AgentStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Adapter: "codex", Prompt: "review", Workspace: "/workspace/pr-42"})
	if err != nil || res.Failed {
		t.Fatalf("agent: %v %+v", err, res)
	}
	out := res.Output.(map[string]any)
	if out["status"] != "success" {
		t.Fatalf("agent output = %+v", out)
	}
	// The run scratch is mounted (not the durable workspace), workspace_from maps to the
	// workdir under /workspace, and the unattended run auto-approves.
	if fa.gotOpt.Workspace != "/scratch" || fa.gotOpt.Workdir != "/workspace/pr-42" || !fa.gotOpt.AutoApprove {
		t.Fatalf("agent opts = %+v", fa.gotOpt)
	}
}

func TestExecutorAgentWorkspaceFromRejectsOutsideMount(t *testing.T) {
	fa := &fakeAgent{result: sandbox.ToolResult{Content: "{}"}}
	ex := &runExecutor{cfg: ExecConfig{Agents: fa, Log: testLogger()}, workspace: "/scratch"}
	res, _ := ex.Agent(context.Background(), automation.AgentStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Adapter: "codex", Prompt: "x", Workspace: "/etc/passwd"})
	if !res.Failed {
		t.Fatal("a workspace_from outside /workspace must be rejected")
	}
}

func TestExecutorAgentStepError(t *testing.T) {
	ex := &runExecutor{cfg: ExecConfig{Agents: &fakeAgent{result: sandbox.ToolResult{Err: "not configured"}}, Log: testLogger()}}
	res, _ := ex.Agent(context.Background(), automation.AgentStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Adapter: "codex", Prompt: "x"})
	if !res.Failed {
		t.Fatal("an agent error should fail the step")
	}
}

// An automation may only LOWER the operator's sandbox limits — a request above the
// configured ceiling is clamped, never raised (round-24 finding).
func TestLimitsForClampsToOperatorCeiling(t *testing.T) {
	ex := &runExecutor{cfg: ExecConfig{Limits: sandbox.Limits{CPUs: "2", Memory: "2g", PIDs: 100}, Log: testLogger()}}
	// Over the ceiling -> clamped to the operator's limits.
	over := ex.limitsFor(automation.SandboxProfile{Resources: automation.Resources{CPUs: 8, MemoryMB: 8192, PIDs: 1000}})
	if over.CPUs != "2" || over.Memory != "2g" || over.PIDs != 100 {
		t.Fatalf("over-ceiling resources must be clamped, got %+v", over)
	}
	// Under the ceiling -> honored (an automation can restrict itself further).
	under := ex.limitsFor(automation.SandboxProfile{Resources: automation.Resources{CPUs: 1, MemoryMB: 512, PIDs: 50}})
	if under.CPUs != "1" || under.Memory != "512m" || under.PIDs != 50 {
		t.Fatalf("under-ceiling resources must be honored, got %+v", under)
	}
}

// An agent_cli step under a network-disabled profile must fail at run time, before the
// agent launches (round-17 finding).
func TestExecutorAgentHonorsNetworkDisabled(t *testing.T) {
	fa := &fakeAgent{result: sandbox.ToolResult{Content: "{}"}}
	ex := &runExecutor{cfg: ExecConfig{Agents: fa, Log: testLogger()}, workspace: "/scratch"}
	res, _ := ex.Agent(context.Background(), automation.AgentStep{
		Run:     automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "disabled"}},
		Adapter: "codex", Prompt: "x", Workspace: "/workspace/pr",
	})
	if !res.Failed {
		t.Fatal("agent_cli under network:disabled must fail at run time")
	}
	if (fa.gotOpt != sandbox.AgentRunOptions{}) {
		t.Fatal("the agent must not be launched when the network is disabled")
	}
}

// An llm step under a network-disabled profile must fail at run time, before the
// provider is contacted (round-18 finding).
func TestExecutorLLMHonorsNetworkDisabled(t *testing.T) {
	calls := 0
	ex := &runExecutor{cfg: ExecConfig{Provider: countingProvider{n: &calls}, Log: testLogger()}}
	res, _ := ex.LLM(context.Background(), automation.LLMStep{
		Run:    automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted, Network: "disabled"}},
		Prompt: "x",
	})
	if !res.Failed {
		t.Fatal("llm under network:disabled must fail at run time")
	}
	if calls != 0 {
		t.Fatal("the provider must not be contacted when the network is disabled")
	}
}

// countingProvider records whether Stream was ever called.
type countingProvider struct{ n *int }

func (countingProvider) Name() string { return "counting" }
func (c countingProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	*c.n++
	ch := make(chan llm.Delta)
	close(ch)
	return ch, nil
}

// An LLM step whose assembled inputs exceed the run output budget must fail before the
// provider call, not allocate an unbounded payload (round-17 finding).
func TestExecutorLLMInputsBounded(t *testing.T) {
	big := strings.Repeat("y", 4000)
	inputs := make([]any, 100) // ~400KB assembled, well over the 5000-byte cap
	for i := range inputs {
		inputs[i] = big
	}
	calls := 0
	ex := &runExecutor{cfg: ExecConfig{Provider: countingProvider{n: &calls}, Log: testLogger()}}
	res, _ := ex.LLM(context.Background(), automation.LLMStep{Run: automation.RunInfo{Profile: automation.SandboxProfile{Mode: automation.ModeUnrestricted}}, Prompt: "agg", Inputs: inputs, MaxOutputBytes: 5000})
	if !res.Failed {
		t.Fatal("oversized LLM inputs must fail the step")
	}
	if calls != 0 {
		t.Fatal("the provider must not be called when inputs exceed the cap")
	}
}

// --- service / scheduler tests ---

func manualToolDef(t *testing.T) automation.Definition {
	t.Helper()
	js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo","hi"]}},
		         {"id":"done","type":"finish","status":"success","message":"ok"}]}`
	d, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := d.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	return d
}

func newTestService(t *testing.T, fs *fakeStore, runner *fakeRunner) *Service {
	t.Helper()
	return New(ServiceConfig{
		Store: fs,
		// NetworkIsolated so service-level tests can exercise network-capable tools (the
		// network_isolated gate itself is covered by the executor/eligibility unit tests).
		Exec:        ExecConfig{Runner: runner, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		ArtifactDir: t.TempDir(),
		Tick:        10 * time.Millisecond,
		Log:         testLogger(),
	})
}

// durablePendingDef is an interval/queue_one automation with a finish-only body (runs
// instantly, no runner needed) — used for the durable-pending-fire tests.
func durablePendingDef(t *testing.T) (automation.Definition, string) {
	t.Helper()
	js := `{"schema_version":"automation.v1","name":"d","trigger":{"type":"interval","every":"1h"},
		"concurrency":{"policy":"queue_one"},
		"sandbox":{"mode":"granular","network":"disabled"},
		"steps":[{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	return def, body
}

// Disabling/updating an automation must clear a durably-queued fire, so reconcile can't
// drain a stale fire for changed or disabled work after a restart (round-21 finding).
func TestDisableClearsDurablePendingFire(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body})
	_ = fs.SetAutomationPendingFire(context.Background(), "atm_1", "tok") // a fire was durably queued
	svc := newTestService(t, fs, &fakeRunner{})
	if _, err := svc.SetEnabled(context.Background(), "u1", "atm_1", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if fs.pendingFireDurable("atm_1") {
		t.Fatal("disabling must clear the durably-queued fire")
	}
	// Re-enabling also starts fresh — no stale pending survives.
	_ = fs.SetAutomationPendingFire(context.Background(), "atm_1", "tok")
	if _, err := svc.SetEnabled(context.Background(), "u1", "atm_1", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if fs.pendingFireDurable("atm_1") {
		t.Fatal("enabling must clear any stale durably-queued fire")
	}
}

// A manual cancel_previous trigger must NOT perform side effects behind an error AND must
// not durably queue work that a crash could lose (rounds 21+22): while a run is active it
// refuses cleanly — the active run is NOT cancelled and nothing is queued.
func TestTriggerNowCancelPreviousRefusesWhileBusy(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"c","trigger":{"type":"manual"},
		"concurrency":{"policy":"cancel_previous"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","100"]}}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body})
	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := newTestService(t, fs, blocked)
	svc.Start(context.Background())
	defer func() { close(blocked.block); svc.Stop() }()

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunRunning })
	// The overlapping manual trigger must be refused cleanly: an error, the active run still
	// running, and NO durable pending fire that a crash could strand.
	if _, err := svc.TriggerNow(context.Background(), "u1", "atm_1"); err == nil {
		t.Fatal("a manual cancel_previous while busy must be refused")
	}
	if svc.firstRunCanceled("atm_1") {
		t.Fatal("a refused manual cancel_previous must NOT cancel the active run")
	}
	if fs.pendingFireDurable("atm_1") {
		t.Fatal("a refused manual trigger must not durably queue anything")
	}
	if fs.runStatus(r1) != automation.RunRunning {
		t.Fatal("the active run must keep running after a refused manual cancel_previous")
	}
}

// A CAPPED scheduled fire (parked as pending for retry) must carry its claimed generation:
// if the automation was edited since the claim, the pending token must NOT be (re-)stamped,
// so a later drain can't run the edited definition off the stale claim (round-42).
func TestCappedFireRespectsClaimedGeneration(t *testing.T) {
	fs := newFakeStore()
	def, body := durablePendingDef(t)
	const curGen, staleGen = int64(5), int64(1) // the row is at gen 5; a fire claimed at gen 1 is stale
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: body, Gen: curGen,
	})
	svc := New(ServiceConfig{Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), MaxConcurrentRuns: 1, Log: testLogger()})
	svc.started = true
	// Occupy the single global slot so the next fire is CAPPED.
	if svc.beginRun("other", "u2", automation.Concurrency{}, "r0", false, int64(1)) != beginStarted {
		t.Fatal("the occupying run should start")
	}
	// Capped fire claimed under a STALE generation -> must NOT stamp a pending token.
	svc.fireWithID("atm", "u1", def, "r1", "", staleGen)
	if fs.pendingFireDurable("atm") {
		t.Fatal("a capped fire claimed under a stale generation must not be marked pending")
	}
	// Capped fire under the CURRENT generation -> stamped for retry.
	svc.fireWithID("atm", "u1", def, "r2", "", curGen)
	if !fs.pendingFireDurable("atm") {
		t.Fatal("a capped fire under the current generation should be marked pending for retry")
	}
}

// SetEnabled must NOT resurrect/enable a stale definition: if a concurrent Update commits a
// new generation between SetEnabled's read and its write, the generation-guarded enable must
// conflict rather than write the old row back enabled (round-60).
func TestSetEnabledConflictsWithConcurrentUpdate(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: false})
	svc := newTestService(t, fs, &fakeRunner{})
	// Interpose a concurrent definition update (bumps gen) AFTER SetEnabled reads the row but
	// BEFORE its generation-guarded write — fire the hook exactly once.
	var once sync.Once
	fs.afterGet = func() {
		once.Do(func() {
			a := fs.autos["atm_1"]
			a.Gen++ // a concurrent Update committed a new generation
			fs.autos["atm_1"] = a
		})
	}
	if _, err := svc.SetEnabled(context.Background(), "u1", "atm_1", true); err == nil {
		t.Fatal("enabling against a stale generation must conflict, not silently succeed")
	}
	fs.afterGet = nil
	if got, _ := fs.GetAutomation(context.Background(), "atm_1", "u1"); got.Enabled {
		t.Fatal("the stale enable must NOT have enabled the automation")
	}
}

// Enabling must enforce the per-user enabled-automation admission cap, but re-affirming an
// already-enabled automation must NOT be blocked by its own count (round-86).
func TestSetEnabledEnforcesPerUserCap(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "a1", OwnerUserID: "u1", Definition: body, Enabled: true, Gen: 1})
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "a2", OwnerUserID: "u1", Definition: body, Enabled: false, Gen: 1})
	svc := New(ServiceConfig{
		Store:             fs,
		Exec:              ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		ArtifactDir:       t.TempDir(),
		MaxEnabledPerUser: 1, // a1 is already enabled, so the cap is reached
		Eligibility: func(context.Context, string) (automation.Eligibility, error) {
			return automation.Eligibility{SandboxAvailable: true, NetworkIsolated: true}, nil
		},
		Log: testLogger(),
	})
	if _, err := svc.SetEnabled(context.Background(), "u1", "a2", true); err == nil {
		t.Fatal("enabling past the per-user cap must be rejected")
	}
	if got, _ := fs.GetAutomation(context.Background(), "a2", "u1"); got.Enabled {
		t.Fatal("a2 must NOT have been enabled past the cap")
	}
	// Re-affirming the already-enabled a1 must not be blocked by its own count.
	if _, err := svc.SetEnabled(context.Background(), "u1", "a1", true); err != nil {
		t.Fatalf("re-enabling an already-enabled automation must not hit the cap: %v", err)
	}
}

// A capped DRAIN (a pending-token fire that hits cap pressure) must NOT mint a fresh pending
// token — that would resurrect a stale occurrence under the current generation and let a later
// tick run it off-schedule. A FRESH capped fire still stamps one for retry (round-49).
func TestCappedDrainDoesNotRestampStaleFire(t *testing.T) {
	fs := newFakeStore()
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: body, Gen: 1,
	})
	svc := New(ServiceConfig{Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), MaxConcurrentRuns: 1, Log: testLogger()})
	svc.started = true
	// Occupy the only global slot so the next fire is CAPPED.
	if svc.beginRun("other", "u2", automation.Concurrency{}, "r0", false, int64(1)) != beginStarted {
		t.Fatal("the occupying run should start")
	}
	// The durable token is empty (e.g. an update cleared it). A capped DRAIN must not stamp one.
	svc.fireWithID("atm", "u1", def, "r1", "tok_stale", 1)
	if fs.pendingFireDurable("atm") {
		t.Fatal("a capped drain must NOT resurrect a pending token")
	}
	// A FRESH capped fire (no pending token) still marks a retry under the current generation.
	svc.fireWithID("atm", "u1", def, "r2", "", 1)
	if !fs.pendingFireDurable("atm") {
		t.Fatal("a fresh capped fire should stamp a pending token for retry")
	}
}

// A NON-pending scheduled fire claimed under one generation must NOT run if the automation
// was edited/re-enabled (gen bumped) since — it can't run the now-current definition off
// its reviewed trigger path (round-41/43).
func TestStaleScheduledFireCancelledOnGenerationChange(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	def, _ := automation.Parse([]byte(body))
	const curGen, staleGen = int64(5), int64(1)
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body,
		NextRunAt: time.Now().Add(time.Hour), Gen: curGen,
	})
	svc := newTestService(t, fs, &fakeRunner{})

	// (a) claimed under a STALE generation -> the current row changed -> cancel, no run row.
	svc.wg.Add(1)
	d1 := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "r_stale", false, "", staleGen)
		close(d1)
	}()
	<-d1
	if fs.runCount() != 0 {
		t.Fatal("a stale scheduled fire (generation changed) must not run or record a side effect")
	}
	// (b) claimed under the CURRENT generation -> runs.
	svc.wg.Add(1)
	d2 := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "r_ok", false, "", curGen)
		close(d2)
	}()
	<-d2
	if fs.runStatus("r_ok") != automation.RunSuccess {
		t.Fatalf("a current-generation fire should run, got %q", fs.runStatus("r_ok"))
	}
}

// Draining an OLDER queued occurrence must not erase a NEWER one queued while it ran: the
// compare-and-clear is by exact token, so a drain of token_A leaves token_B intact (round-40).
func TestDrainDoesNotEraseNewerPending(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, NextRunAt: time.Now().Add(time.Hour),
	})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	defer svc.Stop()
	// A NEWER occurrence (token_B) is the current durable pending.
	_ = fs.SetAutomationPendingFire(context.Background(), "atm_1", "tok_B")
	// Drain an OLDER occurrence (token_A): its claim must lose and NOT clear token_B.
	svc.startPending("atm_1", pendingFire{userID: "u1", runID: "r_A", token: "tok_A", gen: 1})
	waitFor(t, func() bool { return fs.runStatus("r_A") == automation.RunCancelled })
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("a drain of an older token must NOT erase the newer queued occurrence (token_B)")
	}
}

// A popped queued fire must atomically CONSUME the durable pending_fire and abort if it
// was cleared (by an update/disable) in the window after endRun popped it — so a stale
// queued occurrence can't launch a re-enabled/edited definition off-schedule (round-39).
func TestStartPendingConsumesPendingFire(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, NextRunAt: time.Now().Add(time.Hour),
	})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	defer svc.Stop()

	// (a) the durable token was CLEARED (an update invalidated it) -> the popped fire's
	// stale token can't be claimed -> it must NOT run side effects (finalized cancelled).
	svc.startPending("atm_1", pendingFire{userID: "u1", runID: "r_stale", token: "tok_stale", gen: 1})
	waitFor(t, func() bool { return fs.runStatus("r_stale") == automation.RunCancelled })
	// (b) the durable token matches the popped fire -> startPending consumes it + runs.
	_ = fs.SetAutomationPendingFire(context.Background(), "atm_1", "tok_ok")
	svc.startPending("atm_1", pendingFire{userID: "u1", runID: "r_ok", token: "tok_ok", gen: 1})
	waitFor(t, func() bool { return fs.runStatus("r_ok") == automation.RunSuccess })
	if fs.pendingFireDurable("atm_1") {
		t.Fatal("startPending must consume the durable pending_fire")
	}
}

// If the durable pending_fire write fails, a scheduled cancel_previous must NOT cancel
// the active run (it would otherwise cancel-without-a-recoverable-replacement) — round-23.
func TestSchedulerDurableQueueFailureDoesNotCancel(t *testing.T) {
	fs := newFakeStore()
	fs.failPendingFire = true
	svc := New(ServiceConfig{Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), Log: testLogger()})
	svc.started = true
	conc := automation.Concurrency{Policy: automation.ConcurrencyCancel}
	if svc.beginRun("atm", "u1", conc, "r1", false, int64(1)) != beginStarted {
		t.Fatal("r1 should start")
	}
	// The durable queue write fails -> refuse, leaving the active run intact.
	if svc.beginRun("atm", "u1", conc, "r2", false, int64(1)) != beginRefused {
		t.Fatal("a failed durable queue write must refuse the fire")
	}
	if svc.firstRunCanceled("atm") {
		t.Fatal("must NOT cancel the active run when the replacement can't be durably queued")
	}
	if svc.pendingFor("atm") {
		t.Fatal("no in-memory pending may be set when the durable write failed")
	}
}

// TriggerNow must insert the durable run row BEFORE returning, so the run id it hands
// back is immediately pollable — never a 202 for a record that doesn't exist (round-23).
func TestTriggerNowRecordsRunBeforeReturning(t *testing.T) {
	fs := newFakeStore()
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})
	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := newTestService(t, fs, blocked)
	svc.Start(context.Background())
	defer func() { close(blocked.block); svc.Stop() }()

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	// No waitFor: the row must exist the instant TriggerNow returns.
	if got := fs.runStatus(r1); got != automation.RunRunning {
		t.Fatalf("the run row must exist (running) before TriggerNow returns, got %q", got)
	}
}

// A queued fire must be persisted durably (round-20 finding): when queue_one queues an
// overlapping fire, the pending marker is written to the store, not just in memory.
func TestFirePersistsPendingFire(t *testing.T) {
	fs := newFakeStore()
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, NextRunAt: time.Now().Add(time.Hour)})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	defer svc.Stop()
	// Occupy an active run so the next fire is queued by queue_one.
	if svc.beginRun("atm_1", "u1", def.Concurrency, "r1", true, int64(1)) != beginStarted {
		t.Fatal("r1 should occupy the slot")
	}
	svc.fire("atm_1", "u1", def, "", int64(1)) // queue_one + active -> queue + persist
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("a queued fire must be persisted durably (survives a crash/restart)")
	}
}

// If clearing a consumed pending fire fails, the run must abort BEFORE side effects (and
// stay pending for retry) so a later retry can't replay external side effects (round-27).
func TestConsumePendingClearFailureAvoidsSideEffects(t *testing.T) {
	fs := newFakeStore()
	fs.failPendingFire = true // the pre-side-effect clear will fail
	js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"interval","every":"1h"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo","hi"]}}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, PendingFire: "tok"})
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := newTestService(t, fs, fr)
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "tok", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a consumed-pending run must NOT run side effects when the pending clear fails")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("pending_fire must stay set for retry when its clear failed")
	}
	if fs.runStatus("arn_x") != automation.RunCancelled {
		t.Fatalf("the deferred run must be finalized cancelled, got %q", fs.runStatus("arn_x"))
	}
}

// A manual run must NOT consume a parked scheduled pending fire — the pending occurrence
// (e.g. one parked after a cap refusal) survives an unrelated manual trigger (round-27).
func TestManualRunDoesNotConsumeScheduledPending(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body,
		NextRunAt: time.Now().Add(time.Hour), PendingFire: "tok",
	})
	// Not Started: the scheduler tick can't drain the pending during this test.
	svc := newTestService(t, fs, &fakeRunner{})
	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunSuccess })
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("a manual run must not consume a parked scheduled pending fire")
	}
}

// If the run record can't be inserted, a pending fire must STAY set (not be cleared
// before the durable record exists) so it is retried, not lost (round-25 finding).
// A FRESH scheduled fire (no pending token) whose run-row insert fails must be re-marked
// pending — the tick already advanced next_run, so without a marker the occurrence would be
// silently lost. The mark is generation-guarded so a since-edited automation isn't retried (round-65).
func TestFreshFireRetriedAfterInsertFailure(t *testing.T) {
	fs := newFakeStore()
	fs.failInsert = true
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body}) // gen 1, no pending
	svc := newTestService(t, fs, &fakeRunner{})
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("a fresh fire lost to an insert failure must be re-marked pending so the tick retries it")
	}
	if fs.runCount() != 0 {
		t.Fatal("no run row should exist after a failed insert")
	}
}

// A fresh scheduled fire that is INELIGIBLE at run time AND whose terminal failed-row insert
// fails must also be re-marked pending — the same loss class as the main-insert path, on the
// eligibility-skip branch (round-66).
func TestIneligibleFreshFireRetriedWhenTerminalInsertFails(t *testing.T) {
	fs := newFakeStore()
	fs.failInsert = true
	js := `{"schema_version":"automation.v1","name":"e","trigger":{"type":"interval","every":"5m"},
		"sandbox":{"mode":"unrestricted","network":"enabled","secret_refs":["tok"]},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true}) // gen 1
	svc := New(ServiceConfig{
		Store:       fs,
		Exec:        ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(),
		// The granted secret "tok" is gone since enable -> CheckEligibility fails this fire.
		Eligibility: func(context.Context, string) (automation.Eligibility, error) {
			return automation.Eligibility{SandboxAvailable: true, NetworkIsolated: true, Secrets: map[string]bool{}}, nil
		},
		Log: testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("an ineligible fresh fire whose terminal record can't be inserted must be re-marked pending for retry")
	}
}

// A fresh scheduled fire whose pre-run automation re-read fails TRANSIENTLY (not ErrNotFound)
// must be re-marked pending for retry — ClaimDueRun already advanced next_run, so otherwise the
// occurrence is silently lost with no run record (round-79). A genuine ErrNotFound (deleted) is
// still correctly dropped.
func TestFreshFireRetriedAfterTransientReadFailure(t *testing.T) {
	fs := newFakeStore()
	fs.failGetByID = true // GetAutomationByID returns a transient error
	js := `{"schema_version":"automation.v1","name":"e","trigger":{"type":"interval","every":"5m"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body}) // gen 1
	svc := New(ServiceConfig{
		Store:       fs,
		Exec:        ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(),
		Log:         testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("a fresh fire whose automation read fails transiently must be re-marked pending for retry")
	}
	// No run row was created (the read failed before any record).
	if _, gerr := fs.GetAutomationRun(context.Background(), "arn_x", "u1"); gerr == nil {
		t.Fatal("a fire dropped on a transient read failure must not leave a run record")
	}
}

func TestPendingFireNotClearedWhenInsertFails(t *testing.T) {
	fs := newFakeStore()
	fs.failInsert = true
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, PendingFire: "tok"})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "tok", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("pending_fire must stay set when the run record can't be inserted (so it retries)")
	}
	if fs.runCount() != 0 {
		t.Fatal("no run row should exist after a failed insert")
	}
}

// If the ELIGIBILITY-FAILURE terminal record can't be inserted, the pending fire must
// stay set (the consumed fire isn't lost when its failure record didn't persist) — a
// variant of the record-before-clear ordering on the pre-execution path (round-26).
func TestPendingFireKeptWhenEligibilityRecordFails(t *testing.T) {
	fs := newFakeStore()
	fs.failInsert = true
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body, PendingFire: "tok"})
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(),
		// SandboxAvailable=false makes the definition ineligible at the pre-run re-check.
		Eligibility: func(context.Context, string) (automation.Eligibility, error) { return automation.Eligibility{}, nil },
		Log:         testLogger(),
	})
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "tok", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if !fs.pendingFireDurable("atm_1") {
		t.Fatal("pending_fire must stay set when the ineligible terminal record can't be inserted")
	}
	if fs.runCount() != 0 {
		t.Fatal("no run row should exist after a failed terminal insert")
	}
}

// A SCHEDULED fire refused because a service cap is saturated (not the per-automation
// policy) must be marked pending_fire for retry, not silently dropped (round-25 finding).
func TestCappedScheduledFireMarksPending(t *testing.T) {
	fs := newFakeStore()
	def, body := durablePendingDef(t)
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: body})
	svc := New(ServiceConfig{Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), MaxConcurrentRuns: 1, Log: testLogger()})
	svc.started = true
	// Saturate the single global slot with a different automation's run.
	if svc.beginRun("other", "u2", automation.Concurrency{}, "r0", false, int64(1)) != beginStarted {
		t.Fatal("the occupying run should start")
	}
	// A scheduled fire for atm is now capped -> it must be durably marked for retry.
	svc.fireWithID("atm", "u1", def, "r1", "", int64(1))
	if !fs.pendingFireDurable("atm") {
		t.Fatal("a cap-refused scheduled fire must be marked pending_fire for retry")
	}
}

// A durable pending fire refused at startup under cap pressure must be RETRIED (the tick
// re-drains it), not stranded until another restart (round-24 finding).
func TestPendingFireRetriedUnderCap(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	// Two automations, each with a durable pending fire, but only ONE run slot.
	for _, id := range []string{"atm_1", "atm_2"} {
		_ = fs.CreateAutomation(context.Background(), store.Automation{
			ID: id, OwnerUserID: "u1", Enabled: true, Definition: body,
			NextRunAt: time.Now().Add(time.Hour), PendingFire: "tok",
		})
	}
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(), Tick: 10 * time.Millisecond, MaxConcurrentRuns: 1, Log: testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()
	// Startup drains one; the other is refused under the cap and MUST be retried by a tick.
	waitFor(t, func() bool {
		return !fs.pendingFireDurable("atm_1") && !fs.pendingFireDurable("atm_2") && fs.runCount() >= 2
	})
}

// On restart, reconcile must DRAIN a durable pending fire — a queued replacement whose
// schedule slot already advanced is run, not silently lost (round-20 finding).
func TestReconcileDrainsDurablePendingFire(t *testing.T) {
	fs := newFakeStore()
	_, body := durablePendingDef(t)
	// Pre-seed a future schedule (so the scheduling path doesn't fire) + a durable pending.
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Enabled: true, Definition: body,
		NextRunAt: time.Now().Add(time.Hour), PendingFire: "tok",
	})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background()) // reconcile drains the durable pending
	defer svc.Stop()
	// The queued fire runs (a record appears) and the durable marker clears.
	waitFor(t, func() bool { return fs.runCount() >= 1 && !fs.pendingFireDurable("atm_1") })
}

func TestServiceTriggerNowRunsToCompletion(t *testing.T) {
	fs := newFakeStore()
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Name: "t", Definition: body, Enabled: true})
	svc := newTestService(t, fs, &fakeRunner{result: sandbox.RunResult{ExitCode: 0, Stdout: "hi"}})
	svc.Start(context.Background())
	defer svc.Stop()

	runID, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(runID) == automation.RunSuccess })
}

func TestServiceSkipIfRunning(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
		"concurrency":{"policy":"skip_if_running"},"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","1"]}}]}`
	def, _ := automation.Parse([]byte(js))
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})

	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := newTestService(t, fs, blocked)
	svc.Start(context.Background())

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1")
	if err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunRunning })
	// A second manual trigger while the first run is active is skipped.
	if _, err := svc.TriggerNow(context.Background(), "u1", "atm_1"); err == nil {
		t.Fatal("a concurrent run under skip_if_running should be refused")
	}
	close(blocked.block)
	svc.Stop()
}

func TestServiceStopCancelsInFlight(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","100"]}}]}`
	def, _ := automation.Parse([]byte(js))
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})

	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := newTestService(t, fs, blocked)
	svc.Start(context.Background())
	runID, _ := svc.TriggerNow(context.Background(), "u1", "atm_1")
	waitFor(t, func() bool { return fs.runStatus(runID) == automation.RunRunning })

	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return — an in-flight run was not cancelled/waited")
	}
	if st := fs.runStatus(runID); st != automation.RunCancelled {
		t.Fatalf("run status after Stop = %q, want cancelled", st)
	}
}

func TestServiceReconcileMissedSkip(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"t","enabled":true,"trigger":{"type":"interval","every":"5m"},
		"missed_run_policy":"skip","sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"finish","status":"success"}]}`
	def, _ := automation.Parse([]byte(js))
	body, _ := def.MarshalForStore()
	// next_run far in the past (missed while down).
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true,
		NextRunAt: time.Now().Add(-time.Hour),
	})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	svc.Stop()

	got, _ := fs.GetAutomationByID(context.Background(), "atm_1")
	if !got.NextRunAt.After(time.Now()) {
		t.Fatalf("missed-skip should reschedule into the future, got %v", got.NextRunAt)
	}
	if fs.runCount() != 0 {
		t.Fatalf("missed-skip must not run the missed fire (runs=%d)", fs.runCount())
	}
}

func TestServiceReconcileMissedRunOnce(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"t","enabled":true,"trigger":{"type":"interval","every":"5m"},
		"missed_run_policy":"run_once","sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"finish","status":"success"}]}`
	def, _ := automation.Parse([]byte(js))
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{
		ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true,
		NextRunAt: time.Now().Add(-time.Hour),
	})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	waitFor(t, func() bool { return fs.runCount() == 1 })
	svc.Stop()
}

// A run whose record can't be inserted (DB locked/full, or the automation was deleted)
// must NOT run any side effect — fail closed before the workspace/executor (round-2).
func TestServiceInsertFailureSkipsSideEffects(t *testing.T) {
	fs := newFakeStore()
	fs.failInsert = true
	def := manualToolDef(t) // has a shell.exec tool step
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := newTestService(t, fs, fr)
	svc.Start(context.Background())
	defer svc.Stop()

	// The durable run row is inserted SYNCHRONOUSLY before TriggerNow returns, so an insert
	// failure surfaces as an immediate error (no phantom run id) and runs nothing.
	if _, err := svc.TriggerNow(context.Background(), "u1", "atm_1"); err == nil {
		t.Fatal("a manual trigger whose record can't be inserted must return an error, not a run id")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a run with no durable record must never invoke the runner")
	}
	if fs.runCount() != 0 {
		t.Fatalf("no run row should exist, got %d", fs.runCount())
	}
	if svc.inflight("atm_1") != 0 {
		t.Fatal("the run slot must be released after a refused insert")
	}
}

// A scheduled fire whose automation was disabled after the snapshot must be skipped.
func TestServiceDisabledBeforeRunSkips(t *testing.T) {
	fs := newFakeStore()
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	// Stored as DISABLED; a scheduled (non-manual) fire must re-check and skip.
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: false})
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := newTestService(t, fs, fr)
	svc.Start(context.Background())
	defer svc.Stop()

	// Drive a scheduled fire directly (bypassing the tick) for a now-disabled row.
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a disabled automation must not run on a scheduled fire")
	}
	if fs.runCount() != 0 {
		t.Fatal("a skipped fire must not record a run")
	}
}

// A cancellation that arrives before a run binds its cancel func (the beginRun ->
// setRunCancel window) must still cancel the late-bound context (round-3 finding).
func TestServiceLateBoundCancelHonored(t *testing.T) {
	fs := newFakeStore()
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	defer svc.Stop()

	if svc.beginRun("atm_1", "u1", automation.Concurrency{}, "r1", true, int64(1)) != beginStarted {
		t.Fatal("beginRun should succeed")
	}
	// Cancel arrives BEFORE the run binds its cancel func.
	svc.cancelAutomation("atm_1")
	ctx, cancel := context.WithCancel(context.Background())
	svc.setRunCancel("atm_1", "r1", cancel)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("a cancel requested before setRunCancel must cancel the late-bound context")
	}
	svc.endRun("atm_1", "r1")
}

// A scheduled run must re-check eligibility right before firing: if a dependency
// (e.g. a referenced secret) went away after enable, the run must NOT execute side
// effects and must be recorded as failed (round-4 finding).
func TestServiceRechecksEligibilityBeforeRun(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"e","trigger":{"type":"interval","every":"5m"},
		"sandbox":{"mode":"unrestricted","network":"enabled","secret_refs":["tok"]},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})

	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := New(ServiceConfig{
		Store:       fs,
		Exec:        ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(),
		// The secret "tok" went away since enable -> CheckEligibility now fails.
		Eligibility: func(context.Context, string) (automation.Eligibility, error) {
			return automation.Eligibility{SandboxAvailable: true, NetworkIsolated: true, Secrets: map[string]bool{}}, nil
		},
		Log: testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()

	// Drive a scheduled fire directly.
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerInterval, "arn_x", false, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if len(fr.specs) != 0 {
		t.Fatal("an ineligible scheduled run must not invoke the runner")
	}
	run, gerr := fs.GetAutomationRun(context.Background(), "arn_x", "u1")
	if gerr != nil {
		t.Fatalf("a skipped-for-eligibility run should still be recorded: %v", gerr)
	}
	if run.Status != automation.RunFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
}

// cancel_previous must cancel a previous run even if that run hasn't bound its cancel
// func yet (the beginRun->setRunCancel window) — round-5 finding.
func TestServiceCancelPreviousLateBound(t *testing.T) {
	fs := newFakeStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: "{}"})
	svc := newTestService(t, fs, &fakeRunner{})
	svc.Start(context.Background())
	defer svc.Stop()
	conc := automation.Concurrency{Policy: automation.ConcurrencyCancel}

	if svc.beginRun("atm", "u1", conc, "r1", false, int64(1)) != beginStarted {
		t.Fatal("r1 begin")
	}
	// r2 fires under cancel_previous BEFORE r1 bound its cancel func. It cancels r1 and
	// QUEUES a replacement (returns false — it doesn't over-subscribe).
	if svc.beginRun("atm", "u1", conc, "r2", false, int64(1)) == beginStarted {
		t.Fatal("r2 must not start while r1 holds the slot (it queues a replacement)")
	}
	// r1 binds now — it must be cancelled immediately (it was marked canceled by r2).
	ctx, cancel := context.WithCancel(context.Background())
	svc.setRunCancel("atm", "r1", cancel)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel_previous must cancel a late-binding previous run")
	}
	svc.endRun("atm", "r1")
}

// A tool step in an omitted-mode (=> granular) profile must be refused at run time when
// it is not in allowed_tools — round-5 finding (the gate can't depend only on enable).
func TestExecutorOmittedModeEnforcesAllowedTools(t *testing.T) {
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	ex := &runExecutor{cfg: ExecConfig{Runner: fr, Secrets: &fakeSecrets{}, Log: testLogger()}, workspace: "/ws"}
	res, _ := ex.Tool(context.Background(), automation.ToolStep{
		Run:  automation.RunInfo{UserID: "u1", Profile: automation.SandboxProfile{ /* Mode omitted => granular */ }},
		Tool: automation.ToolShellExec, With: map[string]any{"argv": []any{"echo"}},
	})
	if !res.Failed {
		t.Fatal("an omitted mode must be treated as granular and gate allowed_tools")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a tool not in allowed_tools must never reach the runner")
	}
}

// Deleting an automation must remove its owner-private artifact files, not just the DB
// rows (round-9 finding).
// Delete SOFT-deletes: the automation vanishes from the owner's views, but its immutable
// run/artifact records (the durable audit of its side effects) are RETAINED (round-38).
func TestServiceDeleteRetainsAuditRecords(t *testing.T) {
	fs := newFakeStore()
	artDir := t.TempDir()
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: artDir, Log: testLogger(),
	})
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})
	_ = fs.InsertAutomationRun(context.Background(), store.AutomationRun{ID: "arn_1", AutomationID: "atm_1", OwnerUserID: "u1", Status: "success"})
	svc.persistArtifacts("atm_1", "arn_1", []automation.Artifact{{Name: "final.md", Content: []byte("hi"), Size: 2}})
	dir := filepath.Join(artDir, "atm_1")

	if err := svc.Delete(context.Background(), "u1", "atm_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Gone from the owner's view...
	if _, err := svc.Get(context.Background(), "u1", "atm_1"); err == nil {
		t.Fatal("a deleted automation must not be retrievable by its owner")
	}
	if list, _ := svc.List(context.Background(), "u1"); len(list) != 0 {
		t.Fatalf("a deleted automation must not appear in the owner's list, got %d", len(list))
	}
	// ...but the run record + artifact files are RETAINED as audit.
	if _, err := fs.GetAutomationRun(context.Background(), "arn_1", "u1"); err != nil {
		t.Fatalf("the run record must be retained as audit, got %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("artifact files must be retained as audit after delete, got %v", err)
	}
}

// The service-wide run cap must bound concurrent runs ACROSS automations, not just
// within one (round-12 finding).
func TestServiceGlobalRunCap(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"b","trigger":{"type":"manual"},"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["sleep","100"]}}]}`
	def, _ := automation.Parse([]byte(js))
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: true})
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_2", OwnerUserID: "u1", Definition: body, Enabled: true})

	blocked := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}, block: make(chan struct{})}
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Runner: blocked, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), NetworkIsolated: true, Log: testLogger()},
		ArtifactDir: t.TempDir(), MaxConcurrentRuns: 1, Log: testLogger(),
	})
	svc.Start(context.Background())

	r1, err := svc.TriggerNow(context.Background(), "u1", "atm_1") // takes the only slot
	if err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	waitFor(t, func() bool { return fs.runStatus(r1) == automation.RunRunning })
	// A DIFFERENT automation's run is refused while the global slot is taken.
	if _, err := svc.TriggerNow(context.Background(), "u1", "atm_2"); err == nil {
		t.Fatal("the global run cap should refuse a second concurrent run across automations")
	}
	close(blocked.block)
	svc.Stop()
}

// A saturated per-user cap must NOT short-circuit the same-automation concurrency
// policy: cancel_previous must still cancel + queue, and queue_one must still queue,
// instead of the overlapping fire being silently dropped (round-19 finding).
func TestServiceUserCapPreservesConcurrencyPolicy(t *testing.T) {
	// cancel_previous with MaxRunsPerUser=1 (scheduled fires; started set directly so the
	// beginRun-level checks don't need the loop / endRun side effects).
	fs := newFakeStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: "{}"})
	svc := New(ServiceConfig{Store: fs, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), MaxRunsPerUser: 1, Log: testLogger()})
	svc.started = true
	cancelConc := automation.Concurrency{Policy: automation.ConcurrencyCancel}
	if svc.beginRun("atm", "u1", cancelConc, "r1", false, int64(1)) != beginStarted {
		t.Fatal("r1 should start")
	}
	// The user cap is now saturated; the overlapping scheduled fire must STILL cancel + queue.
	if svc.beginRun("atm", "u1", cancelConc, "r2", false, int64(1)) == beginStarted {
		t.Fatal("r2 must not start (it queues a replacement)")
	}
	if !svc.firstRunCanceled("atm") {
		t.Fatal("cancel_previous must cancel the stale run despite the user cap")
	}
	if !svc.pendingFor("atm") {
		t.Fatal("cancel_previous must queue a replacement despite the user cap")
	}

	// queue_one with MaxRunsPerUser=1 (scheduled fire must be queued, not lost).
	fs2 := newFakeStore()
	_ = fs2.CreateAutomation(context.Background(), store.Automation{ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: "{}"})
	svc2 := New(ServiceConfig{Store: fs2, Exec: ExecConfig{Log: testLogger()}, ArtifactDir: t.TempDir(), MaxRunsPerUser: 1, Log: testLogger()})
	svc2.started = true
	qConc := automation.Concurrency{Policy: automation.ConcurrencyQueueOne}
	if svc2.beginRun("atm", "u1", qConc, "r1", false, int64(1)) != beginStarted {
		t.Fatal("scheduled r1 should start")
	}
	if svc2.beginRun("atm", "u1", qConc, "r2", false, int64(1)) == beginStarted {
		t.Fatal("scheduled r2 must not start (it queues)")
	}
	if !svc2.pendingFor("atm") {
		t.Fatal("queue_one must queue the scheduled fire despite the user cap (not drop it)")
	}
}

// cancel_previous under a saturated run cap must NOT kill the active run without a
// replacement: it cancels + queues, never over-subscribes, and the queued replacement
// runs (under its own id) once the slot frees (rounds 13+21).
func TestServiceCancelPreviousUnderCap(t *testing.T) {
	fs := newFakeStore()
	js := `{"schema_version":"automation.v1","name":"c","trigger":{"type":"interval","every":"1h"},
		"concurrency":{"policy":"cancel_previous"},
		"sandbox":{"mode":"granular","network":"disabled"},
		"steps":[{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm", OwnerUserID: "u1", Enabled: true, Definition: body, NextRunAt: time.Now().Add(time.Hour)})
	svc := New(ServiceConfig{
		Store: fs, Exec: ExecConfig{Runner: &fakeRunner{}, Secrets: &fakeSecrets{}, Provider: llm.NewMockProvider(), Log: testLogger()},
		ArtifactDir: t.TempDir(), MaxConcurrentRuns: 1, Log: testLogger(),
	})
	svc.Start(context.Background())
	defer svc.Stop()
	conc := automation.Concurrency{Policy: automation.ConcurrencyCancel}
	// r1 takes the only slot (registered directly so it stays active until we release it).
	if svc.beginRun("atm", "u1", conc, "r1", false, int64(1)) != beginStarted {
		t.Fatal("r1 should start")
	}
	// r2 (scheduled) cancels r1 + queues a replacement; it must NOT start (no over-subscription).
	if svc.beginRun("atm", "u1", conc, "r2", false, int64(1)) == beginStarted {
		t.Fatal("r2 must not start while the cap is saturated by r1")
	}
	if !svc.firstRunCanceled("atm") {
		t.Fatal("cancel_previous should cancel the active run")
	}
	if !svc.pendingFor("atm") {
		t.Fatal("cancel_previous should queue a replacement")
	}
	if svc.inflight("atm") != 1 {
		t.Fatalf("inflight = %d, want 1 (no over-subscription)", svc.inflight("atm"))
	}
	// Release r1's permit — the queued replacement starts under its own id (r2) + completes.
	svc.endRun("atm", "r1")
	waitFor(t, func() bool { return fs.runStatus("r2") == automation.RunSuccess && svc.inflight("atm") == 0 })
}

// The automation env-name dangerous-list must stay in lockstep with sandbox's, so the
// enable-time check and the run-time sandbox drop agree (round-13 finding).
func TestEnvNameClassifierMatchesSandbox(t *testing.T) {
	for _, n := range []string{"PATH", "HOME", "LD_PRELOAD", "DYLD_X", "DOCKER_HOST", "HTTP_PROXY", "HTTPS_PROXY", "GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "GH_HOST", "GH_REPO", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "APPDATA", "BASH_ENV", "GITHUB_TOKEN", "GH_TOKEN", "MY_KEY", "FOO"} {
		if automation.IsDangerousEnvName(n) != sandbox.DangerousEnvName(n) {
			t.Errorf("env-name classifier drift for %q: automation=%v sandbox=%v", n, automation.IsDangerousEnvName(n), sandbox.DangerousEnvName(n))
		}
	}
}

// Even a MANUAL run must abort if the automation was disabled/updated after the trigger
// check but before execution (the pre-begin race — round-16 finding). execute re-checks
// enabled for every trigger.
func TestServiceManualRunAbortsIfDisabledMidWindow(t *testing.T) {
	fs := newFakeStore()
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	// Stored disabled — simulating a disable that landed before execute's re-check.
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: false})
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := newTestService(t, fs, fr)
	svc.Start(context.Background())
	defer svc.Stop()

	// Simulate the real manual path: TriggerNow pre-inserted the running row (recordExists).
	_ = fs.InsertAutomationRun(context.Background(), store.AutomationRun{ID: "arn_x", AutomationID: "atm_1", OwnerUserID: "u1", Status: automation.RunRunning, Trigger: automation.TriggerManual})
	svc.wg.Add(1)
	done := make(chan struct{})
	go func() {
		svc.execute("atm_1", "u1", def, automation.TriggerManual, "arn_x", true, "", int64(1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("execute did not return")
	}
	if len(fr.specs) != 0 {
		t.Fatal("a manual run must perform NO side effects when the automation is disabled at execute time")
	}
	if fs.runStatus("arn_x") != automation.RunCancelled {
		t.Fatalf("the disabled manual run must be finalized as cancelled, got %q", fs.runStatus("arn_x"))
	}
}

// A disabled (unreviewed) draft must NOT be runnable via the manual trigger — that
// would bypass the review-before-enable gate (round-15 finding).
func TestServiceTriggerNowRequiresEnabled(t *testing.T) {
	fs := newFakeStore()
	def := manualToolDef(t)
	body, _ := def.MarshalForStore()
	_ = fs.CreateAutomation(context.Background(), store.Automation{ID: "atm_1", OwnerUserID: "u1", Definition: body, Enabled: false})
	fr := &fakeRunner{result: sandbox.RunResult{ExitCode: 0}}
	svc := newTestService(t, fs, fr)
	svc.Start(context.Background())
	defer svc.Stop()

	if _, err := svc.TriggerNow(context.Background(), "u1", "atm_1"); err == nil {
		t.Fatal("a disabled automation must not be runnable via TriggerNow")
	}
	if len(fr.specs) != 0 || fs.runCount() != 0 {
		t.Fatal("a refused manual run must not execute or record anything")
	}
}

// Create/Update must validate the SUBMITTED schema_version, not silently overwrite it —
// a missing or non-v1 version is rejected (round-28 finding).
func TestCreateUpdateRejectWrongSchemaVersion(t *testing.T) {
	fs := newFakeStore()
	svc := newTestService(t, fs, &fakeRunner{})
	base := `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},"steps":[{"id":"f","type":"finish","status":"success"}]}`
	def, err := automation.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	// A valid v1 document is accepted.
	row, err := svc.Create(context.Background(), "u1", def)
	if err != nil {
		t.Fatalf("valid v1 should be accepted: %v", err)
	}
	// A missing schema_version is rejected.
	def.SchemaVersion = ""
	if _, err := svc.Create(context.Background(), "u1", def); err == nil {
		t.Fatal("a missing schema_version must be rejected")
	}
	// A non-v1 schema_version is rejected.
	def.SchemaVersion = "automation.v2"
	if _, err := svc.Create(context.Background(), "u1", def); err == nil {
		t.Fatal("a non-v1 schema_version must be rejected")
	}
	// Update is gated the same way.
	if _, err := svc.Update(context.Background(), "u1", row.ID, def); err == nil {
		t.Fatal("Update must also reject a non-v1 schema_version")
	}
}

func TestServiceCreateAlwaysDisabled(t *testing.T) {
	fs := newFakeStore()
	svc := newTestService(t, fs, &fakeRunner{})
	def := manualToolDef(t)
	def.Enabled = true // user tries to create pre-enabled
	row, err := svc.Create(context.Background(), "u1", def)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Enabled {
		t.Fatal("a created automation must be disabled until reviewed + enabled")
	}
}

func TestServiceValidationRejectsBadDef(t *testing.T) {
	fs := newFakeStore()
	svc := newTestService(t, fs, &fakeRunner{})
	bad := automation.Definition{SchemaVersion: "automation.v1", Name: "x", Trigger: automation.Trigger{Type: "manual"}} // no steps
	if _, err := svc.Create(context.Background(), "u1", bad); err == nil {
		t.Fatal("create should reject a definition with no steps")
	} else if _, ok := err.(*automation.ValidationErrors); !ok {
		t.Fatalf("error should be *ValidationErrors, got %T", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

var _ = errors.New
