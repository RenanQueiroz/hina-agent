// Package agent builds model context from a conversation's canonical turns.
// This is the single context-construction path: text mode uses it now and live
// voice will reuse it verbatim, so the two interaction modes can never drift.
package agent

import (
	"encoding/json"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// BuildContext converts a system prompt + a conversation's turns into model
// messages. Only user/assistant canonical text becomes context; system/tool
// turns are projected in later phases. Assistant turns marked errored
// (metadata.error) are excluded: a failed/truncated provider response must not
// poison future prompt history, even though its partial text is kept for UI
// replay (carried by the turn + its ErrorEvent).
func BuildContext(systemPrompt string, turns []store.Turn) []llm.Message {
	msgs := make([]llm.Message, 0, len(turns)+1)
	if systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}
	for _, t := range turns {
		switch t.Role {
		case "user":
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: t.CanonicalText})
		case "assistant":
			if turnErrored(t.Metadata) {
				continue
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: t.CanonicalText})
		}
	}
	return msgs
}

// turnErrored reports whether a turn's metadata marks it as a failed turn.
func turnErrored(metadata string) bool {
	if metadata == "" || metadata == "{}" {
		return false
	}
	var m struct {
		Error bool `json:"error"`
	}
	_ = json.Unmarshal([]byte(metadata), &m)
	return m.Error
}
