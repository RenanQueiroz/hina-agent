package agentauth

import (
	"regexp"
	"strings"
)

// maxLineLen caps a streamed login line so a runaway CLI (e.g. a progress bar
// repainting megabytes) can't flood the frontend.
const maxLineLen = 4096

// ansiRe matches ANSI/VT100 escape sequences (CSI ... and a few others) so the
// streamed login view is plain text the browser can render safely.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][AB012]|\x1b[=>]|\x1b\][^\x07]*\x07?`)

// Sanitize strips ANSI escape sequences and other control characters from a line of
// login output, leaving readable plain text (tabs and printable runes only), capped
// at maxLineLen. It is pure and fully unit-tested.
func Sanitize(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r == '\r':
			// drop carriage returns (a CLI progress redraw)
		case r < 0x20 || r == 0x7f:
			// drop other control characters
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxLineLen {
		out = out[:maxLineLen] + "…"
	}
	return strings.TrimRight(out, " ")
}
