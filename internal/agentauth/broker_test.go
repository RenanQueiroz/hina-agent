package agentauth

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
)

// --- fakes ---

type fakeSession struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu      sync.Mutex
	stdin   strings.Builder
	killed  bool
	done    chan struct{}
	waitErr error
}

func newFakeSession() *fakeSession {
	pr, pw := io.Pipe()
	return &fakeSession{pr: pr, pw: pw, done: make(chan struct{})}
}

func (s *fakeSession) Stdout() io.Reader { return s.pr }
func (s *fakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin.Write(p)
}
func (s *fakeSession) Wait() error { <-s.done; return s.waitErr }
func (s *fakeSession) Kill() {
	s.mu.Lock()
	s.killed = true
	s.mu.Unlock()
	s.waitErr = errors.New("killed")
	_ = s.pw.CloseWithError(io.EOF)
	s.closeDone()
}
func (s *fakeSession) emit(text string) { _, _ = s.pw.Write([]byte(text)) }
func (s *fakeSession) finish(err error) {
	_ = s.pw.Close()
	s.waitErr = err
	s.closeDone()
}
func (s *fakeSession) closeDone() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}
func (s *fakeSession) stdinText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin.String()
}

type fakeFactory struct {
	avail bool
	sess  *fakeSession
	spec  SessionSpec
}

func (f *fakeFactory) Available() bool { return f.avail }
func (f *fakeFactory) Start(_ context.Context, spec SessionSpec) (Session, error) {
	f.spec = spec
	return f.sess, nil
}

type stubRunner struct {
	stdout   string
	exitCode int
	avail    bool
	lastSpec sandbox.RunSpec
}

func (s *stubRunner) Available() bool { return s.avail }
func (s *stubRunner) Status() sandbox.Status {
	return sandbox.Status{Available: s.avail, Path: "/usr/bin/sbx"}
}
func (s *stubRunner) Run(_ context.Context, spec sandbox.RunSpec) (sandbox.RunResult, error) {
	s.lastSpec = spec
	return sandbox.RunResult{Stdout: s.stdout, ExitCode: s.exitCode}, nil
}

type brokerKit struct {
	broker  *Broker
	factory *fakeFactory
	runner  *stubRunner
	vault   *vault.Vault
	store   *store.Store
	userID  string
}

func newBrokerKit(t *testing.T) *brokerKit {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u := store.User{ID: id.New("usr"), Username: "alice", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, platform.MasterKeyLen)
	_, _ = rand.Read(key)
	v, err := vault.New(key, filepath.Join(t.TempDir(), "vault"), st)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := sandbox.NewWorkspaceManager(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "run"), nil)
	if err != nil {
		t.Fatal(err)
	}
	factory := &fakeFactory{avail: true, sess: newFakeSession()}
	runner := &stubRunner{avail: true, stdout: "Logged in as alice@example.com"}
	b := New(Config{Runner: runner, Factory: factory, Scratch: ws, State: v, Profiles: st, NetworkIsolated: true})
	return &brokerKit{broker: b, factory: factory, runner: runner, vault: v, store: st, userID: u.ID}
}

func collectFrames(t *testing.T, ch <-chan Frame) []Frame {
	t.Helper()
	var frames []Frame
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return frames
			}
			frames = append(frames, f)
		case <-timeout:
			t.Fatal("timed out collecting frames")
		}
	}
}

func TestBrokerSuccessfulLogin(t *testing.T) {
	k := newBrokerKit(t)
	sid, err := k.broker.StartLogin(k.userID, "codex", true)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, err := k.broker.Subscribe(sid, k.userID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	sess := k.factory.sess
	sess.emit("Visit https://github.com/login/device\n")
	sess.emit("Enter the code: WXYZ-1234\n")
	sess.finish(nil)

	frames := collectFrames(t, ch)
	if !framesHaveHint(frames, HintURL) || !framesHaveHint(frames, HintCode) {
		t.Fatalf("expected URL + code hints, got %+v", frames)
	}
	done := lastDone(frames)
	if done == nil || !done.OK {
		t.Fatalf("expected a successful done frame, got %+v", frames)
	}
	// Profile recorded as browser_state + authenticated; agent-state persisted.
	p, err := k.store.GetAgentProfile(context.Background(), k.userID, "codex")
	if err != nil || p.AuthType != "browser_state" || p.Status != "authenticated" {
		t.Fatalf("profile = %+v (err %v)", p, err)
	}
	if !k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("agent state not persisted after login")
	}
	// The auth container mounted the credential store + set CODEX_HOME.
	if !strings.Contains(strings.Join(k.factory.spec.Env, " "), "CODEX_HOME=/agent/codex") {
		t.Errorf("login env missing CODEX_HOME: %v", k.factory.spec.Env)
	}
}

func TestBrokerLoginStatusFails(t *testing.T) {
	k := newBrokerKit(t)
	k.runner.stdout = "Not logged in"
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.emit("something\n")
	k.factory.sess.finish(nil)

	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("expected a failed done frame, got %+v", frames)
	}
	if _, err := k.store.GetAgentProfile(context.Background(), k.userID, "codex"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no profile should be recorded on a failed login, got %v", err)
	}
	if k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("a fresh failed login must persist no credential store")
	}
}

func TestBrokerFailedReauthKeepsExistingLogin(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	// Prior successful login: seed a credential store + an authenticated profile.
	storeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(storeDir, "auth.json"), []byte("good-token-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	tarBytes, err := sandbox.TarDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindTar, tarBytes)); err != nil {
		t.Fatal(err)
	}
	if err := k.store.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: k.userID, Provider: "codex", AuthType: "browser_state", Status: "authenticated",
	}); err != nil {
		t.Fatal(err)
	}

	// A re-auth whose status check fails must NOT delete the prior working login.
	k.runner.stdout = "Not logged in"
	sid, err := k.broker.StartLogin(k.userID, "codex", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.emit("re-authing\n")
	k.factory.sess.finish(nil)
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("a failed re-auth should report failure: %+v", frames)
	}
	if !k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("a failed re-auth must not delete the prior credential store")
	}
	p, err := k.store.GetAgentProfile(ctx, k.userID, "codex")
	if err != nil || p.Status != "authenticated" {
		t.Fatalf("a failed re-auth must not remove the prior profile: %+v (err %v)", p, err)
	}
}

func TestBrokerConfirmUsesRedactor(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.finish(nil)
	collectFrames(t, ch)
	// The status-confirmation run must carry a redactor so the runner's capture files
	// never hold credential-store tokens unredacted.
	if k.runner.lastSpec.Redactor == nil {
		t.Fatal("the auth-confirmation status run must pass a redactor")
	}
}

// failingProfiles wraps a ProfileStore to force an UpsertAgentProfile failure.
type failingProfiles struct {
	ProfileStore
	failUpsert bool
}

func (f *failingProfiles) UpsertAgentProfile(ctx context.Context, p store.AgentProfile) error {
	if f.failUpsert {
		return errors.New("db down")
	}
	return f.ProfileStore.UpsertAgentProfile(ctx, p)
}

func TestBrokerReauthProfileWriteFailureKeepsState(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	ws, err := sandbox.NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := &failingProfiles{ProfileStore: k.store}
	b := New(Config{Runner: k.runner, Factory: k.factory, Scratch: ws, State: k.vault, Profiles: fp, NetworkIsolated: true})

	// Prior working login.
	storeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(storeDir, "auth.json"), []byte("good-token-value"), 0o600)
	tarBytes, _ := sandbox.TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindTar, tarBytes))
	_ = k.store.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: k.userID, Provider: "codex", AuthType: "browser_state", Status: "authenticated",
	})

	// Re-auth: confirmation succeeds, but the profile write fails.
	fp.failUpsert = true
	sid, err := b.StartLogin(k.userID, "codex", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, _ := b.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.emit("ok\n")
	k.factory.sess.finish(nil)
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("a failed profile write should report failure: %+v", frames)
	}
	// The credential store must survive (no rollback delete) and the prior profile too.
	if !k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("a profile-write failure during re-auth must not delete the credential store")
	}
	if _, err := k.store.GetAgentProfile(ctx, k.userID, "codex"); err != nil {
		t.Fatalf("prior profile lost after a failed re-auth: %v", err)
	}
}

func TestBrokerSetKeyFailureRestoresBrowserLogin(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	fp := &failingProfiles{ProfileStore: k.store}
	b := New(Config{Runner: k.runner, Factory: k.factory, Scratch: k.broker.cfg.Scratch, State: k.vault, Profiles: fp, NetworkIsolated: true})

	// Prior browser_state login.
	storeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(storeDir, "auth.json"), []byte("browser-token-value"), 0o600)
	tarBytes, _ := sandbox.TarDir(storeDir)
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindTar, tarBytes))
	_ = k.store.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: k.userID, Provider: "codex", AuthType: "browser_state", Status: "authenticated",
	})

	// SetKey (rotating to api_key) whose profile write fails must restore the prior
	// browser_state credential, not leave the new key behind a stale browser_state profile.
	fp.failUpsert = true
	if err := b.SetKey(ctx, k.userID, "codex", agentcli.AuthAPIKey, "sk-newkey"); err == nil {
		t.Fatal("SetKey should fail when the profile write fails")
	}
	got, err := k.vault.GetAgentState(k.userID, "codex")
	if err != nil {
		t.Fatalf("prior credential gone: %v", err)
	}
	kind, _, derr := sandbox.DecodeCredState(got)
	if derr != nil || kind != sandbox.CredKindTar {
		t.Fatalf("SetKey failure did not restore the prior browser_state credential (kind=%c err=%v)", kind, derr)
	}
}

func TestBrokerBoundsUnterminatedOutput(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	// A huge UNTERMINATED line (no newline) larger than the raw-pending cap: it must be
	// flushed as bounded line frames rather than buffered unbounded until EOF.
	go func() {
		k.factory.sess.emit(strings.Repeat("x", maxPendingLine+8000))
		k.factory.sess.finish(nil)
	}()
	frames := collectFrames(t, ch)
	gotOutput := false
	for _, f := range frames {
		if f.Type == "output" {
			gotOutput = true
			if len(f.Text) > maxLineLen+10 {
				t.Fatalf("an output frame exceeded the line cap: %d chars", len(f.Text))
			}
		}
	}
	if !gotOutput {
		t.Fatal("unterminated output should be flushed as bounded line(s), not held until EOF")
	}
}

func TestBrokerSetKeyCancelsActiveLogin(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	sid, err := k.broker.StartLogin(k.userID, "codex", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	// Set an API key while a browser login is in progress: the in-flight login must be
	// cancelled so it can't later finishLogin and overwrite the key with browser state.
	if err := k.broker.SetKey(ctx, k.userID, "codex", agentcli.AuthAPIKey, "sk-newkey"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	k.factory.sess.finish(nil)
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("the cancelled login must not report success: %+v", frames)
	}
	// The api_key profile + key blob set by SetKey survive.
	p, err := k.store.GetAgentProfile(ctx, k.userID, "codex")
	if err != nil || p.AuthType != "api_key" {
		t.Fatalf("the in-flight login overwrote the SetKey profile: %+v (err %v)", p, err)
	}
	got, _ := k.vault.GetAgentState(k.userID, "codex")
	kind, _, _ := sandbox.DecodeCredState(got)
	if kind != sandbox.CredKindKey {
		t.Fatalf("the in-flight login overwrote the key blob (kind=%c)", kind)
	}
}

func TestBrokerConfirmRejectsNonZeroExit(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	// The status command prints an auth-looking line but exits NON-ZERO: confirmation
	// must fail closed (no profile recorded).
	k.runner.stdout = "Logged in as alice@example.com"
	k.runner.exitCode = 1
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.emit("ok\n")
	k.factory.sess.finish(nil)
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("a non-zero status exit must fail confirmation: %+v", frames)
	}
	if _, err := k.store.GetAgentProfile(ctx, k.userID, "codex"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no profile should be recorded when status exits non-zero, got %v", err)
	}
}

func TestBrokerStartLoginFencedBySharedLock(t *testing.T) {
	k := newBrokerKit(t)
	// Hold the shared per-user lock to stand in for a SetKey/logout in progress: a new
	// StartLogin reservation must block until it is released (so it can't slip a login
	// between a SetKey's cancel and commit).
	unlock := k.broker.cfg.Locks.Lock(k.userID)
	type res struct {
		sid string
		err error
	}
	done := make(chan res, 1)
	go func() {
		sid, err := k.broker.StartLogin(k.userID, "codex", false)
		done <- res{sid, err}
	}()
	select {
	case <-done:
		unlock()
		t.Fatal("StartLogin reservation must block while the shared per-user lock is held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	var r res
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartLogin did not proceed after the lock released")
	}
	if r.err != nil {
		t.Fatalf("start: %v", r.err)
	}
	// Let the session's run goroutine finish so it doesn't outlive the test.
	ch, unsub, _ := k.broker.Subscribe(r.sid, k.userID)
	defer unsub()
	k.factory.sess.finish(nil)
	collectFrames(t, ch)
}

// failGetState wraps a StateStore to fail GetAgentState (a read/decrypt failure that
// is NOT ErrNotFound).
type failGetState struct {
	StateStore
	failGet bool
}

func (f *failGetState) GetAgentState(userID, provider string) ([]byte, error) {
	if f.failGet {
		return nil, errors.New("decrypt error")
	}
	return f.StateStore.GetAgentState(userID, provider)
}

func TestBrokerSetKeySnapshotReadFailureAborts(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	// A prior working key credential exists.
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindKey, []byte("prior-key")))

	fs := &failGetState{StateStore: k.vault, failGet: true}
	b := New(Config{
		Runner: k.runner, Factory: k.factory, Scratch: k.broker.cfg.Scratch,
		State: fs, Profiles: k.store, NetworkIsolated: true,
	})
	// SetKey can't snapshot the prior credential (read fails) → it must ABORT before
	// overwriting, so the prior credential isn't destroyed by a later rollback.
	if err := b.SetKey(ctx, k.userID, "codex", agentcli.AuthAPIKey, "new-key"); err == nil {
		t.Fatal("SetKey must abort when the prior credential can't be snapshotted")
	}
	got, err := k.vault.GetAgentState(k.userID, "codex")
	if err != nil {
		t.Fatalf("prior credential gone: %v", err)
	}
	kind, data, _ := sandbox.DecodeCredState(got)
	if kind != sandbox.CredKindKey || string(data) != "prior-key" {
		t.Fatalf("prior credential was modified despite a snapshot read failure: kind=%c data=%q", kind, data)
	}
}

func TestBrokerRedactsPastedInputEcho(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	token := "oauth-pasted-token-abcdef123456"
	if err := k.broker.WriteInput(sid, k.userID, token); err != nil {
		t.Fatalf("write input: %v", err)
	}
	// The CLI (on a TTY) echoes the pasted token back to stdout; it must NOT reach the
	// streamed output / SSE history.
	k.factory.sess.emit("you entered: " + token + "\n")
	k.factory.sess.finish(nil)
	frames := collectFrames(t, ch)
	for _, f := range frames {
		if strings.Contains(f.Text, token) {
			t.Fatalf("a pasted token was echoed into the login stream: %q", f.Text)
		}
	}
}

func TestBrokerWriteInputRejectsHugePaste(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	huge := strings.Repeat("a", maxPasteLen+1)
	if err := k.broker.WriteInput(sid, k.userID, huge); err == nil {
		t.Fatal("WriteInput must reject an over-long paste")
	}
	k.factory.sess.finish(nil)
	collectFrames(t, ch)
}

func TestLiveSessionPastedRetentionBounded(t *testing.T) {
	ls := &liveSession{}
	for i := 0; i < 5000; i++ {
		ls.recordPasted("token-value-padding-" + strconv.Itoa(i))
	}
	if len(ls.pasted) > maxPastedEntries {
		t.Fatalf("pasted entry count unbounded: %d > %d", len(ls.pasted), maxPastedEntries)
	}
	total := 0
	for _, p := range ls.pasted {
		total += len(p)
	}
	if total > maxPastedBytes {
		t.Fatalf("pasted byte total unbounded: %d > %d", total, maxPastedBytes)
	}
}

func TestBoundedSetCapsGrowth(t *testing.T) {
	s := newBoundedSet(512)
	for i := 0; i < 5000; i++ {
		s.seenOrAdd("k" + strconv.Itoa(i))
	}
	if s.len() > 512 {
		t.Fatalf("bounded set grew past its cap: %d > 512", s.len())
	}
	// Within the cap, de-dup still works.
	s2 := newBoundedSet(4)
	if s2.seenOrAdd("a") {
		t.Fatal("first add should report not-seen")
	}
	if !s2.seenOrAdd("a") {
		t.Fatal("second add of the same key should report seen")
	}
}

func TestBrokerSetKeyCancelsInFlightRun(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	runCtx, cancel := context.WithCancel(context.Background())
	release := k.broker.cfg.Runs.Add(k.userID, "codex", cancel)
	defer release()
	// Replacing the credential must revoke an in-flight run still using the old one.
	if err := k.broker.SetKey(ctx, k.userID, "codex", agentcli.AuthAPIKey, "sk-newkey"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("SetKey did not cancel the in-flight run")
	}
}

func TestBrokerLogoutCancelsInFlightRun(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindKey, []byte("k")))
	_ = k.store.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: "agp_x", UserID: k.userID, Provider: "codex", AuthType: "api_key", Status: "authenticated",
	})
	runCtx, cancel := context.WithCancel(context.Background())
	release := k.broker.cfg.Runs.Add(k.userID, "codex", cancel)
	defer release()
	if err := k.broker.Logout(ctx, k.userID, "codex"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel the in-flight run")
	}
}

// failDeleteProfiles fails DeleteAgentProfile (a partial-cleanup error during logout).
type failDeleteProfiles struct {
	ProfileStore
}

func (failDeleteProfiles) DeleteAgentProfile(context.Context, string, string) error {
	return errors.New("delete profile failed")
}

func TestBrokerLogoutCancelsRunEvenOnProfileDeleteError(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindKey, []byte("k")))
	runs := &sandbox.RunRegistry{}
	b := New(Config{
		Runner: k.runner, Factory: k.factory, Scratch: k.broker.cfg.Scratch,
		State: k.vault, Profiles: failDeleteProfiles{k.store}, NetworkIsolated: true, Runs: runs,
	})
	runCtx, cancel := context.WithCancel(context.Background())
	release := runs.Add(k.userID, "codex", cancel)
	defer release()
	// Logout fails at the profile delete, but must STILL revoke the in-flight run.
	if err := b.Logout(ctx, k.userID, "codex"); err == nil {
		t.Fatal("expected the profile-delete error to surface")
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("logout must cancel the in-flight run even when profile delete fails")
	}
}

func TestBrokerProcessExitFails(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	k.factory.sess.finish(errors.New("boom")) // process exited non-zero

	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("expected failure on a non-zero login exit, got %+v", frames)
	}
}

func TestBrokerWriteInput(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", true)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	if err := k.broker.WriteInput(sid, k.userID, "PASTE-CODE"); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if got := k.factory.sess.stdinText(); got != "PASTE-CODE\n" {
		t.Fatalf("stdin = %q, want PASTE-CODE\\n", got)
	}
	k.factory.sess.finish(nil)
	collectFrames(t, ch) // drain so the session goroutine finishes before cleanup
}

func TestBrokerWriteInputWrongUserRejected(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", true)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	if err := k.broker.WriteInput(sid, "someone-else", "x"); err == nil {
		t.Fatal("another user must not write to a session")
	}
	k.factory.sess.finish(nil)
	collectFrames(t, ch)
}

func TestBrokerCancel(t *testing.T) {
	k := newBrokerKit(t)
	sid, _ := k.broker.StartLogin(k.userID, "codex", false)
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	if err := k.broker.Cancel(sid, k.userID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("cancelled login should report failure, got %+v", frames)
	}
	if !k.factory.sess.killed {
		t.Error("session should have been killed")
	}
}

func TestBrokerSetKeyAndLogout(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	if err := k.broker.SetKey(ctx, k.userID, "codex", agentcli.AuthAPIKey, "sk-123"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	got, err := k.vault.GetAgentState(k.userID, "codex")
	if err != nil {
		t.Fatalf("get agent state: %v", err)
	}
	kind, data, derr := sandbox.DecodeCredState(got)
	if derr != nil || kind != sandbox.CredKindKey || string(data) != "sk-123" {
		t.Fatalf("stored key = kind %c, %q (err %v)", kind, data, derr)
	}
	p, _ := k.store.GetAgentProfile(ctx, k.userID, "codex")
	if p.AuthType != "api_key" || p.Status != "authenticated" {
		t.Fatalf("profile = %+v", p)
	}
	// Logout removes both.
	if err := k.broker.Logout(ctx, k.userID, "codex"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if k.vault.HasAgentState(k.userID, "codex") {
		t.Error("agent state should be gone after logout")
	}
	if _, err := k.store.GetAgentProfile(ctx, k.userID, "codex"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("profile should be gone after logout, got %v", err)
	}
}

func TestBrokerSetKeyRejectsUnsupportedAuthType(t *testing.T) {
	k := newBrokerKit(t)
	// Pi does not support API-key auth.
	if err := k.broker.SetKey(context.Background(), k.userID, "pi", agentcli.AuthAPIKey, "x"); err == nil {
		t.Fatal("Pi must reject an api_key profile")
	}
}

func TestBrokerRejectsDuplicateLogin(t *testing.T) {
	k := newBrokerKit(t)
	sid, err := k.broker.StartLogin(k.userID, "codex", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	// A second concurrent login for the same (user, provider) must be rejected
	// (atomic reservation), so two containers don't race to write the same store.
	if _, err := k.broker.StartLogin(k.userID, "codex", false); err == nil {
		t.Fatal("a duplicate concurrent login must be rejected")
	}
	k.factory.sess.finish(nil)
	collectFrames(t, ch)
}

func TestBrokerLogoutSerializesOnSharedLock(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	locks := &sandbox.UserLocker{}
	b := New(Config{State: k.vault, Profiles: k.store, Locks: locks})
	_ = k.store.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: k.userID, Provider: "codex", AuthType: "api_key", Status: "authenticated",
	})
	_ = k.vault.PutAgentState(k.userID, "codex", sandbox.EncodeCredState(sandbox.CredKindKey, []byte("sk")))

	// Hold the shared per-user lock to stand in for an in-flight agent run.
	unlock := locks.Lock(k.userID)
	done := make(chan error, 1)
	go func() { done <- b.Logout(ctx, k.userID, "codex") }()
	select {
	case <-done:
		unlock()
		t.Fatal("Logout must block while a run holds the shared per-user lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("logout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Logout did not complete after the shared lock released")
	}
	if k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("logout should have deleted the credential state once it acquired the lock")
	}
}

func TestBrokerBrowserAuthUnavailable(t *testing.T) {
	k := newBrokerKit(t)
	k.factory.avail = false
	if _, err := k.broker.StartLogin(k.userID, "codex", false); err == nil {
		t.Fatal("StartLogin should fail when the factory is unavailable")
	}
}

func TestBrokerLoginRequiresNetworkIsolation(t *testing.T) {
	k := newBrokerKit(t)
	ws, err := sandbox.NewWorkspaceManager(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "r"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A browser login runs a network-on container with credentials, so it is gated on
	// the controlled-egress assertion just like an agent run.
	b := New(Config{Runner: k.runner, Factory: k.factory, Scratch: ws, State: k.vault, Profiles: k.store, NetworkIsolated: false})
	if b.BrowserAuthAvailable() {
		t.Error("browser auth must be unavailable without network isolation")
	}
	if _, err := b.StartLogin(k.userID, "codex", true); err == nil {
		t.Fatal("StartLogin must fail closed when network isolation is unasserted")
	}
}

func TestBrokerLogoutCancelsActiveLogin(t *testing.T) {
	ctx := context.Background()
	k := newBrokerKit(t)
	sid, err := k.broker.StartLogin(k.userID, "codex", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, unsub, _ := k.broker.Subscribe(sid, k.userID)
	defer unsub()
	// Logout while the login is still streaming (not finished): it must cancel the
	// session and leave no profile/state behind — no resurrection.
	if err := k.broker.Logout(ctx, k.userID, "codex"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	frames := collectFrames(t, ch)
	if d := lastDone(frames); d == nil || d.OK {
		t.Fatalf("a logout-cancelled login must not report success: %+v", frames)
	}
	if _, err := k.store.GetAgentProfile(ctx, k.userID, "codex"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no profile must exist after a logout-cancelled login, got %v", err)
	}
	if k.vault.HasAgentState(k.userID, "codex") {
		t.Fatal("no credential state must exist after a logout-cancelled login")
	}
}

func TestBuildAuthArgs(t *testing.T) {
	args := buildAuthArgs(SessionSpec{
		ID: "auth_1", Argv: []string{"codex", "login"}, Env: []string{"CODEX_HOME=/agent/codex"},
		StateDir: "/host/state", StateContainerDir: "/agent/codex",
	}, "mykit", sandbox.Limits{CPUs: "2", Memory: "2g", PIDs: 512})
	joined := strings.Join(args, " ")
	// The auth container must carry the same kit + resource caps as a normal run.
	for _, want := range []string{
		"run -it --name auth_1", "--kit mykit", "--cpus 2", "-m 2g", "--pids-limit 512",
		"/host/state:/agent/codex", "--env CODEX_HOME=/agent/codex", "-- codex login",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("auth args missing %q: %s", want, joined)
		}
	}
}

func framesHaveHint(frames []Frame, kind HintKind) bool {
	for _, f := range frames {
		if f.Type == "hint" && f.Hint != nil && f.Hint.Kind == kind {
			return true
		}
	}
	return false
}

func lastDone(frames []Frame) *Frame {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Type == "done" {
			f := frames[i]
			return &f
		}
	}
	return nil
}
