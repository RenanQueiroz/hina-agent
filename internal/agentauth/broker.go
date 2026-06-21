package agentauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// loginTimeout bounds an interactive login session: a user who never completes the
// device/paste flow shouldn't leave a container running forever.
const loginTimeout = 10 * time.Minute

// frameHistory caps how many recent frames a session retains to replay to a
// subscriber that connects just after StartLogin (so it never misses the login URL).
const frameHistory = 500

// maxPendingLine bounds the raw, not-yet-newline-terminated read buffer, so a CLI
// printing a huge unterminated line can't grow server memory (and keeps the per-read
// hint scan from going quadratic). At the cap the buffer is flushed as one line.
const maxPendingLine = 64 << 10 // 64 KiB

// SessionFactory starts an interactive login session in a short-lived auth
// container. The production factory shells out to `sbx run -it`; tests use a fake.
type SessionFactory interface {
	Available() bool
	Start(ctx context.Context, spec SessionSpec) (Session, error)
}

// SessionSpec is one interactive login invocation.
type SessionSpec struct {
	ID                string
	Argv              []string // login argv (CLI binary first)
	Env               []string // non-secret env (LoginEnv + cred-store env)
	StateDir          string   // host dir holding the materialized agent-state (mounted RW)
	StateContainerDir string   // where StateDir is mounted in the container
}

// Session is a running interactive login process. Stdout streams its (raw) output;
// Write feeds stdin (pasted codes); Wait blocks until it exits; Kill terminates it.
type Session interface {
	Stdout() io.Reader
	Write(p []byte) (int, error)
	Wait() error
	Kill()
}

// StateStore is the vault surface for per-provider agent-state. *vault.Vault fits.
type StateStore interface {
	GetAgentState(userID, provider string) ([]byte, error)
	PutAgentState(userID, provider string, data []byte) error
	HasAgentState(userID, provider string) bool
	DeleteAgentState(userID, provider string) error
}

// ProfileStore is the store surface for agent profiles. *store.Store fits.
type ProfileStore interface {
	UpsertAgentProfile(ctx context.Context, p store.AgentProfile) error
	GetAgentProfile(ctx context.Context, userID, provider string) (store.AgentProfile, error)
	DeleteAgentProfile(ctx context.Context, userID, provider string) error
}

// ScratchProvider yields owner-private ephemeral dirs. *sandbox.WorkspaceManager fits.
type ScratchProvider interface {
	NewScratch() (sandbox.Scratch, error)
}

// Frame is one streamed login event (output line, actionable hint, or terminal
// done). It never carries a persisted credential — only the transient live view.
type Frame struct {
	Type  string `json:"type"`            // "output" | "hint" | "done"
	Text  string `json:"text,omitempty"`  // sanitized output line
	Hint  *Hint  `json:"hint,omitempty"`  // a detected URL/code/prompt
	OK    bool   `json:"ok,omitempty"`    // done: did the login succeed
	Error string `json:"error,omitempty"` // done: failure reason
}

// Config wires the broker.
type Config struct {
	Runner          sandbox.Runner // one-shot status confirmation runs
	Factory         SessionFactory // interactive login sessions
	Scratch         ScratchProvider
	State           StateStore
	Profiles        ProfileStore
	NetworkIsolated bool
	// Locks is the SHORT per-user credential lock shared with the AgentRouter (its
	// refreshed-state persist), so the broker's agent-state/profile writes (SetKey/
	// finishLogin/logout) can't race a run's credential re-persist. It is NOT the long
	// run lock, so a logout/key-rotation stays prompt while a run is in flight. New()
	// creates a private one if nil (fine for isolated tests; production shares one).
	Locks *sandbox.UserLocker
	// Runs is the in-flight agent-run registry shared with the AgentRouter, so a logout
	// can cancel a run holding (or about to launch with) the revoked credential. New()
	// creates a private one if nil.
	Runs *sandbox.RunRegistry
	Log  *slog.Logger
}

// Broker runs and supervises per-user agent logins. It is the single owner of live
// login sessions, their sanitized output streams, and the recording of a profile on
// success.
type Broker struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*liveSession
}

// New builds a Broker.
func New(cfg Config) *Broker {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Locks == nil {
		cfg.Locks = &sandbox.UserLocker{}
	}
	if cfg.Runs == nil {
		cfg.Runs = &sandbox.RunRegistry{}
	}
	return &Broker{cfg: cfg, log: cfg.Log, sessions: make(map[string]*liveSession)}
}

// BrowserAuthAvailable reports whether interactive browser logins can run: the auth
// container factory is usable AND the operator has asserted controlled sandbox egress
// (network_isolated). A browser/device login runs a network-on container carrying
// provider credential state, so it is gated by the SAME fail-closed assertion as an
// agent run — otherwise the most credential-sensitive path would ignore the gate.
// API-key profiles don't need it (they store a key; the RUN is independently gated).
func (b *Broker) BrowserAuthAvailable() bool {
	return b.cfg.NetworkIsolated && b.cfg.Factory != nil && b.cfg.Factory.Available()
}

type liveSession struct {
	id, userID, provider string
	containerDir         string
	statusEnv            []string
	sess                 Session
	scratch              sandbox.Scratch
	stateDir             string
	cancel               context.CancelFunc
	adapter              agentcli.Adapter

	// cancelled is set under Broker.mu by Logout so a login that finishes after a
	// logout does not re-create the credentials/profile the logout removed.
	cancelled bool

	mu      sync.Mutex
	subs    map[int]chan Frame
	nextSub int
	history []Frame
	pasted  []string // values the user pasted via WriteInput, redacted from echoed output
	done    bool
}

const (
	// minPastedRedact is the shortest pasted value treated as secret material (avoids
	// redacting tiny confirmations like "y"); device codes/tokens are well above this.
	minPastedRedact = 4
	// maxPasteLen caps a single pasted input (a code/token is small) so a client can't
	// stream megabytes into one WriteInput.
	maxPasteLen = 4096
	// maxPastedEntries / maxPastedBytes bound the per-session redaction set so repeated
	// pastes over a 10-minute login can't grow server heap (or the per-line scan cost).
	maxPastedEntries = 64
	maxPastedBytes   = 64 << 10
	// maxSeenHints bounds the per-session hint de-dup set so a CLI emitting endless
	// unique URL/code-shaped lines can't grow server memory over the login window.
	maxSeenHints = 512
)

// recordPasted remembers a value the user pasted so it can be scrubbed from echoed
// output (a TTY login reflects stdin to stdout). The retained set is bounded by count
// and total bytes (dropping the oldest), so it can't grow without limit.
func (ls *liveSession) recordPasted(val string) {
	val = strings.TrimRight(val, "\r\n")
	if len(val) < minPastedRedact {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.pasted = append(ls.pasted, val)
	total := 0
	for _, p := range ls.pasted {
		total += len(p)
	}
	for len(ls.pasted) > 1 && (len(ls.pasted) > maxPastedEntries || total > maxPastedBytes) {
		total -= len(ls.pasted[0])
		ls.pasted = ls.pasted[1:]
	}
}

// redactPasted replaces any previously-pasted value in s with the redaction marker.
func (ls *liveSession) redactPasted(s string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, v := range ls.pasted {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// StartLogin begins an interactive login for (userID, provider) and returns a
// session id the caller subscribes to for the streamed output. deviceAuth selects
// the device/paste-code flow.
func (b *Broker) StartLogin(userID, provider string, deviceAuth bool) (string, error) {
	adapter, ok := agentcli.Get(agentcli.Provider(provider))
	if !ok {
		return "", fmt.Errorf("unknown agent %q", provider)
	}
	capability := adapter.Capability()
	if !capability.BrowserAuth {
		return "", fmt.Errorf("%s does not support interactive login", capability.DisplayName)
	}
	if b.cfg.Factory == nil || !b.cfg.Factory.Available() {
		return "", errors.New("interactive login is unavailable: it needs a working sbx install")
	}
	// Fail closed: a browser/device login runs a network-on container with credential
	// state, so it requires the controlled-egress assertion just like an agent run.
	if !b.cfg.NetworkIsolated {
		return "", errors.New("interactive login is disabled unless [sandbox] network_isolated=true (the login container has network access and carries credentials)")
	}
	cs := adapter.CredStore()
	sessionID := id.New("auth")
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)

	// Reserve the (user, provider) slot ATOMICALLY: register a placeholder session
	// under b.mu BEFORE the (slow) container start, so a concurrent duplicate is
	// rejected and a Logout that lands during start can mark this session cancelled
	// (it has no live process to Kill yet — the attach step below honors the flag).
	ls := &liveSession{
		id: sessionID, userID: userID, provider: provider,
		containerDir: cs.ContainerDir, cancel: cancel, adapter: adapter,
		subs: make(map[int]chan Frame),
	}
	// Reserve under the shared per-user lock (then b.mu), so a SetKey/logout can't slip
	// its cancel+commit between this reservation and the session becoming visible — the
	// reservation either fully precedes a SetKey (whose cancel then catches it) or fully
	// follows it. Released immediately; the slow Factory.Start runs unlocked.
	relLock := b.cfg.Locks.Lock(userID)
	b.mu.Lock()
	if b.hasActiveSessionLocked(userID, provider) {
		b.mu.Unlock()
		relLock()
		cancel()
		return "", errors.New("a login for this agent is already in progress")
	}
	b.sessions[sessionID] = ls
	b.mu.Unlock()
	relLock()

	// On any setup failure, drop the reservation.
	fail := func(err error) (string, error) {
		b.mu.Lock()
		delete(b.sessions, sessionID)
		b.mu.Unlock()
		cancel()
		return "", err
	}

	scratch, err := b.cfg.Scratch.NewScratch()
	if err != nil {
		return fail(fmt.Errorf("prepare login scratch: %w", err))
	}
	stateDir := filepath.Join(scratch.Dir, "state")
	if err := platform.EnsurePrivateDir(stateDir); err != nil {
		scratch.Remove()
		return fail(err)
	}
	// Seed the container's credential store with any existing encrypted state (a
	// re-auth keeps the prior config); a fresh login starts empty.
	if b.cfg.State.HasAgentState(userID, provider) {
		if blob, err := b.cfg.State.GetAgentState(userID, provider); err == nil {
			if kind, data, derr := sandbox.DecodeCredState(blob); derr == nil && kind == sandbox.CredKindTar {
				if err := sandbox.UntarToDir(data, stateDir); err != nil {
					b.log.Warn("agentauth: could not unpack existing agent state; starting fresh", "provider", provider, "err", err)
				}
			}
		}
	}

	env := append([]string(nil), adapter.LoginEnv()...)
	if cs.EnvVar != "" {
		env = append(env, cs.EnvVar+"="+cs.ContainerDir)
	}
	sess, err := b.cfg.Factory.Start(ctx, SessionSpec{
		ID:                sessionID,
		Argv:              adapter.LoginArgs(agentcli.LoginOptions{DeviceAuth: deviceAuth}),
		Env:               env,
		StateDir:          stateDir,
		StateContainerDir: cs.ContainerDir,
	})
	if err != nil {
		scratch.Remove()
		return fail(fmt.Errorf("start login: %w", err))
	}

	// Attach the live process. If a Logout marked the reservation cancelled during the
	// (slow) start, abort now so the just-started container is torn down and no profile
	// is ever recorded.
	b.mu.Lock()
	if ls.cancelled {
		delete(b.sessions, sessionID)
		b.mu.Unlock()
		sess.Kill()
		scratch.Remove()
		cancel()
		return "", errors.New("the login was cancelled")
	}
	ls.sess = sess
	ls.scratch = scratch
	ls.stateDir = stateDir
	ls.statusEnv = env
	b.mu.Unlock()

	go b.run(ctx, ls)
	return sessionID, nil
}

// hasActiveSessionLocked reports whether a live session exists for (userID,
// provider). Caller must hold b.mu.
func (b *Broker) hasActiveSessionLocked(userID, provider string) bool {
	for _, ls := range b.sessions {
		if ls.userID == userID && ls.provider == provider {
			return true
		}
	}
	return false
}

// Subscribe registers a frame subscriber for a session the user owns, replaying the
// recent history first so a just-connected client never misses the login URL/code.
// It returns the channel and an unsubscribe func, or an error if the session is
// unknown / not the user's.
func (b *Broker) Subscribe(sessionID, userID string) (<-chan Frame, func(), error) {
	ls := b.lookup(sessionID, userID)
	if ls == nil {
		return nil, nil, errors.New("no such login session")
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ch := make(chan Frame, frameHistory+16)
	for _, f := range ls.history {
		ch <- f
	}
	if ls.done {
		close(ch)
		return ch, func() {}, nil
	}
	subID := ls.nextSub
	ls.nextSub++
	ls.subs[subID] = ch
	return ch, func() {
		ls.mu.Lock()
		if c, ok := ls.subs[subID]; ok {
			delete(ls.subs, subID)
			close(c)
		}
		ls.mu.Unlock()
	}, nil
}

// WriteInput feeds a pasted code/line to a session's stdin (appending a newline).
func (b *Broker) WriteInput(sessionID, userID, data string) error {
	b.mu.Lock()
	ls := b.sessions[sessionID]
	if ls == nil || ls.userID != userID {
		b.mu.Unlock()
		return errors.New("no such login session")
	}
	sess := ls.sess
	b.mu.Unlock()
	if sess == nil {
		return errors.New("the login is still starting; try again")
	}
	if len(data) > maxPasteLen {
		return errors.New("pasted input is too long")
	}
	// Remember the pasted value so a TTY echo of it is scrubbed from the streamed output.
	ls.recordPasted(data)
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	_, err := sess.Write([]byte(data))
	return err
}

// Cancel terminates a login session (marking it cancelled so a placeholder that is
// still starting aborts on attach).
func (b *Broker) Cancel(sessionID, userID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.sessions[sessionID]
	if ls == nil || ls.userID != userID {
		return errors.New("no such login session")
	}
	ls.cancelled = true
	if ls.sess != nil {
		ls.sess.Kill()
	}
	ls.cancel()
	return nil
}

// SetKey configures an API-key / OAuth-token profile without a container: it stores
// the credential as encrypted agent-state and records the profile. The credential
// value is never logged or returned.
func (b *Broker) SetKey(ctx context.Context, userID, provider string, authType agentcli.AuthType, key string) error {
	adapter, ok := agentcli.Get(agentcli.Provider(provider))
	if !ok {
		return fmt.Errorf("unknown agent %q", provider)
	}
	if !supportsAuthType(adapter.Capability(), authType) {
		return fmt.Errorf("%s does not support %s auth", adapter.Capability().DisplayName, authType)
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("the credential value is empty")
	}
	// Serialize with agent runs / logout on the shared per-user lock, so a key write
	// can't interleave with a run's credential re-persist or a concurrent logout.
	unlock := b.cfg.Locks.Lock(userID)
	defer unlock()
	// Cancel any in-flight login for this provider so an older browser login can't later
	// finishLogin and overwrite the key we're about to store (cross-tab/retry race).
	b.cancelSessions(userID, provider)
	// Snapshot any existing credential so a failed profile write can be undone — a
	// rotation (e.g. browser_state -> api_key) overwrites the prior blob with a
	// different kind, which a stale profile would then point at and fail closed on.
	// Abort BEFORE overwriting if the prior credential can't be read (don't risk
	// destroying it on a later rollback we can't undo).
	old, hadOld, serr := b.snapshotState(userID, provider)
	if serr != nil {
		return fmt.Errorf("could not read the existing %s credential; refusing to replace it: %w", provider, serr)
	}
	if err := b.cfg.State.PutAgentState(userID, provider, sandbox.EncodeCredState(sandbox.CredKindKey, []byte(key))); err != nil {
		return fmt.Errorf("store credential: %w", err)
	}
	if err := b.cfg.Profiles.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: userID, Provider: provider,
		AuthType: string(authType), Status: "authenticated", Label: "configured",
	}); err != nil {
		b.restoreState(userID, provider, old, hadOld)
		return fmt.Errorf("record profile: %w", err)
	}
	// Revoke any in-flight run still using the OLD credential — the run's launch fence
	// also refuses if it hasn't launched yet (the credential version changed).
	b.cfg.Runs.Cancel(userID, provider)
	return nil
}

// snapshotState reads the current encrypted agent-state blob for (userID, provider)
// so a credential replacement whose profile write then fails can be undone. It
// distinguishes "no prior credential" (ErrNotFound → hadOld=false, nil error) from a
// read/decrypt failure (a non-nil error): the caller MUST abort before overwriting on
// an error, so a transient read failure can't be mistaken for "no prior state" and let
// restoreState delete a credential it could not snapshot.
func (b *Broker) snapshotState(userID, provider string) (data []byte, hadOld bool, err error) {
	d, gerr := b.cfg.State.GetAgentState(userID, provider)
	if errors.Is(gerr, store.ErrNotFound) {
		return nil, false, nil
	}
	if gerr != nil {
		return nil, false, gerr
	}
	return d, true, nil
}

// restoreState puts back a snapshotted blob (or deletes a freshly-written orphan when
// there was no prior), so a failed profile write leaves the prior login exactly as it
// was rather than a profile/blob mismatch.
func (b *Broker) restoreState(userID, provider string, old []byte, hadOld bool) {
	var err error
	if hadOld {
		err = b.cfg.State.PutAgentState(userID, provider, old)
	} else {
		err = b.cfg.State.DeleteAgentState(userID, provider)
	}
	if err != nil {
		b.log.Warn("agentauth: could not restore prior credential after a failed profile write", "provider", provider, "err", err)
	}
}

// Logout removes a provider's stored credential and profile for a user. It first
// cancels (and fences) any in-flight login for the same (user, provider) under the
// broker lock, so a login that completes concurrently cannot re-create what this
// logout removes (see finishLogin's cancelled check).
func (b *Broker) Logout(ctx context.Context, userID, provider string) error {
	// Take the cred lock FIRST (outer), so a logout serializes with an agent run's
	// credential re-persist, its launch fence, and finishLogin — closing the TOCTOU
	// where a run re-wrote (or launched with) the store after logout deleted it. b.mu
	// (inner) guards the session map.
	unlock := b.cfg.Locks.Lock(userID)
	defer unlock()
	// Cancel any in-flight run holding (or about to launch with) the credential on EVERY
	// return path — even a partial-cleanup error (a failed state/profile delete) must
	// still revoke active runs. Deferred AFTER unlock so it runs BEFORE the lock releases
	// (LIFO), serialized with the run's launch fence: a run that hasn't passed the fence
	// finds the credential gone/changed and refuses; one that has is cancelled.
	defer b.cfg.Runs.Cancel(userID, provider)
	b.mu.Lock()
	b.cancelSessionsLocked(userID, provider)
	err := b.cfg.State.DeleteAgentState(userID, provider)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if err := b.cfg.Profiles.DeleteAgentProfile(ctx, userID, provider); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// boundedSet is a string set used for hint de-duplication that STOPS growing past a
// cap: past it a new key still reports "not seen" (so output keeps streaming) but is
// not recorded, so memory can't grow without limit. At worst a late duplicate re-emits.
type boundedSet struct {
	m   map[string]struct{}
	cap int
}

func newBoundedSet(cap int) *boundedSet { return &boundedSet{m: make(map[string]struct{}), cap: cap} }

// seenOrAdd reports whether k was already recorded; otherwise it records k (up to the
// cap) and reports false.
func (b *boundedSet) seenOrAdd(k string) bool {
	if _, ok := b.m[k]; ok {
		return true
	}
	if len(b.m) < b.cap {
		b.m[k] = struct{}{}
	}
	return false
}

func (b *boundedSet) len() int { return len(b.m) }

// cancelSessions marks every live session for (userID, provider) cancelled and kills
// it, so a login finishing later can't re-create credentials a SetKey/logout removed.
func (b *Broker) cancelSessions(userID, provider string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelSessionsLocked(userID, provider)
}

// cancelSessionsLocked is cancelSessions assuming b.mu is held.
func (b *Broker) cancelSessionsLocked(userID, provider string) {
	for _, ls := range b.sessions {
		if ls.userID == userID && ls.provider == provider {
			ls.cancelled = true
			if ls.sess != nil {
				ls.sess.Kill()
			}
			ls.cancel()
		}
	}
}

func (b *Broker) lookup(sessionID, userID string) *liveSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.sessions[sessionID]
	if ls == nil || ls.userID != userID {
		return nil
	}
	return ls
}

// run drives a session's output stream into frames, then finalizes the login.
func (b *Broker) run(ctx context.Context, ls *liveSession) {
	defer b.endSession(ls)
	r := ls.sess.Stdout()
	buf := make([]byte, 4096)
	var pending []byte
	// seen de-duplicates hints, but is BOUNDED: a drifting/compromised CLI printing many
	// unique URL/code-shaped lines must not grow it without limit over the login window.
	seen := newBoundedSet(maxSeenHints)
	emitHints := func(line string) {
		for _, h := range Detect(line) {
			if seen.seenOrAdd(string(h.Kind) + "|" + h.Value) {
				continue
			}
			hh := h
			ls.broadcast(Frame{Type: "hint", Hint: &hh})
		}
	}
	for {
		n, err := r.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := indexByte(pending, '\n')
				if i < 0 {
					break
				}
				// Redact any value the user PASTED (WriteInput) from the echoed output: a
				// CLI run on a TTY echoes stdin back to stdout, which would otherwise put a
				// pasted OAuth token / API key / device code into the SSE stream + history.
				line := ls.redactPasted(Sanitize(string(pending[:i])))
				pending = pending[i+1:]
				if line != "" {
					ls.broadcast(Frame{Type: "output", Text: line})
					emitHints(line)
				}
			}
			// A drifting/compromised CLI could print a long UNTERMINATED line for the
			// whole 10-minute window; bound the raw buffer (flush it as a line + reset)
			// so memory can't grow without limit and the per-read hint scan stays cheap.
			if len(pending) > maxPendingLine {
				line := ls.redactPasted(Sanitize(string(pending)))
				pending = pending[:0]
				if line != "" {
					ls.broadcast(Frame{Type: "output", Text: line})
					emitHints(line)
				}
			} else if p := ls.redactPasted(Sanitize(string(pending))); p != "" {
				// Surface a URL/code/prompt that arrived on a still-incomplete line (an
				// interactive prompt printed without a trailing newline).
				emitHints(p)
			}
		}
		if err != nil {
			break
		}
	}
	if p := ls.redactPasted(Sanitize(string(pending))); p != "" {
		ls.broadcast(Frame{Type: "output", Text: p})
		emitHints(p)
	}
	waitErr := ls.sess.Wait()
	ok, reason := b.finishLogin(ctx, ls, waitErr)
	ls.broadcast(Frame{Type: "done", OK: ok, Error: reason})
}

// finishLogin confirms the login with the provider status command (reading the
// mounted credential store directly), then — under the broker lock, fenced against a
// concurrent Logout — persists the kind-tagged credential store and records the
// profile, rolling the blob back if the profile write fails.
//
// A FAILED login is non-destructive: nothing is persisted until confirmation
// succeeds, so a failed/cancelled re-auth NEVER deletes the user's existing working
// credentials — only the transient scratch (removed by endSession) is lost.
func (b *Broker) finishLogin(ctx context.Context, ls *liveSession, waitErr error) (bool, string) {
	if waitErr != nil {
		return false, "the login process exited before completing"
	}
	// Confirm against the mounted store (ls.stateDir) BEFORE persisting — no DB/vault
	// write yet, so this slow step holds no lock and a failure leaves prior state intact.
	ok, reason := b.confirm(ctx, ls)
	if !ok {
		return false, reason
	}
	data, err := sandbox.TarDir(ls.stateDir)
	if err != nil {
		b.log.Warn("agentauth: could not archive credential store", "provider", ls.provider, "err", err)
		return false, "could not save the login result"
	}

	// The persist + profile record must be atomic w.r.t. Logout AND a concurrent agent
	// run's credential re-persist: take the shared per-user lock (outer), then the
	// broker lock (inner) to re-check we weren't logged out mid-login, then commit.
	unlock := b.cfg.Locks.Lock(ls.userID)
	defer unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if ls.cancelled {
		// A logout already removed this provider's state/profile under the shared lock;
		// don't resurrect it (and don't touch any newer state a re-auth wrote).
		return false, "the login was cancelled"
	}
	// Snapshot the prior blob so a failed profile write restores the previous working
	// login (a prior api_key/browser_state login isn't bricked by a transient DB error).
	// Abort before overwriting if the prior credential can't be read.
	old, hadOld, serr := b.snapshotState(ls.userID, ls.provider)
	if serr != nil {
		return false, "could not read the existing credential; not replacing it"
	}
	if err := b.cfg.State.PutAgentState(ls.userID, ls.provider, sandbox.EncodeCredState(sandbox.CredKindTar, data)); err != nil {
		b.log.Warn("agentauth: could not persist credential store", "provider", ls.provider, "err", err)
		return false, "could not save the login result"
	}
	if err := b.cfg.Profiles.UpsertAgentProfile(ctx, store.AgentProfile{
		ID: id.New("agp"), UserID: ls.userID, Provider: ls.provider,
		AuthType: string(agentcli.AuthBrowserState), Status: "authenticated", Label: "configured",
	}); err != nil {
		b.restoreState(ls.userID, ls.provider, old, hadOld)
		b.log.Error("agentauth: could not record profile after login; restored the prior credential", "provider", ls.provider, "err", err)
		return false, "could not record the profile"
	}
	// A successful re-auth replaced the credential — revoke any in-flight run still using
	// the old browser state (the run's launch fence refuses an un-launched stale run).
	b.cfg.Runs.Cancel(ls.userID, ls.provider)
	return true, ""
}

// confirm runs the provider's status command in a one-shot sandbox with the just-
// stored credential store mounted read-only. When no runner is available it trusts
// the clean login exit (the broker only starts when the factory — hence sbx — is up,
// so this is a defensive fallback).
func (b *Broker) confirm(ctx context.Context, ls *liveSession) (bool, string) {
	if b.cfg.Runner == nil || !b.cfg.Runner.Available() {
		return true, ""
	}
	// The status command reads the mounted credential store and may print token-shaped
	// material; build a redactor so the runner's capture files never hold a credential
	// outside the vault boundary. Fail closed if the store can't be fully scanned.
	red, rerr := sandbox.CredStoreRedactor(ls.stateDir)
	if rerr != nil {
		return false, "could not scan the credential store for redaction"
	}
	res, err := b.cfg.Runner.Run(ctx, sandbox.RunSpec{
		UserID: ls.userID, Tool: "agent.auth",
		Argv:     ls.adapter.StatusArgs(),
		Env:      ls.statusEnv,
		Mounts:   []sandbox.Mount{{Host: ls.stateDir, Container: ls.containerDir, ReadOnly: true}},
		Redactor: red,
	})
	if err != nil || res.Err != nil {
		return false, "could not confirm the login"
	}
	// Require a clean exit AND an authenticated-looking status — a non-zero status
	// command that happens to print an auth-looking line must NOT pass (fail closed).
	if res.ExitCode != 0 {
		return false, "the provider status command did not succeed"
	}
	if ls.adapter.AuthOK(res.Stdout) {
		return true, ""
	}
	return false, "the provider reports the login did not complete"
}

// endSession tears down a finished session: remove it from the registry, free the
// scratch dir, cancel its context, and close all subscriber channels.
func (b *Broker) endSession(ls *liveSession) {
	b.mu.Lock()
	delete(b.sessions, ls.id)
	b.mu.Unlock()

	ls.mu.Lock()
	ls.done = true
	for id, ch := range ls.subs {
		delete(ls.subs, id)
		close(ch)
	}
	ls.mu.Unlock()

	ls.cancel()
	ls.scratch.Remove()
}

// broadcast records a frame in the history and fans it out to live subscribers
// (non-blocking — a slow subscriber drops frames rather than stalling the reader).
func (ls *liveSession) broadcast(f Frame) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.history = append(ls.history, f)
	if len(ls.history) > frameHistory {
		ls.history = ls.history[len(ls.history)-frameHistory:]
	}
	for _, ch := range ls.subs {
		select {
		case ch <- f:
		default:
		}
	}
}

func supportsAuthType(c agentcli.Capability, t agentcli.AuthType) bool {
	for _, a := range c.AuthTypes {
		if a == t {
			return true
		}
	}
	return false
}

// indexByte returns the index of the first b in p, or -1.
func indexByte(p []byte, b byte) int {
	for i, c := range p {
		if c == b {
			return i
		}
	}
	return -1
}
