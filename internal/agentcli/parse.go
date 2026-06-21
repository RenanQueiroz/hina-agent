package agentcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// jsonObjects scans s for top-level JSON objects, one per line (the JSONL format
// every CLI's --json / stream-json mode emits), and returns each successfully
// decoded object as a generic map. Non-JSON lines (a CLI banner, a progress dot)
// are skipped rather than failing the whole parse — the parsers here are tolerant
// by design because the exact event schema drifts between releases.
func jsonObjects(s string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// firstJSONObject returns the first top-level JSON object in s (the shape Claude's
// and Cursor's --output-format json single-result mode emits, possibly preceded by
// non-JSON banner lines), or nil if none decodes.
func firstJSONObject(s string) map[string]any {
	objs := jsonObjects(s)
	if len(objs) == 0 {
		// --output-format json may emit a single multi-line pretty object rather than
		// one-per-line; try decoding the whole trimmed blob as a last resort.
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &obj); err == nil {
			return obj
		}
		return nil
	}
	return objs[0]
}

// str returns m[key] as a string ("" if absent or not a string).
func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// num returns m[key] as a float64 (0 if absent or not a number). JSON numbers
// decode to float64 in a map[string]any.
func num(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// obj returns m[key] as a nested object (nil if absent or not an object).
func obj(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// usageTokens pulls input/output token counts from a usage-like object, tolerating
// the two common key spellings (input_tokens/output_tokens and prompt_tokens/
// completion_tokens).
func usageTokens(u map[string]any) (in, out int) {
	if u == nil {
		return 0, 0
	}
	pick := func(keys ...string) int {
		for _, k := range keys {
			if v := num(u, k); v != 0 {
				return int(v)
			}
		}
		return 0
	}
	in = pick("input_tokens", "prompt_tokens")
	out = pick("output_tokens", "completion_tokens")
	return in, out
}

// lastNonEmptyLine returns the last non-blank line of s, trimmed. It is the
// fallback for extracting a final message when no structured event carried it.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// classifyStatus maps a RawResult's execution flags + exit code to a normalized
// status. TimedOut/Cancelled take precedence over the exit code (a killed process
// usually exits non-zero, but the reason is the kill, not a task failure).
func classifyStatus(raw RawResult, isError bool) string {
	switch {
	case raw.Cancelled:
		return StatusCancelled
	case raw.TimedOut:
		return StatusTimeout
	case isError || raw.ExitCode != 0:
		return StatusError
	default:
		return StatusOK
	}
}

// baseResult seeds an AgentRunResult with the fields common to every adapter from
// the raw run outcome (provider + exit/timing + captured-output paths).
func baseResult(p Provider, raw RawResult) AgentRunResult {
	return AgentRunResult{
		Provider:   p,
		ExitCode:   raw.ExitCode,
		DurationMs: raw.Duration.Milliseconds(),
		StdoutPath: raw.StdoutPath,
		StderrPath: raw.StderrPath,
	}
}

// finalizeStatus sets Status (and a default Err message when the run did not
// succeed) from the run flags plus the CLI-reported error signal. Adapters call it
// last, after extracting the final text/usage, so a timeout/cancel/non-zero exit is
// reflected consistently across every provider. A non-empty res.Err is preserved.
func finalizeStatus(res *AgentRunResult, raw RawResult, isError bool) {
	res.Status = classifyStatus(raw, isError)
	if res.Err != "" {
		return
	}
	switch res.Status {
	case StatusTimeout:
		res.Err = "the agent run timed out"
	case StatusCancelled:
		res.Err = "the agent run was cancelled"
	case StatusError:
		res.Err = errSummary(raw)
	}
}

// errSummary builds a short error message for a failed run: the last non-empty
// stderr line when present, else a generic non-zero-exit note.
func errSummary(raw RawResult) string {
	if s := lastNonEmptyLine(raw.Stderr); s != "" {
		return s
	}
	return fmt.Sprintf("the agent exited with code %d", raw.ExitCode)
}
