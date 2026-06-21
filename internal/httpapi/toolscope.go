package httpapi

import (
	"context"
	"sync"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/agent"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
)

// toolScopeKey carries the per-turn (user, conversation) scope into the shared
// agent loop, so the single tool hook can attribute a model-requested tool call
// to the right user's sandbox/secrets/audit without a per-turn loop instance.
type toolScopeKey struct{}

func withToolScope(ctx context.Context, sc sandbox.Scope) context.Context {
	return context.WithValue(ctx, toolScopeKey{}, sc)
}

func toolScopeFrom(ctx context.Context) (sandbox.Scope, bool) {
	sc, ok := ctx.Value(toolScopeKey{}).(sandbox.Scope)
	return sc, ok
}

// toolHook is the agent loop's ToolHook: it routes a model-requested tool call to
// the per-user sandbox Router, gated on the sandbox being enabled and the turn
// carrying a scope. It returns the failure inside ToolResult.Err (so the model can
// recover) except for a context cancellation, which propagates as an interrupt.
func (s *Server) toolHook(ctx context.Context, call agent.ToolCall) (agent.ToolResult, error) {
	scope, ok := toolScopeFrom(ctx)
	if !ok || scope.UserID == "" {
		return agent.ToolResult{Err: "tool execution has no user scope"}, nil
	}
	// A callable-agent run (agent.<provider>.run) routes to the AgentRouter; every
	// other tool (shell/file/HTTP) routes to the raw sandbox Router.
	if sandbox.Handles(call.Name) {
		if s.agentRouter == nil {
			return agent.ToolResult{Err: "callable agents are disabled on this server"}, nil
		}
		res, err := s.agentRouter.Handle(ctx, scope, sandbox.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		if err != nil {
			return agent.ToolResult{}, err
		}
		return agent.ToolResult{Content: res.Content, Err: res.Err}, nil
	}
	if !s.cfg.Sandbox.Enabled || s.router == nil {
		return agent.ToolResult{Err: "sandbox tool execution is disabled on this server"}, nil
	}
	res, err := s.router.Handle(ctx, scope, sandbox.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	if err != nil {
		return agent.ToolResult{}, err // ctx cancellation -> interrupt, preserve partial
	}
	return agent.ToolResult{Content: res.Content, Err: res.Err}, nil
}

// approvalRegistry is the bus-event + HTTP-decision approver: the Router emits a
// ToolCallRequested event carrying a call id, and Approve blocks until the owning
// user POSTs a decision (or the timeout denies it). It implements sandbox.Approver.
type approvalRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
	timeout time.Duration
}

type pendingApproval struct {
	ch     chan bool
	convID string // the decision must come from this conversation's owner
}

func newApprovalRegistry(timeout time.Duration) *approvalRegistry {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &approvalRegistry{pending: make(map[string]*pendingApproval), timeout: timeout}
}

// Approve registers the pending call, signals onRegistered (the Router emits the
// request event then — so a fast decision can't arrive before the pending entry
// exists), and blocks for a decision. An undecided call is denied at the timeout
// (fail safe); a cancelled context returns its error so the loop classifies it as
// an interrupt.
func (a *approvalRegistry) Approve(ctx context.Context, req sandbox.ApprovalRequest, onRegistered func()) (bool, error) {
	p := &pendingApproval{ch: make(chan bool, 1), convID: req.ConversationID}
	a.mu.Lock()
	a.pending[req.CallID] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, req.CallID)
		a.mu.Unlock()
	}()

	if onRegistered != nil {
		onRegistered()
	}

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()
	select {
	case ok := <-p.ch:
		return ok, nil
	case <-timer.C:
		return false, nil // undecided -> deny
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Decide resolves a pending approval. It verifies the decision comes from the same
// conversation the call was raised in, and reports whether a pending call matched.
func (a *approvalRegistry) Decide(callID, convID string, approve bool) bool {
	a.mu.Lock()
	p, ok := a.pending[callID]
	a.mu.Unlock()
	if !ok || p.convID != convID {
		return false
	}
	select {
	case p.ch <- approve:
		return true
	default:
		return false // already decided
	}
}
