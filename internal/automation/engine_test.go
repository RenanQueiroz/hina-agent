package automation

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seqToolsDef builds n SEQUENTIAL top-level tool steps (no parallel/for_each aggregate), for
// exercising the record / final-output byte budgets without tripping the aggregate fail-closed.
func seqToolsDef(t *testing.T, n int) Definition {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"t` + strconv.Itoa(i) + `","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}}`)
	}
	js := `{"schema_version":"automation.v1","name":"s","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[` + b.String() + `]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	return def
}

// parallelToolsDef builds a single parallel group of n shell.exec tool steps.
func parallelToolsDef(t *testing.T, n, maxToolCalls int) Definition {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"t` + strconv.Itoa(i) + `","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}}`)
	}
	js := `{"schema_version":"automation.v1","name":"p","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"budget":{"max_tool_calls":` + strconv.Itoa(maxToolCalls) + `},
		"steps":[{"id":"grp","type":"parallel","steps":[` + b.String() + `]}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	return def
}

// concExec tracks the maximum number of concurrent Tool executions.
type concExec struct {
	mu       sync.Mutex
	cur, max int
}

func (c *concExec) Tool(context.Context, ToolStep) (StepResult, error) {
	c.mu.Lock()
	c.cur++
	if c.cur > c.max {
		c.max = c.cur
	}
	c.mu.Unlock()
	time.Sleep(5 * time.Millisecond) // hold the slot so concurrency is observable
	c.mu.Lock()
	c.cur--
	c.mu.Unlock()
	return StepResult{Output: map[string]any{"ok": true}}, nil
}
func (c *concExec) Agent(context.Context, AgentStep) (StepResult, error) { return StepResult{}, nil }
func (c *concExec) LLM(context.Context, LLMStep) (StepResult, error)     { return StepResult{}, nil }

// A large parallel group must NOT launch more than MaxParallelism leaf steps at once
// (round-11 finding: unbounded sbx fan-out).
func TestParallelFanoutBounded(t *testing.T) {
	def := parallelToolsDef(t, 8, 0)
	cx := &concExec{}
	rec := Run(context.Background(), def, RunOptions{RunID: "p1", Caps: Caps{MaxParallelism: 2}}, cx)
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if cx.max > 2 {
		t.Fatalf("max concurrent tool steps = %d, want <= 2", cx.max)
	}
}

// A parallel group of tool steps exceeding the tool-call budget fails the run.
func TestToolCallBudgetEnforced(t *testing.T) {
	def := parallelToolsDef(t, 5, 2) // budget allows 2 tool calls
	cx := &concExec{}
	rec := Run(context.Background(), def, RunOptions{RunID: "p2", Caps: Caps{MaxParallelism: 1}}, cx)
	if rec.Status != RunFailed {
		t.Fatalf("status = %q, want failed (tool-call budget)", rec.Status)
	}
	if !strings.Contains(rec.Error, "budget") {
		t.Errorf("error = %q, want a budget abort", rec.Error)
	}
}

// fakeExec is a deterministic Executor for engine tests: no sbx, no CLI, no model.
type fakeExec struct {
	items     []any // github.notifications result
	toolCalls int64
	agentN    int64
	llmN      int64

	mu       sync.Mutex
	agentLog []string
}

func (f *fakeExec) Tool(_ context.Context, req ToolStep) (StepResult, error) {
	atomic.AddInt64(&f.toolCalls, 1)
	switch req.Tool {
	case ToolGithubNotifications:
		return StepResult{Output: map[string]any{"items": f.items}, Log: "checked notifications"}, nil
	case ToolGithubPRCheckout:
		notif, _ := req.With["notification"].(map[string]any)
		pr := notif["pr"]
		return StepResult{Output: map[string]any{"workspace": "/workspace/pr", "pr": pr}, Log: "checked out"}, nil
	case ToolGithubPRComment:
		return StepResult{Output: map[string]any{"posted": true, "pr": req.With["pr"]}, Log: "posted"}, nil
	default:
		return StepResult{Output: map[string]any{"ok": true}}, nil
	}
}

func (f *fakeExec) Agent(_ context.Context, req AgentStep) (StepResult, error) {
	atomic.AddInt64(&f.agentN, 1)
	f.mu.Lock()
	f.agentLog = append(f.agentLog, req.Adapter+":"+req.Workspace)
	f.mu.Unlock()
	return StepResult{Output: map[string]any{"status": "success", "final_text": "review by " + req.Adapter}, Log: "ran " + req.Adapter}, nil
}

func (f *fakeExec) LLM(_ context.Context, req LLMStep) (StepResult, error) {
	atomic.AddInt64(&f.llmN, 1)
	return StepResult{Output: map[string]any{"markdown": "combined review", "inputs_seen": len(req.Inputs)}, Log: "merged"}, nil
}

func mustDef(t *testing.T) Definition {
	t.Helper()
	d, err := Parse([]byte(prReviewJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if verrs := d.Validate(); verrs != nil {
		t.Fatalf("validate: %v", verrs)
	}
	return d
}

// Exit criterion: a notifications check that finds nothing finishes WITHOUT waking
// any LLM or agent.
func TestRunEmptyNotificationsSkipsWithoutModel(t *testing.T) {
	def := mustDef(t)
	fx := &fakeExec{items: nil}
	rec := Run(context.Background(), def, RunOptions{RunID: "r1", Trigger: "interval"}, fx)

	if rec.Status != RunSkipped {
		t.Fatalf("status = %q, want skipped (record: %+v)", rec.Status, rec)
	}
	if fx.agentN != 0 || fx.llmN != 0 {
		t.Fatalf("woke models: agent=%d llm=%d (must be 0)", fx.agentN, fx.llmN)
	}
	if rec.Message != "No matching review requests." {
		t.Errorf("message = %q", rec.Message)
	}
	if rec.ModelCalls != 0 || rec.AgentRuns != 0 {
		t.Errorf("accounting non-zero on a skip: model=%d agent=%d", rec.ModelCalls, rec.AgentRuns)
	}
}

// Exit criterion: when a PR matches, checkout -> parallel agent reviews -> llm
// combine -> post; each agent ran; an artifact is captured; the run record is
// complete.
func TestRunOnePRFullFlow(t *testing.T) {
	def := mustDef(t)
	fx := &fakeExec{items: []any{map[string]any{"pr": float64(42)}}}
	rec := Run(context.Background(), def, RunOptions{RunID: "r2", Trigger: "manual"}, fx)

	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s), want success", rec.Status, rec.Error)
	}
	if fx.agentN != 2 {
		t.Errorf("agent runs = %d, want 2 (codex+claude)", fx.agentN)
	}
	if fx.llmN != 1 {
		t.Errorf("llm calls = %d, want 1 (combine)", fx.llmN)
	}
	if rec.AgentRuns != 2 || rec.ModelCalls != 1 {
		t.Errorf("accounting: agent=%d model=%d", rec.AgentRuns, rec.ModelCalls)
	}
	if len(rec.Artifacts) != 1 || rec.Artifacts[0].Name != "final-review.md" {
		t.Fatalf("artifacts = %+v, want one final-review.md", rec.Artifacts)
	}
	if !strings.Contains(string(rec.Artifacts[0].Content), "combined review") {
		t.Errorf("artifact content = %q", rec.Artifacts[0].Content)
	}
	// Both agents ran and received the per-PR checkout workspace (workspace_from
	// resolved to the checkout's /workspace path).
	if len(fx.agentLog) != 2 {
		t.Errorf("agent log = %v, want 2 entries", fx.agentLog)
	}
	for _, l := range fx.agentLog {
		if !strings.Contains(l, "/workspace/pr") {
			t.Errorf("agent workspace not resolved from workspace_from: %q", l)
		}
	}
}

// Two PRs -> two iterations -> two artifacts with unique names.
func TestRunForEachMultipleArtifacts(t *testing.T) {
	def := mustDef(t)
	fx := &fakeExec{items: []any{
		map[string]any{"pr": float64(1)},
		map[string]any{"pr": float64(2)},
	}}
	rec := Run(context.Background(), def, RunOptions{RunID: "r3", Trigger: "manual"}, fx)
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if len(rec.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(rec.Artifacts))
	}
	if rec.Artifacts[0].Name == rec.Artifacts[1].Name {
		t.Errorf("artifact names must be unique: %q == %q", rec.Artifacts[0].Name, rec.Artifacts[1].Name)
	}
}

// Budget: max_agent_runs caps spawned agents and fails the run when exceeded.
func TestRunAgentBudgetEnforced(t *testing.T) {
	def := mustDef(t)
	def.Budget.MaxAgentRuns = 1 // the parallel group asks for 2
	fx := &fakeExec{items: []any{map[string]any{"pr": float64(9)}}}
	rec := Run(context.Background(), def, RunOptions{RunID: "r4", Trigger: "manual"}, fx)
	if rec.Status != RunFailed {
		t.Fatalf("status = %q, want failed (budget)", rec.Status)
	}
	if !strings.Contains(rec.Error, "budget") {
		t.Errorf("error = %q, want a budget message", rec.Error)
	}
}

// Server caps clamp an over-asking definition budget.
func TestCapsClampBudget(t *testing.T) {
	eb := clampBudget(Budget{MaxModelCalls: 1000, TimeoutSeconds: 100000}, Caps{MaxModelCalls: 5, Timeout: 0})
	if eb.maxModelCalls != 5 {
		t.Errorf("maxModelCalls = %d, want clamped to 5", eb.maxModelCalls)
	}
}

func TestContinueOnError(t *testing.T) {
	// A failing tool with continue_on_error doesn't fail the run.
	def := Definition{
		SchemaVersion: SchemaVersion,
		Name:          "x",
		Trigger:       Trigger{Type: TriggerManual},
		Steps: []Step{
			{ID: "a", Type: StepTool, Tool: ToolShellExec, ContinueOnError: true},
			{ID: "b", Type: StepFinish, Status: FinishSuccess, Message: "done"},
		},
	}
	rec := Run(context.Background(), def, RunOptions{RunID: "r5"}, &failingExec{})
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q, want success (continue_on_error swallowed the failure)", rec.Status)
	}
}

// gateExec records which tool step ids ran, and seeds a non-empty list for find.
type gateExec struct {
	mu  sync.Mutex
	ran []string
}

func (g *gateExec) Tool(_ context.Context, req ToolStep) (StepResult, error) {
	g.mu.Lock()
	g.ran = append(g.ran, req.StepID)
	g.mu.Unlock()
	if req.Tool == ToolGithubNotifications {
		return StepResult{Output: map[string]any{"items": []any{map[string]any{"pr": float64(1)}}}}, nil
	}
	return StepResult{Output: map[string]any{"ok": true}}, nil
}
func (g *gateExec) Agent(context.Context, AgentStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"ok": true}}, nil
}
func (g *gateExec) LLM(context.Context, LLMStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"text": "x"}}, nil
}

func (g *gateExec) didRun(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.ran {
		if r == id {
			return true
		}
	}
	return false
}

// A condition inside a parallel group must gate its branch target — a false guard
// must not run the target tool (round-2 finding).
func TestParallelConditionGatesTarget(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"g","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[
			{"id":"find","type":"tool","tool":"github.notifications","with":{}},
			{"id":"grp","type":"parallel","steps":[
				{"id":"cond","type":"condition","if":{"input":"find.items","op":"is_empty"},"then":["gated"],"else":[]},
				{"id":"gated","type":"tool","tool":"shell.exec","with":{"argv":["echo","hi"]}}
			]},
			{"id":"fin","type":"finish","status":"success"}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	gx := &gateExec{}
	rec := Run(context.Background(), def, RunOptions{RunID: "g1"}, gx)
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if !gx.didRun("find") {
		t.Error("find should have run")
	}
	// find.items is NON-empty -> is_empty is false -> the condition takes the (empty)
	// else branch -> the then-target 'gated' must NOT run.
	if gx.didRun("gated") {
		t.Fatal("a false parallel condition must not run its branch-target tool")
	}
}

// continue_on_error on a for_each must NOT swallow a budget abort (round-2 finding).
func TestForEachContinueOnErrorDoesNotSwallowBudget(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"b","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"budget":{"max_agent_runs":1},
		"steps":[
			{"id":"seed","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"seed.list","continue_on_error":true,"steps":[
				{"id":"a","type":"agent_cli","adapter":"codex","prompt_template":"x"}
			]}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	bx := &budgetExec{}
	rec := Run(context.Background(), def, RunOptions{RunID: "b1"}, bx)
	if rec.Status != RunFailed {
		t.Fatalf("status = %q, want failed — a budget abort must not be swallowed by continue_on_error", rec.Status)
	}
	if !strings.Contains(rec.Error, "budget") {
		t.Errorf("error = %q, want a budget abort", rec.Error)
	}
	if bx.agentN != 1 {
		t.Errorf("agent runs = %d, want exactly 1 (the budget capped the second)", bx.agentN)
	}
}

// A condition branch-target cycle (entry -> a -> b -> a) must fail the run, not recurse
// forever (round-3 finding).
func TestConditionCycleFailsNotHangs(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"c","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[
			{"id":"seed","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"entry","type":"condition","if":{"input":"seed.ok","op":"exists"},"then":["a"]},
			{"id":"a","type":"condition","if":{"input":"seed.ok","op":"exists"},"then":["b"]},
			{"id":"b","type":"condition","if":{"input":"seed.ok","op":"exists"},"then":["a"]}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	// A statically-known condition cycle (entry -> a -> b -> a) is rejected at VALIDATION,
	// before enable — so a definition with an earlier side-effecting step can't be enabled
	// and then run that side effect + fail on the cycle every fire (round-49).
	if v := def.Validate(); v == nil || !strings.Contains(v.Error(), "cycle") {
		t.Fatalf("validate must reject the condition cycle, got %v", v)
	}
	// The engine's runtime guard stays as defense-in-depth: running a cyclic def (one that
	// reached the engine some other way) fails fast and never hangs.
	rec := Run(context.Background(), def, RunOptions{RunID: "c1"}, &gateExec{})
	if rec.Status != RunFailed {
		t.Fatalf("status = %q, want failed (cycle)", rec.Status)
	}
	if !strings.Contains(rec.Error, "cycle") {
		t.Errorf("error = %q, want a cycle abort", rec.Error)
	}
}

// artExec seeds a 5-item list and returns a fixed-length markdown for each llm step.
type artExec struct{}

func (artExec) Tool(context.Context, ToolStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"list": []any{float64(1), float64(2), float64(3), float64(4), float64(5)}}}, nil
}
func (artExec) Agent(context.Context, AgentStep) (StepResult, error) { return StepResult{}, nil }
func (artExec) LLM(context.Context, LLMStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"markdown": strings.Repeat("x", 50)}}, nil
}

func artDef(t *testing.T, maxArtifactBytes int) Definition {
	t.Helper()
	js := `{"schema_version":"automation.v1","name":"a","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"budget":{"max_artifact_bytes":` + strconv.Itoa(maxArtifactBytes) + `},
		"steps":[
			{"id":"seed","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"seed.list","steps":[
				{"id":"combine","type":"llm","prompt_template":"x"}
			]}],
		"outputs":[{"type":"artifact","from_step":"combine","from_field":"markdown","name":"r.md"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	return def
}

// Artifact promotion must honor BOTH a total-bytes budget AND a count cap (round-6
// finding: the total was previously cap*maxArtifactsPerRun and the count unbounded).
func TestArtifactBudgetEnforced(t *testing.T) {
	t.Run("total", func(t *testing.T) {
		def := artDef(t, 30) // 5 iterations each promote a 50-byte markdown
		rec := Run(context.Background(), def, RunOptions{RunID: "a1"}, artExec{})
		var total int64
		for _, a := range rec.Artifacts {
			total += a.Size
		}
		if total > 30 {
			t.Fatalf("total artifact bytes = %d, want <= 30 (per-run total cap)", total)
		}
	})
	t.Run("count", func(t *testing.T) {
		old := maxArtifactsPerRun
		maxArtifactsPerRun = 2
		defer func() { maxArtifactsPerRun = old }()
		def := artDef(t, 1000000) // generous byte budget; the COUNT cap must bind
		rec := Run(context.Background(), def, RunOptions{RunID: "a2"}, artExec{})
		if len(rec.Artifacts) > 2 {
			t.Fatalf("artifacts = %d, want <= 2 (count cap)", len(rec.Artifacts))
		}
	})
}

// twoFieldExec returns a tool output with two distinct fields.
type twoFieldExec struct{}

func (twoFieldExec) Tool(context.Context, ToolStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"x": "X-content", "y": "Y-content"}}, nil
}
func (twoFieldExec) Agent(context.Context, AgentStep) (StepResult, error) { return StepResult{}, nil }
func (twoFieldExec) LLM(context.Context, LLMStep) (StepResult, error)     { return StepResult{}, nil }

// Two artifact outputs from the SAME step must BOTH be promoted, not silently overwritten
// (round-37 finding).
func TestMultipleArtifactsFromOneStep(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"a","trigger":{"type":"manual"},"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"s","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"f","type":"finish","status":"success"}
		],
		"outputs":[
			{"type":"artifact","name":"a.md","from_step":"s","from_field":"x"},
			{"type":"artifact","name":"b.md","from_step":"s","from_field":"y"}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	rec := Run(context.Background(), def, RunOptions{RunID: "r"}, twoFieldExec{})
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	names := map[string]bool{}
	for _, a := range rec.Artifacts {
		names[a.Name] = true
	}
	if len(rec.Artifacts) != 2 || !names["a.md"] || !names["b.md"] {
		t.Fatalf("both artifacts must be promoted, got %d: %v", len(rec.Artifacts), names)
	}
}

// A for_each with many cheap (condition-only) iterations must not balloon the run-record
// StepLog structure past the node budget — once exhausted, nested logs are truncated under
// one marker (round-36 finding). Cap set low via Caps so the test trips it deterministically.
func TestForEachStepLogNodeBudgetBounded(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"l","trigger":{"type":"manual"},"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"list","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"list.items","steps":[
				{"id":"body","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}}
			]},
			{"id":"f","type":"finish","status":"success"}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	rec := Run(context.Background(), def, RunOptions{RunID: "r", Caps: Caps{MaxStepLogNodes: 10}}, loopExec{n: 200, out: "x"})
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	total := 0
	for i := range rec.Steps {
		total += countLogNodes(rec.Steps[i])
	}
	// 200 iterations × condition logs would be hundreds of nodes; the budget caps it.
	if total > 10+50 { // node cap + the marker + the non-loop top-level steps
		t.Fatalf("recorded %d StepLog nodes, want bounded by the ~10-node budget", total)
	}
}

// loopExec returns an items array for the "list" step and a fixed-size output otherwise.
type loopExec struct {
	n   int
	out string
}

func (l loopExec) Tool(_ context.Context, ts ToolStep) (StepResult, error) {
	if ts.StepID == "list" {
		arr := make([]any, l.n)
		for i := range arr {
			arr[i] = float64(i)
		}
		return StepResult{Output: map[string]any{"items": arr}}, nil
	}
	return StepResult{Output: map[string]any{"data": l.out}}, nil
}
func (loopExec) Agent(context.Context, AgentStep) (StepResult, error) { return StepResult{}, nil }
func (loopExec) LLM(context.Context, LLMStep) (StepResult, error)     { return StepResult{}, nil }

func findStepLog(logs []StepLog, id string) *StepLog {
	for i := range logs {
		if logs[i].StepID == id {
			return &logs[i]
		}
		if l := findStepLog(logs[i].Children, id); l != nil {
			return l
		}
	}
	return nil
}

// A for_each loop must bound its accumulated aggregate by BYTES, not just item count —
// many large iteration outputs can't grow the in-memory/recorded aggregate without limit
// (round-35 finding). With outputCap = 1 MiB, 30 × 100 KiB outputs (~3 MiB unbounded) must
// stay ~1 MiB.
func TestForEachAggregateBudgetBounded(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"l","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"list","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"list.items","steps":[{"id":"body","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}}]},
			{"id":"f","type":"finish","status":"success"}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	rec := Run(context.Background(), def, RunOptions{RunID: "r"}, loopExec{n: 30, out: strings.Repeat("z", 100*1024)})
	// A for_each whose aggregate exceeds the output cap FAILS CLOSED (round-64): it must not
	// expose a PARTIAL set of iteration outputs to later steps. The aggregate is still bounded
	// in memory (it stopped appending at the cap, never accumulating the full ~3 MiB).
	if rec.Status != RunFailed || !strings.Contains(rec.Error, "aggregate") {
		t.Fatalf("status = %q (%s), want failed on a truncated aggregate", rec.Status, rec.Error)
	}
	loop := findStepLog(rec.Steps, "loop")
	if loop == nil {
		t.Fatal("no loop step log")
	}
	if int64(len(loop.Output)) > (1<<20)+(64<<10) { // outputCap 1 MiB + a small margin
		t.Fatalf("for_each aggregate = %d bytes, want bounded ~1 MiB (unbounded would be ~3 MiB)", len(loop.Output))
	}
}

// countingLoopExec returns an items array for "list" and a fixed-size output otherwise,
// counting the side-effecting body invocations.
type countingLoopExec struct {
	n    int
	out  string
	body *int
}

func (l countingLoopExec) Tool(_ context.Context, ts ToolStep) (StepResult, error) {
	if ts.StepID == "list" {
		arr := make([]any, l.n)
		for i := range arr {
			arr[i] = float64(i)
		}
		return StepResult{Output: map[string]any{"items": arr}}, nil
	}
	*l.body++ // a side-effecting body step (here a github.pr_comment) ran
	return StepResult{Output: map[string]any{"data": l.out}}, nil
}
func (countingLoopExec) Agent(context.Context, AgentStep) (StepResult, error) {
	return StepResult{}, nil
}
func (countingLoopExec) LLM(context.Context, LLMStep) (StepResult, error) { return StepResult{}, nil }

// Once a for_each aggregate crosses the output cap the run is doomed (fail-closed), so the loop
// must STOP immediately — never run another side-effecting iteration (post a comment, mutate
// state) whose output it must discard and repeat on retry (round-78).
func TestForEachAbortsAfterAggregateCap(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"l","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"list","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"list.items","steps":[
				{"id":"body","type":"tool","tool":"github.pr_comment","with":{"repo":"o/r","pr":1,"body":"x"}}]},
			{"id":"f","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	var bodyRuns int
	// 20 iterations × ~300 KiB outputs cross the ~1 MiB aggregate cap after only a few items.
	rec := Run(context.Background(), def, RunOptions{RunID: "r"}, countingLoopExec{n: 20, out: strings.Repeat("z", 300*1024), body: &bodyRuns})
	if rec.Status != RunFailed || !strings.Contains(rec.Error, "aggregate") {
		t.Fatalf("status = %q (%s), want failed on a truncated aggregate", rec.Status, rec.Error)
	}
	if bodyRuns == 0 {
		t.Fatal("expected the cap-crossing iterations to run")
	}
	if bodyRuns >= 20 {
		t.Fatalf("the loop ran all %d side-effecting iterations after the aggregate was already over budget; must stop at the cap-crossing one", bodyRuns)
	}
}

// recordLogs must enforce the node budget BEFORE appending a subtree: a single oversized
// nested subtree (a big for_each/parallel iteration log) must NOT be appended wholesale and
// overshoot the cap — it's dropped for a truncation marker instead (round-71).
func TestRecordLogsRejectsOversizedSubtree(t *testing.T) {
	e := &engine{maxStepLogs: 5}
	big := StepLog{StepID: "p", Children: make([]StepLog, 9)} // 1 + 9 = 10 nodes
	if n := countLogNodes(big); n != 10 {
		t.Fatalf("countLogNodes(big) = %d, want 10", n)
	}
	out := e.recordLogs(nil, []StepLog{big})
	total := 0
	for _, l := range out {
		total += countLogNodes(l)
	}
	if total > e.maxStepLogs+1 { // +1 allows the single truncation marker
		t.Fatalf("recordLogs appended %d nodes for a subtree over the %d-node budget", total, e.maxStepLogs)
	}
	if len(out) != 1 || out[0].StepID != "_truncated" {
		t.Fatalf("an over-budget subtree should yield only a truncation marker, got %+v", out)
	}
}

// A capped for_each aggregate must FAIL CLOSED before a later step can consume the partial
// set: the downstream step must never run (round-64).
func TestCappedAggregateDoesNotFeedLaterStep(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"l","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"list","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}},
			{"id":"loop","type":"for_each","items_from":"list.items","steps":[{"id":"body","type":"tool","tool":"shell.exec","with":{"argv":["echo"]}}]},
			{"id":"use","type":"llm","inputs":["loop"],"prompt_template":"summarize the reviews"},
			{"id":"f","type":"finish","status":"success"}
		]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	rec := Run(context.Background(), def, RunOptions{RunID: "r"}, loopExec{n: 30, out: strings.Repeat("z", 100*1024)})
	if rec.Status != RunFailed {
		t.Fatalf("status = %q, want failed (truncated aggregate)", rec.Status)
	}
	if findStepLog(rec.Steps, "use") != nil {
		t.Fatal("the downstream step must NOT run after a capped for_each aggregate")
	}
}

// A parallel group whose merged aggregate exceeds the output cap also fails closed (round-64).
func TestCappedParallelAggregateFailsClosed(t *testing.T) {
	def := parallelToolsDef(t, 10, 0)
	rec := Run(context.Background(), def, RunOptions{RunID: "r", Caps: Caps{MaxLogBytes: 2500, MaxParallelism: 8}}, fixedOutputExec{out: strings.Repeat("z", 1000)})
	if rec.Status != RunFailed || !strings.Contains(rec.Error, "aggregate") {
		t.Fatalf("status = %q (%s), want failed on a truncated parallel aggregate", rec.Status, rec.Error)
	}
}

// Final-output assembly must stay bounded by the record budget even with many large step
// outputs — collectOutputs marshals incrementally, not the whole scope at once (round-34).
func TestFinalOutputBudgetBounded(t *testing.T) {
	def := seqToolsDef(t, 10) // 10 sequential tool steps, each ~1KB of output
	rec := Run(context.Background(), def, RunOptions{RunID: "r", Caps: Caps{MaxLogBytes: 2500, MaxParallelism: 8}}, fixedOutputExec{out: strings.Repeat("z", 1000)})
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if int64(len(rec.FinalOutput)) > 2500 {
		t.Fatalf("final_output = %d bytes, want <= the 2500-byte record budget", len(rec.FinalOutput))
	}
}

// fixedOutputExec returns a fixed-size output for every tool step.
type fixedOutputExec struct{ out string }

func (f fixedOutputExec) Tool(context.Context, ToolStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"data": f.out}}, nil
}
func (fixedOutputExec) Agent(context.Context, AgentStep) (StepResult, error) {
	return StepResult{}, nil
}
func (fixedOutputExec) LLM(context.Context, LLMStep) (StepResult, error) { return StepResult{}, nil }

func sumOutputBytes(logs []StepLog) int {
	total := 0
	for _, l := range logs {
		total += len(l.Output)
		total += sumOutputBytes(l.Children)
	}
	return total
}

// Many near-cap step outputs must not bloat the run record far past max_log_bytes — the
// cumulative output bytes are charged against the run-record budget (round-28 finding).
func TestRunRecordOutputBudgetBounded(t *testing.T) {
	def := seqToolsDef(t, 10) // 10 sequential tool steps, each producing ~1KB of output
	rec := Run(context.Background(), def, RunOptions{RunID: "r", Caps: Caps{MaxLogBytes: 2500, MaxParallelism: 8}}, fixedOutputExec{out: strings.Repeat("z", 1000)})
	if rec.Status != RunSuccess {
		t.Fatalf("status = %q (%s)", rec.Status, rec.Error)
	}
	if got := sumOutputBytes(rec.Steps); int64(got) > 2500 {
		t.Fatalf("recorded output bytes = %d, want <= the 2500-byte run-record budget", got)
	}
}

// blockExec's Tool blocks until its context is cancelled — used to prove a finish in a
// parallel group cancels sibling steps (round-12 finding); without the fix the run hangs.
type blockExec struct{}

func (blockExec) Tool(ctx context.Context, _ ToolStep) (StepResult, error) {
	<-ctx.Done()
	return StepResult{}, ctx.Err()
}
func (blockExec) Agent(context.Context, AgentStep) (StepResult, error) { return StepResult{}, nil }
func (blockExec) LLM(context.Context, LLMStep) (StepResult, error)     { return StepResult{}, nil }

func TestParallelFinishCancelsSiblings(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"f","trigger":{"type":"manual"},"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"grp","type":"parallel","steps":[
			{"id":"blocker","type":"tool","tool":"shell.exec","with":{"argv":["sleep"]}},
			{"id":"done","type":"finish","status":"skipped","message":"stop"}
		]}]}`
	def, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if v := def.Validate(); v != nil {
		t.Fatalf("validate: %v", v)
	}
	done := make(chan RunRecord, 1)
	go func() { done <- Run(context.Background(), def, RunOptions{RunID: "f1"}, blockExec{}) }()
	select {
	case rec := <-done:
		if rec.Status != RunSkipped {
			t.Fatalf("status = %q, want skipped (the finish terminates the run)", rec.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a finish in a parallel group must cancel sibling steps (the run hung)")
	}
}

// budgetExec seeds a 2-item list and counts agent runs.
type budgetExec struct {
	mu     sync.Mutex
	agentN int
}

func (b *budgetExec) Tool(context.Context, ToolStep) (StepResult, error) {
	return StepResult{Output: map[string]any{"list": []any{float64(1), float64(2)}}}, nil
}
func (b *budgetExec) Agent(context.Context, AgentStep) (StepResult, error) {
	b.mu.Lock()
	b.agentN++
	b.mu.Unlock()
	return StepResult{Output: map[string]any{"ok": true}}, nil
}
func (b *budgetExec) LLM(context.Context, LLMStep) (StepResult, error) {
	return StepResult{}, nil
}

type failingExec struct{}

func (failingExec) Tool(context.Context, ToolStep) (StepResult, error) {
	return StepResult{Failed: true, Err: "boom"}, nil
}
func (failingExec) Agent(context.Context, AgentStep) (StepResult, error) {
	return StepResult{Failed: true, Err: "boom"}, nil
}
func (failingExec) LLM(context.Context, LLMStep) (StepResult, error) {
	return StepResult{Failed: true, Err: "boom"}, nil
}
