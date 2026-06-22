package autorun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
)

// maxAssistAttempts bounds the LLM-assisted creation retry loop: explain the schema,
// ask for JSON, validate, feed the validation errors back, retry — up to this many
// times. A draft is ALWAYS returned for human review even if it never fully validates.
const maxAssistAttempts = 4

// maxAssistOutputBytes bounds one assist draft (an automation.v1 document is small;
// this caps a runaway model response).
const maxAssistOutputBytes = 512 << 10

// AssistResult is the outcome of an LLM-assisted draft. Definition is ALWAYS syntactically
// valid JSON (the canonical draft, or "null" when even the raw model output didn't parse) —
// the unparsed text is surfaced in RawText so the API response can't be poisoned.
type AssistResult struct {
	Definition json.RawMessage
	RawText    string // the model's raw (unparsed) output, when Definition couldn't be parsed
	Valid      bool
	Issues     *automation.ValidationErrors
	Attempts   int
}

// Assist drafts an automation.v1 document from a natural-language request using the
// active LLM, validating after each attempt and feeding the structural errors back
// to the model on a retry. It returns the best draft for the user to REVIEW before
// enabling (schema-valid is not the same as safe). It never enables anything.
func (s *Service) Assist(ctx context.Context, prompt string) (AssistResult, error) {
	if s.cfg.Exec.Provider == nil {
		return AssistResult{}, fmt.Errorf("no LLM provider is configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return AssistResult{}, fmt.Errorf("a description is required")
	}

	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: automation.SchemaGuide()},
		{Role: llm.RoleUser, Content: "Create an automation for this request:\n" + prompt},
	}

	var best AssistResult
	for attempt := 1; attempt <= maxAssistAttempts; attempt++ {
		text, _, err := streamText(ctx, s.cfg.Exec.Provider, msgs, maxAssistOutputBytes)
		if err != nil {
			if best.Definition != nil {
				return best, nil
			}
			return AssistResult{}, err
		}
		raw := extractJSON(text)

		def, perr := automation.Parse([]byte(raw))
		if perr != nil {
			// Not even parseable — keep Definition syntactically valid (null), surface the raw
			// text + parse error so the API stays valid JSON, then feed the error back and retry.
			best = AssistResult{
				Definition: json.RawMessage("null"),
				RawText:    raw,
				Issues:     &automation.ValidationErrors{Issues: []automation.Issue{{Path: "definition", Message: "not valid JSON: " + perr.Error()}}},
				Attempts:   attempt,
			}
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: "That was not valid JSON for the schema (" + perr.Error() + "). Return ONLY a corrected automation.v1 JSON object."},
			)
			continue
		}
		def.Enabled = false
		// Re-serialize the normalized definition so the user always sees canonical JSON.
		canonDraft, _ := def.MarshalForStore()
		best = AssistResult{Definition: json.RawMessage(canonDraft), Attempts: attempt}
		if verrs := def.Validate(); verrs != nil {
			best.Issues = verrs
			msgs = append(msgs,
				llm.Message{Role: llm.RoleAssistant, Content: raw},
				llm.Message{Role: llm.RoleUser, Content: "The document has validation errors. Fix EXACTLY these and return ONLY the corrected JSON:\n" + verrs.Error()},
			)
			continue
		}
		// Valid — return the canonical form.
		canon := canonDraft
		return AssistResult{Definition: json.RawMessage(canon), Valid: true, Attempts: attempt}, nil
	}
	return best, nil
}

// extractJSON pulls the first complete JSON object out of a model reply (which may
// wrap it in a code fence or prose).
func extractJSON(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	start := strings.IndexByte(t, '{')
	end := strings.LastIndexByte(t, '}')
	if start < 0 || end <= start {
		return strings.TrimSpace(t)
	}
	return strings.TrimSpace(t[start : end+1])
}
