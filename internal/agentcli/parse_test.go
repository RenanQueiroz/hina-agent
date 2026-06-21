package agentcli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodexParseNewSchema(t *testing.T) {
	a, _ := Get(ProviderCodex)
	stdout := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started","turn_id":"u1"}`,
		`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"main.go"},{"path":"go.mod"}]}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Done: fixed the bug."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1200,"output_tokens":340}}`,
	}, "\n")
	res := a.Parse(ProviderCodex, RawResult{Stdout: stdout, ExitCode: 0})
	if res.Status != StatusOK {
		t.Errorf("status = %q", res.Status)
	}
	if res.FinalText != "Done: fixed the bug." {
		t.Errorf("final = %q", res.FinalText)
	}
	if res.InputTokens != 1200 || res.OutputTokens != 340 {
		t.Errorf("tokens = %d/%d", res.InputTokens, res.OutputTokens)
	}
	if len(res.ChangedFiles) != 2 || res.ChangedFiles[0] != "main.go" || res.ChangedFiles[1] != "go.mod" {
		t.Errorf("changed files = %v", res.ChangedFiles)
	}
}

func TestCodexParseOldSchema(t *testing.T) {
	a, _ := Get(ProviderCodex)
	stdout := strings.Join([]string{
		`{"id":"0","msg":{"type":"agent_message","message":"hello world"}}`,
		`{"id":"1","msg":{"type":"token_count","input_tokens":10,"output_tokens":20}}`,
	}, "\n")
	res := a.Parse(ProviderCodex, RawResult{Stdout: stdout, ExitCode: 0})
	if res.FinalText != "hello world" || res.InputTokens != 10 || res.OutputTokens != 20 {
		t.Errorf("old-schema parse wrong: %+v", res)
	}
}

func TestCodexParseFallbackToLastLine(t *testing.T) {
	a, _ := Get(ProviderCodex)
	res := a.Parse(ProviderCodex, RawResult{Stdout: "not json\nfinal answer line\n", ExitCode: 0})
	if res.FinalText != "final answer line" {
		t.Errorf("fallback final = %q", res.FinalText)
	}
}

func TestClaudeParseJSON(t *testing.T) {
	a, _ := Get(ProviderClaude)
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":"All set.","num_turns":2,"total_cost_usd":0.0123,"usage":{"input_tokens":500,"output_tokens":80},"session_id":"s1"}`
	res := a.Parse(ProviderClaude, RawResult{Stdout: stdout, ExitCode: 0})
	if res.FinalText != "All set." {
		t.Errorf("final = %q", res.FinalText)
	}
	if res.CostUSD != 0.0123 {
		t.Errorf("cost = %v", res.CostUSD)
	}
	if res.InputTokens != 500 || res.OutputTokens != 80 {
		t.Errorf("tokens = %d/%d", res.InputTokens, res.OutputTokens)
	}
	if res.Status != StatusOK {
		t.Errorf("status = %q", res.Status)
	}
}

func TestClaudeParseError(t *testing.T) {
	a, _ := Get(ProviderClaude)
	stdout := `{"type":"result","subtype":"error","is_error":true,"result":"boom"}`
	res := a.Parse(ProviderClaude, RawResult{Stdout: stdout, ExitCode: 0})
	if res.Status != StatusError {
		t.Errorf("is_error result must classify as error, got %q", res.Status)
	}
}

func TestClaudeStructuredSurfacesJSON(t *testing.T) {
	a, _ := Get(ProviderClaude)
	stdout := `{"type":"result","is_error":false,"result":"{\"verdict\":\"pass\",\"score\":9}"}`
	res := a.Parse(ProviderClaude, RawResult{Stdout: stdout, ExitCode: 0})
	if len(res.Structured) == 0 {
		t.Fatal("expected structured output to be surfaced")
	}
	var m map[string]any
	if err := json.Unmarshal(res.Structured, &m); err != nil || m["verdict"] != "pass" {
		t.Errorf("structured = %s", res.Structured)
	}
}

func TestCursorParseJSON(t *testing.T) {
	a, _ := Get(ProviderCursor)
	stdout := `{"type":"result","result":"refactored","usage":{"input_tokens":12,"output_tokens":3}}`
	res := a.Parse(ProviderCursor, RawResult{Stdout: stdout, ExitCode: 0})
	if res.FinalText != "refactored" || res.InputTokens != 12 {
		t.Errorf("cursor parse: %+v", res)
	}
}

func TestPiParseRPC(t *testing.T) {
	a, _ := Get(ProviderPi)
	stdout := strings.Join([]string{
		`{"type":"message","role":"assistant","content":"thinking..."}`,
		`{"type":"message","role":"assistant","content":"the local answer"}`,
		`{"type":"result","usage":{"input_tokens":7,"output_tokens":11}}`,
	}, "\n")
	res := a.Parse(ProviderPi, RawResult{Stdout: stdout, ExitCode: 0})
	if res.FinalText != "the local answer" {
		t.Errorf("pi final = %q", res.FinalText)
	}
	if res.InputTokens != 7 || res.OutputTokens != 11 {
		t.Errorf("pi tokens = %d/%d", res.InputTokens, res.OutputTokens)
	}
}

func TestParseStatusClassification(t *testing.T) {
	a, _ := Get(ProviderCodex)
	// Timeout takes precedence over a non-zero exit.
	to := a.Parse(ProviderCodex, RawResult{Stdout: "", ExitCode: 137, TimedOut: true})
	if to.Status != StatusTimeout || to.Err == "" {
		t.Errorf("timeout classify: %+v", to)
	}
	can := a.Parse(ProviderCodex, RawResult{Stdout: "", ExitCode: 130, Cancelled: true})
	if can.Status != StatusCancelled {
		t.Errorf("cancel classify: %+v", can)
	}
	// A non-zero exit with no timeout/cancel is an error, with stderr summarized.
	er := a.Parse(ProviderCodex, RawResult{Stdout: "", Stderr: "fatal: nope\n", ExitCode: 1})
	if er.Status != StatusError || er.Err != "fatal: nope" {
		t.Errorf("error classify: %+v", er)
	}
}

func TestBaseResultCarriesPaths(t *testing.T) {
	a, _ := Get(ProviderCodex)
	res := a.Parse(ProviderCodex, RawResult{
		Stdout: `{"type":"agent_message","message":"x"}`, ExitCode: 0,
		Duration: 1500 * time.Millisecond, StdoutPath: "/p/out.log", StderrPath: "/p/err.log",
	})
	if res.DurationMs != 1500 || res.StdoutPath != "/p/out.log" || res.StderrPath != "/p/err.log" {
		t.Errorf("base fields not carried: %+v", res)
	}
}
