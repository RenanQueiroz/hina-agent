package autorun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// Store is the persistence surface the automation service needs. *store.Store
// satisfies it; an interface keeps the service unit-testable with a fake.
type Store interface {
	CreateAutomation(ctx context.Context, a store.Automation) error
	UpdateAutomation(ctx context.Context, a store.Automation) error
	GetAutomation(ctx context.Context, id, ownerUserID string) (store.Automation, error)
	GetAutomationByID(ctx context.Context, id string) (store.Automation, error)
	ListAutomationsByUser(ctx context.Context, userID string) ([]store.Automation, error)
	ListSchedulableAutomations(ctx context.Context) ([]store.Automation, error)
	CountEnabledByUser(ctx context.Context, userID string) (int, error)
	SoftDeleteAutomation(ctx context.Context, id, ownerUserID string) error
	SetAutomationEnabled(ctx context.Context, id, ownerUserID string, enabled bool, next time.Time, expectedGen int64, maxEnabled int) (bool, error)
	SetAutomationSchedule(ctx context.Context, id string, next, last time.Time) error
	SetAutomationPendingFire(ctx context.Context, id, token string) error
	SetPendingFireIfCurrent(ctx context.Context, id, token string, gen int64) (bool, error)
	ClaimPendingFire(ctx context.Context, id, token string) (bool, error)
	ClaimDueRun(ctx context.Context, id string, expectedNext, next, last time.Time) (bool, error)
	InsertAutomationRun(ctx context.Context, r store.AutomationRun) error
	FinalizeAutomationRun(ctx context.Context, r store.AutomationRun) error
	GetAutomationRun(ctx context.Context, id, ownerUserID string) (store.AutomationRun, error)
	ListAutomationRuns(ctx context.Context, automationID, ownerUserID string, limit int) ([]store.AutomationRun, error)
	MarkRunningRunsInterrupted(ctx context.Context, status, errMsg string) (int, error)
	InsertAutomationArtifact(ctx context.Context, a store.AutomationArtifact) error
	ListAutomationArtifacts(ctx context.Context, runID string) ([]store.AutomationArtifact, error)
	GetAutomationArtifact(ctx context.Context, id, ownerUserID string) (store.AutomationArtifact, error)
}

// ServiceConfig wires the automation service.
type ServiceConfig struct {
	Store       Store
	Exec        ExecConfig
	Workspaces  *sandbox.WorkspaceManager
	Caps        automation.Caps
	ArtifactDir string // owner-private root for promoted artifact files
	Tick        time.Duration
	// Eligibility resolves the runtime facts (server gates + owner secrets/agents)
	// that decide whether a definition may be ENABLED. Injected by the HTTP wiring so
	// autorun stays decoupled from the agents catalog.
	Eligibility func(ctx context.Context, userID string) (automation.Eligibility, error)
	// MaxConcurrentRuns bounds how many automation runs may execute concurrently across
	// the WHOLE service (0 = unlimited). MaxRunsPerUser bounds per owner (0 = unlimited).
	// Together with the per-run MaxParallelism cap they keep total sbx/LLM fan-out bounded
	// so many due automations can't exhaust the host.
	MaxConcurrentRuns int
	MaxRunsPerUser    int
	// MaxEnabledPerUser bounds how many automations one user may have ENABLED at once (0 =
	// unlimited). It is admission control against a user enabling an unbounded number of
	// automations (each a standing scheduler candidate + stored definition), complementing the
	// manual-excluding ListSchedulableAutomations query.
	MaxEnabledPerUser int
	// MaxWorkspaceBytes caps a single run's scratch disk use (0 = unwatched). A watchdog
	// kills a run whose VISIBLE scratch exceeds it. MinFreeBytes is the authoritative
	// host-disk guard: it kills a run when the scratch FILESYSTEM's free space drops below
	// this floor (via statfs, so it catches blocks held by open-but-unlinked files that a
	// directory walk can't see — closing the killed process's fds frees them).
	// WorkspaceWatchInterval is the poll period (default 2s).
	MaxWorkspaceBytes      int64
	MinFreeBytes           int64
	WorkspaceWatchInterval time.Duration
	Log                    *slog.Logger
	Now                    func() time.Time
}

// Service owns automation definitions, the durable scheduler, and run execution.
type Service struct {
	cfg ServiceConfig
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	active   map[string][]*activeRun // automationID -> in-flight runs
	pending  map[string]pendingFire  // automationID -> a queued replacement fire
	userRuns map[string]int          // userID -> active run count (per-user cap)
	runSlots chan struct{}           // global active-run permits (nil = unlimited)
	started  bool
	stopCtx  context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
}

// activeRun tracks one in-flight run for concurrency + cancellation. canceled records
// a cancellation that arrived BEFORE the run bound its cancel func (the window between
// beginRun and setRunCancel), so the late-bound context is cancelled immediately.
type activeRun struct {
	runID    string
	userID   string
	cancel   context.CancelFunc
	canceled bool
}

// pendingFire is a queued replacement (queue_one / cancel_previous) waiting for the
// active run to drain. It carries the pre-generated runID + owner so the queued run
// reuses that id — letting a manual trigger return a real, pollable run id rather than
// reporting an error for work it actually accepted.
type pendingFire struct {
	userID string
	runID  string
	token  string // the durable pending_fire token this occurrence holds (compare-and-clear)
	gen    int64  // the automation generation when this fire was QUEUED (the stale-fire guard)
}

// beginOutcome is the result of admitting a run through the concurrency policy + caps.
type beginOutcome int

const (
	beginRefused beginOutcome = iota // not admitted by the per-automation policy (intentional skip)
	beginStarted                     // a new active run was registered (caller runs it)
	beginQueued                      // a replacement was cancelled/queued (will start later)
	beginCapped                      // policy admitted it, but a service/per-user cap is saturated
)

// New builds a Service.
func New(cfg ServiceConfig) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 5 * time.Second
	}
	s := &Service{
		cfg:      cfg,
		log:      cfg.Log,
		now:      cfg.Now,
		active:   map[string][]*activeRun{},
		pending:  map[string]pendingFire{},
		userRuns: map[string]int{},
	}
	if cfg.MaxConcurrentRuns > 0 {
		s.runSlots = make(chan struct{}, cfg.MaxConcurrentRuns)
	}
	return s
}

// --- CRUD ---

// Create validates + stores a new automation for the owner. A created automation is
// always stored DISABLED (enable is a separate, eligibility-gated action) — schema-
// valid is not the same as safe-to-run.
func (s *Service) Create(ctx context.Context, userID string, def automation.Definition) (store.Automation, error) {
	// Validate the SUBMITTED schema_version (Validate rejects anything but automation.v1) —
	// do NOT overwrite it first, or a missing/stale/future version would be silently masked.
	if verrs := def.Validate(); verrs != nil {
		return store.Automation{}, verrs
	}
	def.Enabled = false // stored disabled regardless of what was submitted
	body, err := json.Marshal(def)
	if err != nil {
		return store.Automation{}, err
	}
	row := store.Automation{
		ID: id.New("atm"), OwnerUserID: userID, Name: def.Name, Trigger: def.Trigger.Type,
		Definition: string(body), Enabled: false,
	}
	if err := s.cfg.Store.CreateAutomation(ctx, row); err != nil {
		return store.Automation{}, err
	}
	return s.cfg.Store.GetAutomation(ctx, row.ID, userID)
}

// Update replaces a user's automation definition. Updating always disables it (the
// user must re-enable after reviewing changes), and reschedules.
func (s *Service) Update(ctx context.Context, userID, autoID string, def automation.Definition) (store.Automation, error) {
	existing, err := s.cfg.Store.GetAutomation(ctx, autoID, userID)
	if err != nil {
		return store.Automation{}, err
	}
	// Validate the SUBMITTED schema_version before mutating — never mask a wrong version.
	if verrs := def.Validate(); verrs != nil {
		return store.Automation{}, verrs
	}
	def.Enabled = false
	body, _ := json.Marshal(def)
	existing.Name = def.Name
	existing.Trigger = def.Trigger.Type
	existing.Definition = string(body)
	existing.Enabled = false
	existing.NextRunAt = time.Time{}
	if err := s.cfg.Store.UpdateAutomation(ctx, existing); err != nil {
		return store.Automation{}, err
	}
	s.cancelAutomation(autoID) // a definition change cancels any in-flight run
	return s.cfg.Store.GetAutomation(ctx, autoID, userID)
}

// SetEnabled enables/disables an automation. Enabling runs the eligibility gate
// (agents/secrets/network) and computes the first next-run; disabling cancels any
// in-flight run.
func (s *Service) SetEnabled(ctx context.Context, userID, autoID string, enabled bool) (store.Automation, error) {
	row, err := s.cfg.Store.GetAutomation(ctx, autoID, userID)
	if err != nil {
		return store.Automation{}, err
	}
	def, perr := automation.Parse([]byte(row.Definition))
	if perr != nil {
		return store.Automation{}, fmt.Errorf("stored definition is invalid: %w", perr)
	}
	var next time.Time
	if enabled {
		// Admission cap: bound how many automations a user may have enabled at once. Only when
		// flipping a currently-DISABLED automation on (re-affirming an already-enabled one, or
		// an idempotent enable, must not be blocked by its own count).
		if s.cfg.MaxEnabledPerUser > 0 && !row.Enabled {
			n, cerr := s.cfg.Store.CountEnabledByUser(ctx, userID)
			if cerr != nil {
				return store.Automation{}, fmt.Errorf("could not check the enabled-automation limit")
			}
			if n >= s.cfg.MaxEnabledPerUser {
				return store.Automation{}, fmt.Errorf("you have %d automations enabled (the limit is %d); disable one before enabling another", n, s.cfg.MaxEnabledPerUser)
			}
		}
		if verrs := def.Validate(); verrs != nil {
			return store.Automation{}, verrs
		}
		if verrs, eerr := s.checkEligibility(ctx, userID, def); eerr != nil {
			return store.Automation{}, eerr
		} else if verrs != nil {
			return store.Automation{}, verrs
		}
		n, nerr := def.NextAfterNow(time.Time{}, s.now())
		if nerr != nil {
			return store.Automation{}, nerr
		}
		next = n
	}
	// Flip ONLY enabled + its schedule via a generation-guarded partial update — NOT a
	// full-row write — so a concurrent definition Update can't be clobbered by this stale row,
	// and we never enable a definition that changed since it was read + validated above. If the
	// generation moved (a concurrent edit), this returns a conflict the caller should retry.
	// For a FRESH enable, also pass the cap so the admission is enforced ATOMICALLY in the same
	// UPDATE (the pre-check above is a friendly fast path; this closes the count-then-update race
	// where concurrent enables of different rows could all commit past the cap).
	capArg := 0
	if enabled && !row.Enabled {
		capArg = s.cfg.MaxEnabledPerUser
	}
	ok, err := s.cfg.Store.SetAutomationEnabled(ctx, autoID, userID, enabled, next, row.Gen, capArg)
	if err != nil {
		return store.Automation{}, err
	}
	if !ok {
		if capArg > 0 {
			return store.Automation{}, errors.New("could not enable: the automation changed, or you reached the enabled-automation limit — reload and try again")
		}
		return store.Automation{}, errors.New("the automation changed while you were enabling it; reload and try again")
	}
	// Cancel AFTER the durable disable commits: once enabled=0 is persisted, ClaimDueRun
	// (WHERE enabled=1) can no longer win a new fire, so cancelling here catches any run
	// that started before the commit without leaving a window for a new one to escape.
	if !enabled {
		s.cancelAutomation(autoID)
	}
	return s.cfg.Store.GetAutomation(ctx, autoID, userID)
}

// Get returns one automation (owner-scoped).
func (s *Service) Get(ctx context.Context, userID, autoID string) (store.Automation, error) {
	return s.cfg.Store.GetAutomation(ctx, autoID, userID)
}

// List returns a user's automations.
func (s *Service) List(ctx context.Context, userID string) ([]store.Automation, error) {
	return s.cfg.Store.ListAutomationsByUser(ctx, userID)
}

// Delete SOFT-deletes a user's automation: it marks the row deleted+disabled FIRST (so a
// scheduler tick can no longer claim it, and it vanishes from the owner's views), THEN
// cancels any in-flight run. The immutable run/artifact records — the only durable audit
// of the automation's side effects (deterministic tool steps run directly via sbx, not the
// interactive sandbox_runs path) — are RETAINED, not cascade-deleted, and the cancelled
// in-flight run still finalizes against its surviving run row.
func (s *Service) Delete(ctx context.Context, userID, autoID string) error {
	if err := s.cfg.Store.SoftDeleteAutomation(ctx, autoID, userID); err != nil {
		return err
	}
	s.cancelAutomation(autoID)
	return nil
}

// checkEligibility assembles the runtime facts and runs the definition's eligibility
// check.
func (s *Service) checkEligibility(ctx context.Context, userID string, def automation.Definition) (*automation.ValidationErrors, error) {
	if s.cfg.Eligibility == nil {
		return nil, nil
	}
	elig, err := s.cfg.Eligibility(ctx, userID)
	if err != nil {
		return nil, err
	}
	return def.CheckEligibility(elig), nil
}

// --- runs / history ---

// ListRuns returns an automation's run history (owner-scoped).
func (s *Service) ListRuns(ctx context.Context, userID, autoID string, limit int) ([]store.AutomationRun, error) {
	if _, err := s.cfg.Store.GetAutomation(ctx, autoID, userID); err != nil {
		return nil, err
	}
	return s.cfg.Store.ListAutomationRuns(ctx, autoID, userID, limit)
}

// GetRun returns one run record (owner-scoped).
func (s *Service) GetRun(ctx context.Context, userID, runID string) (store.AutomationRun, error) {
	return s.cfg.Store.GetAutomationRun(ctx, runID, userID)
}

// ListArtifacts returns a run's promoted artifacts.
func (s *Service) ListArtifacts(ctx context.Context, userID, runID string) ([]store.AutomationArtifact, error) {
	if _, err := s.cfg.Store.GetAutomationRun(ctx, runID, userID); err != nil {
		return nil, err
	}
	return s.cfg.Store.ListAutomationArtifacts(ctx, runID)
}

// ReadArtifact returns an artifact's metadata + content bytes (owner-scoped). The
// path is validated to stay inside the artifact root (defense in depth).
func (s *Service) ReadArtifact(ctx context.Context, userID, artifactID string) (store.AutomationArtifact, []byte, error) {
	art, err := s.cfg.Store.GetAutomationArtifact(ctx, artifactID, userID)
	if err != nil {
		return store.AutomationArtifact{}, nil, err
	}
	if !s.withinArtifactRoot(art.Path) {
		return store.AutomationArtifact{}, nil, errors.New("artifact path is outside the artifact root")
	}
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return store.AutomationArtifact{}, nil, err
	}
	return art, data, nil
}

// TriggerNow starts a manual run of an automation immediately (owner-scoped),
// returning the run id. It respects the cancel_previous concurrency policy but
// otherwise always starts a new run.
func (s *Service) TriggerNow(ctx context.Context, userID, autoID string) (string, error) {
	row, err := s.cfg.Store.GetAutomation(ctx, autoID, userID)
	if err != nil {
		return "", err
	}
	// Honor the review-before-enable gate: a created/updated automation is stored
	// DISABLED until the owner reviews + enables it. A manual run of a still-disabled
	// draft would bypass that gate, so refuse it — enable (review) it first.
	if !row.Enabled {
		return "", errors.New("enable the automation before running it (review the draft first)")
	}
	def, perr := automation.Parse([]byte(row.Definition))
	if perr != nil {
		return "", fmt.Errorf("stored definition is invalid: %w", perr)
	}
	// A manual run still requires the run to be eligible (agents/secrets/network).
	if verrs, eerr := s.checkEligibility(ctx, userID, def); eerr != nil {
		return "", eerr
	} else if verrs != nil {
		return "", verrs
	}
	// A manual trigger only ever STARTS a run now or is refused — it never durably queues
	// (no run record exists until execute, so a queued manual run could be lost on a crash
	// with the user already told it succeeded). Scheduled cancel_previous/queue_one still
	// queue durably (drained by reconcile); manual cancel_previous refuses without cancelling.
	runID := id.New("arn")
	// Add to the wait group BEFORE admission (atomic w.r.t. Stop, same reasoning as fireWithID);
	// every path that doesn't hand the balance to execute below calls wg.Done().
	s.wg.Add(1)
	if s.beginRun(autoID, row.OwnerUserID, def.Concurrency, runID, true, row.Gen) != beginStarted {
		s.wg.Done()
		return "", errors.New("a run is already in progress, or the run-concurrency limit is reached — try again shortly")
	}
	// Insert the durable run row SYNCHRONOUSLY before returning, so the id we hand back is
	// always pollable — never a 202 for a run with no record. If the insert fails, release
	// the slot and return an error rather than an orphaned id.
	started := s.now().UTC()
	if err := withWriteCtx(func(ctx context.Context) error {
		return s.cfg.Store.InsertAutomationRun(ctx, store.AutomationRun{
			ID: runID, AutomationID: autoID, OwnerUserID: row.OwnerUserID,
			Status: automation.RunRunning, Trigger: automation.TriggerManual, StartedAt: started,
		})
	}); err != nil {
		s.endRun(autoID, runID)
		s.wg.Done()
		return "", fmt.Errorf("could not record the run; not started")
	}
	// A manual run does NOT consume a scheduled pending_fire (a parked retry occurrence must
	// survive an unrelated manual trigger) — consumePending=false. execute's deferred wg.Done()
	// balances the speculative add above.
	go s.execute(autoID, row.OwnerUserID, def, automation.TriggerManual, runID, true, "", row.Gen)
	return runID, nil
}

// --- run execution ---

// execute runs one automation end to end: insert the run row, run the engine in an
// ephemeral workspace, persist artifacts + the immutable record, and clear the
// in-flight marker. It uses the scheduler's stop context so a shutdown cancels it.
// recordExists is true when a durable run row was ALREADY inserted before execute (a
// manual trigger inserts it synchronously so its returned id is immediately pollable);
// scheduled fires insert it here. Either way every exit path finalizes that row.
// pendingToken is non-empty ONLY for a run launched to drain a durable pending_fire (a
// tick/reconcile/startPending drain) — it names the EXACT queued occurrence this run
// consumes, claimed compare-and-clear so an unrelated/newer occurrence can't be erased.
func (s *Service) execute(autoID, userID string, def automation.Definition, trigger, runID string, recordExists bool, pendingToken string, claimGen int64) {
	defer s.wg.Done()
	defer s.endRun(autoID, runID)

	baseCtx := context.Background()
	s.mu.Lock()
	if s.stopCtx != nil {
		baseCtx = s.stopCtx
	}
	s.mu.Unlock()
	runCtx, cancel := context.WithCancel(baseCtx)
	s.setRunCancel(autoID, runID, cancel)
	defer cancel()
	started := s.now().UTC()

	// Re-verify the automation is still present AND still enabled right before any side
	// effect — for EVERY trigger including manual. This closes the disable/update/delete-
	// after-claim race: a row snapshotted by the tick, a manual trigger, or a queued
	// replacement may have been disabled/updated/deleted since (Update always disables),
	// and the review-before-enable gate must hold even for a manual run.
	cur, ferr := withWriteAutomation(func(ctx context.Context) (store.Automation, error) {
		return s.cfg.Store.GetAutomationByID(ctx, autoID)
	})
	if ferr != nil {
		gone := errors.Is(ferr, store.ErrNotFound)
		msg := "automation no longer exists"
		if !gone {
			msg = "could not load the automation before the run: " + ferr.Error()
		}
		// A pre-inserted (manual) row must be finalized, not orphaned.
		if recordExists {
			s.finalizeRun(runID, started, automation.RunCancelled, msg)
			s.log.Info("autorun: "+msg+"; skipping run", "automation", autoID)
			return
		}
		// Fresh scheduled fire (no record): if the row is genuinely GONE (deleted), dropping the
		// occurrence is correct. But a TRANSIENT read failure must NOT silently lose the occurrence
		// ClaimDueRun already advanced next_run past — stamp a generation-guarded pending token so
		// the tick retries it (matching the insert/eligibility failure paths). A pending-DRAIN fire
		// keeps its token set, so it is already retried.
		if pendingToken == "" && !gone {
			if _, perr := s.persistPendingDurable(autoID, id.New("pf"), claimGen); perr != nil {
				s.log.Warn("autorun: could not mark a fresh fire for retry after a transient read failure; the occurrence is lost", "automation", autoID, "err", perr)
			}
		}
		s.log.Info("autorun: "+msg+"; skipping run", "automation", autoID)
		return
	}
	if !cur.Enabled {
		if recordExists {
			s.finalizeRun(runID, started, automation.RunCancelled, "automation disabled/updated before the run started")
		}
		s.log.Info("autorun: automation disabled/updated before the run started; skipping", "automation", autoID)
		return
	}
	// GENERATION guard: gen is a monotonic counter bumped ONLY on a user edit (Create/Update/
	// SetEnabled/Delete), never on the scheduler's next_run advance — and unlike a wall-clock
	// updated_at it can't collide on same-instant edits. If it differs from the generation
	// captured when this fire was claimed, the automation was edited or disabled-then-re-enabled
	// in the window — the claimed occurrence is STALE and must NOT run the now-current
	// definition off its reviewed trigger path. (Pending drains are additionally token-guarded.)
	// Every real automation has a positive generation, so this compares exactly — no bypass.
	if cur.Gen != claimGen {
		if recordExists {
			s.finalizeRun(runID, started, automation.RunCancelled, "automation changed since the fire was claimed; skipping")
		}
		s.log.Info("autorun: automation changed since the fire was claimed; skipping the stale occurrence", "automation", autoID)
		return
	}
	// Run the CURRENT stored definition (== the claimed one, since the generation matches).
	if curDef, perr := automation.Parse([]byte(cur.Definition)); perr == nil {
		def = curDef
	}

	// Re-run the FULL eligibility check right before any side effect. The enable-time
	// (and manual-trigger-time) check can go stale: a referenced secret may have been
	// deleted/renamed, an agent de-authenticated, or the network/agents gate flipped.
	// A run must NOT fire under a now-invalid safety posture — fail closed, recording a
	// failed run for visibility, before inserting the running row or creating a workspace.
	if verrs, eerr := s.recheckEligibility(userID, def); eerr != nil || verrs != nil {
		msg := "automation is no longer eligible to run"
		if verrs != nil {
			msg = "no longer eligible: " + verrs.Error()
		} else if eerr != nil {
			msg = "eligibility check failed: " + eerr.Error()
		}
		if recordExists {
			s.finalizeRun(runID, started, automation.RunFailed, msg)
		} else if rerr := s.recordTerminalRun(runID, autoID, userID, trigger, started, automation.RunFailed, msg); rerr != nil {
			// The terminal record didn't persist. A pending-DRAIN fire's token stays set, so the
			// tick re-drains it; a FRESH fire (pendingToken empty) has no marker and ClaimDueRun
			// already advanced next_run, so stamp a generation-guarded pending token (same as the
			// main-insert failure path) — else the occurrence would be silently lost.
			if pendingToken == "" {
				if _, perr := s.persistPendingDurable(autoID, id.New("pf"), claimGen); perr != nil {
					s.log.Warn("autorun: could not mark an ineligible fresh fire for retry; the occurrence is lost", "automation", autoID, "err", perr)
				}
			}
			s.log.Error("autorun: could not record the ineligible run; leaving it pending for retry", "automation", autoID, "err", rerr)
			return
		}
		// A pending-draining run consumed this (ineligible) occurrence into a durable failed
		// record -> consume its token so it isn't retried forever (best-effort: no side
		// effects ran, so a transient failure here only risks a duplicate failed row).
		if pendingToken != "" {
			_, _ = s.claimPending(autoID, pendingToken)
		}
		s.log.Warn("autorun: automation no longer eligible; skipping run", "automation", autoID, "reason", msg)
		return
	}

	// Fail closed: a side-effecting run MUST have a durable record. A manual trigger
	// already inserted the row synchronously (so its returned id is pollable); a scheduled
	// fire inserts it here. If the insert can't commit, abort BEFORE any workspace/run.
	if !recordExists {
		if err := withWriteCtx(func(ctx context.Context) error {
			return s.cfg.Store.InsertAutomationRun(ctx, store.AutomationRun{
				ID: runID, AutomationID: autoID, OwnerUserID: userID,
				Status: automation.RunRunning, Trigger: trigger, StartedAt: started,
			})
		}); err != nil {
			// A pending-DRAIN fire (pendingToken != "") already has a durable token that stays
			// set -> the tick re-drains it. A FRESH scheduled fire has none, and the tick already
			// advanced next_run via ClaimDueRun, so without a marker this occurrence would be
			// silently LOST. Stamp a generation-guarded pending token so the tick retries it (an
			// edit since the claim invalidates the stamp, dropping a now-stale occurrence).
			if pendingToken == "" {
				if _, perr := s.persistPendingDurable(autoID, id.New("pf"), claimGen); perr != nil {
					s.log.Warn("autorun: could not mark a fresh fire for retry after a failed insert; the occurrence is lost", "automation", autoID, "err", perr)
				}
			}
			s.log.Error("autorun: could not record the run; refusing to run it (no durable record)", "automation", autoID, "err", err)
			return
		}
	}
	// A pending-draining run must atomically CLAIM its exact occurrence token BEFORE any
	// side effect (done after the insert so a failed insert above leaves the token for
	// retry). The claim by-token means: a transient claim FAILURE aborts (so a retry can't
	// replay external side effects); a LOST claim means this occurrence was invalidated
	// (update/disable cleared the token) or superseded by a newer queued occurrence (a
	// different token) — either way this run must not perform side effects.
	if pendingToken != "" {
		claimed, err := s.claimPending(autoID, pendingToken)
		if err != nil {
			s.log.Error("autorun: could not claim the pending marker; deferring without side effects", "automation", autoID, "err", err)
			s.finalizeRun(runID, started, automation.RunCancelled, "could not claim pending marker before running; will retry")
			return
		}
		if !claimed {
			s.log.Info("autorun: queued occurrence superseded/invalidated before running; skipping", "automation", autoID)
			s.finalizeRun(runID, started, automation.RunCancelled, "queued occurrence was superseded or invalidated")
			return
		}
	}

	var scratchDir, agentStateRoot string
	var watched []sandbox.Scratch
	var wsExceeded atomic.Bool
	if s.cfg.Workspaces != nil {
		sc, serr := s.cfg.Workspaces.NewScratch()
		if serr != nil {
			// Fail closed: without the run's OWN ephemeral scratch, a deterministic tool
			// would run with no workspace and an agent_cli step would fall back to the
			// user's durable workspace. Finalize the run as failed instead of executing.
			s.log.Error("autorun: could not create the run workspace; failing the run", "automation", autoID, "err", serr)
			s.finalizeFailed(runID, started, "could not create the run workspace")
			return
		}
		scratchDir = sc.Dir
		defer sc.Remove()
		watched = []sandbox.Scratch{sc}
		// A SEPARATE per-run scratch for agent_cli credential/staging dirs: it is NOT mounted
		// at /workspace (so a sibling/parallel step can't read another step's agent credential
		// store), but it IS watched alongside the run scratch so an agent's RW state dir counts
		// toward the per-run disk cap instead of escaping it (only MinFreeBytes would otherwise
		// catch it). If it can't be created, agent state falls back to a sibling scratch (still
		// bounded by the filesystem free-space floor) — degrade, don't fail the run.
		if asc, aerr := s.cfg.Workspaces.NewScratch(); aerr == nil {
			agentStateRoot = asc.Dir
			defer asc.Remove()
			watched = append(watched, asc)
		} else {
			s.log.Warn("autorun: could not create the agent-state scratch; agent disk use bounded only by the free-space floor", "automation", autoID, "err", aerr)
		}
		// CPU/memory/PID/timeout limits don't bound DISK: a write-heavy shell.exec/agent_cli
		// could fill the host. Watch the run scratch(es) and KILL the run (cancel its context)
		// before they exceed the per-run workspace cap OR drive the scratch filesystem below
		// its free-space floor (the latter catches open-but-unlinked files a du-walk misses).
		if s.cfg.MaxWorkspaceBytes > 0 || s.cfg.MinFreeBytes > 0 {
			go s.watchWorkspace(runCtx, watched, cancel, &wsExceeded)
		}
	}

	exec := &runExecutor{cfg: s.cfg.Exec, workspace: scratchDir, agentStateRoot: agentStateRoot}
	rec := automation.Run(runCtx, def, automation.RunOptions{
		RunID: runID, AutomationID: autoID, UserID: userID, Trigger: trigger,
		Caps: s.cfg.Caps, Now: s.now,
	}, exec)
	// Final SYNCHRONOUS disk check: a fast step can write past the cap and exit BEFORE the
	// watchdog's first/next poll, leaving wsExceeded false — catch it here before finalizing as
	// success (also fails closed if a scratch became unmeasurable). The watchdog catches an
	// ONGOING overrun mid-run; this catches a completed-but-over-cap one.
	if !wsExceeded.Load() && len(watched) > 0 {
		if over, _ := s.scratchOverLimit(watched); over {
			wsExceeded.Store(true)
		}
	}
	if wsExceeded.Load() {
		// The watchdog (or the final check) killed/failed the run for exceeding the workspace
		// disk cap / driving the scratch filesystem below its free-space floor — report that as
		// the terminal reason rather than the resulting context-cancellation error.
		rec.Status = automation.RunFailed
		rec.Error = "run killed: exceeded the per-run workspace disk cap or the host scratch filesystem ran low on space"
		if rec.FinishedAt.IsZero() {
			rec.FinishedAt = s.now().UTC()
		}
	}

	if failed := s.persistArtifacts(autoID, runID, rec.Artifacts); len(failed) > 0 {
		// An artifact the run promised wasn't stored — don't finish "success" while the record
		// names an un-downloadable output. Drop the failed artifacts from the record (so it
		// matches what's actually retrievable), record an explicit error, and fail a run that
		// would otherwise have succeeded (the promised output is incomplete).
		rec.Artifacts = dropArtifacts(rec.Artifacts, failed)
		note := fmt.Sprintf("%d artifact(s) failed to persist: %s", len(failed), strings.Join(failed, ", "))
		if rec.Error == "" {
			rec.Error = note
		} else {
			rec.Error += "; " + note
		}
		if rec.Status == automation.RunSuccess {
			rec.Status = automation.RunFailed
		}
	}
	recJSON, _ := json.Marshal(rec)
	if err := withWriteCtx(func(ctx context.Context) error {
		return s.cfg.Store.FinalizeAutomationRun(ctx, store.AutomationRun{
			ID: runID, Status: rec.Status, Error: rec.Error, Record: string(recJSON), FinishedAt: rec.FinishedAt,
		})
	}); err != nil {
		s.log.Error("autorun: failed to finalize run record", "run", runID, "err", err)
	}
	s.log.Info("autorun: run finished", "automation", autoID, "run", runID, "status", rec.Status,
		"model_calls", rec.ModelCalls, "agent_runs", rec.AgentRuns)
}

// recheckEligibility re-runs the enable-time eligibility check just before a run, under
// a detached context (decoupled from a possibly-cancelled run context).
func (s *Service) recheckEligibility(userID string, def automation.Definition) (*automation.ValidationErrors, error) {
	if s.cfg.Eligibility == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	elig, err := s.cfg.Eligibility(ctx, userID)
	if err != nil {
		return nil, err
	}
	return def.CheckEligibility(elig), nil
}

// recordTerminalRun inserts a finished run row (a run that was skipped/failed before it
// could execute), so the history shows WHY it didn't run. It returns the insert error so
// a caller consuming a pending fire only clears the retry marker once the record is durable.
func (s *Service) recordTerminalRun(runID, autoID, userID, trigger string, started time.Time, status, msg string) error {
	finished := s.now().UTC()
	rec := automation.RunRecord{Status: status, Trigger: trigger, Error: msg, StartedAt: started, FinishedAt: finished}
	recJSON, _ := json.Marshal(rec)
	return withWriteCtx(func(ctx context.Context) error {
		return s.cfg.Store.InsertAutomationRun(ctx, store.AutomationRun{
			ID: runID, AutomationID: autoID, OwnerUserID: userID, Status: status,
			Trigger: trigger, Error: msg, Record: string(recJSON), StartedAt: started, FinishedAt: finished,
		})
	})
}

// scratchOverLimit reports whether the COMBINED watched-scratch usage exceeds the per-run
// disk cap, or the scratch filesystem dropped below the free-space floor. It FAILS CLOSED on
// a size-scan error: a sandboxed process that makes a subtree unreadable to the Hina process
// must not let that scratch silently contribute 0 bytes and dodge the cap. Returns a reason.
func (s *Service) scratchOverLimit(scratches []sandbox.Scratch) (bool, string) {
	if s.cfg.MaxWorkspaceBytes > 0 {
		var total int64
		for _, sc := range scratches {
			n, err := sc.Size()
			if err != nil {
				return true, "could not measure the run scratch size (failing closed)"
			}
			total += n
		}
		if total > s.cfg.MaxWorkspaceBytes {
			return true, "exceeded the per-run workspace disk cap"
		}
	}
	// Authoritative host-disk guard: the scratch FILESYSTEM's free space (via statfs) accounts
	// open-but-unlinked blocks a directory walk can't, so a run that opens + unlinks + keeps
	// writing still trips this. Killing it closes those fds + frees them. A statfs error here
	// is left non-fatal (the dir is Hina-owned, not attacker-controlled like the du walk).
	if s.cfg.MinFreeBytes > 0 {
		if free, err := platform.FreeBytes(scratches[0].Dir); err == nil && free < s.cfg.MinFreeBytes {
			return true, "the host scratch filesystem ran low on free space"
		}
	}
	return false, ""
}

// watchWorkspace cancels the run (setting exceeded) once the COMBINED watched-scratch usage
// grows past MaxWorkspaceBytes, bounding host-disk use for an unattended run. The slice is the
// run workspace PLUS the agent-state root (an agent_cli credential dir the per-run cap would
// otherwise miss). It scans IMMEDIATELY and then on the ticker (so a burst doesn't run cap-free
// until the first poll), and returns when the run context is done. Portable (a directory-walk
// du), not byte-exact — it bounds UNBOUNDED growth, allowing a small overshoot between polls.
func (s *Service) watchWorkspace(ctx context.Context, scratches []sandbox.Scratch, cancel context.CancelFunc, exceeded *atomic.Bool) {
	if len(scratches) == 0 {
		return
	}
	interval := s.cfg.WorkspaceWatchInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	trip := func(reason string) bool {
		exceeded.Store(true)
		s.log.Warn("autorun: killing the run for disk use", "reason", reason)
		cancel()
		return true
	}
	check := func() bool { // full: du-walk (per-run cap) + statfs (free-space floor)
		if over, reason := s.scratchOverLimit(scratches); over {
			return trip(reason)
		}
		return false
	}
	// The statfs free-space guard is CHEAP (no directory walk) and is the cross-tenant backstop,
	// so poll it MUCH more often than the expensive per-run du-walk — this shrinks the window in
	// which a transient write-then-delete could starve the shared scratch filesystem for OTHER
	// tenants before a poll observes it. (A hard per-run quota that closes the sub-poll race
	// entirely is a quota-capable scratch filesystem on the sbx host; see SECURITY.md.)
	fastFreeCheck := func() bool {
		if s.cfg.MinFreeBytes <= 0 {
			return false
		}
		if free, err := platform.FreeBytes(scratches[0].Dir); err == nil && free < s.cfg.MinFreeBytes {
			return trip("the host scratch filesystem ran low on free space")
		}
		return false
	}
	if check() { // an immediate scan, before the first tick
		return
	}
	var fastC <-chan time.Time
	if s.cfg.MinFreeBytes > 0 {
		fast := interval / 4
		if fast < 250*time.Millisecond {
			fast = 250 * time.Millisecond
		}
		if fast > interval {
			fast = interval
		}
		ft := time.NewTicker(fast)
		defer ft.Stop()
		fastC = ft.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if check() {
				return
			}
		case <-fastC:
			if fastFreeCheck() {
				return
			}
		}
	}
}

// claimPending atomically consumes the EXACT pending-fire token a drain is running, so it
// can't erase a newer queued occurrence (different token) or one invalidated by an update
// (cleared token). Returns whether the claim was won + any write error.
func (s *Service) claimPending(autoID, token string) (bool, error) {
	var claimed bool
	err := withWriteCtx(func(ctx context.Context) error {
		c, e := s.cfg.Store.ClaimPendingFire(ctx, autoID, token)
		claimed = c
		return e
	})
	return claimed, err
}

// finalizeRun marks an already-inserted run row with a terminal status + message (used
// for the pre-execution failure paths of a run whose row already exists — e.g. a manual
// run pre-inserted by TriggerNow that turns out disabled/ineligible at execute time).
func (s *Service) finalizeRun(runID string, started time.Time, status, msg string) {
	finished := s.now().UTC()
	rec := automation.RunRecord{Status: status, Error: msg, StartedAt: started, FinishedAt: finished}
	recJSON, _ := json.Marshal(rec)
	_ = withWriteCtx(func(ctx context.Context) error {
		return s.cfg.Store.FinalizeAutomationRun(ctx, store.AutomationRun{
			ID: runID, Status: status, Error: msg, Record: string(recJSON), FinishedAt: finished,
		})
	})
}

// finalizeFailed marks an already-inserted run row as failed (used when the run can't
// even start, e.g. its workspace couldn't be created).
func (s *Service) finalizeFailed(runID string, started time.Time, msg string) {
	rec := automation.RunRecord{Status: automation.RunFailed, Error: msg, StartedAt: started, FinishedAt: s.now().UTC()}
	recJSON, _ := json.Marshal(rec)
	_ = withWriteCtx(func(ctx context.Context) error {
		return s.cfg.Store.FinalizeAutomationRun(ctx, store.AutomationRun{
			ID: runID, Status: automation.RunFailed, Error: msg, Record: string(recJSON), FinishedAt: rec.FinishedAt,
		})
	})
}

// persistArtifacts writes each promoted artifact's (already redacted + capped) content to an
// owner-private file under <ArtifactDir>/<automationID>/<runID>/ and records its metadata.
// Grouping by automation lets Delete remove the whole subtree. If the metadata insert fails,
// the just-written file is removed (no orphan). It returns the names of any artifacts that
// could NOT be persisted (file write or metadata insert failed) so the caller can surface the
// loss in the run record instead of finishing "success" while naming an unstored artifact.
func (s *Service) persistArtifacts(autoID, runID string, artifacts []automation.Artifact) []string {
	if len(artifacts) == 0 || s.cfg.ArtifactDir == "" {
		return nil
	}
	var failed []string
	dir := filepath.Join(s.cfg.ArtifactDir, sanitizeFileName(autoID), sanitizeFileName(runID))
	if err := platform.EnsurePrivateDir(dir); err != nil {
		s.log.Warn("autorun: could not create artifact dir", "err", err)
		for _, a := range artifacts {
			failed = append(failed, a.Name)
		}
		return failed
	}
	for i, a := range artifacts {
		fname := fmt.Sprintf("%03d-%s", i, sanitizeFileName(a.Name))
		path := filepath.Join(dir, fname)
		if err := os.WriteFile(path, a.Content, 0o600); err != nil {
			s.log.Warn("autorun: could not write artifact", "name", a.Name, "err", err)
			failed = append(failed, a.Name)
			continue
		}
		art := store.AutomationArtifact{ID: id.New("art"), RunID: runID, Name: a.Name, StepID: a.StepID, Path: path, Size: a.Size}
		if err := withWriteCtx(func(ctx context.Context) error { return s.cfg.Store.InsertAutomationArtifact(ctx, art) }); err != nil {
			// Don't leave an orphaned file with no metadata pointing at it.
			_ = os.Remove(path)
			s.log.Warn("autorun: could not record artifact; removed the orphan file", "name", a.Name, "err", err)
			failed = append(failed, a.Name)
		}
	}
	return failed
}

// dropArtifacts returns the artifacts whose names are NOT in the failed set, so the run record
// doesn't claim an artifact that isn't actually stored/downloadable.
func dropArtifacts(arts []automation.Artifact, failed []string) []automation.Artifact {
	if len(failed) == 0 {
		return arts
	}
	bad := make(map[string]bool, len(failed))
	for _, n := range failed {
		bad[n] = true
	}
	kept := arts[:0]
	for _, a := range arts {
		if !bad[a.Name] {
			kept = append(kept, a)
		}
	}
	return kept
}

// withinArtifactRoot confirms a stored path is inside the configured artifact root.
func (s *Service) withinArtifactRoot(path string) bool {
	if s.cfg.ArtifactDir == "" {
		return false
	}
	root, err := filepath.Abs(s.cfg.ArtifactDir)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	// Inside the root iff the relative path is neither ".." nor begins with "../"
	// (a leading "..foo" filename is fine — only a path SEGMENT of ".." escapes).
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sanitizeFileName keeps an artifact file name to a safe set (the name is already
// validated on save, but defense in depth before it hits the filesystem).
func sanitizeFileName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "artifact"
	}
	return string(out)
}

// withWriteCtx runs a persistence write under a short-lived context decoupled from a
// (possibly cancelled) run, so a run record / artifact always persists even when the
// run itself was cancelled by shutdown.
func withWriteCtx(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fn(ctx)
}

// withWriteAutomation is withWriteCtx for a store read that must complete even when
// the run context is cancelled.
func withWriteAutomation(fn func(ctx context.Context) (store.Automation, error)) (store.Automation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fn(ctx)
}
