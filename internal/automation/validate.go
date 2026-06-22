package automation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Issue is one structural validation problem, tagged with the JSON path of the
// offending field so the builder UI can point at it for repair.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationErrors is the structural-validation result: a collected list of issues
// (not just the first) so the UI / the LLM-assisted retry loop can fix them in one
// pass. It is an error when non-empty.
type ValidationErrors struct {
	Issues []Issue `json:"issues"`
}

func (e *ValidationErrors) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return "no validation issues"
	}
	parts := make([]string, 0, len(e.Issues))
	for _, is := range e.Issues {
		parts = append(parts, is.Path+": "+is.Message)
	}
	return "invalid automation: " + strings.Join(parts, "; ")
}

func (e *ValidationErrors) add(path, format string, args ...any) {
	e.Issues = append(e.Issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
}

// HasIssues reports whether any structural issue was recorded.
func (e *ValidationErrors) HasIssues() bool { return e != nil && len(e.Issues) > 0 }

var stepIDPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// reservedRoots are names a step id may NOT take, because the selector engine binds
// them as loop variables inside a for_each.
var reservedRoots = map[string]bool{"item": true, "index": true}

// Validate runs the structural checks — the in-Go equivalent of the automation.v1
// JSON Schema the frontend validates against with Ajv. It returns a *ValidationErrors
// (non-nil only when there are issues) so callers can surface every problem at once.
// It is context-free: eligibility (secrets/agents/policy exist) is a separate pass.
func (d Definition) Validate() *ValidationErrors {
	errs := &ValidationErrors{}

	if d.SchemaVersion != SchemaVersion {
		errs.add("schema_version", "must be %q (got %q)", SchemaVersion, d.SchemaVersion)
	}
	if strings.TrimSpace(d.Name) == "" {
		errs.add("name", "is required")
	} else if len(d.Name) > maxNameLen {
		errs.add("name", "must be at most %d characters", maxNameLen)
	}
	if len(d.Description) > maxDescriptionLen {
		errs.add("description", "must be at most %d characters", maxDescriptionLen)
	}
	if _, err := d.Location(); err != nil {
		errs.add("timezone", "%v", err)
	}
	d.validateTrigger(errs)
	switch d.MissedRunPolicy {
	case "", MissedSkip, MissedRunOnce:
	default:
		errs.add("missed_run_policy", "must be %q or %q", MissedSkip, MissedRunOnce)
	}
	d.validateConcurrency(errs)
	d.validateBudget(errs)
	d.validateSandbox(errs)

	// Steps: collect ids (recursive, unique, bounded) first — for the duplicate check — then
	// validate each step and its references against the outputs VISIBLE at that step (prior
	// siblings + the enclosing scope), never a forward or out-of-scope reference.
	ids := map[string]bool{}
	d.collectStepIDs(d.Steps, "steps", 0, ids, errs)
	if len(d.Steps) == 0 {
		errs.add("steps", "at least one step is required")
	}
	d.validateLevel(d.Steps, "steps", false, true, map[string]bool{}, errs)
	// Outputs are promoted as each step runs (including nested for_each children, with a
	// per-iteration suffix), so an output may reference ANY step id in the document.
	d.validateOutputs(ids, errs)

	if errs.HasIssues() {
		return errs
	}
	return nil
}

func (d Definition) validateTrigger(errs *ValidationErrors) {
	switch d.Trigger.Type {
	case TriggerManual:
	case TriggerInterval:
		if _, err := d.Trigger.interval(); err != nil {
			errs.add("trigger.every", "%v", err)
		}
		if d.Trigger.Cron != "" {
			errs.add("trigger.cron", "must be empty for an interval trigger")
		}
	case TriggerCron:
		if _, err := parseCron(d.Trigger.Cron); err != nil {
			errs.add("trigger.cron", "%v", err)
		}
		if d.Trigger.Every != "" {
			errs.add("trigger.every", "must be empty for a cron trigger")
		}
	case "":
		errs.add("trigger.type", "is required (interval|cron|manual)")
	default:
		errs.add("trigger.type", "must be interval|cron|manual (got %q)", d.Trigger.Type)
	}
}

func (d Definition) validateConcurrency(errs *ValidationErrors) {
	switch d.Concurrency.Policy {
	case "", ConcurrencySkip, ConcurrencyQueueOne, ConcurrencyParallel, ConcurrencyCancel:
	default:
		errs.add("concurrency.policy", "must be skip_if_running|queue_one|parallel|cancel_previous")
	}
	if d.Concurrency.MaxParallel < 0 || d.Concurrency.MaxParallel > 64 {
		errs.add("concurrency.max_parallel", "must be between 0 and 64")
	}
	if d.Concurrency.Policy == ConcurrencyParallel && d.Concurrency.MaxParallel < 1 {
		errs.add("concurrency.max_parallel", "must be at least 1 for the parallel policy")
	}
}

func (d Definition) validateBudget(errs *ValidationErrors) {
	b := d.Budget
	for _, f := range []struct {
		path string
		val  int64
	}{
		{"budget.timeout_seconds", int64(b.TimeoutSeconds)},
		{"budget.max_model_calls", int64(b.MaxModelCalls)},
		{"budget.max_agent_runs", int64(b.MaxAgentRuns)},
		{"budget.max_tool_calls", int64(b.MaxToolCalls)},
		{"budget.max_log_bytes", b.MaxLogBytes},
		{"budget.max_artifact_bytes", b.MaxArtifactBytes},
	} {
		if f.val < 0 {
			errs.add(f.path, "must be >= 0")
		}
	}
}

func (d Definition) validateSandbox(errs *ValidationErrors) {
	s := d.Sandbox
	switch s.Mode {
	case "", ModeGranular, ModeUnrestricted:
	default:
		errs.add("sandbox.mode", "must be granular|unrestricted")
	}
	switch s.Network {
	case "", "enabled", "disabled":
	default:
		errs.add("sandbox.network", "must be enabled|disabled")
	}
	for i, h := range s.AllowedHostServices {
		if !knownHostServices[h] {
			errs.add(fmt.Sprintf("sandbox.allowed_host_services[%d]", i), "unknown host service %q (only llamacpp)", h)
		}
	}
	for i, t := range s.AllowedTools {
		if !knownTools[t] {
			errs.add(fmt.Sprintf("sandbox.allowed_tools[%d]", i), "unknown tool %q", t)
		}
	}
	if r := s.Resources; r.CPUs < 0 || r.MemoryMB < 0 || r.PIDs < 0 {
		errs.add("sandbox.resources", "cpus/memory_mb/pids must be >= 0")
	}
	for _, f := range []struct {
		path string
		n    int
	}{
		{"sandbox.allowed_cli_tools", len(s.AllowedCLITools)},
		{"sandbox.allowed_tools", len(s.AllowedTools)},
		{"sandbox.secret_refs", len(s.SecretRefs)},
		{"sandbox.agent_auth_refs", len(s.AgentAuthRefs)},
		{"sandbox.allowed_host_services", len(s.AllowedHostServices)},
	} {
		if f.n > maxListLen {
			errs.add(f.path, "must have at most %d entries", maxListLen)
		}
	}
}

// collectStepIDs walks the step tree recording every id, flagging duplicates,
// bad-pattern/reserved ids, the total-count cap, and the nesting-depth cap.
func (d Definition) collectStepIDs(steps []Step, path string, depth int, ids map[string]bool, errs *ValidationErrors) {
	if depth > maxStepDepth {
		errs.add(path, "steps are nested too deeply (max %d)", maxStepDepth)
		return
	}
	for i := range steps {
		s := &steps[i]
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case s.ID == "":
			errs.add(p+".id", "is required")
		case !stepIDPattern.MatchString(s.ID):
			errs.add(p+".id", "must match [a-zA-Z_][a-zA-Z0-9_]* (got %q)", s.ID)
		case reservedRoots[s.ID]:
			errs.add(p+".id", "%q is reserved (a for_each loop variable)", s.ID)
		case ids[s.ID]:
			errs.add(p+".id", "duplicate step id %q", s.ID)
		default:
			ids[s.ID] = true
		}
		if len(ids) > maxSteps {
			errs.add(path, "too many steps (max %d)", maxSteps)
			return
		}
		if len(s.Steps) > 0 {
			d.collectStepIDs(s.Steps, p+".steps", depth+1, ids, errs)
		}
	}
}

// idSet returns the set of ids of the immediate steps in a slice (siblings), for
// then/else target validation.
func idSet(steps []Step) map[string]bool {
	out := make(map[string]bool, len(steps))
	for i := range steps {
		out[steps[i].ID] = true
	}
	return out
}

// validateStep checks one step's type-specific fields and that its references point at a
// step whose output is VISIBLE here (a prior sibling or an enclosing-scope step), or a loop
// variable when inLoop. siblings is the id set at this nesting level (for then/else); visible
// is the set of step ids whose outputs are available at this step's execution point.
func (d Definition) validateStep(s *Step, path string, inLoop bool, siblings, visible map[string]bool, errs *ValidationErrors) {
	switch s.Type {
	case StepTool:
		if !knownTools[s.Tool] {
			errs.add(path+".tool", "unknown tool %q", s.Tool)
		}
		d.checkWith(s.With, path+".with", inLoop, visible, errs)
		// Tool-specific required fields, checked at enable-time so a malformed LATER tool step
		// can't pass validation, run earlier side-effecting steps, then fail mid-run on every
		// scheduled fire (the runtime builders still reject it as defense in depth). A required
		// field may be supplied literally, via a `${template}`, or via a `<field>_from` reference.
		validateToolWith(s.Tool, s.With, path, errs)
		// A tool step's workspace_from is honored at run time (it sets the command's working dir
		// to a prior checkout under /workspace), so it must reference a VISIBLE prior step — the
		// same rule agent_cli gets — else an imported/LLM-drafted automation could enable with a
		// missing/forward/out-of-scope reference and only fail mid-run after side effects.
		if s.WorkspaceFrom != "" {
			d.checkWorkspaceRef(s.WorkspaceFrom, path+".workspace_from", inLoop, visible, errs)
		}
	case StepCondition:
		d.validateCondition(s, path, inLoop, siblings, visible, errs)
	case StepForEach:
		if s.ItemsFrom == "" {
			errs.add(path+".items_from", "is required")
		} else {
			d.checkRef(s.ItemsFrom, path+".items_from", inLoop, visible, errs)
		}
		if len(s.Steps) == 0 {
			errs.add(path+".steps", "a for_each needs at least one child step")
		}
		// Children run sequentially in a loop-local scope: they see the enclosing scope (this
		// for_each's `visible`, NOT its own not-yet-set output) + the loop item + prior siblings.
		d.validateLevel(s.Steps, path+".steps", true, true, visible, errs)
	case StepParallel:
		if len(s.Steps) == 0 {
			errs.add(path+".steps", "a parallel group needs at least one child step")
		}
		// Children run CONCURRENTLY against a read-only snapshot: each sees only the enclosing
		// scope, never a sibling's output — so a parallel child can't reference another.
		d.validateLevel(s.Steps, path+".steps", inLoop, false, visible, errs)
	case StepLLM:
		if strings.TrimSpace(s.PromptTemplate) == "" {
			errs.add(path+".prompt_template", "is required for an llm step")
		} else {
			d.checkTemplate(s.PromptTemplate, path+".prompt_template", inLoop, visible, errs)
		}
		if len(s.Inputs) > maxListLen {
			errs.add(path+".inputs", "too many inputs (max %d)", maxListLen)
		}
		for i, in := range s.Inputs {
			d.checkRef(in, fmt.Sprintf("%s.inputs[%d]", path, i), inLoop, visible, errs)
		}
		d.checkSchemaRef(s, path, errs)
	case StepAgentCLI:
		if !knownAdapters[s.Adapter] {
			errs.add(path+".adapter", "must be codex|claude|cursor|pi (got %q)", s.Adapter)
		}
		if strings.TrimSpace(s.PromptTemplate) == "" {
			errs.add(path+".prompt_template", "is required for an agent_cli step")
		} else {
			d.checkTemplate(s.PromptTemplate, path+".prompt_template", inLoop, visible, errs)
		}
		// workspace_from points the agent at a per-run checkout. It must reference a prior
		// github.pr_checkout step's .workspace output (the runtime maps it to a workdir under
		// the run's /workspace mount and rejects anything outside it).
		if s.WorkspaceFrom != "" {
			d.checkWorkspaceRef(s.WorkspaceFrom, path+".workspace_from", inLoop, visible, errs)
		}
		if s.MaxTurns < 0 {
			errs.add(path+".max_turns", "must be >= 0")
		}
		d.checkSchemaRef(s, path, errs)
	case StepFinish:
		switch s.Status {
		case FinishSuccess, FinishSkipped, FinishFailed:
		default:
			errs.add(path+".status", "must be success|skipped|failed")
		}
		if len(s.Message) > maxStringFieldLen {
			errs.add(path+".message", "is too long")
		}
	case "":
		errs.add(path+".type", "is required")
	default:
		errs.add(path+".type", "unknown step type %q", s.Type)
	}
}

func (d Definition) validateCondition(s *Step, path string, inLoop bool, siblings, visible map[string]bool, errs *ValidationErrors) {
	if s.If == nil {
		errs.add(path+".if", "is required for a condition step")
	} else {
		if s.If.Input == "" {
			errs.add(path+".if.input", "is required")
		} else {
			d.checkRef(s.If.Input, path+".if.input", inLoop, visible, errs)
		}
		switch s.If.Op {
		case OpIsEmpty, OpIsNotEmpty, OpExists, OpNotExists:
		case OpEq, OpNe, OpContains, OpGt, OpLt:
			if len(s.If.Value) == 0 {
				errs.add(path+".if.value", "is required for the %q operator", s.If.Op)
			}
		case "":
			errs.add(path+".if.op", "is required")
		default:
			errs.add(path+".if.op", "unknown operator %q", s.If.Op)
		}
	}
	if len(s.Then) == 0 && len(s.Else) == 0 {
		errs.add(path, "a condition needs a then and/or else branch")
	}
	for i, t := range s.Then {
		if !siblings[t] {
			errs.add(fmt.Sprintf("%s.then[%d]", path, i), "%q is not a sibling step id", t)
		}
	}
	for i, t := range s.Else {
		if !siblings[t] {
			errs.add(fmt.Sprintf("%s.else[%d]", path, i), "%q is not a sibling step id", t)
		}
	}
}

// validateLevel validates a step list, threading the set of step ids whose outputs are
// VISIBLE. `visible` is the enclosing scope at the start of this list. For a SEQUENTIAL list
// (top level, for_each children, condition branches) each step additionally sees the prior
// siblings that ran before it; for a PARALLEL group each child sees ONLY the enclosing scope
// (concurrent children can't reference each other). A nested step's children are validated
// against the scope visible at the nesting point — so a forward reference (to a later step)
// or an out-of-scope reference (into a sibling's nested children) is rejected at validation,
// before an automation with irreversible earlier side effects can be enabled.
func (d Definition) validateLevel(steps []Step, path string, inLoop, sequential bool, visible map[string]bool, errs *ValidationErrors) {
	siblings := idSet(steps)
	targets := branchTargets(steps)
	idx := indexByID(steps)
	// A condition branch graph (condition -> its then/else targets) must be ACYCLIC: a
	// self-target or a cycle (entry -> a -> b -> a) is rejected here, at enable, rather than
	// caught by the runtime only AFTER earlier siblings have executed their side effects.
	checkBranchCycles(steps, path, idx, errs)
	running := cloneSet(visible)
	validated := map[string]bool{}
	for i := range steps {
		s := &steps[i]
		if targets[s.ID] {
			// A branch target is dispatched BY its condition (in the condition's scope), not
			// at its own list position — validate it there, not here.
			continue
		}
		d.validateStepTree(steps, i, path, inLoop, sequential, siblings, running, targets, idx, validated, errs)
		// Only a step that DEFINITELY produces an output becomes visible to later sequential
		// siblings: a condition/finish writes none, and a branch target (handled above) may
		// never run — exposing either would let a later step validate against an output that
		// isn't there at run time, the exact repeat-on-every-fire failure round-47 closed.
		if sequential && producesOutput(s.Type) {
			running[s.ID] = true
		}
	}
}

// validateStepTree validates one non-target step, and — if it is a condition — each of its
// branch targets' bodies against the SAME visible scope (mirroring runCondition, which
// dispatches a target in the condition's scope, never the target's own list position). The
// `validated` guard makes a condition chain (a target that is itself a condition) terminate.
func (d Definition) validateStepTree(steps []Step, i int, path string, inLoop, sequential bool, siblings, running, targets map[string]bool, idx map[string]int, validated map[string]bool, errs *ValidationErrors) {
	s := &steps[i]
	d.validateStep(s, fmt.Sprintf("%s[%d]", path, i), inLoop, siblings, running, errs)
	if s.Type != StepCondition {
		return
	}
	for _, tid := range branchList(s) {
		j, ok := idx[tid]
		if !ok || validated[tid] {
			continue
		}
		validated[tid] = true
		d.validateStepTree(steps, j, path, inLoop, sequential, siblings, running, targets, idx, validated, errs)
	}
}

// checkBranchCycles runs a DFS over the condition branch graph at one level — edges go from
// a condition to each of its then/else targets; only conditions have out-edges — and reports
// a cycle (a target reached while still on the current DFS stack, including a self-target).
// A diamond (two conditions targeting the same leaf) is NOT a cycle (the black/gray coloring
// distinguishes "done" from "on the stack").
func checkBranchCycles(steps []Step, path string, idx map[string]int, errs *ValidationErrors) {
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	found := false
	var dfs func(id string)
	dfs = func(id string) {
		if found {
			return
		}
		i, ok := idx[id]
		if !ok || steps[i].Type != StepCondition {
			color[id] = black // a non-condition (or unknown) target is a leaf — no out-edges
			return
		}
		color[id] = gray
		for _, t := range branchList(&steps[i]) {
			switch color[t] {
			case gray:
				errs.add(fmt.Sprintf("%s[%d]", path, i), "condition branch cycle through %q", t)
				found = true
				return
			case white:
				dfs(t)
				if found {
					return
				}
			}
		}
		color[id] = black
	}
	for i := range steps {
		if steps[i].Type == StepCondition && color[steps[i].ID] == white {
			dfs(steps[i].ID)
			if found {
				return
			}
		}
	}
}

// branchTargets returns the ids that appear in some condition's then/else at this level
// (the steps that run conditionally, dispatched by their condition).
func branchTargets(steps []Step) map[string]bool {
	t := map[string]bool{}
	for i := range steps {
		if steps[i].Type == StepCondition {
			for _, id := range steps[i].Then {
				t[id] = true
			}
			for _, id := range steps[i].Else {
				t[id] = true
			}
		}
	}
	return t
}

// indexByID maps each step id at a level to its slice index.
func indexByID(steps []Step) map[string]int {
	m := make(map[string]int, len(steps))
	for i := range steps {
		m[steps[i].ID] = i
	}
	return m
}

// branchList returns a condition's then targets followed by its else targets.
func branchList(s *Step) []string {
	out := make([]string, 0, len(s.Then)+len(s.Else))
	out = append(out, s.Then...)
	return append(out, s.Else...)
}

// producesOutput reports whether a step type writes a referenceable output into the scope.
// A condition (only branches) and a finish (ends the run) produce none.
func producesOutput(t string) bool {
	switch t {
	case StepTool, StepLLM, StepAgentCLI, StepForEach, StepParallel:
		return true
	}
	return false
}

// cloneSet returns a shallow copy of a string set (so a nested level can extend its own
// visible-scope without mutating the parent's).
func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+2)
	for k := range in {
		out[k] = true
	}
	return out
}

// checkRef validates a bare reference: its root must be a VISIBLE step id (a prior sibling
// or an enclosing-scope step), or a loop variable when inLoop.
func (d Definition) checkRef(ref, path string, inLoop bool, visible map[string]bool, errs *ValidationErrors) {
	root := rootID(ref)
	if root == "" {
		errs.add(path, "malformed reference %q", ref)
		return
	}
	if reservedRoots[root] {
		if !inLoop {
			errs.add(path, "%q is only available inside a for_each", root)
		}
		return
	}
	if !visible[root] {
		errs.add(path, "reference %q targets step %q, which has no output visible here (it must be a prior step in the same or an enclosing scope)", ref, root)
	}
}

// findStep returns the step with the given id anywhere in the tree. workspace_from must point at
// a SPECIFIC workspace-producing step, so the validator needs the target's type/tool — which the
// per-scope visibility map alone doesn't carry.
func (d Definition) findStep(id string) (Step, bool) {
	var walk func(steps []Step) (Step, bool)
	walk = func(steps []Step) (Step, bool) {
		for _, s := range steps {
			if s.ID == id {
				return s, true
			}
			if len(s.Steps) > 0 {
				if found, ok := walk(s.Steps); ok {
					return found, true
				}
			}
		}
		return Step{}, false
	}
	return walk(d.Steps)
}

// checkWorkspaceRef validates a step's workspace_from. Beyond checkRef's visibility/scope rules,
// it requires the reference to resolve to a prior github.pr_checkout step's ".workspace" output —
// the only workspace-producing step. A visible-but-wrong reference (a reserved/loop root, a
// non-checkout step, or a wrong field like "checkout.nope"/"notifications.items") would otherwise
// pass plain checkRef and fail only when the step is REACHED at run time, after earlier tool
// steps may already have made irreversible changes — the exact enable-vs-mid-run hole we close.
func (d Definition) checkWorkspaceRef(ref, path string, inLoop bool, visible map[string]bool, errs *ValidationErrors) {
	root := rootID(ref)
	if root == "" {
		errs.add(path, "workspace_from %q is malformed", ref)
		return
	}
	st, ok := d.findStep(root)
	if !ok || reservedRoots[root] || !visible[root] {
		errs.add(path, "workspace_from %q must reference a prior github.pr_checkout step visible here", ref)
		return
	}
	if st.Type != StepTool || st.Tool != ToolGithubPRCheckout {
		errs.add(path, "workspace_from %q must reference a github.pr_checkout step (the only step that produces a workspace)", ref)
		return
	}
	if field := strings.TrimPrefix(strings.TrimPrefix(ref, root), "."); field != "workspace" {
		errs.add(path, "workspace_from %q must end in .workspace (a github.pr_checkout's checked-out path)", ref)
	}
}

// checkTemplate validates every ${...} root in a template string against the visible scope.
// It parses the template STRICTLY (the same conditions the runtime expander fails closed on):
// an unterminated or malformed reference is rejected here, before enable, so a definition
// can't run an earlier side effect and then fail expanding a later broken template every fire.
func (d Definition) checkTemplate(tmpl, path string, inLoop bool, visible map[string]bool, errs *ValidationErrors) {
	if len(tmpl) > maxStringFieldLen {
		errs.add(path, "is too long")
		return
	}
	roots, err := scanTemplateRefs(tmpl)
	if err != nil {
		errs.add(path, "%v", err)
		return
	}
	for _, root := range roots {
		if reservedRoots[root] {
			if !inLoop {
				errs.add(path, "%q is only available inside a for_each", root)
			}
			continue
		}
		if !visible[root] {
			errs.add(path, "template references step %q, which has no output visible here (it must be a prior step in the same or an enclosing scope)", root)
		}
	}
}

// fieldPresent reports whether a tool `with` object satisfies a required field — either a
// `<field>_from` reference (dynamic, resolved at run time), a non-string value, or a non-empty
// string (a literal or a `${template}`). A literal empty string does NOT count.
func fieldPresent(obj map[string]json.RawMessage, field string) bool {
	if _, ok := obj[field+"_from"]; ok {
		return true
	}
	raw, ok := obj[field]
	if !ok {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	return true // a non-string value (number/array/object/bool) is present
}

// validateToolWith enforces each deterministic tool's REQUIRED fields AND the TYPE + side-effect-
// free SHAPE of its statically-known values at enable-time, mirroring what the runtime argv builders
// reject — so an imported/LLM-drafted automation can't validate with a malformed tool step and fail
// only after earlier steps caused irreversible side effects. A field may be a literal (type +
// shape checked), a `${template}`, or a `<field>_from` reference (dynamic — only its presence and
// reference shape are checked, since the value resolves at run time).
func validateToolWith(tool string, with json.RawMessage, path string, errs *ValidationErrors) {
	obj := map[string]json.RawMessage{}
	if len(with) > 0 {
		if err := json.Unmarshal(with, &obj); err != nil {
			return // a malformed `with` is already reported by checkWith
		}
	}
	// Every `<field>_from` must be a STRING reference. checkWith validates string `_from` refs but
	// silently ignores a non-string one (e.g. url_from: 123), which the runtime resolver rejects.
	for k, raw := range obj {
		if strings.HasSuffix(k, "_from") {
			if !rawIsString(raw) {
				errs.add(path+".with."+k, "%q must be a string reference", k)
			}
			// A field given in BOTH literal and `<field>_from` forms is ambiguous: resolveWith
			// writes both to the same output key in nondeterministic MAP order, so the literal
			// could override the resolved reference and slip an unexpanded ${...} array/string past
			// the shape checks (which short-circuit when `_from` is present). Reject the ambiguity.
			if base := strings.TrimSuffix(k, "_from"); obj[base] != nil {
				errs.add(path+".with."+base, "specify either %q or %q, not both", base, k)
			}
		}
	}
	repoPR := fieldPresent(obj, "notification") || (fieldPresent(obj, "repo") && fieldPresent(obj, "pr"))
	switch tool {
	case ToolGithubNotifications:
		checkStringArrayField(obj, "reasons", path, errs, false)
	case ToolHTTPRequest:
		if !fieldPresent(obj, "url") {
			errs.add(path+".with.url", "http.request requires a url (or url_from)")
		}
		checkStringShape(obj, "url", path, errs, ValidateLiteralURL)
		checkStringArrayField(obj, "headers", path, errs, false)
	case ToolGithubPRComment:
		if !repoPR {
			errs.add(path+".with", "github.pr_comment requires repo + pr (or a notification)")
		}
		if !fieldPresent(obj, "body") {
			errs.add(path+".with.body", "github.pr_comment requires a body (or body_from)")
		}
		checkStringShape(obj, "body", path, errs, nil)
		checkRepoPRShape(obj, path, errs)
	case ToolGithubPRCheckout:
		if !repoPR {
			errs.add(path+".with", "github.pr_checkout requires repo + pr (or a notification)")
		}
		checkRepoPRShape(obj, path, errs)
	case ToolShellExec:
		if !fieldPresent(obj, "argv") && !fieldPresent(obj, "command") {
			errs.add(path+".with", "shell.exec requires an argv array (or a command string)")
		}
		checkStringShape(obj, "command", path, errs, nil)
		checkStringArrayField(obj, "argv", path, errs, true)
	}
}

func rawIsString(raw json.RawMessage) bool { var s string; return json.Unmarshal(raw, &s) == nil }

// checkStringShape: a PRESENT field (other than a `<field>_from` reference) must be a JSON string;
// a plain literal string (not a `${template}`) additionally runs shapeFn when given. A non-string
// literal (e.g. url: 123) is rejected — the runtime builder reads these via asString and would
// otherwise treat the bad value as empty/invalid mid-run.
func checkStringShape(obj map[string]json.RawMessage, field, path string, errs *ValidationErrors, shapeFn func(string) error) {
	if _, ok := obj[field+"_from"]; ok {
		return // a reference; its string-ness is checked by the `_from` loop
	}
	raw, ok := obj[field]
	if !ok {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		errs.add(path+".with."+field, "%s must be a string", field)
		return
	}
	if strings.Contains(s, "${") {
		return // a template -> resolved at run time
	}
	if shapeFn != nil {
		if err := shapeFn(s); err != nil {
			errs.add(path+".with."+field, "%v", err)
		}
	}
}

// checkRepoPRShape type/shape-checks a github.* tool's repo (bare owner/repo string), pr (a
// positive integer — a number or a numeric string), and notification (an object or a ${...} ref).
func checkRepoPRShape(obj map[string]json.RawMessage, path string, errs *ValidationErrors) {
	checkStringShape(obj, "repo", path, errs, ValidateGitHubRepo)
	if _, ok := obj["pr_from"]; !ok {
		if raw, ok := obj["pr"]; ok && !validPRLiteral(raw) {
			errs.add(path+".with.pr", "pr must be a positive PR number")
		}
	}
	if _, ok := obj["notification_from"]; !ok {
		if raw, ok := obj["notification"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				// A string notification must be a ${...} reference (resolved at run time); a bare
				// literal string is not a notification.
				if !strings.Contains(s, "${") {
					errs.add(path+".with.notification", "notification must be an object or a ${...} reference")
				}
			} else {
				var m map[string]json.RawMessage
				if json.Unmarshal(raw, &m) != nil {
					errs.add(path+".with.notification", "notification must be an object or a ${...} reference")
				} else {
					checkLiteralNotification(m, path, errs)
				}
			}
		}
	}
}

// checkLiteralNotification validates a LITERAL notification OBJECT carries a bare repository AND
// a positive pr — the runtime reads exactly those to identify the PR. Without this, notification:
// {} or {"repository":"bad","pr":0} would pass enable-time checks and only fail when the github.*
// tool is reached, after earlier side-effecting steps may already have run. A nested ${...} repo
// is dynamic (resolved at run time).
func checkLiteralNotification(m map[string]json.RawMessage, path string, errs *ValidationErrors) {
	// The runtime resolver (resolveWith) expands only TOP-LEVEL `with` strings + `*_from` refs,
	// NOT fields nested inside a literal object — so a `${...}` inside a literal notification
	// object reaches repoAndPR UNEXPANDED and silently fails / falls back to the wrong PR. A
	// literal notification object therefore must carry a LITERAL bare repository + positive pr; a
	// DYNAMIC notification must use `notification_from` or `notification: "${item}"` (whole-object).
	var rep string
	if r, ok := m["repository"]; !ok || json.Unmarshal(r, &rep) != nil || strings.TrimSpace(rep) == "" {
		errs.add(path+".with.notification.repository", "is required (a bare owner/repo); use notification_from or notification:\"${...}\" for a dynamic notification")
	} else if strings.Contains(rep, "${") {
		errs.add(path+".with.notification.repository", "can't be a ${...} template inside a literal object (the runtime won't expand it); use notification_from or notification:\"${...}\"")
	} else if err := ValidateGitHubRepo(rep); err != nil {
		errs.add(path+".with.notification.repository", "%v", err)
	}
	if !strictPositiveInt(m["pr"]) {
		errs.add(path+".with.notification.pr", "must be a positive integer (a ${...} template nested in a literal object won't resolve)")
	}
}

// strictPositiveInt accepts ONLY a positive integer — a JSON number or a numeric string, NO
// template — for a value NESTED in a literal object, which the runtime resolver does not expand.
func strictPositiveInt(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n > 0 && n == float64(int64(n))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return i > 0
		}
	}
	return false
}

// validPRLiteral accepts a positive integer (a JSON number) or a numeric string, or a ${template}
// (dynamic). A zero/negative number, a non-numeric or "0" string, or a non-scalar is rejected.
func validPRLiteral(raw json.RawMessage) bool {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n > 0 && n == float64(int64(n))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.Contains(s, "${") {
			return true
		}
		if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return i > 0
		}
	}
	return false
}

// checkStringArrayField validates a LITERAL string-array tool field (argv / headers / reasons).
// Each element must be a literal STRING with no `${...}` template — resolveWith expands only
// top-level strings + `*_from` refs, never array elements, so a per-element template would reach
// the tool UNEXPANDED (e.g. shell.exec running the literal "${item.pr}" instead of the PR). For a
// dynamic array use `<field>_from` (a whole-array reference). When requireNonEmpty (argv), the
// array and each element must also be non-empty.
func checkStringArrayField(obj map[string]json.RawMessage, field, path string, errs *ValidationErrors, requireNonEmpty bool) {
	if _, ok := obj[field+"_from"]; ok {
		return // a dynamic whole-array reference
	}
	raw, ok := obj[field]
	if !ok {
		return
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		errs.add(path+".with."+field, "%s must be an array", field)
		return
	}
	if requireNonEmpty && len(a) == 0 {
		errs.add(path+".with."+field, "%s must be a non-empty array", field)
		return
	}
	for i, el := range a {
		elPath := fmt.Sprintf("%s.with.%s[%d]", path, field, i)
		var s string
		if json.Unmarshal(el, &s) != nil {
			errs.add(elPath, "%s elements must be strings", field)
			continue
		}
		if strings.Contains(s, "${") {
			errs.add(elPath, "a ${...} template inside an array element won't be expanded by the runtime; use %s_from for a dynamic array", field)
			continue
		}
		if requireNonEmpty && strings.TrimSpace(s) == "" {
			errs.add(elPath, "%s elements must be non-empty", field)
		}
	}
}

// checkWith validates a tool's `with` object: every string value is treated as a
// template (so ${...} roots are checked), and every `*_from` key's string value is
// treated as a bare reference.
func (d Definition) checkWith(with json.RawMessage, path string, inLoop bool, visible map[string]bool, errs *ValidationErrors) {
	if len(with) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(with, &obj); err != nil {
		errs.add(path, "must be a JSON object")
		return
	}
	for k, raw := range obj {
		var sval string
		if json.Unmarshal(raw, &sval) == nil {
			if strings.HasSuffix(k, "_from") {
				d.checkRef(sval, path+"."+k, inLoop, visible, errs)
			} else {
				d.checkTemplate(sval, path+"."+k, inLoop, visible, errs)
			}
		}
	}
}

// checkSchemaRef validates that an output_schema_ref resolves to an embedded schema
// (when one is referenced and no inline output_schema is given).
func (d Definition) checkSchemaRef(s *Step, path string, errs *ValidationErrors) {
	if s.OutputSchemaRef == "" || len(s.OutputSchema) > 0 {
		return
	}
	if _, ok := d.Schemas[s.OutputSchemaRef]; !ok {
		errs.add(path+".output_schema_ref", "references unknown schema %q (add it to the top-level \"schemas\" map or use output_schema)", s.OutputSchemaRef)
	}
}

func (d Definition) validateOutputs(allIDs map[string]bool, errs *ValidationErrors) {
	if len(d.Outputs) > maxListLen {
		errs.add("outputs", "too many outputs")
		return
	}
	for i, o := range d.Outputs {
		p := fmt.Sprintf("outputs[%d]", i)
		if o.Type != OutputArtifact {
			errs.add(p+".type", "must be %q (the only output kind in v1)", OutputArtifact)
		}
		if o.FromStep == "" {
			errs.add(p+".from_step", "is required")
		} else if !allIDs[o.FromStep] {
			errs.add(p+".from_step", "unknown step %q", o.FromStep)
		}
		if strings.TrimSpace(o.Name) == "" {
			errs.add(p+".name", "is required")
		} else if !safeArtifactName(o.Name) {
			errs.add(p+".name", "must be a simple file name (letters, digits, dot, dash, underscore)")
		}
	}
}

var artifactNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// safeArtifactName rejects path separators / traversal so a promoted artifact name
// can never escape its run's artifact dir.
func safeArtifactName(name string) bool {
	return artifactNamePattern.MatchString(name) && !strings.Contains(name, "..")
}
