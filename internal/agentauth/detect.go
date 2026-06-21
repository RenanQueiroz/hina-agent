// Package agentauth is the per-user agent auth broker (Phase 8). It runs a provider
// CLI's interactive login inside a short-lived, network-on `sbx` auth container with
// the user's encrypted agent-state mounted, streams a SANITIZED view of the login
// output to the frontend (highlighting login URLs and device/paste codes), feeds
// pasted codes back to the container's stdin, and — on success — confirms with the
// provider's status command and records the profile. Device-code / paste-code are
// the mandatory fallback because a localhost browser callback cannot reach into the
// sandbox container (Phase 8 plan).
//
// The powerful bearer material the login produces never leaves the vault boundary:
// it lives only in the encrypted agent-state blob and is re-encrypted after login;
// the broker streams output but never persists a token/URL/code anywhere but the
// transient live stream, and the admin/user UI only ever sees a coarse status.
package agentauth

import (
	"regexp"
	"strings"
)

// HintKind classifies a piece of actionable information detected in login output.
type HintKind string

const (
	// HintURL is a login/verification URL the user should open in a new tab.
	HintURL HintKind = "url"
	// HintCode is a device/verification code the user must enter at that URL.
	HintCode HintKind = "code"
	// HintPrompt is a line asking the user to paste something back (the frontend
	// shows an input box wired to WriteInput).
	HintPrompt HintKind = "prompt"
)

// Hint is one actionable item surfaced from a sanitized login line.
type Hint struct {
	Kind  HintKind `json:"kind"`
	Value string   `json:"value"`
}

var (
	urlRe = regexp.MustCompile(`https?://[^\s'"]+`)
	// A device code like ABCD-1234 / WXYZ-EFGH (two+ alphanumeric groups joined by
	// hyphens), the common one-time-code shape across these CLIs.
	deviceCodeRe = regexp.MustCompile(`\b[A-Z0-9]{4,}(?:-[A-Z0-9]{4,})+\b`)
	// A code introduced by an explicit "code" keyword: "code: ABC123XYZ".
	keyedCodeRe   = regexp.MustCompile(`(?i)\bcode[:\s]+([A-Za-z0-9]{6,})\b`)
	trailingPunct = ".,;:!?)]}>'\""
)

// Detect extracts actionable hints (URLs, codes, paste prompts) from one already-
// sanitized line of login output. It is pure so the detection rules are fully
// unit-tested without a real login.
func Detect(line string) []Hint {
	var hints []Hint
	for _, u := range urlRe.FindAllString(line, -1) {
		hints = append(hints, Hint{Kind: HintURL, Value: strings.TrimRight(u, trailingPunct)})
	}
	for _, c := range deviceCodeRe.FindAllString(line, -1) {
		hints = append(hints, Hint{Kind: HintCode, Value: c})
	}
	for _, m := range keyedCodeRe.FindAllStringSubmatch(line, -1) {
		// Avoid duplicating a hyphenated code already caught by deviceCodeRe.
		if !containsHint(hints, HintCode, m[1]) {
			hints = append(hints, Hint{Kind: HintCode, Value: m[1]})
		}
	}
	if isPastePrompt(line) {
		hints = append(hints, Hint{Kind: HintPrompt, Value: strings.TrimSpace(line)})
	}
	return hints
}

// isPastePrompt reports whether a line is asking the user to paste/enter a code or
// token back into the CLI (so the frontend should show an input box).
func isPastePrompt(line string) bool {
	l := strings.ToLower(line)
	hasTarget := strings.Contains(l, "code") || strings.Contains(l, "token") || strings.Contains(l, "key")
	if !hasTarget {
		return false
	}
	for _, verb := range []string{"paste", "enter the", "enter your", "enter code", "input the"} {
		if strings.Contains(l, verb) {
			return true
		}
	}
	// A line that ends with a prompt colon after asking about a code/token.
	return strings.HasSuffix(strings.TrimSpace(l), ":") && (strings.Contains(l, "paste") || strings.Contains(l, "enter"))
}

func containsHint(hints []Hint, kind HintKind, val string) bool {
	for _, h := range hints {
		if h.Kind == kind && (h.Value == val || strings.Contains(h.Value, val)) {
			return true
		}
	}
	return false
}
