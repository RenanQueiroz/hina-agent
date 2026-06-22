package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Automation is one stored automation.v1 definition plus its generated DB fields.
// Definition is the canonical JSON document (the import/export contract); Name is a
// queryable projection of definition.name; NextRunAt/LastRunAt drive the scheduler.
type Automation struct {
	ID          string
	OwnerUserID string
	Name        string
	Trigger     string // scalar projection of definition.trigger.type (for the list view)
	Definition  string // JSON (automation.v1)
	Enabled     bool
	NextRunAt   time.Time
	LastRunAt   time.Time
	// PendingFire holds a per-queue TOKEN ("" = none) recording that a queue_one/cancel_previous
	// replacement was queued after the schedule already advanced. A drain consumes ONLY its own
	// token (compare-and-clear) so it can't erase a newer queued occurrence; reconcile drains it.
	PendingFire string
	// Gen is a monotonic generation counter bumped on every user-visible transition
	// (create/update/enable/disable/delete). The scheduler uses it as a reliable
	// compare-and-set token to detect that an automation changed between a fire being
	// claimed and run — it never collides like a wall-clock updated_at can.
	Gen       int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AutomationRun is one immutable run record. Record holds the full JSON RunRecord
// (step logs, accounting, final output); the scalar columns are the queryable
// projection used by the history view + the scheduler's stale-run recovery.
type AutomationRun struct {
	ID           string
	AutomationID string
	OwnerUserID  string
	Status       string
	Trigger      string
	Error        string
	Record       string // JSON
	StartedAt    time.Time
	FinishedAt   time.Time
}

// AutomationArtifact is one promoted artifact's metadata; Path points at the
// owner-private file holding the (redacted, capped) content.
type AutomationArtifact struct {
	ID        string
	RunID     string
	Name      string
	StepID    string
	Path      string
	Size      int64
	CreatedAt time.Time
}

// --- automations ---

// CreateAutomation inserts a new automation.
func (s *Store) CreateAutomation(ctx context.Context, a Automation) error {
	now := nowUTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO automations(id, owner_user_id, name, trigger, definition, enabled, next_run_at, last_run_at, pending_fire, gen, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,1,?,?)`,
		a.ID, a.OwnerUserID, a.Name, a.Trigger, a.Definition, boolInt(a.Enabled),
		nullTime(a.NextRunAt), nullTime(a.LastRunAt), a.PendingFire, formatTime(a.CreatedAt), formatTime(now),
	)
	return err
}

// SetAutomationPendingFire durably stamps a queued replacement fire's TOKEN ("" clears),
// so a crash/restart can't lose a fire whose schedule slot already advanced. A new queue
// overwrites the previous token (queue_one keeps only the latest waiting occurrence).
func (s *Store) SetAutomationPendingFire(ctx context.Context, id, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE automations SET pending_fire=? WHERE id=?`, token, id)
	return err
}

// SetPendingFireIfCurrent stamps the token ONLY if the row's generation still EXACTLY
// matches the gen the fire was claimed under, and it's still enabled + not deleted. So a
// capped/queued fire can't be (re-)stamped after a user edit (which bumps gen) changed the
// definition — which would let it drain off-schedule — returning whether it stamped. The
// comparison is always exact: every real automation has a positive generation (created at
// 1, bumped on each edit), so there is no "skip the check" escape hatch.
func (s *Store) SetPendingFireIfCurrent(ctx context.Context, id, token string, gen int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE automations SET pending_fire=? WHERE id=? AND gen=? AND enabled=1 AND deleted=0`, token, id, gen)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimPendingFire atomically clears pending_fire ONLY if it still holds the EXACT token
// the caller is draining, returning whether the caller WON. So a queued occurrence is
// consumed exactly once; a drain can't erase a NEWER occurrence (different token), and an
// update/disable (which clears the token) invalidates a stale already-popped fire — it
// can't launch an edited/re-enabled definition off-schedule.
func (s *Store) ClaimPendingFire(ctx context.Context, id, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE automations SET pending_fire='' WHERE id=? AND pending_fire=?`, id, token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateAutomation replaces a user's automation definition + enabled + schedule.
// Scoped to the owner; ErrNotFound when nothing matched.
func (s *Store) UpdateAutomation(ctx context.Context, a Automation) error {
	// A definition/enable/schedule change always invalidates any durably-queued fire
	// (it was queued for the OLD definition or an enabled state the user just changed) —
	// clear pending_fire in the same transition so reconcile can't drain a stale fire. Bump
	// gen so a fire CLAIMED under the prior generation is detected as stale at run time.
	res, err := s.db.ExecContext(ctx,
		`UPDATE automations SET name=?, trigger=?, definition=?, enabled=?, next_run_at=?, last_run_at=?, pending_fire='', gen=gen+1, updated_at=?
		 WHERE id=? AND owner_user_id=?`,
		a.Name, a.Trigger, a.Definition, boolInt(a.Enabled), nullTime(a.NextRunAt), nullTime(a.LastRunAt),
		formatTime(nowUTC()), a.ID, a.OwnerUserID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAutomationEnabled flips ONLY the enabled flag + its schedule bookkeeping (next_run,
// pending_fire) and bumps gen, WITHOUT rewriting the definition — and only when the row is
// still at expectedGen (and not deleted). So an enable/disable can't clobber a concurrent
// definition update with a stale full-row write, and can't enable a definition that changed
// since it was read+validated. Returns whether the conditional update matched (false = the
// generation moved or the row is gone: the caller should reload + retry).
// When maxEnabled > 0 AND enabled is true, the UPDATE additionally requires that the owner has
// FEWER than maxEnabled OTHER automations enabled — folded into the WHERE so the cap admission is
// ATOMIC with the enable transition (SQLite serializes writers, so concurrent enables can't each
// observe a stale count and all commit past the cap; a separate count-then-update would race).
// The caller passes maxEnabled only for a FRESH enable (a currently-disabled row); a reaffirm or a
// disable passes 0 so it isn't blocked by its own/existing count.
func (s *Store) SetAutomationEnabled(ctx context.Context, id, ownerUserID string, enabled bool, next time.Time, expectedGen int64, maxEnabled int) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if enabled && maxEnabled > 0 {
		res, err = s.db.ExecContext(ctx,
			`UPDATE automations SET enabled=1, next_run_at=?, pending_fire='', gen=gen+1, updated_at=?
			 WHERE id=? AND owner_user_id=? AND deleted=0 AND gen=?
			   AND (SELECT COUNT(*) FROM automations WHERE owner_user_id=? AND enabled=1 AND deleted=0 AND id != ?) < ?`,
			nullTime(next), formatTime(nowUTC()), id, ownerUserID, expectedGen, ownerUserID, id, maxEnabled)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE automations SET enabled=?, next_run_at=?, pending_fire='', gen=gen+1, updated_at=?
			 WHERE id=? AND owner_user_id=? AND deleted=0 AND gen=?`,
			boolInt(enabled), nullTime(next), formatTime(nowUTC()), id, ownerUserID, expectedGen)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetAutomationSchedule updates only the scheduler bookkeeping (next/last run),
// without touching the definition. Used by the scheduler at startup reconcile.
func (s *Store) SetAutomationSchedule(ctx context.Context, id string, next, last time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE automations SET next_run_at=?, last_run_at=? WHERE id=?`,
		nullTime(next), nullTime(last), id,
	)
	return err
}

// ClaimDueRun atomically advances an enabled automation's schedule from expectedNext
// to next (stamping last_run), but ONLY if the row still has expectedNext and is still
// enabled — returning whether the claim won. The scheduler advances the slot via this
// BEFORE launching the run, so a crash/shutdown between claim and launch leaves the
// slot already advanced (the fire is skipped, never re-run as "missed" on restart),
// and a concurrent claimant can't double-fire the same due slot.
func (s *Store) ClaimDueRun(ctx context.Context, id string, expectedNext, next, last time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE automations SET next_run_at=?, last_run_at=? WHERE id=? AND enabled=1 AND next_run_at=?`,
		nullTime(next), nullTime(last), id, nullTime(expectedNext),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetAutomation returns a NON-deleted automation scoped to its owner, or ErrNotFound.
func (s *Store) GetAutomation(ctx context.Context, id, ownerUserID string) (Automation, error) {
	return s.scanAutomation(s.db.QueryRowContext(ctx,
		automationSelect+` WHERE id=? AND owner_user_id=? AND deleted=0`, id, ownerUserID))
}

// SoftDeleteAutomation marks an automation deleted + disabled (keeping its immutable
// run/artifact records as audit) rather than hard-deleting + cascading them away.
func (s *Store) SoftDeleteAutomation(ctx context.Context, id, ownerUserID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE automations SET deleted=1, enabled=0, pending_fire='', next_run_at=NULL, gen=gen+1, updated_at=? WHERE id=? AND owner_user_id=? AND deleted=0`,
		formatTime(nowUTC()), id, ownerUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAutomationByID returns an automation by id without an owner scope (the
// scheduler owns no user session). Callers must not expose it cross-user.
func (s *Store) GetAutomationByID(ctx context.Context, id string) (Automation, error) {
	return s.scanAutomation(s.db.QueryRowContext(ctx, automationSelect+` WHERE id=?`, id))
}

// ListAutomationsByUser returns a user's automations as SUMMARY rows (no definition),
// newest first — so the list view can't scan count × definition bytes.
func (s *Store) ListAutomationsByUser(ctx context.Context, userID string) ([]Automation, error) {
	return s.queryAutomations(ctx, automationSummarySelect+` WHERE owner_user_id=? AND deleted=0 ORDER BY created_at DESC`, true, userID)
}

// ListSchedulableAutomations returns the enabled automations the scheduler might actually need
// to fire — those with a durable pending_fire token OR a non-manual trigger — across all users,
// WITH the full definition. It uses the SCALAR `trigger`/`pending_fire` columns to EXCLUDE manual
// automations (which never fire on a schedule), so the scheduler doesn't reload + parse every
// enabled manual definition on every tick (a normal user enabling many large manual automations
// must not be able to degrade other tenants). A non-manual row that isn't due yet is still
// returned (its next_run_at TEXT can't be compared safely in SQL — RFC3339Nano precision/zone)
// and the scheduler skips it after a cheap parse; a per-user enabled-automation cap bounds that.
func (s *Store) ListSchedulableAutomations(ctx context.Context) ([]Automation, error) {
	return s.queryAutomations(ctx, automationSelect+` WHERE enabled=1 AND deleted=0 AND (pending_fire != '' OR trigger != 'manual') ORDER BY id`, false)
}

// CountEnabledByUser returns how many non-deleted automations a user currently has enabled, for
// the per-user enabled-automation admission cap (so one user can't enable an unbounded number).
func (s *Store) CountEnabledByUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM automations WHERE owner_user_id=? AND enabled=1 AND deleted=0`, userID).Scan(&n)
	return n, err
}

// DeleteAutomation removes a user's automation (cascading its runs + artifacts).
func (s *Store) DeleteAutomation(ctx context.Context, id, ownerUserID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM automations WHERE id=? AND owner_user_id=?`, id, ownerUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const automationSelect = `SELECT id, owner_user_id, name, trigger, definition, enabled, next_run_at, last_run_at, pending_fire, gen, created_at, updated_at FROM automations`

// automationSummarySelect OMITS the (potentially large) definition JSON for the LIST view,
// so listing a user's automations can't materialize count × definition bytes.
const automationSummarySelect = `SELECT id, owner_user_id, name, trigger, enabled, next_run_at, last_run_at, pending_fire, gen, created_at, updated_at FROM automations`

func (s *Store) queryAutomations(ctx context.Context, query string, summary bool, args ...any) ([]Automation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Automation
	for rows.Next() {
		a, err := scanAutomationRow(rows, summary)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

// scanAutomationRow scans a full row, or — when summary is true — a row from
// automationSummarySelect that omits the definition (a.Definition stays "").
func scanAutomationRow(row rowScanner, summary bool) (Automation, error) {
	var (
		a                            Automation
		enabled                      int
		next, last, created, updated sql.NullString
		err                          error
	)
	if summary {
		err = row.Scan(&a.ID, &a.OwnerUserID, &a.Name, &a.Trigger, &enabled, &next, &last, &a.PendingFire, &a.Gen, &created, &updated)
	} else {
		err = row.Scan(&a.ID, &a.OwnerUserID, &a.Name, &a.Trigger, &a.Definition, &enabled, &next, &last, &a.PendingFire, &a.Gen, &created, &updated)
	}
	if err != nil {
		return Automation{}, err
	}
	a.Enabled = enabled != 0
	a.NextRunAt, _ = parseTime(next.String)
	a.LastRunAt, _ = parseTime(last.String)
	a.CreatedAt, _ = parseTime(created.String)
	a.UpdatedAt, _ = parseTime(updated.String)
	return a, nil
}

func (s *Store) scanAutomation(row *sql.Row) (Automation, error) {
	a, err := scanAutomationRow(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return Automation{}, ErrNotFound
	}
	return a, err
}

// --- automation runs ---

// InsertAutomationRun appends a run record (typically as "running", finalized later).
func (s *Store) InsertAutomationRun(ctx context.Context, r AutomationRun) error {
	if r.Record == "" {
		r.Record = "{}"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO automation_runs(id, automation_id, owner_user_id, status, trigger, error, record, started_at, finished_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.AutomationID, r.OwnerUserID, r.Status, r.Trigger, r.Error, r.Record,
		nullTime(r.StartedAt), nullTime(r.FinishedAt),
	)
	return err
}

// FinalizeAutomationRun writes the terminal status + full record for a run.
func (s *Store) FinalizeAutomationRun(ctx context.Context, r AutomationRun) error {
	if r.Record == "" {
		r.Record = "{}"
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE automation_runs SET status=?, error=?, record=?, finished_at=? WHERE id=?`,
		r.Status, r.Error, r.Record, nullTime(r.FinishedAt), r.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAutomationRun returns one run scoped to its owner, or ErrNotFound.
func (s *Store) GetAutomationRun(ctx context.Context, id, ownerUserID string) (AutomationRun, error) {
	var (
		r                 AutomationRun
		started, finished sql.NullString
	)
	err := s.db.QueryRowContext(ctx, automationRunSelect+` WHERE id=? AND owner_user_id=?`, id, ownerUserID).
		Scan(&r.ID, &r.AutomationID, &r.OwnerUserID, &r.Status, &r.Trigger, &r.Error, &r.Record, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationRun{}, ErrNotFound
	}
	if err != nil {
		return AutomationRun{}, err
	}
	r.StartedAt, _ = parseTime(started.String)
	r.FinishedAt, _ = parseTime(finished.String)
	return r, nil
}

// ListAutomationRuns returns an automation's runs (owner-scoped), newest first, capped at
// limit. It returns SUMMARY rows ONLY — the (potentially multi-MiB) record JSON is OMITTED
// from the projection so a history listing can't pull ~limit × max_log_bytes through SQLite
// + the Go heap (an authenticated DoS path). Load the full record via GetAutomationRun.
func (s *Store) ListAutomationRuns(ctx context.Context, automationID, ownerUserID string, limit int) ([]AutomationRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		automationRunSummarySelect+` WHERE automation_id=? AND owner_user_id=? ORDER BY started_at DESC LIMIT ?`,
		automationID, ownerUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutomationRun
	for rows.Next() {
		var (
			r                 AutomationRun
			started, finished sql.NullString
		)
		// No record column scanned -> r.Record stays "" for list rows.
		if err := rows.Scan(&r.ID, &r.AutomationID, &r.OwnerUserID, &r.Status, &r.Trigger, &r.Error, &started, &finished); err != nil {
			return nil, err
		}
		r.StartedAt, _ = parseTime(started.String)
		r.FinishedAt, _ = parseTime(finished.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunningRunsInterrupted finalizes any run still marked "running" (a hard crash
// left it dangling) as cancelled, so the history never shows a perpetually-running
// row after a restart. Called once on startup before the scheduler resumes.
func (s *Store) MarkRunningRunsInterrupted(ctx context.Context, status, errMsg string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE automation_runs SET status=?, error=?, finished_at=? WHERE status='running'`,
		status, errMsg, formatTime(nowUTC()),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

const automationRunSelect = `SELECT id, automation_id, owner_user_id, status, trigger, error, record, started_at, finished_at FROM automation_runs`

// automationRunSummarySelect omits the (large) record column for the history list path.
const automationRunSummarySelect = `SELECT id, automation_id, owner_user_id, status, trigger, error, started_at, finished_at FROM automation_runs`

// --- automation artifacts ---

// InsertAutomationArtifact records one promoted artifact's metadata.
func (s *Store) InsertAutomationArtifact(ctx context.Context, a AutomationArtifact) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO automation_artifacts(id, run_id, name, step_id, path, size, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		a.ID, a.RunID, a.Name, a.StepID, a.Path, a.Size, formatTime(a.CreatedAt),
	)
	return err
}

// ListAutomationArtifacts returns a run's artifacts, oldest first.
func (s *Store) ListAutomationArtifacts(ctx context.Context, runID string) ([]AutomationArtifact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, name, step_id, path, size, created_at FROM automation_artifacts WHERE run_id=? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutomationArtifact
	for rows.Next() {
		var (
			a       AutomationArtifact
			created string
		)
		if err := rows.Scan(&a.ID, &a.RunID, &a.Name, &a.StepID, &a.Path, &a.Size, &created); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = parseTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAutomationArtifact returns one artifact joined to its owning run's owner, so
// the HTTP layer can scope a download to the owner. ErrNotFound when absent.
func (s *Store) GetAutomationArtifact(ctx context.Context, id, ownerUserID string) (AutomationArtifact, error) {
	var (
		a       AutomationArtifact
		created string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT a.id, a.run_id, a.name, a.step_id, a.path, a.size, a.created_at
		 FROM automation_artifacts a JOIN automation_runs r ON a.run_id=r.id
		 WHERE a.id=? AND r.owner_user_id=?`, id, ownerUserID).
		Scan(&a.ID, &a.RunID, &a.Name, &a.StepID, &a.Path, &a.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationArtifact{}, ErrNotFound
	}
	if err != nil {
		return AutomationArtifact{}, err
	}
	a.CreatedAt, _ = parseTime(created)
	return a, nil
}

// --- helpers ---

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullTime maps a zero time to SQL NULL, else RFC3339Nano text.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}
