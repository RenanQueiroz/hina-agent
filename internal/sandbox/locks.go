package sandbox

import "sync"

// UserLocker serializes a user's sandbox activity. The raw-tool Router, the
// AgentRouter, and the agent auth broker share ONE instance per server so a tool
// call, an agent run, a SetKey, and a logout for the same user can't race: the
// quota preflight stays meaningful, and agent-state/profile writes are serialized
// (closing the logout-vs-run-persist TOCTOU).
type UserLocker struct{ m sync.Map }

// Lock acquires the per-user mutex and returns its unlock func.
func (u *UserLocker) Lock(userID string) func() {
	v, _ := u.m.LoadOrStore(userID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
