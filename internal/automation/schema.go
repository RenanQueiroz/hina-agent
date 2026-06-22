// Package automation is Hina's user-owned scheduled-workflow engine (Phase 9). It
// defines the versioned `automation.v1` document, a durable server-up-only
// scheduler, a deterministic-then-model step engine that runs each step inside the
// user's `sbx` sandbox under the automation's permission profile, and the immutable
// run records those produce. The JSON document is the portable contract (import/
// export); the generated database fields (id, owner, timestamps, last/next run) are
// NOT part of it. See plans/phase-09-automations.md and research-findings.md C4 for
// the settled semantics (selectors, error policy, idempotency, artifact promotion).
package automation

import "encoding/json"

// SchemaVersion is the only document version this build accepts. An import with any
// other schema_version is rejected (a future breaking change bumps to automation.v2
// with a documented migration); additive optional fields stay v1.
const SchemaVersion = "automation.v1"

// Trigger types.
const (
	TriggerInterval = "interval"
	TriggerCron     = "cron"
	TriggerManual   = "manual"
)

// Missed-run policies (while the server was down). Default skip; run_once is an
// explicit opt-in (run exactly once at startup if a fire was missed). Catch-up/
// backfill of every missed fire is deferred (surprising external side effects).
const (
	MissedSkip    = "skip"
	MissedRunOnce = "run_once"
)

// Concurrency policies for overlapping fires of one automation.
const (
	ConcurrencySkip     = "skip_if_running" // drop a fire while a run is active
	ConcurrencyQueueOne = "queue_one"       // keep at most one waiting run
	ConcurrencyParallel = "parallel"        // allow up to MaxParallel concurrent runs
	ConcurrencyCancel   = "cancel_previous" // cancel the active run, then start
)

// Sandbox profile modes.
const (
	ModeGranular     = "granular"
	ModeUnrestricted = "unrestricted"
)

// Step types.
const (
	StepTool      = "tool"
	StepCondition = "condition"
	StepForEach   = "for_each"
	StepParallel  = "parallel"
	StepLLM       = "llm"
	StepAgentCLI  = "agent_cli"
	StepFinish    = "finish"
)

// Finish statuses (the terminal status a finish step stamps on the run).
const (
	FinishSuccess = "success"
	FinishSkipped = "skipped"
	FinishFailed  = "failed"
)

// Definition is one `automation.v1` document. It is BOTH the import/export contract
// and the stored definition; the generated DB fields live alongside it in the
// store row, never inside this struct, so Export round-trips cleanly.
type Definition struct {
	SchemaVersion   string         `json:"schema_version"`
	Name            string         `json:"name"`
	Description     string         `json:"description,omitempty"`
	Enabled         bool           `json:"enabled"`
	Timezone        string         `json:"timezone,omitempty"`
	Trigger         Trigger        `json:"trigger"`
	MissedRunPolicy string         `json:"missed_run_policy,omitempty"`
	Concurrency     Concurrency    `json:"concurrency"`
	Budget          Budget         `json:"budget"`
	Sandbox         SandboxProfile `json:"sandbox"`
	Steps           []Step         `json:"steps"`
	Outputs         []Output       `json:"outputs,omitempty"`
	// Schemas is an optional embedded map of name -> JSON Schema, so an llm/agent_cli
	// step's output_schema_ref can resolve to a schema that travels WITH the document
	// (keeping export self-contained). Unreferenced entries are informational.
	Schemas map[string]json.RawMessage `json:"schemas,omitempty"`
}

// Trigger fires a run. interval uses Every (a Go duration); cron uses Cron (a
// 5-field expression evaluated in Timezone); manual fires only on an explicit run.
type Trigger struct {
	Type  string `json:"type"`
	Every string `json:"every,omitempty"`
	Cron  string `json:"cron,omitempty"`
}

// Concurrency governs overlapping fires. MaxParallel applies to the parallel policy
// (>=1); the others treat it as 1.
type Concurrency struct {
	Policy      string `json:"policy"`
	MaxParallel int    `json:"max_parallel,omitempty"`
}

// Budget bounds one run. Zero means "use the server default cap"; a value over the
// server cap is clamped at run time. Wall time, model calls, spawned agent runs,
// and captured log/artifact bytes are all bounded so an automation can't run away.
type Budget struct {
	TimeoutSeconds   int   `json:"timeout_seconds,omitempty"`
	MaxModelCalls    int   `json:"max_model_calls,omitempty"`
	MaxAgentRuns     int   `json:"max_agent_runs,omitempty"`
	MaxToolCalls     int   `json:"max_tool_calls,omitempty"` // deterministic tool invocations
	MaxLogBytes      int64 `json:"max_log_bytes,omitempty"`
	MaxArtifactBytes int64 `json:"max_artifact_bytes,omitempty"`
}

// SandboxProfile is the automation's permission profile — the second confirmation
// gate (the first is mandatory human review before enable). It is the ONLY policy
// an automation run is bound by (it does NOT inherit the owner's interactive
// Sandbox Environment). unrestricted allows any CLI tool + permitted network;
// granular allow-lists tools/network/host-services/secrets/agents explicitly.
type SandboxProfile struct {
	Mode                string    `json:"mode"`
	Network             string    `json:"network,omitempty"` // enabled | disabled
	AllowedHostServices []string  `json:"allowed_host_services,omitempty"`
	AllowedCLITools     []string  `json:"allowed_cli_tools,omitempty"`
	AllowedTools        []string  `json:"allowed_tools,omitempty"`   // typed tool names (github.* / http.request / ...)
	SecretRefs          []string  `json:"secret_refs,omitempty"`     // vaulted secret names granted to the run
	AgentAuthRefs       []string  `json:"agent_auth_refs,omitempty"` // configured agent auth profiles granted
	Resources           Resources `json:"resources,omitempty"`
}

// Resources caps a run's container resources (0 = the server default).
type Resources struct {
	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`
	PIDs     int `json:"pids,omitempty"`
}

// Step is one workflow step. Only the fields meaningful for its Type are set; the
// rest stay zero/omitted. References (then/else/items_from/inputs/workspace_from/
// condition input/output from_step) name a prior step id and a dot/bracket path
// into its output — see selector.go.
type Step struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	// tool
	Tool string          `json:"tool,omitempty"`
	With json.RawMessage `json:"with,omitempty"`

	// condition
	If   *Condition `json:"if,omitempty"`
	Then []string   `json:"then,omitempty"`
	Else []string   `json:"else,omitempty"`

	// for_each + parallel
	ItemsFrom string `json:"items_from,omitempty"`
	Steps     []Step `json:"steps,omitempty"`

	// llm + agent_cli
	Inputs          []string        `json:"inputs,omitempty"`
	PromptTemplate  string          `json:"prompt_template,omitempty"`
	OutputSchemaRef string          `json:"output_schema_ref,omitempty"`
	OutputSchema    json.RawMessage `json:"output_schema,omitempty"`

	// agent_cli
	Adapter       string `json:"adapter,omitempty"`
	WorkspaceFrom string `json:"workspace_from,omitempty"`
	Model         string `json:"model,omitempty"`
	MaxTurns      int    `json:"max_turns,omitempty"`

	// finish
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`

	// error policy (any step): on error, null this step's output and continue rather
	// than failing the run/loop/group. Default false (fail). v1 has no auto-retry.
	ContinueOnError bool `json:"continue_on_error,omitempty"`
}

// Condition is a condition step's predicate: resolve Input (a reference) and compare
// with Op (optionally against Value).
type Condition struct {
	Input string          `json:"input"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Condition operators.
const (
	OpIsEmpty    = "is_empty"
	OpIsNotEmpty = "is_not_empty"
	OpExists     = "exists"
	OpNotExists  = "not_exists"
	OpEq         = "eq"
	OpNe         = "ne"
	OpContains   = "contains"
	OpGt         = "gt"
	OpLt         = "lt"
)

// Output promotes a step's result into a durable artifact (the only output kind in
// v1; notification/file/http/shell are deferred). FromField (optional) selects a
// field of the step output; otherwise the whole output is promoted.
type Output struct {
	Type      string `json:"type"`
	FromStep  string `json:"from_step"`
	FromField string `json:"from_field,omitempty"`
	Name      string `json:"name"`
}

// Output types.
const OutputArtifact = "artifact"

// Parse decodes a definition document, rejecting unknown fields so a typo or a
// stale/foreign field surfaces as an error rather than being silently dropped.
func Parse(data []byte) (Definition, error) {
	dec := json.NewDecoder(boundedReader(data))
	dec.DisallowUnknownFields()
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return Definition{}, err
	}
	return d, nil
}

// MarshalForStore returns the canonical compact JSON for persistence (schema_version
// is forced to the current version).
func (d Definition) MarshalForStore() (string, error) {
	d.SchemaVersion = SchemaVersion
	b, err := json.Marshal(d)
	return string(b), err
}

// Export returns the canonical, indented `automation.v1` JSON for download/round-
// trip: the definition with the in-memory Enabled flag forced false (an export is a
// template to review before enabling) and only secret_refs (never values) present —
// which is already true because the document never holds a secret value. Generated
// DB fields are not part of Definition, so they are naturally absent.
func (d Definition) Export() ([]byte, error) {
	clone := d
	clone.SchemaVersion = SchemaVersion
	clone.Enabled = false
	return json.MarshalIndent(clone, "", "  ")
}
