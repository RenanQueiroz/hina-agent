package automation

import (
	"context"
	"encoding/json"
)

// Executor is the seam between the orchestration engine (this package) and the
// side-effecting world (the sbx sandbox, the callable-agent adapters, the LLM
// provider). The engine resolves every reference/template, enforces budgets and
// control flow, and assembles the immutable run record; the Executor performs one
// resolved tool/agent/llm step inside the run's per-automation sandbox and returns
// its REDACTED structured output. Keeping it an interface makes the engine fully
// testable without a real sbx/CLI/model, exactly like the Phase 7/8 fakes.
//
// An Executor returns a Go error ONLY for an execution-layer failure (sandbox
// unavailable, ctx cancelled) — an "expected" step failure (a tool exited non-zero,
// an agent reported failure) is reported in StepResult so the engine records it and
// applies the step's error policy.
type Executor interface {
	Tool(ctx context.Context, req ToolStep) (StepResult, error)
	Agent(ctx context.Context, req AgentStep) (StepResult, error)
	LLM(ctx context.Context, req LLMStep) (StepResult, error)
}

// RunInfo identifies the run + owner an executor step belongs to (for workspace
// scoping, secret resolution, and audit).
type RunInfo struct {
	RunID        string
	AutomationID string
	UserID       string
	Profile      SandboxProfile
}

// ToolStep is a resolved deterministic tool invocation. With holds the tool's
// arguments after template/`*_from` resolution (so values are concrete).
type ToolStep struct {
	Run       RunInfo
	StepID    string
	Tool      string
	With      map[string]any
	Workspace string // resolved from a prior checkout (workspace_from), if any
}

// AgentStep is a resolved callable-agent run. Prompt is already expanded; Schema is
// the resolved output schema (inline or from the document's schemas map), if any.
type AgentStep struct {
	Run       RunInfo
	StepID    string
	Adapter   string
	Prompt    string
	Model     string
	MaxTurns  int
	Workspace string
	Schema    json.RawMessage
}

// LLMStep is a resolved aggregation/verification model call. Inputs are the resolved
// values of the step's `inputs` references, in order.
type LLMStep struct {
	Run            RunInfo
	StepID         string
	Prompt         string
	Inputs         []any
	Schema         json.RawMessage
	MaxOutputBytes int64 // run output budget — the executor truncates the model stream to this
}

// StepResult is one step's outcome. Output is the structured value (decoded JSON)
// placed into the selector scope for later steps; Log is a short redacted human
// summary; Failed + Err report an expected step-level failure (non-zero exit, agent
// reported failure) the engine records and routes through the error policy.
type StepResult struct {
	Output any
	Log    string
	Failed bool
	Err    string
}
