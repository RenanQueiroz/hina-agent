// Package agent builds model context from a conversation's canonical turns.
// This is the single context-construction path: text mode uses it now and live
// voice will reuse it verbatim, so the two interaction modes can never drift.
package agent

import (
	"encoding/json"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// charsPerMs estimates spoken text density (~150 wpm ≈ 15 chars/s) to map a voice
// turn's played-audio duration to roughly how many characters of the reply the user
// actually heard before a barge-in. It is deliberately approximate (no TTS word
// timestamps yet); used only to build a CONSERVATIVE heard-prefix for an interrupted
// assistant turn so the model isn't told the assistant said text the user never heard.
const charsPerMs = 0.015

// BuildContext converts a system prompt + a conversation's turns into model
// messages. Only user/assistant canonical text becomes context; system/tool
// turns are projected in later phases. Assistant turns marked errored
// (metadata.error) are excluded: a failed/truncated provider response must not
// poison future prompt history. An assistant voice turn cut short by a barge-in
// (metadata.interrupted) contributes only the prefix the user actually heard (from
// metadata.played_ms) plus an explicit "[interrupted]" marker — never the full
// generated text the user didn't hear.
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
			meta := parseTurnMeta(t.Metadata)
			if meta.Error {
				continue
			}
			content := t.CanonicalText
			// A voice turn cut short DURING PLAYBACK carries played_ms (the heard boundary)
			// — truncate to the heard prefix, INCLUDING played_ms == 0 (a barge-in before
			// any audio played: the user heard nothing, so just "[interrupted]"). A
			// text-mode interrupt — or a voice turn interrupted during generation before
			// playback — has NO played_ms and keeps its (already-partial) canonical text,
			// matching the text path. Presence of played_ms, not its value, is the signal.
			if meta.Interrupted && meta.PlayedMs != nil {
				content = interruptedContent(t.CanonicalText, *meta.PlayedMs)
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: content})
		}
	}
	return msgs
}

// turnMeta is the subset of a turn's metadata BuildContext interprets. PlayedMs is a
// pointer so its PRESENCE (a voice playback interrupt, even at 0 ms) is distinguishable
// from its ABSENCE (a text-mode or pre-playback generation interrupt).
type turnMeta struct {
	Error       bool   `json:"error"`
	Interrupted bool   `json:"interrupted"`
	PlayedMs    *int64 `json:"played_ms"`
}

func parseTurnMeta(metadata string) turnMeta {
	var m turnMeta
	if metadata == "" || metadata == "{}" {
		return m
	}
	_ = json.Unmarshal([]byte(metadata), &m)
	return m
}

// interruptedContent returns the heard-prefix (word-aligned) of an interrupted
// assistant reply plus an [interrupted] marker, so the model sees what the user
// actually heard and that the reply was cut off — never the unheard tail. An empty
// or unknown played duration yields just the marker.
func interruptedContent(text string, playedMs int64) string {
	prefix := heardPrefix(text, playedMs)
	if prefix == "" {
		return "[interrupted]"
	}
	return prefix + " [interrupted]"
}

// heardPrefix estimates the leading portion of text the user heard in playedMs of
// playback, trimmed back to a word boundary so it never cuts mid-word.
func heardPrefix(text string, playedMs int64) string {
	text = strings.TrimSpace(text)
	if playedMs <= 0 || text == "" {
		return ""
	}
	n := int(float64(playedMs) * charsPerMs)
	if n >= len(text) {
		return text
	}
	if n <= 0 {
		return ""
	}
	// Trim back to the last word boundary at or before n (no mid-word cut).
	cut := strings.LastIndexByte(text[:n], ' ')
	if cut <= 0 {
		cut = n
	}
	return strings.TrimSpace(text[:cut])
}
