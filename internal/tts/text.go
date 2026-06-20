package tts

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// defaultLang is used when an Options.Lang is empty or unrecognized. Supertonic
// wraps text in <lang>…</lang> tags that are themselves tokenized, so the tag
// must be a language the model knows; we fall back to English rather than panic
// (the upstream example panics on an unknown lang — unacceptable on a server).
const defaultLang = "en"

// knownLangs is the set of Supertonic language tags. cjkLangs use a shorter
// per-chunk rune cap (denser scripts), matching the reference splitter.
var (
	knownLangs = map[string]bool{
		"en": true, "ko": true, "ja": true, "zh": true, "es": true, "fr": true,
		"de": true, "it": true, "pt": true, "pl": true, "nl": true, "ru": true,
		"tr": true, "ar": true, "hi": true, "id": true, "vi": true, "th": true,
		"cs": true, "da": true, "fi": true, "el": true, "he": true, "hu": true,
		"no": true, "ro": true, "sk": true, "sv": true, "uk": true, "ca": true,
		"fa": true, "na": true,
	}
	cjkLangs = map[string]bool{"ko": true, "ja": true, "zh": true}
)

// resolveLang normalizes a requested language to a known tag, falling back to en.
func resolveLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if knownLangs[lang] {
		return lang
	}
	return defaultLang
}

// maxRunesFor is the per-chunk rune cap for a language (denser CJK scripts get a
// smaller cap, mirroring the upstream 300/120 split).
func maxRunesFor(lang string) int {
	if cjkLangs[lang] {
		return 120
	}
	return 300
}

var (
	emojiRe      = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{2190}-\x{21FF}\x{2B00}-\x{2BFF}\x{FE00}-\x{FE0F}\x{200D}]`)
	whitespaceRe = regexp.MustCompile(`\s+`)
	spaceBefore  = regexp.MustCompile(`\s+([,.!?;:])`)
	dupQuoteRe   = regexp.MustCompile(`"{2,}`)
)

// dash/quote/symbol replacements applied before tokenization. Pairs of
// (from, to); order matters so multi-char sequences are handled first.
var charReplacer = strings.NewReplacer(
	"–", "-", "—", "-", "‑", "-", // en/em/non-breaking dashes
	"_", " ",
	"“", `"`, "”", `"`,
	"‘", "'", "’", "'", "´", "'", "`", "'",
	"[", " ", "]", " ", "|", " ", "/", " ", "#", " ", "→", " ", "←", " ",
	"♥", "", "☆", "", "♡", "", "©", "", "\\", "",
)

// exprReplacer expands a few expressions to spoken form (case-insensitive forms
// handled by lowercasing the search via explicit entries).
var exprReplacer = strings.NewReplacer(
	"@", " at ",
	"e.g.,", "for example, ", "e.g.", "for example",
	"i.e.,", "that is, ", "i.e.", "that is",
)

// preprocessText normalizes one sentence/chunk and wraps it in the language tag
// the model expects. It is deterministic and CGo-free.
func preprocessText(text, lang string) string {
	lang = resolveLang(lang)
	text = norm.NFKD.String(text)
	text = emojiRe.ReplaceAllString(text, "")
	text = exprReplacer.Replace(text)
	text = charReplacer.Replace(text)
	text = spaceBefore.ReplaceAllString(text, "$1")
	text = dupQuoteRe.ReplaceAllString(text, `"`)
	text = whitespaceRe.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	if text == "" {
		text = "."
	} else if !endsWithTerminal(text) {
		text += "."
	}
	return "<" + lang + ">" + text + "</" + lang + ">"
}

// endsWithTerminal reports whether s ends in sentence-terminal punctuation (or a
// closing quote/bracket after it), so preprocess doesn't append a spurious ".".
func endsWithTerminal(s string) bool {
	r := []rune(strings.TrimSpace(s))
	for i := len(r) - 1; i >= 0; i-- {
		switch r[i] {
		case '"', '\'', ')', ']', '}', '»':
			continue // skip trailing closers
		case '.', '!', '?', '…', '。', '！', '？':
			return true
		default:
			return false
		}
	}
	return false
}

// SplitSentences segments text into sentences for streaming synthesis. It is
// decimal-aware: a period between two digits (e.g. "3.14") is NOT a boundary, so
// numbers aren't chopped mid-token. Boundaries are runs of terminal punctuation
// (. ! ? … and CJK variants) followed by whitespace or end-of-text. Returns the
// trimmed, non-empty sentences in order; a text with no terminal punctuation
// yields a single sentence.
func SplitSentences(text string) []string {
	r := []rune(text)
	var out []string
	var cur []rune
	flush := func() {
		s := strings.TrimSpace(string(cur))
		if s != "" {
			out = append(out, s)
		}
		cur = cur[:0]
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		cur = append(cur, c)
		if !isTerminal(c) {
			continue
		}
		// A '.' between digits is a decimal point, not a sentence end.
		if c == '.' && i > 0 && i+1 < len(r) && unicode.IsDigit(r[i-1]) && unicode.IsDigit(r[i+1]) {
			continue
		}
		// Consume any immediately following terminal punctuation ("?!", "...").
		for i+1 < len(r) && isTerminal(r[i+1]) {
			i++
			cur = append(cur, r[i])
		}
		// Boundary only if the terminal is followed by whitespace or end.
		if i+1 >= len(r) || unicode.IsSpace(r[i+1]) {
			flush()
		}
	}
	flush()
	if len(out) == 0 {
		if s := strings.TrimSpace(text); s != "" {
			out = []string{s}
		}
	}
	return out
}

func isTerminal(c rune) bool {
	switch c {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	}
	return false
}

// chunkByRunes splits an over-long sentence into pieces of at most maxRunes,
// preferring to break on whitespace so words aren't cut. A sentence within the
// cap is returned unchanged.
func chunkByRunes(sentence string, maxRunes int) []string {
	r := []rune(sentence)
	if len(r) <= maxRunes {
		return []string{sentence}
	}
	var out []string
	for len(r) > maxRunes {
		// Find the last space at or before the cap; if none, hard-split at the cap.
		split := maxRunes
		for j := maxRunes; j > 0; j-- {
			if unicode.IsSpace(r[j]) {
				split = j
				break
			}
		}
		piece := strings.TrimSpace(string(r[:split]))
		if piece != "" {
			out = append(out, piece)
		}
		r = r[split:]
	}
	if tail := strings.TrimSpace(string(r)); tail != "" {
		out = append(out, tail)
	}
	return out
}

// segments splits raw text into the ordered list of synthesis chunks: sentences
// (decimal-aware), each further capped to the language's per-chunk rune limit.
func segments(text, lang string) []string {
	max := maxRunesFor(resolveLang(lang))
	var out []string
	for _, s := range SplitSentences(text) {
		out = append(out, chunkByRunes(s, max)...)
	}
	return out
}
