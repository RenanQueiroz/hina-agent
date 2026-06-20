package asr

import (
	"strings"
	"unicode"
)

// attentionWords are optional leading address words allowed before the agent
// name ("hey Hina", "ok Hina"). They are consumed only when the name follows, so
// a request that merely starts with "hi" isn't mangled.
var attentionWords = map[string]bool{
	"hey": true, "ok": true, "okay": true, "hi": true, "hello": true, "yo": true,
}

// WakeMatcher detects and strips a leading address to the agent (the configured
// name or an alias) from a transcript, case-insensitively, BEFORE the text would
// feed the LLM. Stripping the address at the session layer means a single
// mis-transcription only costs wake detection for that turn — the request body is
// preserved either way (research-findings B3 / phase-05 §7). An empty matcher
// detects nothing and returns the text untouched.
type WakeMatcher struct {
	// phrases are the lower-cased address phrases (name + aliases), each split
	// into words, sorted longest-first so a multi-word alias wins over a prefix.
	phrases [][]string
}

// NewWakeMatcher builds a matcher from the agent name and its aliases. Blank
// entries are ignored; a matcher with no usable phrases is inert.
func NewWakeMatcher(name string, aliases []string) *WakeMatcher {
	w := &WakeMatcher{}
	add := func(s string) {
		words := wordsLower(s)
		if len(words) > 0 {
			w.phrases = append(w.phrases, words)
		}
	}
	add(name)
	for _, a := range aliases {
		add(a)
	}
	// Longest phrase first so "okay computer" beats a bare "computer" alias.
	for i := 1; i < len(w.phrases); i++ {
		for j := i; j > 0 && len(w.phrases[j]) > len(w.phrases[j-1]); j-- {
			w.phrases[j], w.phrases[j-1] = w.phrases[j-1], w.phrases[j]
		}
	}
	return w
}

// Enabled reports whether the matcher has any address phrases.
func (w *WakeMatcher) Enabled() bool { return w != nil && len(w.phrases) > 0 }

// wtok is one transcript word with its byte span in the original string.
type wtok struct {
	lower      string
	start, end int
}

// Strip detects a leading address (optionally preceded by one attention word)
// and removes it, returning whether the agent was addressed and the remaining
// request body (original casing, trimmed). When no address is found it returns
// (false, trimmed text) so the request is never corrupted by a failed match.
func (w *WakeMatcher) Strip(text string) (detected bool, body string) {
	if !w.Enabled() {
		return false, strings.TrimSpace(text)
	}
	toks := tokenize(text)
	if len(toks) == 0 {
		return false, strings.TrimSpace(text)
	}
	// Try matching the name at the very start, then after a single attention word.
	for _, startIdx := range []int{0, 1} {
		if startIdx == 1 && !(len(toks) > 1 && attentionWords[toks[0].lower]) {
			continue
		}
		if n := w.matchAt(toks, startIdx); n > 0 {
			last := toks[startIdx+n-1]
			return true, strings.TrimSpace(trimLeadingSep(text[last.end:]))
		}
	}
	return false, strings.TrimSpace(text)
}

// matchAt returns the number of tokens a phrase occupies if one matches starting
// at toks[i], else 0. Phrases are pre-sorted longest-first.
func (w *WakeMatcher) matchAt(toks []wtok, i int) int {
	for _, ph := range w.phrases {
		if i+len(ph) > len(toks) {
			continue
		}
		ok := true
		for k, word := range ph {
			if toks[i+k].lower != word {
				ok = false
				break
			}
		}
		if ok {
			return len(ph)
		}
	}
	return 0
}

// tokenize splits s into word tokens (letters/digits, with intra-word
// apostrophes), recording each token's byte span and a lower-cased form.
// Punctuation and whitespace are separators.
func tokenize(s string) []wtok {
	var toks []wtok
	start := -1
	flush := func(end int) {
		if start >= 0 {
			toks = append(toks, wtok{lower: strings.ToLower(s[start:end]), start: start, end: end})
			start = -1
		}
	}
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (r == '\'' && start >= 0) {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(s))
	return toks
}

// wordsLower tokenizes s into its lower-cased word list (no spans).
func wordsLower(s string) []string {
	toks := tokenize(s)
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.lower
	}
	return out
}

// trimLeadingSep drops leading separator characters (the punctuation/space that
// followed the address, e.g. the comma in "Hina, ...").
func trimLeadingSep(s string) string {
	return strings.TrimLeftFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
