package agentauth

import (
	"strings"
	"testing"
)

func TestDetectURL(t *testing.T) {
	hints := Detect("Open https://github.com/login/device to continue.")
	if len(hints) != 1 || hints[0].Kind != HintURL || hints[0].Value != "https://github.com/login/device" {
		t.Fatalf("url detect = %+v", hints)
	}
}

func TestDetectTrimsTrailingPunct(t *testing.T) {
	hints := Detect("visit https://claude.ai/auth?x=1.")
	if len(hints) != 1 || hints[0].Value != "https://claude.ai/auth?x=1" {
		t.Fatalf("expected trailing dot trimmed, got %+v", hints)
	}
}

func TestDetectDeviceCode(t *testing.T) {
	hints := Detect("Your one-time code is ABCD-1234")
	if !hasHint(hints, HintCode, "ABCD-1234") {
		t.Fatalf("device code not detected: %+v", hints)
	}
}

func TestDetectKeyedCode(t *testing.T) {
	hints := Detect("Enter the code: A1B2C3D4 in your browser")
	if !hasHint(hints, HintCode, "A1B2C3D4") {
		t.Fatalf("keyed code not detected: %+v", hints)
	}
	// And the line is a paste prompt.
	if !hasHint(hints, HintPrompt, "") {
		t.Fatalf("expected a prompt hint: %+v", hints)
	}
}

func TestDetectPastePrompt(t *testing.T) {
	for _, line := range []string{
		"Paste the authorization code here:",
		"Enter your API token:",
	} {
		if !hasHintKind(Detect(line), HintPrompt) {
			t.Errorf("expected a prompt hint for %q", line)
		}
	}
}

func TestDetectNoFalsePositive(t *testing.T) {
	hints := Detect("Starting login flow...")
	if len(hints) != 0 {
		t.Fatalf("unexpected hints on a plain line: %+v", hints)
	}
}

func TestSanitizeStripsANSI(t *testing.T) {
	in := "\x1b[32mLogged in\x1b[0m as \x1b[1malice\x1b[0m"
	if got := Sanitize(in); got != "Logged in as alice" {
		t.Fatalf("sanitize = %q", got)
	}
}

func TestSanitizeStripsControlAndCaps(t *testing.T) {
	// The bell control char is dropped; the tab becomes a space; the CR is dropped.
	if got := Sanitize("a\x07b\tc\r"); got != "ab c" {
		t.Fatalf("control strip = %q", got)
	}
	long := strings.Repeat("x", maxLineLen+100)
	if got := Sanitize(long); len(got) <= maxLineLen || !strings.HasSuffix(got, "…") {
		t.Fatalf("cap not applied: len=%d", len(got))
	}
}

func hasHint(hints []Hint, kind HintKind, val string) bool {
	for _, h := range hints {
		if h.Kind == kind && (val == "" || h.Value == val) {
			return true
		}
	}
	return false
}

func hasHintKind(hints []Hint, kind HintKind) bool { return hasHint(hints, kind, "") }
