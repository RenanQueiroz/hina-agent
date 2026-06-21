package voice

import "strings"

// defaultBackchannels are short acknowledgements a listener utters while the
// assistant talks ("yeah", "uh-huh") that should NOT interrupt it. NeMo's idea
// (research-findings / phase-06): filter these during assistant speech, but
// interrupt as soon as real content accumulates.
var defaultBackchannels = []string{
	"yeah", "yep", "yes", "yup", "ok", "okay", "k", "right", "sure",
	"uh-huh", "uhhuh", "uh huh", "mhm", "mmhm", "mm-hmm", "mmhmm", "mm", "hmm",
	"cool", "nice", "thanks", "thank you", "got it", "i see", "gotcha", "totally", "exactly",
}

// Backchannel decides whether a partial transcript heard while the assistant is
// speaking is a mere backchannel (ignore) or a real interruption (barge in). It is
// configurable (phrase list + the non-backchannel word count that confirms an
// interruption) and can be disabled for aggressive interruption.
type Backchannel struct {
	phrases  map[string]bool // normalized backchannel phrases (single- and multi-word)
	maxWords int             // longest phrase in words (bounds the all-backchannel check)
	minWords int             // >= this many non-backchannel words confirms an interruption
	enabled  bool
}

// NewBackchannel builds a filter. Empty phrases use the defaults; minWords <= 0
// falls back to 2. enabled=false makes every non-empty partial an interruption
// (aggressive mode).
func NewBackchannel(phrases []string, minWords int, enabled bool) *Backchannel {
	if len(phrases) == 0 {
		phrases = defaultBackchannels
	}
	if minWords <= 0 {
		minWords = 2
	}
	b := &Backchannel{phrases: make(map[string]bool), minWords: minWords, enabled: enabled}
	for _, p := range phrases {
		norm := normalizePhrase(p)
		if norm == "" {
			continue
		}
		b.phrases[norm] = true
		if n := len(strings.Fields(norm)); n > b.maxWords {
			b.maxWords = n
		}
	}
	return b
}

// IsBackchannel reports whether text is purely backchannel acknowledgement(s) — so
// it should not interrupt the assistant. Empty text is treated as backchannel
// (nothing meaningful was said). When the filter is disabled, only empty text is a
// backchannel.
func (b *Backchannel) IsBackchannel(text string) bool {
	words := tokenize(text)
	if len(words) == 0 {
		return true
	}
	if !b.enabled {
		return false
	}
	return b.nonBackchannelCount(words) == 0
}

// Interrupts reports whether a partial transcript heard during assistant speech
// should confirm a barge-in: it carries at least minWords non-backchannel words.
// When disabled, any non-empty partial interrupts.
func (b *Backchannel) Interrupts(text string) bool {
	words := tokenize(text)
	if len(words) == 0 {
		return false
	}
	if !b.enabled {
		return true
	}
	return b.nonBackchannelCount(words) >= b.minWords
}

// nonBackchannelCount returns how many words remain after greedily consuming known
// backchannel phrases (longest-first), so multi-word phrases ("uh huh", "thank
// you") are matched as a unit and don't inflate the count.
func (b *Backchannel) nonBackchannelCount(words []string) int {
	n := 0
	for i := 0; i < len(words); {
		matched := 0
		for w := min(b.maxWords, len(words)-i); w >= 1; w-- {
			if b.phrases[strings.Join(words[i:i+w], " ")] {
				matched = w
				break
			}
		}
		if matched == 0 {
			n++
			i++
		} else {
			i += matched
		}
	}
	return n
}

// tokenize lowercases and splits text into alphanumeric word tokens (punctuation
// stripped), so "Yeah," and "yeah" match.
func tokenize(text string) []string {
	fields := strings.Fields(text)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if w := normalizeWord(f); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// normalizePhrase lowercases + collapses a backchannel phrase to space-joined
// word tokens so list entries and transcripts normalize identically.
func normalizePhrase(p string) string { return strings.Join(tokenize(p), " ") }
