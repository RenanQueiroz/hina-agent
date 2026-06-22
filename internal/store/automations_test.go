package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func autoTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.CreateUser(context.Background(), User{ID: "usr_1", Username: "u", Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return st
}

// The generation counter must increment on every user-visible transition and be a reliable
// compare-and-set token for the scheduler's stale-fire guard — a wall-clock updated_at can
// collide on same-instant edits; an incrementing integer can't (round-43).
func TestAutomationGenerationCAS(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	a := Automation{ID: "atm_g", OwnerUserID: "usr_1", Definition: `{"schema_version":"automation.v1"}`, Enabled: true}
	if err := st.CreateAutomation(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := st.GetAutomation(ctx, "atm_g", "usr_1")
	if got.Gen != 1 {
		t.Fatalf("a created automation should start at gen 1, got %d", got.Gen)
	}
	// A stamp at the CURRENT generation wins; the row is now updated.
	if ok, err := st.SetPendingFireIfCurrent(ctx, "atm_g", "tok1", 1); err != nil || !ok {
		t.Fatalf("stamp at current gen = (%v, %v), want (true, nil)", ok, err)
	}
	// A user edit bumps the generation (and clears the pending token).
	a.Enabled = true
	if err := st.UpdateAutomation(ctx, a); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = st.GetAutomation(ctx, "atm_g", "usr_1")
	if got.Gen != 2 || got.PendingFire != "" {
		t.Fatalf("update should bump gen to 2 and clear pending, got gen=%d pending=%q", got.Gen, got.PendingFire)
	}
	// A stamp at the now-STALE generation 1 must be rejected (the core round-43 guard).
	if ok, err := st.SetPendingFireIfCurrent(ctx, "atm_g", "stale", 1); err != nil || ok {
		t.Fatalf("stamp at a stale gen = (%v, %v), want (false, nil)", ok, err)
	}
	// And a soft-delete bumps it again.
	if err := st.SoftDeleteAutomation(ctx, "atm_g", "usr_1"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	got, _ = st.GetAutomationByID(ctx, "atm_g")
	if got.Gen != 3 {
		t.Fatalf("soft delete should bump gen to 3, got %d", got.Gen)
	}
}

// SetAutomationEnabled must be generation-guarded: a stale-generation enable (one racing a
// concurrent edit) matches nothing and leaves the row untouched, and it never rewrites the
// definition — only the enabled flag + schedule (round-60).
func TestSetAutomationEnabledGenerationGuard(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	if err := st.CreateAutomation(ctx, Automation{ID: "atm_e", OwnerUserID: "usr_1", Definition: `{"schema_version":"automation.v1"}`}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetAutomation(ctx, "atm_e", "usr_1")
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	// A STALE generation matches nothing — the enable is a no-op (conflict).
	if ok, err := st.SetAutomationEnabled(ctx, "atm_e", "usr_1", true, future, got.Gen+99, 0); err != nil || ok {
		t.Fatalf("stale-gen enable = (%v, %v), want (false, nil)", ok, err)
	}
	if cur, _ := st.GetAutomation(ctx, "atm_e", "usr_1"); cur.Enabled {
		t.Fatal("a stale-generation enable must not enable the row")
	}
	// The CURRENT generation matches — enables + bumps gen.
	if ok, err := st.SetAutomationEnabled(ctx, "atm_e", "usr_1", true, future, got.Gen, 0); err != nil || !ok {
		t.Fatalf("current-gen enable = (%v, %v), want (true, nil)", ok, err)
	}
	cur, _ := st.GetAutomation(ctx, "atm_e", "usr_1")
	if !cur.Enabled || cur.Gen != got.Gen+1 || cur.NextRunAt.IsZero() {
		t.Fatalf("enable should flip enabled + bump gen + set next, got %+v", cur)
	}
}

// ListSchedulableAutomations must EXCLUDE enabled MANUAL automations (they never fire on a
// schedule, so the scheduler shouldn't reload + parse them every tick) but INCLUDE a manual one
// carrying a pending_fire token, and CountEnabledByUser must count per owner (round-86).
func TestListSchedulableExcludesManual(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	_ = st.CreateUser(ctx, User{ID: "u1", Username: "u1", Role: "user", PasswordHash: "x"})
	_ = st.CreateUser(ctx, User{ID: "u2", Username: "u2", Role: "user", PasswordHash: "x"})
	_ = st.CreateAutomation(ctx, Automation{ID: "man", OwnerUserID: "u1", Trigger: "manual", Definition: "{}", Enabled: true})
	_ = st.CreateAutomation(ctx, Automation{ID: "iv", OwnerUserID: "u1", Trigger: "interval", Definition: "{}", Enabled: true})
	_ = st.CreateAutomation(ctx, Automation{ID: "off", OwnerUserID: "u1", Trigger: "interval", Definition: "{}", Enabled: false})
	_ = st.CreateAutomation(ctx, Automation{ID: "manp", OwnerUserID: "u2", Trigger: "manual", Definition: "{}", Enabled: true})
	_ = st.SetAutomationPendingFire(ctx, "manp", "pf_tok") // a manual row with a pending drain is still schedulable

	got, err := st.ListSchedulableAutomations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	if ids["man"] {
		t.Error("an enabled MANUAL automation (no pending fire) must NOT be schedulable")
	}
	if ids["off"] {
		t.Error("a DISABLED automation must not be schedulable")
	}
	if !ids["iv"] || !ids["manp"] {
		t.Errorf("a non-manual row and a pending-fire manual row must be schedulable, got %v", ids)
	}
	if n, _ := st.CountEnabledByUser(ctx, "u1"); n != 2 { // man + iv enabled; off disabled
		t.Errorf("CountEnabledByUser(u1) = %d, want 2", n)
	}
}

// The enabled-automation cap must be ATOMIC with the enable write: many CONCURRENT enables of
// different disabled automations must never leave the owner above the cap (the count predicate is
// folded into the UPDATE, and SQLite serializes writers). A count-then-update would race (round-87).
func TestSetAutomationEnabledCapAtomic(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	const cap, total = 3, 12
	for i := 0; i < total; i++ {
		if err := st.CreateAutomation(ctx, Automation{ID: fmt.Sprintf("a%02d", i), OwnerUserID: "usr_1", Trigger: "interval", Definition: "{}", Enabled: false}); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour).UTC()
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("a%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, gerr := st.GetAutomation(ctx, id, "usr_1")
			if gerr != nil {
				return
			}
			_, _ = st.SetAutomationEnabled(ctx, id, "usr_1", true, future, got.Gen, cap)
		}()
	}
	wg.Wait()
	n, err := st.CountEnabledByUser(ctx, "usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if n > cap { // the safety invariant Codex requires: never exceed the cap
		t.Fatalf("concurrent enables left %d enabled, exceeding the cap %d", n, cap)
	}
	if n != cap { // WAL + busy_timeout serializes writers, so exactly `cap` should win
		t.Fatalf("expected exactly %d enabled (the cap), got %d", cap, n)
	}
}

func TestAutomationCRUD(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	a := Automation{ID: "atm_1", OwnerUserID: "usr_1", Name: "PR review", Definition: `{"schema_version":"automation.v1"}`, Enabled: false}
	if err := st.CreateAutomation(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetAutomation(ctx, "atm_1", "usr_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "PR review" || got.Enabled {
		t.Fatalf("got %+v", got)
	}
	// Owner scoping: another user can't read it.
	if _, err := st.GetAutomation(ctx, "atm_1", "usr_other"); err != ErrNotFound {
		t.Fatalf("cross-user get = %v, want ErrNotFound", err)
	}
	// Enable + schedule.
	a.Enabled = true
	a.NextRunAt = time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	if err := st.UpdateAutomation(ctx, a); err != nil {
		t.Fatalf("update: %v", err)
	}
	enabled, err := st.ListSchedulableAutomations(ctx)
	if err != nil || len(enabled) != 1 {
		t.Fatalf("schedulable list = %v (%d)", err, len(enabled))
	}
	if enabled[0].NextRunAt.IsZero() {
		t.Error("next_run_at should round-trip")
	}
	// Delete.
	if err := st.DeleteAutomation(ctx, "atm_1", "usr_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetAutomation(ctx, "atm_1", "usr_1"); err != ErrNotFound {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestAutomationRunsAndArtifacts(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	_ = st.CreateAutomation(ctx, Automation{ID: "atm_1", OwnerUserID: "usr_1", Name: "x", Definition: "{}"})

	run := AutomationRun{ID: "arn_1", AutomationID: "atm_1", OwnerUserID: "usr_1", Status: "running", Trigger: "manual", StartedAt: time.Now().UTC()}
	if err := st.InsertAutomationRun(ctx, run); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	run.Status = "success"
	run.Record = `{"status":"success","steps":[]}`
	run.FinishedAt = time.Now().UTC()
	if err := st.FinalizeAutomationRun(ctx, run); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	runs, err := st.ListAutomationRuns(ctx, "atm_1", "usr_1", 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "success" {
		t.Fatalf("list runs = %v %+v", err, runs)
	}

	art := AutomationArtifact{ID: "art_1", RunID: "arn_1", Name: "final-review.md", StepID: "combine", Path: "/tmp/x", Size: 42}
	if err := st.InsertAutomationArtifact(ctx, art); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	got, err := st.GetAutomationArtifact(ctx, "art_1", "usr_1")
	if err != nil || got.Size != 42 {
		t.Fatalf("get artifact = %v %+v", err, got)
	}
	// Owner scoping on the artifact (joined through the run).
	if _, err := st.GetAutomationArtifact(ctx, "art_1", "usr_other"); err != ErrNotFound {
		t.Fatalf("cross-user artifact = %v", err)
	}
}

// The automation LIST path must omit the (potentially large) definition column, while Get
// still loads it — so listing can't materialize count × definition bytes (round-34 finding).
func TestListAutomationsByUserOmitsDefinition(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	bigDef := `{"big":"` + strings.Repeat("x", 1<<20) + `"}` // ~1 MiB
	_ = st.CreateAutomation(ctx, Automation{ID: "atm_1", OwnerUserID: "usr_1", Name: "x", Trigger: "manual", Definition: bigDef})

	list, err := st.ListAutomationsByUser(ctx, "usr_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v %+v", err, list)
	}
	if list[0].Definition != "" {
		t.Fatalf("list must omit the definition column, got %d bytes", len(list[0].Definition))
	}
	if list[0].Trigger != "manual" {
		t.Fatalf("list must carry the trigger scalar, got %q", list[0].Trigger)
	}
	// Get still returns the full definition.
	one, err := st.GetAutomation(ctx, "atm_1", "usr_1")
	if err != nil || one.Definition != bigDef {
		t.Fatalf("get must include the full definition: err=%v lenGot=%d", err, len(one.Definition))
	}
}

// The run-history LIST path must omit the (potentially multi-MiB) record column, while
// the per-run DETAIL fetch still loads it — so listing can't materialize ~limit × records
// (round-33 finding).
func TestListAutomationRunsOmitsRecord(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	_ = st.CreateAutomation(ctx, Automation{ID: "atm_1", OwnerUserID: "usr_1", Name: "x", Definition: "{}"})
	bigRecord := `{"big":"` + strings.Repeat("x", 1<<20) + `"}` // ~1 MiB
	run := AutomationRun{ID: "arn_1", AutomationID: "atm_1", OwnerUserID: "usr_1", Status: "success", Trigger: "manual", Record: bigRecord, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	if err := st.InsertAutomationRun(ctx, run); err != nil {
		t.Fatalf("insert: %v", err)
	}
	runs, err := st.ListAutomationRuns(ctx, "atm_1", "usr_1", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list = %v %+v", err, runs)
	}
	if runs[0].Record != "" {
		t.Fatalf("list must omit the record column, got %d bytes", len(runs[0].Record))
	}
	// The detail fetch still returns the full record.
	one, err := st.GetAutomationRun(ctx, "arn_1", "usr_1")
	if err != nil || one.Record != bigRecord {
		t.Fatalf("detail must include the full record: err=%v lenGot=%d", err, len(one.Record))
	}
}

func TestClaimDueRun(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	next1 := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	if err := st.CreateAutomation(ctx, Automation{ID: "atm_1", OwnerUserID: "usr_1", Definition: "{}", Enabled: true, NextRunAt: next1}); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.GetAutomationByID(ctx, "atm_1")
	next2 := next1.Add(5 * time.Minute)
	// First claim with the current next wins and advances the slot.
	ok, err := st.ClaimDueRun(ctx, "atm_1", cur.NextRunAt, next2, next1)
	if err != nil || !ok {
		t.Fatalf("first claim should win: ok=%v err=%v", ok, err)
	}
	// A second claim with the STALE expected next must lose (the slot already advanced),
	// so a crash-and-retry can't double-fire the same due slot.
	ok2, _ := st.ClaimDueRun(ctx, "atm_1", cur.NextRunAt, next2.Add(time.Minute), next1)
	if ok2 {
		t.Fatal("a stale claim must not win (no double-fire)")
	}
	// A claim on a disabled automation loses too.
	got, _ := st.GetAutomationByID(ctx, "atm_1")
	got.Enabled = false
	_ = st.UpdateAutomation(ctx, got)
	if ok3, _ := st.ClaimDueRun(ctx, "atm_1", got.NextRunAt, next2.Add(time.Hour), next1); ok3 {
		t.Fatal("a claim on a disabled automation must lose")
	}
}

func TestMarkRunningRunsInterrupted(t *testing.T) {
	st := autoTestStore(t)
	ctx := context.Background()
	_ = st.CreateAutomation(ctx, Automation{ID: "atm_1", OwnerUserID: "usr_1", Name: "x", Definition: "{}"})
	_ = st.InsertAutomationRun(ctx, AutomationRun{ID: "arn_1", AutomationID: "atm_1", OwnerUserID: "usr_1", Status: "running", StartedAt: time.Now().UTC()})
	n, err := st.MarkRunningRunsInterrupted(ctx, "cancelled", "server restarted")
	if err != nil || n != 1 {
		t.Fatalf("mark = %d %v", n, err)
	}
	got, _ := st.GetAutomationRun(ctx, "arn_1", "usr_1")
	if got.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}
