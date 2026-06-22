package automation

import (
	"encoding/json"
	"time"
)

// Run statuses (the terminal state of a run record).
const (
	RunRunning   = "running"
	RunSuccess   = "success"
	RunSkipped   = "skipped"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Step statuses inside a run record.
const (
	StepStatusSuccess = "success"
	StepStatusSkipped = "skipped"
	StepStatusFailed  = "failed"
	StepStatusError   = "error" // failed but continue_on_error swallowed it
)

// StepLog is the immutable record of one executed step. Output is the structured
// value the step produced (already secret-redacted by the executor), fed into the
// selector scope for later steps; Log is a short human-readable, redacted, capped
// summary. Children holds nested logs (for_each iterations, parallel/condition
// branch members).
type StepLog struct {
	StepID    string          `json:"step_id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at"`
	Output    json.RawMessage `json:"output,omitempty"`
	Log       string          `json:"log,omitempty"`
	Error     string          `json:"error,omitempty"`
	Branch    string          `json:"branch,omitempty"`    // condition: "then" | "else"
	Iteration int             `json:"iteration,omitempty"` // for_each item index
	Children  []StepLog       `json:"children,omitempty"`
}

// Artifact is a promoted output captured into the immutable run record. Content is
// already redacted + size-capped.
type Artifact struct {
	Name    string `json:"name"`
	StepID  string `json:"step_id"`
	Size    int64  `json:"size"`
	Content []byte `json:"-"` // stored owner-private on disk, not echoed in JSON
}

// RunRecord is the immutable outcome of one run: input snapshot, per-step logs,
// resource accounting, promoted artifacts, the final output, and status/error.
type RunRecord struct {
	Status      string          `json:"status"`
	Trigger     string          `json:"trigger"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	Steps       []StepLog       `json:"steps"`
	FinalOutput json.RawMessage `json:"final_output,omitempty"`
	Message     string          `json:"message,omitempty"`
	Error       string          `json:"error,omitempty"`
	ModelCalls  int             `json:"model_calls"`
	AgentRuns   int             `json:"agent_runs"`
	LogBytes    int64           `json:"log_bytes"`
	Artifacts   []Artifact      `json:"artifacts,omitempty"`
}

// DurationMs is the wall-clock run duration in milliseconds.
func (r RunRecord) DurationMs() int64 {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt).Milliseconds()
}
