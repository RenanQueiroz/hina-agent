package automation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// newScope creates an empty resolution scope.
func newScope() scope { return scope{} }

// child returns a shallow copy of the scope: the child can read every parent entry
// and add its own bindings (loop item/index, child outputs) without mutating the
// parent. This is how loop iterations and parallel children get an isolated view.
func (sc scope) child() scope {
	out := make(scope, len(sc)+2)
	for k, v := range sc {
		out[k] = v
	}
	return out
}

// outputs returns the subset of this scope holding the given steps' outputs, keyed
// by step id (used to collect a loop iteration's or a parallel group's child
// results).
func (sc scope) outputs(steps []Step) map[string]any {
	out := map[string]any{}
	for i := range steps {
		if v, ok := sc[steps[i].ID]; ok {
			out[steps[i].ID] = v
		}
	}
	return out
}

// indexLevel builds the id->step map for a level and the set of step ids that are
// the TARGET of a condition branch at this level (so they are not auto-run in
// sequence — they run only when their branch fires).
func indexLevel(steps []Step) (map[string]*Step, map[string]bool) {
	byID := make(map[string]*Step, len(steps))
	for i := range steps {
		byID[steps[i].ID] = &steps[i]
	}
	targets := map[string]bool{}
	for i := range steps {
		if steps[i].Type == StepCondition {
			for _, t := range steps[i].Then {
				targets[t] = true
			}
			for _, t := range steps[i].Else {
				targets[t] = true
			}
		}
	}
	return byID, targets
}

// maxForEachItems bounds a single for_each fan-out so a runaway/hostile upstream
// result (e.g. a huge notifications list) can't spawn an unbounded number of
// sandboxed tool/agent runs. A loop over more items than this fails the step
// (visible) rather than silently truncating.
const maxForEachItems = 1000

// resolveItems resolves a for_each items_from reference to a slice. A missing or
// non-array value is an error (a loop needs an array; an empty array runs zero
// iterations); an over-large array is rejected (bounded fan-out).
func (e *engine) resolveItems(ref string, sc scope) ([]any, error) {
	v, err := sc.resolve(ref)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("items_from %q is not an array", ref)
	}
	if len(arr) > maxForEachItems {
		return nil, fmt.Errorf("for_each over %d items exceeds the %d-item limit", len(arr), maxForEachItems)
	}
	return arr, nil
}

// resolveInputs resolves an llm step's inputs references in order.
func (e *engine) resolveInputs(refs []string, sc scope) ([]any, error) {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		v, err := sc.resolve(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// resolveWorkspace resolves a workspace_from reference to a string path (an earlier
// checkout's `workspace` field). Empty ref yields "".
func (e *engine) resolveWorkspace(ref string, sc scope) (string, error) {
	if ref == "" {
		return "", nil
	}
	v, err := sc.resolve(ref)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("workspace_from %q is not a string", ref)
	}
	return s, nil
}

// resolveWith resolves a tool's `with` object: a `*_from` key holds a bare reference
// (resolved to the raw value, key renamed without the suffix); a string value that
// is EXACTLY one ${ref} resolves to the raw referenced value (preserving type); any
// other string is template-expanded; non-string values pass through.
func (e *engine) resolveWith(with json.RawMessage, sc scope) (map[string]any, error) {
	out := map[string]any{}
	if len(with) == 0 {
		return out, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(with, &obj); err != nil {
		return nil, fmt.Errorf("tool `with` must be a JSON object")
	}
	for k, raw := range obj {
		var sval string
		if json.Unmarshal(raw, &sval) == nil {
			if strings.HasSuffix(k, "_from") {
				v, err := sc.resolve(sval)
				if err != nil {
					return nil, err
				}
				out[strings.TrimSuffix(k, "_from")] = v
				continue
			}
			if ref, ok := singleRef(sval); ok {
				v, err := sc.resolve(ref)
				if err != nil {
					return nil, err
				}
				out[k] = v
				continue
			}
			expanded, err := sc.expand(sval)
			if err != nil {
				return nil, err
			}
			out[k] = expanded
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("tool `with`.%s is not valid JSON", k)
		}
		out[k] = v
	}
	return out, nil
}

// singleRef reports whether s is exactly one ${ref} with nothing around it, and
// returns the inner reference. This is what lets `notification: "${item}"` resolve to
// the whole object rather than its JSON string form.
func singleRef(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "${") || !strings.HasSuffix(t, "}") {
		return "", false
	}
	inner := t[2 : len(t)-1]
	if strings.Contains(inner, "${") || strings.Contains(inner, "}") {
		return "", false
	}
	return strings.TrimSpace(inner), true
}

// evalCondition evaluates a condition predicate against the scope.
func (e *engine) evalCondition(c *Condition, sc scope) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("missing condition")
	}
	v, rerr := sc.resolve(c.Input)
	found := rerr == nil
	if rerr != nil {
		if _, isNF := rerr.(errRefNotFound); !isNF {
			return false, rerr // a structural reference error is fatal
		}
		v = nil
	}
	switch c.Op {
	case OpIsEmpty:
		return isEmptyValue(v, found), nil
	case OpIsNotEmpty:
		return !isEmptyValue(v, found), nil
	case OpExists:
		return found && v != nil, nil
	case OpNotExists:
		return !found || v == nil, nil
	case OpEq:
		return jsonEqual(v, c.Value), nil
	case OpNe:
		return !jsonEqual(v, c.Value), nil
	case OpContains:
		return valueContains(v, c.Value), nil
	case OpGt, OpLt:
		return numericCompare(v, c.Value, c.Op)
	default:
		return false, fmt.Errorf("unknown operator %q", c.Op)
	}
}

// isEmptyValue treats not-found, null, "", [], and {} as empty.
func isEmptyValue(v any, found bool) bool {
	if !found || v == nil {
		return true
	}
	switch t := v.(type) {
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case bool:
		return !t
	default:
		return false
	}
}

// decodeValue decodes a condition's Value into a comparable any.
func decodeValue(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

// jsonEqual compares a resolved value to a condition Value by canonical JSON.
func jsonEqual(v any, raw json.RawMessage) bool {
	want, ok := decodeValue(raw)
	if !ok {
		return false
	}
	a, err1 := json.Marshal(v)
	b, err2 := json.Marshal(want)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// valueContains: a string contains the value's string form; an array contains an
// element JSON-equal to the value.
func valueContains(v any, raw json.RawMessage) bool {
	want, ok := decodeValue(raw)
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		ws, ok := want.(string)
		return ok && strings.Contains(t, ws)
	case []any:
		wb, _ := json.Marshal(want)
		for _, el := range t {
			if eb, err := json.Marshal(el); err == nil && string(eb) == string(wb) {
				return true
			}
		}
	}
	return false
}

// numericCompare compares two numbers for gt/lt.
func numericCompare(v any, raw json.RawMessage, op string) (bool, error) {
	a, ok1 := toFloat(v)
	wantVal, _ := decodeValue(raw)
	b, ok2 := toFloat(wantVal)
	if !ok1 || !ok2 {
		return false, fmt.Errorf("%s requires numeric operands", op)
	}
	if op == OpGt {
		return a > b, nil
	}
	return a < b, nil
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}

// promoteMap indexes the artifact outputs by their source step id.
// promoteMap groups every artifact output by its source step. It keeps a SLICE per step
// so two artifacts configured from the same step both promote (the prior map[step]Output
// silently dropped all but the last).
func promoteMap(outs []Output) map[string][]Output {
	m := map[string][]Output{}
	for _, o := range outs {
		if o.Type == OutputArtifact {
			m[o.FromStep] = append(m[o.FromStep], o)
		}
	}
	return m
}

// maybePromote captures a step's output as a durable artifact when an output
// references it. Markdown/text values are stored raw; structured values as indented
// JSON. Names are made unique per repeated (looped) promotion, and the per-artifact
// + total budgets bound capture.
func (e *engine) maybePromote(id string, v any) {
	outs := e.promote[id]
	if len(outs) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// One promotion "round" per call (a looped step calls this once per iteration); the
	// iteration index suffixes every configured artifact's name so repeats stay unique.
	iter := e.promoteCount[id]
	e.promoteCount[id]++
	for _, o := range outs {
		val := v
		if o.FromField != "" {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			fv, ok := m[o.FromField]
			if !ok {
				continue
			}
			val = fv
		}
		var content []byte
		if s, ok := val.(string); ok {
			content = []byte(s)
		} else if b, err := json.MarshalIndent(val, "", "  "); err == nil {
			content = b
		} else {
			continue
		}
		// Hard COUNT cap: a loop over a huge list can't accumulate unbounded artifact rows.
		if len(e.artifacts) >= maxArtifactsPerRun {
			break
		}
		// Hard TOTAL-bytes cap: max_artifact_bytes is the per-run TOTAL artifact budget.
		// Truncate to whatever total remains; once exhausted, drop further artifacts.
		if cap := e.budget.maxArtifact; cap > 0 {
			remaining := cap - e.artifactBytes
			if remaining <= 0 {
				break
			}
			if int64(len(content)) > remaining {
				content = content[:remaining]
			}
		}
		name := o.Name
		if iter > 0 {
			name = suffixName(o.Name, iter)
		}
		e.artifactBytes += int64(len(content))
		e.artifacts = append(e.artifacts, Artifact{Name: name, StepID: id, Size: int64(len(content)), Content: content})
	}
}

// maxArtifactsPerRun bounds how many promoted artifacts a single run keeps (a loop
// over a huge list can't accumulate unbounded artifact rows). A package var so tests
// can shrink it.
var maxArtifactsPerRun = 200

// suffixName inserts -n before a file extension (final-review.md -> final-review-1.md).
func suffixName(name string, n int) string {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return fmt.Sprintf("%s-%d", name, n)
	}
	return fmt.Sprintf("%s-%d%s", name[:dot], n, name[dot:])
}

// collectOutputs assembles the run's final-output snapshot from the completed top-level
// scope (artifacts were promoted during execution). It marshals each scope value ONE at a
// time and accumulates only up to the output cap — never marshaling the whole scope
// speculatively — so a run with many large step outputs can't allocate hundreds of MB
// during finalization. Entries that would overflow the cap are dropped from the snapshot.
func (e *engine) collectOutputs(rec *RunRecord, sc scope) {
	rec.Artifacts = e.artifacts
	cap := e.budget.outputCap()
	outputs := map[string]json.RawMessage{}
	var total int64
	for k, v := range sc {
		if k == "item" || k == "index" {
			continue
		}
		ev, err := json.Marshal(v) // one entry at a time (bounded by that value's size)
		if err != nil {
			continue
		}
		if total+int64(len(ev)) > cap {
			continue // would overflow the record budget -> drop this entry from the snapshot
		}
		total += int64(len(ev))
		outputs[k] = ev
	}
	payload := map[string]any{}
	if len(outputs) > 0 {
		payload["outputs"] = outputs
	}
	if rec.Message != "" {
		payload["message"] = rec.Message
	}
	if len(rec.Artifacts) > 0 {
		names := make([]string, 0, len(rec.Artifacts))
		for _, a := range rec.Artifacts {
			names = append(names, a.Name)
		}
		payload["artifacts"] = names
	}
	if len(payload) == 0 {
		return
	}
	// payload is now bounded (outputs total <= cap; message/artifacts are small).
	if b, err := json.Marshal(payload); err == nil && int64(len(b)) <= cap && e.chargeRecordBytes(int64(len(b))) {
		rec.FinalOutput = b
	}
}
