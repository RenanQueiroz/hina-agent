package voice

import (
	"strings"
	"time"
)

// semanticBaseSilence is the short trailing-silence the low-level detector uses in
// semantic mode to report a *candidate* turn end. The Semantic detector then decides
// whether to commit it now (utterance looks complete) or keep waiting (it trails
// off). It must be well under any eagerness maxWait.
const semanticBaseSilence = 480 * time.Millisecond

// completeWait is the trailing silence required to commit a complete-looking
// utterance (a finished sentence shouldn't hang). maxWait (eagerness) bounds how
// long an incomplete-looking one ("umm…") is given to continue before a forced
// commit.
const completeWait = semanticBaseSilence

// eagernessMaxWait maps eagerness to the force-commit cap (research-findings B8).
func eagernessMaxWait(e Eagerness) time.Duration {
	switch e {
	case EagerHigh:
		return 2 * time.Second
	case EagerLow:
		return 8 * time.Second
	default: // medium / auto
		return 4 * time.Second
	}
}

// fillerEndings are trailing tokens that mark an utterance as unfinished — the
// user is thinking, not done. A turn ending in one of these is held open (up to
// maxWait) rather than committed.
var fillerEndings = map[string]bool{
	"um": true, "umm": true, "uh": true, "uhh": true, "uhm": true,
	"er": true, "err": true, "ah": true, "hmm": true, "hm": true,
	"like": true, "so": true, "and": true, "but": true, "or": true,
	"because": true, "the": true, "a": true, "to": true, "of": true,
	"i": true, "well": true, "lemme": true, "let": true,
}

// Semantic is the v1 semantic turn detector: a small, benchmark-driven heuristic
// classifier over the latest partial transcript plus the trailing-silence duration.
// It is deliberately tiny (no model) — it commits a complete-looking utterance fast
// and gives an incomplete-looking one room to finish, bounded by eagerness.
type Semantic struct {
	completeWait time.Duration
	maxWait      time.Duration
}

// NewSemantic builds a detector for the given eagerness.
func NewSemantic(e Eagerness) *Semantic {
	return &Semantic{completeWait: completeWait, maxWait: eagernessMaxWait(e)}
}

// Commit reports whether the turn should be committed now, given the latest partial
// transcript and how long trailing silence has lasted. It commits when the wait
// reaches maxWait (force-commit, regardless of content) or when the utterance looks
// complete and the wait reached completeWait; otherwise it keeps waiting.
func (s *Semantic) Commit(text string, trailingSilence time.Duration) bool {
	if trailingSilence >= s.maxWait {
		return true
	}
	if LooksComplete(text) && trailingSilence >= s.completeWait {
		return true
	}
	return false
}

// MaxWait is the force-commit cap in effect (exposed for the Pipeline timeout).
func (s *Semantic) MaxWait() time.Duration { return s.maxWait }

// LooksComplete reports whether a partial transcript reads like a finished
// utterance: non-empty, and not ending in a filler/conjunction that signals the
// speaker is mid-thought. Sentence-final punctuation is a strong complete signal.
func LooksComplete(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	// A terminal punctuation mark is a clear end.
	switch text[len(text)-1] {
	case '.', '!', '?':
		return true
	}
	fields := strings.Fields(text)
	last := normalizeWord(fields[len(fields)-1])
	if last == "" {
		return false
	}
	// A trailing filler / conjunction / article means the user is still going.
	if fillerEndings[last] {
		return false
	}
	return true
}

// normalizeWord lowercases a token and strips surrounding punctuation so "and,"
// and "And" match the filler set.
func normalizeWord(w string) string {
	w = strings.ToLower(w)
	return strings.TrimFunc(w, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '\''
	})
}
