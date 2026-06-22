package autorun

import (
	"context"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/id"
)

// Start launches the durable scheduler: it finalizes any run a crash left "running",
// reconciles each enabled automation's next-run (applying the missed-run policy for a
// fire missed while the server was down), then ticks until ctx is cancelled. It is
// idempotent — a second call is a no-op.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stopCtx, s.stop = context.WithCancel(ctx)
	stopCtx := s.stopCtx
	s.mu.Unlock()

	// A hard crash can leave run rows marked "running" forever — finalize them so the
	// history is honest after a restart.
	if n, err := withWriteIntCtx(func(c context.Context) (int, error) {
		return s.cfg.Store.MarkRunningRunsInterrupted(c, automation.RunCancelled, "server restarted before the run finished")
	}); err != nil {
		s.log.Warn("autorun: could not reconcile stale runs", "err", err)
	} else if n > 0 {
		s.log.Info("autorun: finalized stale running runs", "count", n)
	}

	s.reconcile(stopCtx)

	s.wg.Add(1)
	go s.loop(stopCtx)
}

// Stop cancels the scheduler loop AND every in-flight run, then waits for them to
// finalize their records (bounded by the run budgets). After Stop returns nothing
// lingers: no goroutines, no unfinalized run rows.
func (s *Service) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	if s.stop != nil {
		s.stop()
	}
	// Cancel every active run so its engine unwinds promptly. Mark canceled too: a run still
	// in the beginRun->setRunCancel window has no cancel func yet, so the flag makes it cancel
	// the moment it binds (mirrors cancelAutomation) — Stop can't leave an admitted run running.
	for _, runs := range s.active {
		for _, r := range runs {
			r.canceled = true
			if r.cancel != nil {
				r.cancel()
			}
		}
	}
	s.started = false
	s.mu.Unlock()

	s.wg.Wait()
}

// loop ticks on the configured interval, firing every due automation.
func (s *Service) loop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// reconcile sets the initial next-run for each enabled automation on startup. A fire
// that was due while the server was down is handled by the missed-run policy: skip
// just schedules the next future fire; run_once fires exactly once now, then schedules.
func (s *Service) reconcile(ctx context.Context) {
	autos, err := s.cfg.Store.ListSchedulableAutomations(ctx)
	if err != nil {
		s.log.Warn("autorun: could not load enabled automations", "err", err)
		return
	}
	now := s.now()
	for _, row := range autos {
		def, perr := automation.Parse([]byte(row.Definition))
		if perr != nil {
			s.log.Warn("autorun: skipping automation with an invalid stored definition", "id", row.ID)
			continue
		}
		// Drain a durably-queued replacement that survived a crash/restart. A scheduled
		// automation re-fires it now (subject to its policy + caps); a manual-trigger one
		// doesn't auto-fire on restart (the user re-triggers), so just clear the stale flag.
		if row.PendingFire != "" {
			if def.Trigger.Type == automation.TriggerManual {
				if cerr := s.cfg.Store.SetAutomationPendingFire(ctx, row.ID, ""); cerr != nil {
					s.log.Warn("autorun: could not clear pending fire", "id", row.ID, "err", cerr)
				}
			} else {
				s.fire(row.ID, row.OwnerUserID, def, row.PendingFire, row.Gen) // drain THIS token
			}
		}
		if def.Trigger.Type == automation.TriggerManual {
			continue
		}
		switch {
		case row.NextRunAt.IsZero():
			s.scheduleNext(ctx, row.ID, def, now, now)
		case row.NextRunAt.After(now):
			// A future fire survived the restart — keep it.
		default:
			// Missed while down. CLAIM the slot (atomically advance next_run) BEFORE firing
			// the opt-in run_once, and only fire if the claim won — so a crash or a failed
			// schedule write can't leave the slot due and re-fire it as missed on the next
			// restart (same advance-before-fire guarantee as the tick).
			next, nerr := def.NextAfterNow(row.NextRunAt, now)
			if nerr != nil {
				s.log.Error("autorun: could not compute next run on reconcile; clearing schedule", "id", row.ID, "err", nerr)
				_ = s.cfg.Store.SetAutomationSchedule(ctx, row.ID, time.Time{}, now)
				continue
			}
			claimed, cerr := s.cfg.Store.ClaimDueRun(ctx, row.ID, row.NextRunAt, next, now)
			if cerr != nil || !claimed {
				continue
			}
			if def.MissedRunPolicy == automation.MissedRunOnce {
				s.fire(row.ID, row.OwnerUserID, def, "", row.Gen) // a fresh missed-run occurrence (not a drain)
			}
		}
	}
}

// tick fires every enabled automation whose next-run is due, then advances its
// schedule (anchored to the scheduled time so cadence doesn't drift).
func (s *Service) tick(ctx context.Context) {
	autos, err := s.cfg.Store.ListSchedulableAutomations(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, row := range autos {
		def, perr := automation.Parse([]byte(row.Definition))
		if perr != nil || def.Trigger.Type == automation.TriggerManual {
			continue
		}
		// Retry a durably-queued replacement that couldn't be admitted earlier (e.g. a
		// startup drain refused under cap pressure) — checked REGARDLESS of next_run, since a
		// pending fire isn't tied to the schedule. Only when no run of this automation is
		// active (a slot may be free); if it's still refused, pending_fire stays set and the
		// next tick retries. execute clears pending_fire once the replacement runs. Skip the
		// scheduled claim this tick so the just-started replacement isn't re-queued.
		if row.PendingFire != "" && s.inflight(row.ID) == 0 {
			s.fire(row.ID, row.OwnerUserID, def, row.PendingFire, row.Gen) // drain THIS token
			continue
		}
		if row.NextRunAt.IsZero() || row.NextRunAt.After(now) {
			continue
		}
		next, err := def.NextAfterNow(row.NextRunAt, now)
		if err != nil {
			// Can't compute the next fire (a pathological stored trigger): clear the slot
			// so it stops re-firing every tick.
			s.log.Error("autorun: could not compute next run; clearing schedule", "id", row.ID, "err", err)
			_ = s.cfg.Store.SetAutomationSchedule(ctx, row.ID, time.Time{}, now)
			continue
		}
		// CLAIM the due slot (advance next_run) BEFORE launching the run, so a crash/
		// shutdown between claim and launch can't leave the slot due and be re-fired as a
		// missed run on restart. Only fire if we won the claim.
		claimed, cerr := s.cfg.Store.ClaimDueRun(ctx, row.ID, row.NextRunAt, next, now)
		if cerr != nil || !claimed {
			continue
		}
		s.fire(row.ID, row.OwnerUserID, def, "", row.Gen) // a fresh scheduled occurrence (not a drain)
	}
}

// scheduleNext computes + persists the next fire (and stamps last-run as now). If the
// next fire can't be computed (a pathological stored trigger that slipped past
// validation), it CLEARS next_run instead of leaving it in the past — otherwise every
// tick would re-fire the automation forever. The automation stays enabled but dormant,
// and the error is logged loudly so the cause is visible.
func (s *Service) scheduleNext(ctx context.Context, autoID string, def automation.Definition, anchor, now time.Time) {
	next, err := def.NextAfterNow(anchor, now)
	if err != nil {
		s.log.Error("autorun: could not compute next run; clearing schedule to stop re-firing", "id", autoID, "err", err)
		if cerr := s.cfg.Store.SetAutomationSchedule(ctx, autoID, time.Time{}, now); cerr != nil {
			s.log.Warn("autorun: could not clear next run", "id", autoID, "err", cerr)
		}
		return
	}
	if err := s.cfg.Store.SetAutomationSchedule(ctx, autoID, next, now); err != nil {
		s.log.Warn("autorun: could not persist next run", "id", autoID, "err", err)
	}
}

// fire starts (or queues) a run. pendingToken is non-empty ONLY for a run launched to
// DRAIN a specific durable pending_fire occurrence (it names the token execute consumes).
// claimGen is the automation row's UpdatedAt when this fire was claimed (the generation
// guard: execute cancels the run if a user edit changed it since).
func (s *Service) fire(autoID, userID string, def automation.Definition, pendingToken string, claimGen int64) {
	s.fireWithID(autoID, userID, def, id.New("arn"), pendingToken, claimGen)
}

// fireWithID starts (or queues) a run under a specific run id — so a queued replacement
// reuses the id that was reported when it was first accepted.
func (s *Service) fireWithID(autoID, userID string, def automation.Definition, runID, pendingToken string, claimGen int64) {
	// Increment the wait group BEFORE admission, so Stop can never observe an admitted active
	// run while the counter is still zero (which would let Stop's wg.Wait return before the run
	// finalizes its record). Every non-started outcome balances this speculative add below; a
	// beginStarted hands the balance to execute's deferred wg.Done().
	s.wg.Add(1)
	outcome := s.beginRun(autoID, userID, def.Concurrency, runID, false, claimGen)
	if outcome == beginStarted {
		go s.execute(autoID, userID, def, def.Trigger.Type, runID, false, pendingToken, claimGen)
		return
	}
	s.wg.Done() // not started now — balance the speculative add
	switch outcome {
	case beginQueued:
		// The durable pending_fire token was already stamped INSIDE beginRun (before the
		// destructive transition); the queued run adds its own wg count when it later starts.
	case beginCapped:
		if pendingToken != "" {
			// This fire is DRAINING an existing durable token that hit cap pressure — its
			// token is already durable (or was cleared by an update, which must NOT be
			// resurrected). Minting a fresh token here would re-stamp a stale occurrence
			// under the current generation and let a later tick run it off-schedule; instead
			// leave the durable token as-is so a later tick re-drains it only if it survives.
			return
		}
		// A FRESH capped fire: the schedule was already advanced (claim-before-fire) but a
		// service/per-user cap is saturated. Stamp a pending-fire token so a later tick retries
		// this occurrence when a slot frees — but ONLY if the row is still at the claimed
		// generation, so a fire claimed under generation A isn't re-stamped (and drained
		// off-schedule) after an edit changed the definition.
		if stamped, err := s.persistPendingDurable(autoID, id.New("pf"), claimGen); err != nil {
			s.log.Warn("autorun: could not mark a capped fire for retry; the occurrence is lost", "id", autoID, "err", err)
		} else if !stamped {
			s.log.Info("autorun: capped fire's automation changed since it was claimed; dropping the stale occurrence", "id", autoID)
		}
	}
}

// persistPendingDurable durably stamps a queued replacement fire's token, but ONLY if the
// row's generation still matches claimGen (and it's enabled + not deleted) — so a fire
// claimed under one generation can't be (re-)stamped after a user edit, which would let it
// drain off-schedule. Returns whether it stamped. Runs under the scheduler lock (in
// beginRun) so the token commits BEFORE the active run is cancelled / the in-memory queue
// is set — a crash in the gap can't strand a replacement.
func (s *Service) persistPendingDurable(autoID, token string, claimGen int64) (bool, error) {
	var stamped bool
	err := withWriteCtx(func(c context.Context) error {
		ok, e := s.cfg.Store.SetPendingFireIfCurrent(c, autoID, token, claimGen)
		stamped = ok
		return e
	})
	return stamped, err
}

// --- concurrency tracking ---

// beginRun decides whether a new run may start under the automation's concurrency
// policy and, if so, registers it as active. The policy applies equally to scheduled
// fires and manual triggers (so a manual run honors skip_if_running too); only the
// not-started guard distinguishes them, letting a manual run proceed before the loop
// has started.
func (s *Service) beginRun(autoID, userID string, conc automation.Concurrency, runID string, manual bool, claimGen int64) beginOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started && !manual {
		return beginRefused
	}
	active := s.active[autoID]
	policy := conc.Policy
	if policy == "" {
		policy = automation.ConcurrencySkip
	}
	switch policy {
	case automation.ConcurrencyParallel:
		max := conc.MaxParallel
		if max < 1 {
			max = 1
		}
		if len(active) >= max {
			return beginRefused
		}
	case automation.ConcurrencyCancel:
		if len(active) > 0 {
			// A MANUAL trigger can't be durably queued (no run record exists until execute,
			// so a crash before the replacement starts would lose a run we already reported
			// success for). Refuse it cleanly WITHOUT cancelling — no side effect behind the
			// response, and the active run keeps going. The user can retry.
			if manual {
				return beginRefused
			}
			// A SCHEDULED fire cancels the active run(s) and QUEUES a durable replacement that
			// starts from endRun once the cancelled run releases its permit. Acquiring a new
			// permit here would dead-lock under a saturated run cap (the only free slot is the
			// one the cancelled run still holds), turning cancel_previous into a destructive
			// cancel-without-replacement.
			//
			// Persist pending_fire BEFORE the destructive in-memory transition, so a crash in
			// the gap can't leave the active run cancelled with only an in-memory replacement
			// that a restart would never drain. If the durable write fails, do NOT cancel.
			token := id.New("pf")
			if stamped, err := s.persistPendingDurable(autoID, token, claimGen); err != nil || !stamped {
				s.log.Warn("autorun: could not durably queue cancel_previous replacement (write failed or generation changed); leaving the active run", "id", autoID, "err", err)
				return beginRefused
			}
			// Mark canceled BEFORE the cancel func is bound so a run still in the
			// beginRun->setRunCancel window is cancelled the moment it binds.
			for _, r := range active {
				r.canceled = true
				if r.cancel != nil {
					r.cancel()
				}
			}
			s.pending[autoID] = pendingFire{userID: userID, runID: runID, token: token, gen: claimGen}
			return beginQueued
		}
		// No active run — fall through to acquire a permit and start now.
	case automation.ConcurrencyQueueOne:
		if len(active) > 0 {
			// Queue at most one waiting scheduled fire; a manual trigger that can't run now is
			// simply refused (no side effect, so the user can safely retry).
			if manual {
				return beginRefused
			}
			// Durably admit the queued fire (stamp its token) before recording it in memory,
			// but only if the row is still at the claimed generation (else the fire is stale).
			token := id.New("pf")
			if stamped, err := s.persistPendingDurable(autoID, token, claimGen); err != nil || !stamped {
				s.log.Warn("autorun: could not durably queue replacement (write failed or generation changed); dropping this fire", "id", autoID, "err", err)
				return beginRefused
			}
			s.pending[autoID] = pendingFire{userID: userID, runID: runID, token: token, gen: claimGen}
			return beginQueued
		}
	default: // skip_if_running
		if len(active) > 0 {
			return beginRefused
		}
	}
	// The per-automation policy has admitted a NEW active run. Only NOW enforce the
	// per-user + service-wide caps — checking them earlier would let a saturated user cap
	// short-circuit cancel_previous/queue_one (the policy must cancel/queue the stale run
	// even when the cap is full, or a scheduled fire whose next_run_at already advanced is
	// silently lost).
	// A cap refusal (vs the per-automation policy above) is distinct: the occurrence SHOULD
	// run but capacity is full, so callers mark it for retry rather than dropping it.
	if s.cfg.MaxRunsPerUser > 0 && s.userRuns[userID] >= s.cfg.MaxRunsPerUser {
		return beginCapped
	}
	if s.runSlots != nil {
		select {
		case s.runSlots <- struct{}{}:
		default:
			return beginCapped
		}
	}
	s.userRuns[userID]++
	s.active[autoID] = append(s.active[autoID], &activeRun{runID: runID, userID: userID})
	return beginStarted
}

// endRun clears an in-flight run, releases its global + per-user permits, and (for
// queue_one) starts a pending fire.
func (s *Service) endRun(autoID, runID string) {
	s.mu.Lock()
	runs := s.active[autoID]
	kept := runs[:0]
	released := false
	for _, r := range runs {
		if r.runID != runID {
			kept = append(kept, r)
			continue
		}
		// Release the permits this run held (exactly once, when we find + drop it).
		if !released {
			released = true
			if s.userRuns[r.userID] > 0 {
				s.userRuns[r.userID]--
				if s.userRuns[r.userID] == 0 {
					delete(s.userRuns, r.userID)
				}
			}
			if s.runSlots != nil {
				select {
				case <-s.runSlots:
				default:
				}
			}
		}
	}
	if len(kept) == 0 {
		delete(s.active, autoID)
	} else {
		s.active[autoID] = kept
	}
	pf, hasPending := s.pending[autoID]
	pending := hasPending && len(s.active[autoID]) == 0 && s.started
	if pending {
		delete(s.pending, autoID)
	}
	s.mu.Unlock()

	if pending {
		s.startPending(autoID, pf)
	}
}

// startPending starts a queued replacement run after the prior one drained, REUSING the
// run id reported when the fire was accepted and passing the occurrence's TOKEN so execute
// claims it compare-and-clear before any side effect — if an update/disable cleared the
// token, or a newer occurrence superseded it, the claim loses and this run performs no
// side effects (it must not launch an edited/re-enabled definition off its trigger path).
func (s *Service) startPending(autoID string, pf pendingFire) {
	row, err := s.cfg.Store.GetAutomationByID(context.Background(), autoID)
	if err != nil {
		return
	}
	def, perr := automation.Parse([]byte(row.Definition))
	if perr != nil {
		return
	}
	s.fireWithID(autoID, pf.userID, def, pf.runID, pf.token, pf.gen)
}

// setRunCancel binds a run's cancel func once its context exists (so Stop / a
// definition change / cancel_previous can interrupt it). If a cancellation was already
// requested in the window before binding, it is honored immediately.
func (s *Service) setRunCancel(autoID, runID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.active[autoID] {
		if r.runID == runID {
			r.cancel = cancel
			if r.canceled {
				cancel() // a cancel arrived before we bound — honor it now
			}
			return
		}
	}
}

// cancelAutomation cancels any in-flight runs of an automation (on disable/update/
// delete) and clears any queued fire. It marks each run canceled so a cancel that
// races ahead of setRunCancel is not lost.
func (s *Service) cancelAutomation(autoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, autoID)
	for _, r := range s.active[autoID] {
		r.canceled = true
		if r.cancel != nil {
			r.cancel()
		}
	}
}

// inflight returns how many runs of an automation are currently active (for tests +
// admin observability).
func (s *Service) inflight(autoID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active[autoID])
}

// pendingFor reports whether a queued (queue_one / cancel_previous replacement) fire is
// waiting for the automation.
func (s *Service) pendingFor(autoID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[autoID]
	return ok
}

// firstRunCanceled reports whether the first active run of an automation is marked
// cancelled (test observability).
func (s *Service) firstRunCanceled(autoID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.active[autoID]
	return len(runs) > 0 && runs[0].canceled
}

// withWriteIntCtx is withWriteCtx for a (int, error) write.
func withWriteIntCtx(fn func(ctx context.Context) (int, error)) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fn(ctx)
}
