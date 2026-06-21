package sandbox

import (
	"context"
	"sync"
)

// RunRegistry tracks in-flight agent runs per (user, provider) so a credential
// revocation (the broker's logout) can cancel any run that would otherwise keep using
// the revoked credential — closing the window where a logout returns success while a
// just-materialized-but-not-yet-launched run still launches with the old credential.
//
// It is shared (like the cred lock) between the AgentRouter, which registers each
// launch UNDER the cred lock right after a final profile re-check, and the broker,
// which cancels under the SAME lock right after deleting the profile/state. Because
// both happen under the cred lock, a concurrent logout either runs first (the run's
// re-check then sees the profile gone and refuses) or second (it cancels the just-
// registered run's context — a pre-cancelled context makes Runner.Run abort before it
// launches anything).
type RunRegistry struct {
	mu   sync.Mutex
	next int
	runs map[string]map[int]runHandle
}

type runHandle struct {
	provider string
	cancel   context.CancelFunc
}

// Add registers an in-flight run's cancel func and returns a release to unregister it.
func (r *RunRegistry) Add(userID, provider string, cancel context.CancelFunc) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[string]map[int]runHandle{}
	}
	id := r.next
	r.next++
	m := r.runs[userID]
	if m == nil {
		m = map[int]runHandle{}
		r.runs[userID] = m
	}
	m[id] = runHandle{provider: provider, cancel: cancel}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if mm := r.runs[userID]; mm != nil {
			delete(mm, id)
			if len(mm) == 0 {
				delete(r.runs, userID)
			}
		}
	}
}

// Cancel cancels every in-flight run for (userID, provider) — used by logout to revoke
// a credential from a run that has it (or is about to launch with it).
func (r *RunRegistry) Cancel(userID, provider string) {
	r.mu.Lock()
	var cancels []context.CancelFunc
	for _, h := range r.runs[userID] {
		if h.provider == provider {
			cancels = append(cancels, h.cancel)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}
