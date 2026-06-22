package automation

import (
	"strings"
	"testing"
)

// prReviewJSON is the canonical GitHub PR-review automation (the phase's proof),
// trimmed to what the engine needs and made self-contained (inline schemas).
const prReviewJSON = `{
  "schema_version": "automation.v1",
  "name": "GitHub PR review requests",
  "description": "Check for requested PR reviews and draft a combined review.",
  "enabled": false,
  "timezone": "America/New_York",
  "trigger": {"type": "interval", "every": "5m"},
  "missed_run_policy": "skip",
  "concurrency": {"policy": "skip_if_running", "max_parallel": 1},
  "budget": {"timeout_seconds": 1800, "max_model_calls": 12, "max_agent_runs": 4, "max_log_bytes": 1048576, "max_artifact_bytes": 5242880},
  "sandbox": {
    "mode": "granular",
    "network": "enabled",
    "allowed_host_services": ["llamacpp"],
    "allowed_cli_tools": ["sh", "git", "gh", "codex", "claude", "cursor-agent", "pi"],
    "allowed_tools": ["github.notifications", "github.pr_checkout", "github.pr_comment"],
    "secret_refs": ["github_token"],
    "agent_auth_refs": ["codex_browser", "claude_browser"],
    "resources": {"cpus": 4, "memory_mb": 8192, "pids": 512}
  },
  "steps": [
    {"id": "find_review_requests", "type": "tool", "tool": "github.notifications", "with": {"reasons": ["review_requested"], "include_participating": true}},
    {"id": "skip_if_empty", "type": "condition", "if": {"input": "find_review_requests.items", "op": "is_empty"}, "then": ["finish_noop"], "else": ["review_each_pr"]},
    {"id": "finish_noop", "type": "finish", "status": "skipped", "message": "No matching review requests."},
    {"id": "review_each_pr", "type": "for_each", "items_from": "find_review_requests.items", "steps": [
      {"id": "checkout_pr", "type": "tool", "tool": "github.pr_checkout", "with": {"notification": "${item}"}},
      {"id": "agent_reviews", "type": "parallel", "steps": [
        {"id": "codex_review", "type": "agent_cli", "adapter": "codex", "workspace_from": "checkout_pr.workspace", "prompt_template": "Review PR ${item.pr}."},
        {"id": "claude_review", "type": "agent_cli", "adapter": "claude", "workspace_from": "checkout_pr.workspace", "prompt_template": "Review PR ${item.pr}."}
      ]},
      {"id": "combine_reviews", "type": "llm", "inputs": ["agent_reviews"], "prompt_template": "Merge the reviews for PR ${item.pr}."},
      {"id": "post_review", "type": "tool", "tool": "github.pr_comment", "with": {"notification": "${item}", "body_from": "combine_reviews.markdown"}}
    ]}
  ],
  "outputs": [{"type": "artifact", "from_step": "combine_reviews", "name": "final-review.md"}]
}`

func TestParseAndValidatePRReview(t *testing.T) {
	def, err := Parse([]byte(prReviewJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if verrs := def.Validate(); verrs != nil {
		t.Fatalf("validate: %v", verrs)
	}
}

func TestExportStripsEnabledAndRoundTrips(t *testing.T) {
	def, _ := Parse([]byte(prReviewJSON))
	def.Enabled = true
	out, err := def.Export()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"enabled": true`) {
		t.Error("export must force enabled=false")
	}
	// Re-parse + re-validate the export (round-trip).
	def2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse export: %v", err)
	}
	if verrs := def2.Validate(); verrs != nil {
		t.Fatalf("re-validate export: %v", verrs)
	}
}

func TestValidateRejectsUnknownSchema(t *testing.T) {
	def, _ := Parse([]byte(strings.Replace(prReviewJSON, "automation.v1", "automation.v2", 1)))
	if def.Validate() == nil {
		t.Fatal("a foreign schema_version must fail validation")
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	if _, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","bogus":1,"trigger":{"type":"manual"},"steps":[]}`)); err == nil {
		t.Fatal("unknown field must be rejected")
	}
}

// writable_paths was removed because the runtime never enforced it — a document declaring
// it must now be REJECTED, not silently accepted as an unenforced permission (round-32).
func TestParseRejectsRemovedWritablePaths(t *testing.T) {
	js := `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"granular","writable_paths":["workspace"]},
		"steps":[{"id":"f","type":"finish","status":"success"}]}`
	if _, err := Parse([]byte(js)); err == nil {
		t.Fatal("the removed writable_paths field must be rejected (not presented as an enforced permission)")
	}
}

func TestValidateCollectsIssues(t *testing.T) {
	bad := `{
		"schema_version": "automation.v1",
		"name": "",
		"trigger": {"type": "interval", "every": "nope"},
		"concurrency": {"policy": "weird"},
		"steps": [
			{"id": "a", "type": "tool", "tool": "no.such.tool"},
			{"id": "a", "type": "finish", "status": "bogus"},
			{"id": "ref", "type": "llm", "prompt_template": "see ${ghost.x}"}
		]
	}`
	def, err := Parse([]byte(bad))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	verrs := def.Validate()
	if verrs == nil {
		t.Fatal("expected validation issues")
	}
	wantPaths := []string{"name", "trigger.every", "concurrency.policy", "steps[0].tool", "steps[1].id", "steps[1].status"}
	have := map[string]bool{}
	for _, is := range verrs.Issues {
		have[is.Path] = true
	}
	for _, p := range wantPaths {
		if !have[p] {
			t.Errorf("missing issue for %q (got %+v)", p, verrs.Issues)
		}
	}
}

func TestValidateRejectsMCPCall(t *testing.T) {
	// mcp.call execution is deferred, so it must not be a selectable tool (no enable-then-
	// fail-on-first-run drift).
	def, _ := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"m","type":"tool","tool":"mcp.call","with":{}}]}`))
	if def.Validate() == nil {
		t.Fatal("mcp.call must be rejected (deferred — not a known tool)")
	}
}

func TestValidateRejectsReservedStepID(t *testing.T) {
	def, _ := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},"steps":[{"id":"item","type":"finish","status":"success"}]}`))
	if def.Validate() == nil {
		t.Fatal("step id 'item' is reserved and must be rejected")
	}
}

// A FORWARD reference (a step referencing a step that runs LATER) must be rejected at
// validation — otherwise the engine would run the earlier (possibly irreversible) step,
// then fail on the unresolvable forward reference, and repeat that partial side effect on
// every fire (round-47).
func TestValidateRejectsForwardReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"post","type":"tool","tool":"http.request","with":{"url":"https://x","body_from":"later.out"}},
			{"id":"later","type":"llm","prompt_template":"hi"},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	verrs := def.Validate()
	if verrs == nil || !strings.Contains(verrs.Error(), "later") {
		t.Fatalf("a forward reference must be rejected, got %v", verrs)
	}
}

// Each deterministic tool's REQUIRED fields are enforced at enable-time, so a malformed later
// tool step can't validate, run earlier side-effecting steps, then fail mid-run every fire — a
// required field may be a literal, a ${template}, or a <field>_from reference (round-79).
func TestValidateToolRequiredFields(t *testing.T) {
	bad := map[string]string{
		"http no url":        `{"id":"x","type":"tool","tool":"http.request","with":{"method":"GET"}}`,
		"pr_comment no body": `{"id":"x","type":"tool","tool":"github.pr_comment","with":{"repo":"o/r","pr":1}}`,
		"pr_comment no repo": `{"id":"x","type":"tool","tool":"github.pr_comment","with":{"pr":1,"body":"hi"}}`,
		"pr_checkout no pr":  `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r"}}`,
		"shell no argv/cmd":  `{"id":"x","type":"tool","tool":"shell.exec","with":{}}`,
		"http empty url lit": `{"id":"x","type":"tool","tool":"http.request","with":{"url":"  "}}`,
	}
	for name, step := range bad {
		js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[` + step + `,{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if def.Validate() == nil {
			t.Fatalf("%s: a missing/empty required tool field must be rejected at enable-time", name)
		}
	}
	// Valid literal AND dynamic (_from) forms must pass (a "prev" step makes refs visible).
	good := []string{
		`{"id":"x","type":"tool","tool":"http.request","with":{"url":"https://x"}}`,
		`{"id":"x","type":"tool","tool":"http.request","with":{"url_from":"prev.u"}}`,
		`{"id":"x","type":"tool","tool":"github.pr_comment","with":{"repo":"o/r","pr":1,"body":"hi"}}`,
		`{"id":"x","type":"tool","tool":"github.pr_comment","with":{"notification_from":"prev.item","body_from":"prev.b"}}`,
		`{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification_from":"prev.item"}}`,
		`{"id":"x","type":"tool","tool":"shell.exec","with":{"command":"ls"}}`,
	}
	for i, step := range good {
		js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[{"id":"prev","type":"tool","tool":"github.notifications","with":{}},` + step + `,{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("good[%d]: parse: %v", i, err)
		}
		if v := def.Validate(); v != nil {
			t.Fatalf("good[%d]: a valid tool `with` should pass, got %v", i, v)
		}
	}
}

// Beyond required-field PRESENCE, the enable-time validator checks the SHAPE of statically-known
// (literal) tool arguments so a malformed literal can't pass enable and fail mid-run after earlier
// side effects: a bad/internal URL, a non-bare repo, pr<=0, an empty argv, or a non-string *_from
// (round-80). Dynamic ${...}/_from values resolve + are checked at run time.
func TestValidateToolLiteralShapes(t *testing.T) {
	bad := map[string]string{
		"url not a url":      `{"id":"x","type":"tool","tool":"http.request","with":{"url":"not-a-url"}}`,
		"url internal host":  `{"id":"x","type":"tool","tool":"http.request","with":{"url":"http://localhost:8733/x"}}`,
		"url metadata ip":    `{"id":"x","type":"tool","tool":"http.request","with":{"url":"http://169.254.169.254/latest"}}`,
		"url private ip":     `{"id":"x","type":"tool","tool":"http.request","with":{"url":"http://10.0.0.5/x"}}`,
		"url_from nonstring": `{"id":"x","type":"tool","tool":"http.request","with":{"url_from":123}}`,
		"repo non-bare url":  `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"https://github.com/o/r","pr":1}}`,
		"repo extra path":    `{"id":"x","type":"tool","tool":"github.pr_comment","with":{"repo":"o/r/extra","pr":1,"body":"hi"}}`,
		"pr zero":            `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":0}}`,
		"argv empty":         `{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":[]}}`,
		// Non-string / wrong-type literals for fields the runtime reads as a specific type.
		"url number":          `{"id":"x","type":"tool","tool":"http.request","with":{"url":123}}`,
		"repo number":         `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":123,"pr":1}}`,
		"notification number": `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":123}}`,
		"pr string zero":      `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":"0"}}`,
		"pr non-numeric str":  `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":"abc"}}`,
		"argv object":         `{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":{}}}`,
		"argv empty element":  `{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":["git",""]}}`,
		"body number":         `{"id":"x","type":"tool","tool":"github.pr_comment","with":{"repo":"o/r","pr":1,"body":123}}`,
		// A literal notification OBJECT must carry a bare repository + positive pr.
		"notification empty": `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{}}}`,
		"notification bad":   `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{"repository":"bad","pr":0}}}`,
		"notification no pr": `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{"repository":"o/r"}}}`,
		// A ${...} NESTED inside a LITERAL notification object is rejected: the runtime resolver
		// expands only top-level strings/_from, so it would reach the tool unexpanded (round-83).
		"notification nested tmpl repo": `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{"repository":"${prev.repository}","pr":1}}}`,
		"notification nested tmpl pr":   `{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{"repository":"o/r","pr":"${prev.pr}"}}}`,
		// A ${...} inside a literal ARRAY element (argv/headers/reasons) won't be expanded either.
		"argv element tmpl":    `{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":["echo","${prev.pr}"]}}`,
		"headers element tmpl": `{"id":"x","type":"tool","tool":"http.request","with":{"url":"https://api.github.com/x","headers":["X-Id: ${prev.id}"]}}`,
		"reasons element tmpl": `{"id":"x","type":"tool","tool":"github.notifications","with":{"reasons":["${prev.r}"]}}`,
		// A field given in BOTH literal + _from forms is ambiguous (nondeterministic at runtime)
		// and must be rejected — else the skipped literal array could slip a ${...} through.
		"argv + argv_from":       `{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":["echo","${prev.pr}"],"argv_from":"prev.cmd"}}`,
		"headers + headers_from": `{"id":"x","type":"tool","tool":"http.request","with":{"url":"https://api.github.com/x","headers":["X-Id: ${prev.id}"],"headers_from":"prev.h"}}`,
		"reasons + reasons_from": `{"id":"x","type":"tool","tool":"github.notifications","with":{"reasons":["mention"],"reasons_from":"prev.r"}}`,
		"url + url_from":         `{"id":"x","type":"tool","tool":"http.request","with":{"url":"https://x","url_from":"prev.u"}}`,
	}
	for name, step := range bad {
		js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[{"id":"prev","type":"tool","tool":"github.notifications","with":{}},` + step + `,{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if def.Validate() == nil {
			t.Fatalf("%s: a malformed literal tool argument must be rejected at enable-time", name)
		}
	}
	good := []string{
		`{"id":"x","type":"tool","tool":"http.request","with":{"url":"https://api.github.com/x"}}`,
		`{"id":"x","type":"tool","tool":"http.request","with":{"url":"${prev.u}"}}`, // template -> dynamic, shape skipped
		`{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":5}}`,
		`{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":"5"}}`,                      // numeric string pr
		`{"id":"x","type":"tool","tool":"github.pr_checkout","with":{"notification":{"repository":"o/r","pr":1}}}`, // notification object
		`{"id":"x","type":"tool","tool":"shell.exec","with":{"argv":["git","status"]}}`,
		`{"id":"x","type":"tool","tool":"shell.exec","with":{"argv_from":"prev.cmd"}}`, // dynamic whole-array ref
	}
	for i, step := range good {
		js := `{"schema_version":"automation.v1","name":"t","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[{"id":"prev","type":"tool","tool":"github.notifications","with":{}},` + step + `,{"id":"f","type":"finish","status":"success"}]}`
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("good[%d]: parse: %v", i, err)
		}
		if v := def.Validate(); v != nil {
			t.Fatalf("good[%d]: a valid tool argument should pass, got %v", i, v)
		}
	}
}

// A tool step's workspace_from is honored at run time, so a missing/forward/out-of-scope
// reference must be rejected at validation (enable-time), not silently enabled to fail mid-run
// after earlier side effects already happened (round-76).
func TestValidateRejectsToolWorkspaceFrom(t *testing.T) {
	cases := map[string]string{
		"forward": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"later.workspace"},
				{"id":"later","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":1}},
				{"id":"done","type":"finish","status":"success"}]}`,
		"unknown": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"nope.workspace"},
				{"id":"done","type":"finish","status":"success"}]}`,
		"cross-scope": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
				{"id":"loop","type":"for_each","items_from":"seed.items","steps":[
					{"id":"co","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":1}}]},
				{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"co.workspace"},
				{"id":"done","type":"finish","status":"success"}]}`,
		// Visible root (a real pr_checkout) but the WRONG field — passes plain checkRef, fails
		// the workspace-specific validator.
		"wrong-field": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"co","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":1}},
				{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"co.nope"},
				{"id":"done","type":"finish","status":"success"}]}`,
		// Visible root but a NON-checkout step (no workspace output) — must be rejected.
		"non-checkout": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
				{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"seed.items"},
				{"id":"done","type":"finish","status":"success"}]}`,
		// The SAME validator guards the agent_cli path (a bad workspace_from there is rejected too).
		"agent_cli non-checkout": `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
				{"id":"review","type":"agent_cli","adapter":"codex","prompt_template":"go","workspace_from":"seed.items"},
				{"id":"done","type":"finish","status":"success"}]}`,
	}
	for name, js := range cases {
		def, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if verrs := def.Validate(); verrs == nil {
			t.Fatalf("%s: an invalid tool workspace_from must be rejected", name)
		}
	}
	// A VALID tool workspace_from (a prior sibling checkout) must pass.
	ok := `{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"co","type":"tool","tool":"github.pr_checkout","with":{"repo":"o/r","pr":1}},
			{"id":"build","type":"tool","tool":"shell.exec","with":{"argv":["make"]},"workspace_from":"co.workspace"},
			{"id":"done","type":"finish","status":"success"}]}`
	def, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.Validate(); verrs != nil {
		t.Fatalf("a valid tool workspace_from should pass, got %v", verrs)
	}
}

// A CROSS-SCOPE reference (a top-level step referencing a step nested inside a for_each)
// must be rejected — the nested output never leaks to the enclosing scope (round-47).
func TestValidateRejectsCrossScopeReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
			{"id":"loop","type":"for_each","items_from":"seed.items","steps":[
				{"id":"inner","type":"llm","prompt_template":"x"}]},
			{"id":"use","type":"llm","inputs":["inner.out"],"prompt_template":"y"},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	verrs := def.Validate()
	if verrs == nil || !strings.Contains(verrs.Error(), "inner") {
		t.Fatalf("a reference into a for_each's nested child must be rejected, got %v", verrs)
	}
}

// A PARALLEL group's children run concurrently against a read-only snapshot, so one child
// referencing a SIBLING child's output (which may not exist yet) must be rejected (round-47).
func TestValidateRejectsParallelSiblingReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"par","type":"parallel","steps":[
				{"id":"a","type":"llm","prompt_template":"x"},
				{"id":"b","type":"llm","inputs":["a.out"],"prompt_template":"y"}]},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	verrs := def.Validate()
	if verrs == nil || !strings.Contains(verrs.Error(), `"a"`) {
		t.Fatalf("a parallel child referencing a sibling child must be rejected, got %v", verrs)
	}
}

// A valid BACKWARD reference into an enclosing scope must still pass: a for_each child
// referencing a prior top-level step (the enclosing scope) is allowed.
func TestValidateAllowsEnclosingScopeReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
			{"id":"loop","type":"for_each","items_from":"seed.items","steps":[
				{"id":"inner","type":"llm","inputs":["seed.items"],"prompt_template":"x ${item}"}]},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.Validate(); verrs != nil {
		t.Fatalf("a for_each child referencing an enclosing-scope step should be valid: %v", verrs)
	}
}

// A reference to a CONDITIONAL branch target's output must be rejected: the target runs only
// when its branch fires, so a later step relying on it would fail at run time (round-48).
func TestValidateRejectsUntakenBranchReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
			{"id":"cond","type":"condition","if":{"input":"seed.items","op":"is_empty"},"then":["t_then"],"else":["t_else"]},
			{"id":"t_then","type":"llm","prompt_template":"x"},
			{"id":"t_else","type":"llm","prompt_template":"y"},
			{"id":"use","type":"llm","inputs":["t_then.out"],"prompt_template":"z"},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.Validate(); verrs == nil || !strings.Contains(verrs.Error(), "t_then") {
		t.Fatalf("a reference to a conditional branch target must be rejected, got %v", verrs)
	}
}

// A reference to a CONDITION step's id must be rejected — a condition writes no output (round-48).
func TestValidateRejectsConditionOutputReference(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted","network":"enabled"},
		"steps":[
			{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
			{"id":"cond","type":"condition","if":{"input":"seed.items","op":"exists"},"then":["done"]},
			{"id":"use","type":"llm","inputs":["cond.result"],"prompt_template":"x"},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.Validate(); verrs == nil || !strings.Contains(verrs.Error(), "cond") {
		t.Fatalf("a reference to a condition step's output must be rejected, got %v", verrs)
	}
}

// A MALFORMED template (unterminated or empty ${}) after a side-effecting step must be
// rejected at validation, not left to fail-closed expansion every fire (round-48).
func TestValidateRejectsMalformedTemplate(t *testing.T) {
	for _, tmpl := range []string{"hello ${seed.items", "x ${} y"} {
		def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
			"sandbox":{"mode":"unrestricted","network":"enabled"},
			"steps":[
				{"id":"post","type":"tool","tool":"http.request","with":{"url":"https://x"}},
				{"id":"bad","type":"llm","prompt_template":"` + tmpl + `"},
				{"id":"done","type":"finish","status":"success"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if verrs := def.Validate(); verrs == nil || !strings.Contains(verrs.Error(), "malformed") && !strings.Contains(verrs.Error(), "unterminated") {
			t.Fatalf("a malformed template %q must be rejected, got %v", tmpl, verrs)
		}
	}
}

// A self-targeting condition (then references itself) is a one-node cycle and must be
// rejected at validation — like any condition branch cycle (round-49).
func TestValidateRejectsSelfTargetCondition(t *testing.T) {
	def, err := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[
			{"id":"seed","type":"tool","tool":"github.notifications","with":{"reasons":["mention"]}},
			{"id":"loop","type":"condition","if":{"input":"seed.items","op":"exists"},"then":["loop"]},
			{"id":"done","type":"finish","status":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if verrs := def.Validate(); verrs == nil || !strings.Contains(verrs.Error(), "cycle") {
		t.Fatalf("a self-targeting condition must be rejected as a cycle, got %v", verrs)
	}
}

func TestValidateArtifactName(t *testing.T) {
	def, _ := Parse([]byte(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},
		"steps":[{"id":"s","type":"finish","status":"success"}],
		"outputs":[{"type":"artifact","from_step":"s","name":"../escape"}]}`))
	if def.Validate() == nil {
		t.Fatal("a traversal artifact name must be rejected")
	}
}
