package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Caps are the server-enforced ceilings a run's budget is clamped to (a definition
// can ask for LESS, never MORE). Zero in the definition means "use the cap".
type Caps struct {
	Timeout          time.Duration
	MaxModelCalls    int
	MaxAgentRuns     int
	MaxToolCalls     int
	MaxLogBytes      int64
	MaxArtifactBytes int64
	MaxStepLogNodes  int // run-record StepLog-node ceiling (0 -> maxStepLogNodes default)
	// MaxParallelism bounds how many leaf steps (tool/agent/llm) a single run may execute
	// CONCURRENTLY across all parallel groups — so one parallel group with hundreds of
	// children can't launch hundreds of sbx runs at once. 0 -> defaultParallelism.
	MaxParallelism int
}

// defaultParallelism bounds within-run leaf concurrency when Caps.MaxParallelism is 0.
const defaultParallelism = 8

// RunOptions parameterizes one run.
type RunOptions struct {
	RunID        string
	AutomationID string
	UserID       string
	Trigger      string
	Caps         Caps
	Now          func() time.Time // injectable clock (defaults to time.Now)
}

// effectiveBudget is the per-run budget after clamping the definition to the caps.
type effectiveBudget struct {
	timeout       time.Duration
	maxModelCalls int
	maxAgentRuns  int
	maxToolCalls  int
	maxLogBytes   int64
	maxArtifact   int64
}

func clampBudget(b Budget, c Caps) effectiveBudget {
	pick := func(want, cap int64) int64 {
		if cap <= 0 {
			return want
		}
		if want <= 0 || want > cap {
			return cap
		}
		return want
	}
	eb := effectiveBudget{
		maxModelCalls: int(pick(int64(b.MaxModelCalls), int64(c.MaxModelCalls))),
		maxAgentRuns:  int(pick(int64(b.MaxAgentRuns), int64(c.MaxAgentRuns))),
		maxToolCalls:  int(pick(int64(b.MaxToolCalls), int64(c.MaxToolCalls))),
		maxLogBytes:   pick(b.MaxLogBytes, c.MaxLogBytes),
		maxArtifact:   pick(b.MaxArtifactBytes, c.MaxArtifactBytes),
	}
	wantT := time.Duration(b.TimeoutSeconds) * time.Second
	switch {
	case c.Timeout <= 0:
		eb.timeout = wantT
	case wantT <= 0 || wantT > c.Timeout:
		eb.timeout = c.Timeout
	default:
		eb.timeout = wantT
	}
	return eb
}

// errBudget is the engine's internal signal that a budget ceiling was hit; it fails
// the run.
type budgetError struct{ msg string }

func (e budgetError) Error() string { return e.msg }

// isAbort reports whether a fatal error is a MANDATORY abort that continue_on_error
// must NOT swallow: a budget ceiling or a context cancellation/deadline (the per-run
// timeout or shutdown). A group can swallow an ordinary step failure, never these.
func isAbort(err error) bool {
	if err == nil {
		return false
	}
	var be budgetError
	if errors.As(err, &be) {
		return true
	}
	return isCanceled(err)
}

// isCanceled reports whether err is a context cancellation/deadline.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// finishSignal is raised by a finish step (or a budget/fatal abort) to unwind the
// step tree and stamp the run's terminal status.
type finishSignal struct {
	status  string // RunSuccess | RunSkipped | RunFailed
	message string
}

// engine holds one run's mutable state.
type engine struct {
	def    Definition
	exec   Executor
	info   RunInfo
	budget effectiveBudget
	now    func() time.Time

	// promote maps a step id to the artifact output(s) it feeds (a step may feed several),
	// with a per-id counter so a step inside a loop yields uniquely-named artifacts. Built
	// once in Run.
	promote      map[string][]Output
	promoteCount map[string]int

	// sem bounds concurrent leaf-step execution across all parallel groups in this run.
	sem chan struct{}

	mu            sync.Mutex
	modelCalls    int
	agentRuns     int
	toolCalls     int
	logBytes      int64
	maxStepLogs   int  // run-record StepLog-node ceiling for this run
	recordedLogs  int  // total StepLog NODES recorded (bounds the run-record structure)
	logsTruncated bool // a step-log truncation marker has already been emitted
	artifacts     []Artifact
	artifactBytes int64
}

// maxStepLogNodes bounds how many StepLog nodes a run record may hold. Even cheap steps
// (e.g. condition children) create a node, so a for_each over thousands of items with many
// children could otherwise balloon the record structure past every byte budget (which only
// charges log TEXT + outputs). Once exhausted, further nested logs are dropped under one
// truncation marker.
const maxStepLogNodes = 50000

// Run executes a definition and returns its immutable run record. It never returns
// an error: every failure (budget, executor, cancellation) is captured in the
// record's Status/Error so the caller can persist it. ctx cancellation (shutdown /
// cancel_previous) yields a Cancelled record.
func Run(ctx context.Context, def Definition, opts RunOptions, exec Executor) RunRecord {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	par := opts.Caps.MaxParallelism
	if par <= 0 {
		par = defaultParallelism
	}
	maxLogs := opts.Caps.MaxStepLogNodes
	if maxLogs <= 0 {
		maxLogs = maxStepLogNodes
	}
	e := &engine{
		def:          def,
		exec:         exec,
		info:         RunInfo{RunID: opts.RunID, AutomationID: opts.AutomationID, UserID: opts.UserID, Profile: def.Sandbox},
		budget:       clampBudget(def.Budget, opts.Caps),
		now:          now,
		promote:      promoteMap(def.Outputs),
		promoteCount: map[string]int{},
		sem:          make(chan struct{}, par),
		maxStepLogs:  maxLogs,
	}
	rec := RunRecord{Trigger: opts.Trigger, StartedAt: now().UTC(), Status: RunRunning}

	runCtx := ctx
	var cancel context.CancelFunc
	if e.budget.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, e.budget.timeout)
		defer cancel()
	}

	sc := newScope()
	logs, fin, fatal := e.execLevel(runCtx, def.Steps, sc)
	rec.Steps = logs
	rec.ModelCalls = e.modelCalls
	rec.AgentRuns = e.agentRuns
	rec.LogBytes = e.logBytes
	rec.FinishedAt = now().UTC()

	switch {
	case ctx.Err() != nil && runCtx.Err() == ctx.Err():
		// The PARENT ctx was cancelled (shutdown / cancel_previous), not just the
		// per-run timeout — record the run as cancelled, not failed.
		rec.Status = RunCancelled
		rec.Error = "run cancelled"
	case fatal != nil:
		rec.Status = RunFailed
		rec.Error = fatal.Error()
	case fin != nil:
		rec.Status = fin.status
		rec.Message = fin.message
	default:
		rec.Status = RunSuccess
	}

	// Assemble the final-output snapshot from the completed top-level scope (artifacts
	// were promoted during execution). A cancelled/failed run still records what it
	// managed to complete.
	e.collectOutputs(&rec, sc)
	return rec
}

// execLevel runs one level of steps in order, dispatching condition branches and
// stopping on a finish/fatal. Steps that are the TARGET of a condition branch at
// this level are not auto-run in sequence — they run only when their branch fires.
func (e *engine) execLevel(ctx context.Context, steps []Step, sc scope) ([]StepLog, *finishSignal, error) {
	byID, targets := indexLevel(steps)
	var logs []StepLog
	for i := range steps {
		s := &steps[i]
		if targets[s.ID] {
			continue
		}
		sub, fin, fatal := e.runStep(ctx, s, sc, byID, targets, nil)
		logs = append(logs, sub...)
		if fatal != nil {
			return logs, nil, fatal
		}
		if fin != nil {
			return logs, fin, nil
		}
	}
	return logs, nil, nil
}

// runStep executes one step (recursing for condition branches / nested levels),
// returning the step's log(s), an optional finish signal, and a fatal error. chain is
// the stack of condition step ids currently being dispatched at this level, used to
// reject a condition-target cycle (A -> B -> A) that would otherwise recurse forever.
func (e *engine) runStep(ctx context.Context, s *Step, sc scope, byID map[string]*Step, targets map[string]bool, chain []string) ([]StepLog, *finishSignal, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	switch s.Type {
	case StepCondition:
		return e.runCondition(ctx, s, sc, byID, targets, chain)
	case StepFinish:
		log := e.startLog(s)
		log.Status = StepStatusSuccess
		log.EndedAt = e.now().UTC()
		log.Log = s.Message
		return []StepLog{log}, &finishSignal{status: finishRunStatus(s.Status), message: s.Message}, nil
	case StepForEach:
		return e.runForEach(ctx, s, sc)
	case StepParallel:
		return e.runParallel(ctx, s, sc)
	case StepTool, StepLLM, StepAgentCLI:
		log, fatal := e.runLeaf(ctx, s, sc)
		if fatal != nil {
			return []StepLog{log}, nil, fatal
		}
		if log.Status == StepStatusFailed {
			return []StepLog{log}, nil, fmt.Errorf("step %q failed: %s", s.ID, log.Error)
		}
		return []StepLog{log}, nil, nil
	default:
		return nil, nil, fmt.Errorf("step %q has unsupported type %q", s.ID, s.Type)
	}
}

// runCondition evaluates the predicate, picks a branch, and runs each branch target
// (looked up among the level's siblings) as children of the condition log. chain is the
// stack of conditions already being dispatched; re-entering a condition on it is a
// cycle (A -> B -> A) and is failed rather than recursing forever.
func (e *engine) runCondition(ctx context.Context, s *Step, sc scope, byID map[string]*Step, targets map[string]bool, chain []string) ([]StepLog, *finishSignal, error) {
	log := e.startLog(s)
	if containsID(chain, s.ID) {
		log.Status = StepStatusFailed
		log.Error = "condition branch-target cycle"
		log.EndedAt = e.now().UTC()
		return []StepLog{log}, nil, fmt.Errorf("condition %q forms a branch-target cycle", s.ID)
	}
	childChain := append(append([]string(nil), chain...), s.ID)
	taken, err := e.evalCondition(s.If, sc)
	if err != nil {
		log.Status = StepStatusFailed
		log.Error = err.Error()
		log.EndedAt = e.now().UTC()
		if s.ContinueOnError {
			log.Status = StepStatusError
			return []StepLog{log}, nil, nil
		}
		return []StepLog{log}, nil, fmt.Errorf("condition %q: %w", s.ID, err)
	}
	branch := s.Else
	log.Branch = "else"
	if taken {
		branch = s.Then
		log.Branch = "then"
	}
	log.Status = StepStatusSuccess
	log.EndedAt = e.now().UTC()
	for _, tid := range branch {
		ts, ok := byID[tid]
		if !ok {
			continue // validated earlier; defensive
		}
		sub, fin, fatal := e.runStep(ctx, ts, sc, byID, targets, childChain)
		log.Children = append(log.Children, sub...)
		if fatal != nil {
			return []StepLog{log}, nil, fatal
		}
		if fin != nil {
			return []StepLog{log}, fin, nil
		}
	}
	return []StepLog{log}, nil, nil
}

// containsID reports whether id is already on the dispatch chain.
func containsID(chain []string, id string) bool {
	for _, c := range chain {
		if c == id {
			return true
		}
	}
	return false
}

// countLogNodes returns the total StepLog nodes in a log subtree (the log + its children).
func countLogNodes(l StepLog) int {
	n := 1
	for i := range l.Children {
		n += countLogNodes(l.Children[i])
	}
	return n
}

// recordLogs appends src to dst, bounded by the run's StepLog-node budget: once exhausted
// it appends a single truncation marker and drops the rest, so a loop with many iterations
// can't balloon the record structure beyond what byte budgets (text/outputs) account for.
func (e *engine) recordLogs(dst, src []StepLog) []StepLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range src {
		// Count the WHOLE subtree BEFORE appending: a nested for_each/parallel/condition log can
		// be thousands of nodes, so checking only "already at the cap" then appending wholesale
		// would overshoot the budget by that subtree's size. If it doesn't fit the remaining
		// allowance, drop it and leave a single truncation marker (the byte budgets bound text/
		// outputs separately; this caps the record's STRUCTURE).
		if e.recordedLogs+countLogNodes(src[i]) > e.maxStepLogs {
			if !e.logsTruncated {
				e.logsTruncated = true
				e.recordedLogs++
				dst = append(dst, StepLog{StepID: "_truncated", Type: "marker", Status: StepStatusSkipped, Error: "step-log budget exhausted; further step logs omitted"})
			}
			break
		}
		dst = append(dst, src[i])
		e.recordedLogs += countLogNodes(src[i])
	}
	return dst
}

// appendBoundedAgg appends out to agg only while the aggregate's marshaled size stays
// within cap. Once exceeded (or already truncated) it appends nothing but a single
// truncation marker, so a loop/parallel aggregate can't grow without bound in memory.
func appendBoundedAgg(agg []any, out any, aggBytes, cap int64, truncated bool) ([]any, int64, bool) {
	if truncated {
		return agg, aggBytes, true
	}
	b, err := json.Marshal(out)
	if err != nil {
		return agg, aggBytes, truncated // unmarshalable -> skip (not in the recorded aggregate)
	}
	if aggBytes+int64(len(b)) > cap {
		return append(agg, map[string]any{"_truncated": "aggregate exceeded the record budget"}), aggBytes, true
	}
	return append(agg, out), aggBytes + int64(len(b)), false
}

// runForEach iterates items_from, binding item/index, running the child steps per
// iteration in a loop-local scope. The step's output is the array of per-iteration
// child-output maps. An item failure fails the loop unless continue_on_error.
func (e *engine) runForEach(ctx context.Context, s *Step, sc scope) ([]StepLog, *finishSignal, error) {
	log := e.startLog(s)
	items, err := e.resolveItems(s.ItemsFrom, sc)
	if err != nil {
		log.Status = StepStatusFailed
		log.Error = err.Error()
		log.EndedAt = e.now().UTC()
		if s.ContinueOnError {
			log.Status = StepStatusError
			return []StepLog{log}, nil, nil
		}
		return []StepLog{log}, nil, fmt.Errorf("for_each %q: %w", s.ID, err)
	}
	var results []any
	var aggBytes int64
	truncated := false
	aggCap := e.budget.outputCap()
	for idx, item := range items {
		if err := ctx.Err(); err != nil {
			return []StepLog{log}, nil, err
		}
		iter := sc.child()
		iter["item"] = item
		iter["index"] = float64(idx)
		childLogs, fin, fatal := e.execLevel(ctx, s.Steps, iter)
		for j := range childLogs {
			childLogs[j].Iteration = idx
		}
		log.Children = e.recordLogs(log.Children, childLogs)
		// Bound the accumulated aggregate by BYTES (not just item count): marshal each
		// iteration's output independently and stop appending once the aggregate would
		// exceed the record budget — so large-but-valid loop outputs can't accumulate
		// hundreds of MB in memory (the iterations still RUN; only the snapshot is capped).
		results, aggBytes, truncated = appendBoundedAgg(results, iter.outputs(s.Steps), aggBytes, aggCap, truncated)
		// A real abort (a budget ceiling or a context cancellation/deadline) always ends the
		// whole run immediately — continue_on_error can never swallow it.
		if fatal != nil && isAbort(fatal) {
			log.Status = StepStatusFailed
			log.Error = fatal.Error()
			log.EndedAt = e.now().UTC()
			return []StepLog{log}, nil, fatal
		}
		// FAIL CLOSED the MOMENT the aggregate can no longer fit the output cap. The run is
		// already doomed — no downstream step can consume a partial aggregate (and the final
		// output would be refused) — so running ANOTHER side-effecting iteration would only post
		// comments / call endpoints / mutate state whose output we must discard, and repeat that
		// on every retry. Stop before the next item. (This iteration already ran; its side
		// effects are unavoidable, but no later iteration's are.) Mirrors the tool/agent/llm
		// fail-closed boundary, just hoisted out of the loop tail so it can't run extra work.
		if truncated {
			return e.failTruncatedAggregate(&log, "for_each", s.ID)
		}
		if fatal != nil {
			// A non-abort item failure: continue_on_error may swallow it, else it fails the loop.
			if s.ContinueOnError {
				continue
			}
			log.Status = StepStatusFailed
			log.Error = fatal.Error()
			log.EndedAt = e.now().UTC()
			return []StepLog{log}, nil, fatal
		}
		if fin != nil {
			// A finish inside a loop ends the whole run (truncation was handled above).
			log.Status = StepStatusSuccess
			log.EndedAt = e.now().UTC()
			e.setOutput(sc, s.ID, results, &log)
			return []StepLog{log}, fin, nil
		}
	}
	log.Status = StepStatusSuccess
	log.EndedAt = e.now().UTC()
	e.setOutput(sc, s.ID, results, &log)
	return []StepLog{log}, nil, nil
}

// runParallel runs the group's ROOT child steps concurrently against a shared
// read-only snapshot of the scope (children can't see each other's outputs). It builds
// the full group index ONCE so a condition child can gate its branch targets (a
// branch target is NOT spawned as an independent root — it runs only when its branch
// fires), then merges all child outputs into the step's output map.
func (e *engine) runParallel(ctx context.Context, s *Step, sc scope) ([]StepLog, *finishSignal, error) {
	log := e.startLog(s)
	snap := sc.child()
	byID, targets := indexLevel(s.Steps)
	// Roots = the steps that are NOT a condition branch target; only these run directly.
	var roots []int
	for i := range s.Steps {
		if !targets[s.Steps[i].ID] {
			roots = append(roots, i)
		}
	}
	type childOut struct {
		logs  []StepLog
		fin   *finishSignal
		fatal error
		out   map[string]any
	}
	// A child context so the FIRST non-swallowed fatal cancels the remaining siblings —
	// they stop launching new sbx runs instead of running to completion after the group
	// has already failed.
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	results := make([]childOut, len(roots))
	var wg sync.WaitGroup
	for ri, i := range roots {
		wg.Add(1)
		go func(ri, i int) {
			defer wg.Done()
			local := snap.child()
			logs, fin, fatal := e.runStep(pctx, &s.Steps[i], local, byID, targets, nil)
			// Collect every group step's output this child produced (its own + any branch
			// targets a condition child dispatched into its local scope).
			results[ri] = childOut{logs: logs, fin: fin, fatal: fatal, out: local.outputs(s.Steps)}
			// Drain siblings on the first fatal the group won't swallow, OR on a finish (a
			// terminal step ends the whole run — siblings must not keep running side effects).
			if fin != nil || (fatal != nil && (!s.ContinueOnError || isAbort(fatal))) {
				pcancel()
			}
		}(ri, i)
	}
	wg.Wait()

	merged := map[string]any{}
	var mergedBytes int64
	mergedTruncated := false
	aggCap := e.budget.outputCap()
	var fatal error
	var fin *finishSignal
	for _, r := range results {
		log.Children = e.recordLogs(log.Children, r.logs)
		// Bound the merged aggregate by BYTES so many large parallel child outputs can't
		// accumulate without limit in memory (the children still ran; only the snapshot is
		// capped). Marshal each value independently — never the whole merged map at once.
		for k, v := range r.out {
			if mergedTruncated {
				break
			}
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			if mergedBytes+int64(len(b)) > aggCap {
				merged["_truncated"] = "parallel aggregate exceeded the record budget"
				mergedTruncated = true
				break
			}
			merged[k] = v
			mergedBytes += int64(len(b))
		}
		// continue_on_error may swallow an ordinary child failure, but NEVER a budget
		// ceiling or a context cancellation/deadline. Prefer the ROOT-cause fatal (the
		// one that triggered sibling cancellation) over the resulting context.Canceled.
		if r.fatal != nil && (!s.ContinueOnError || isAbort(r.fatal)) {
			if fatal == nil || (isCanceled(fatal) && !isCanceled(r.fatal)) {
				fatal = r.fatal
			}
		}
		if r.fin != nil && fin == nil {
			fin = r.fin
		}
	}
	// If the only fatals were the cancellations WE triggered (no surviving root cause),
	// and the parent ctx is still live, treat the group as completed rather than failed
	// (defensive — a real root cause is normally present).
	log.EndedAt = e.now().UTC()
	if fatal != nil && !(isCanceled(fatal) && ctx.Err() == nil) {
		log.Status = StepStatusFailed
		log.Error = fatal.Error()
		return []StepLog{log}, nil, fatal
	}
	fatal = nil
	// FAIL CLOSED on a truncated merge: a later step must not consume a PARTIAL set of the
	// parallel children's outputs (same boundary as for_each + the leaf steps).
	if mergedTruncated {
		return e.failTruncatedAggregate(&log, "parallel", s.ID)
	}
	log.Status = StepStatusSuccess
	e.setOutput(sc, s.ID, merged, &log)
	if fin != nil {
		return []StepLog{log}, fin, nil
	}
	return []StepLog{log}, nil, nil
}

// failTruncatedAggregate marks a for_each/parallel step failed (NO output written to the
// scope) when its aggregate exceeded the run output cap — so a partial aggregate can't feed a
// later side-effecting step. The child iterations already ran; only their combined snapshot
// couldn't be completed within the budget.
func (e *engine) failTruncatedAggregate(log *StepLog, kind, id string) ([]StepLog, *finishSignal, error) {
	log.Status = StepStatusFailed
	log.Error = fmt.Sprintf("%s %q aggregate output exceeded the run output cap; refusing to expose a partial result", kind, id)
	log.EndedAt = e.now().UTC()
	return []StepLog{*log}, nil, fmt.Errorf("%s %q: aggregate output exceeded the output cap", kind, id)
}

// runLeaf executes a tool/llm/agent_cli step via the Executor, enforcing the model/
// agent-call budgets and recording the (redacted) output into the scope. A returned
// fatal error means an execution-layer failure or a step failure the policy doesn't
// swallow.
func (e *engine) runLeaf(ctx context.Context, s *Step, sc scope) (StepLog, error) {
	log := e.startLog(s)
	// Bound concurrent leaf execution across the whole run (a huge parallel group can't
	// launch hundreds of sbx runs at once). Sequential steps acquire uncontended.
	if err := e.acquire(ctx); err != nil {
		log.Status = StepStatusFailed
		log.Error = err.Error()
		log.EndedAt = e.now().UTC()
		return log, err
	}
	res, execErr := e.dispatchLeaf(ctx, s, sc)
	e.release()
	log.EndedAt = e.now().UTC()
	// A context cancellation/deadline is a mandatory abort: surface it AS ctx.Err() (not
	// whatever the executor returned) so isAbort reliably classifies it upstream and
	// continue_on_error can never swallow a timeout/shutdown.
	if ctx.Err() != nil {
		log.Status = StepStatusFailed
		log.Error = ctx.Err().Error()
		return log, ctx.Err()
	}
	if execErr != nil {
		// A budget error is likewise fatal regardless of continue_on_error.
		if _, isBudget := execErr.(budgetError); isBudget {
			log.Status = StepStatusFailed
			log.Error = execErr.Error()
			return log, execErr
		}
		log.Status = StepStatusFailed
		log.Error = execErr.Error()
		if s.ContinueOnError {
			log.Status = StepStatusError
			e.setOutput(sc, s.ID, nil, &log)
			return log, nil
		}
		return log, execErr
	}
	log.Log = e.capLog(res.Log)
	if res.Failed {
		log.Status = StepStatusFailed
		log.Error = res.Err
		if s.ContinueOnError {
			log.Status = StepStatusError
			e.setOutput(sc, s.ID, res.Output, &log)
			return log, nil
		}
		return log, nil // non-fatal step failure -> runStep maps to a fatal unless continue
	}
	log.Status = StepStatusSuccess
	e.setOutput(sc, s.ID, res.Output, &log)
	return log, nil
}

// dispatchLeaf resolves the step's arguments and routes to the Executor, charging
// the model/agent budgets first (fail closed before doing the work).
func (e *engine) dispatchLeaf(ctx context.Context, s *Step, sc scope) (StepResult, error) {
	switch s.Type {
	case StepTool:
		if err := e.chargeTool(); err != nil {
			return StepResult{}, err
		}
		with, err := e.resolveWith(s.With, sc)
		if err != nil {
			return StepResult{}, err
		}
		ws, err := e.resolveWorkspace(s.WorkspaceFrom, sc)
		if err != nil {
			return StepResult{}, err
		}
		return e.exec.Tool(ctx, ToolStep{Run: e.info, StepID: s.ID, Tool: s.Tool, With: with, Workspace: ws})
	case StepLLM:
		if err := e.chargeModel(); err != nil {
			return StepResult{}, err
		}
		prompt, err := sc.expand(s.PromptTemplate)
		if err != nil {
			return StepResult{}, err
		}
		inputs, err := e.resolveInputs(s.Inputs, sc)
		if err != nil {
			return StepResult{}, err
		}
		return e.exec.LLM(ctx, LLMStep{Run: e.info, StepID: s.ID, Prompt: prompt, Inputs: inputs, Schema: e.schemaFor(s), MaxOutputBytes: e.budget.outputCap()})
	case StepAgentCLI:
		if err := e.chargeAgent(); err != nil {
			return StepResult{}, err
		}
		prompt, err := sc.expand(s.PromptTemplate)
		if err != nil {
			return StepResult{}, err
		}
		ws, err := e.resolveWorkspace(s.WorkspaceFrom, sc)
		if err != nil {
			return StepResult{}, err
		}
		return e.exec.Agent(ctx, AgentStep{
			Run: e.info, StepID: s.ID, Adapter: s.Adapter, Prompt: prompt,
			Model: s.Model, MaxTurns: s.MaxTurns, Workspace: ws, Schema: e.schemaFor(s),
		})
	default:
		return StepResult{}, fmt.Errorf("step %q is not a leaf step", s.ID)
	}
}

func (e *engine) chargeModel() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.budget.maxModelCalls > 0 && e.modelCalls >= e.budget.maxModelCalls {
		return budgetError{fmt.Sprintf("model-call budget exhausted (max %d)", e.budget.maxModelCalls)}
	}
	e.modelCalls++
	return nil
}

func (e *engine) chargeAgent() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.budget.maxAgentRuns > 0 && e.agentRuns >= e.budget.maxAgentRuns {
		return budgetError{fmt.Sprintf("agent-run budget exhausted (max %d)", e.budget.maxAgentRuns)}
	}
	e.agentRuns++
	return nil
}

func (e *engine) chargeTool() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.budget.maxToolCalls > 0 && e.toolCalls >= e.budget.maxToolCalls {
		return budgetError{fmt.Sprintf("tool-call budget exhausted (max %d)", e.budget.maxToolCalls)}
	}
	e.toolCalls++
	return nil
}

// acquire takes a leaf-execution slot, honoring ctx cancellation so a cancelled run
// never blocks forever waiting for a slot.
func (e *engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *engine) release() { <-e.sem }

// capLog truncates a step log to keep within the run's log-byte budget, charging
// the accumulator. Past the budget, logs are dropped with a marker.
func (e *engine) capLog(s string) string {
	if s == "" {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.budget.maxLogBytes <= 0 {
		e.logBytes += int64(len(s))
		return s
	}
	remaining := e.budget.maxLogBytes - e.logBytes
	if remaining <= 0 {
		return "[log budget exhausted]"
	}
	if int64(len(s)) > remaining {
		s = s[:remaining] + "…[truncated]"
	}
	e.logBytes += int64(len(s))
	return s
}

// schemaFor resolves a step's output schema: the inline one, else a document-embedded
// schema named by output_schema_ref.
func (e *engine) schemaFor(s *Step) json.RawMessage {
	if len(s.OutputSchema) > 0 {
		return s.OutputSchema
	}
	if s.OutputSchemaRef != "" {
		if sch, ok := e.def.Schemas[s.OutputSchemaRef]; ok {
			return sch
		}
	}
	return nil
}

// startLog opens a step log with its start time.
func (e *engine) startLog(s *Step) StepLog {
	return StepLog{StepID: s.ID, Type: s.Type, StartedAt: e.now().UTC()}
}

// setOutput records a step's structured output into the scope AND onto its log
// (marshaled). A value that can't be marshaled is dropped from the log but still
// placed in the scope. The recorded bytes are charged against the run-record budget
// (shared with the step logs) so many capped outputs can't bloat automation_runs.record
// far past max_log_bytes — once the budget is exhausted further outputs are dropped from
// the record (they remain in the scope for downstream steps).
func (e *engine) setOutput(sc scope, id string, v any, log *StepLog) {
	sc[id] = v
	if v == nil {
		return
	}
	if b, err := json.Marshal(v); err == nil && int64(len(b)) <= e.budget.outputCap() && e.chargeRecordBytes(int64(len(b))) {
		log.Output = b
	}
	e.maybePromote(id, v)
}

// chargeRecordBytes charges n bytes against the cumulative run-record budget (the same
// pool capLog uses for step-log strings), returning false when the budget is exhausted so
// the caller drops the bytes from the durable record. Zero budget = unlimited.
func (e *engine) chargeRecordBytes(n int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.budget.maxLogBytes <= 0 {
		e.logBytes += n
		return true
	}
	if e.logBytes+n > e.budget.maxLogBytes {
		return false
	}
	e.logBytes += n
	return true
}

// outputCap bounds a single step's recorded output JSON (reuse the log budget, or a
// generous default).
func (b effectiveBudget) outputCap() int64 {
	if b.maxLogBytes > 0 {
		return b.maxLogBytes
	}
	return 1 << 20
}

func finishRunStatus(status string) string {
	switch status {
	case FinishSkipped:
		return RunSkipped
	case FinishFailed:
		return RunFailed
	default:
		return RunSuccess
	}
}
