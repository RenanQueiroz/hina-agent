package autorun

import (
	"encoding/json"
	"strings"
	"testing"
)

// intArg must parse PR numbers STRICTLY: a fractional value must NOT silently truncate to a
// different PR (which would post a comment / check out the wrong pull request) (round-82).
func TestIntArgStrict(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(7), 7},
		{float64(7.9), 0}, // must NOT become 7
		{float64(0), 0},
		{float64(-3), 0},
		{"5", 5},
		{" 5 ", 5},
		{"7.9", 0},
		{"abc", 0},
		{json.Number("7"), 7},
		{json.Number("7.9"), 0},
		{map[string]any{}, 0},
	}
	for _, c := range cases {
		if got := intArg(c.in); got != c.want {
			t.Errorf("intArg(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The PR tools must REFUSE a non-integral PR number rather than truncate it to a different PR.
func TestPRToolsRejectNonIntegralPR(t *testing.T) {
	if _, err := buildPRComment(map[string]any{"repo": "o/r", "pr": float64(7.9), "body": "x"}); err == nil {
		t.Fatal("github.pr_comment must reject a non-integral pr (not truncate to #7)")
	}
	if _, err := buildPRCheckout(map[string]any{"repo": "o/r", "pr": float64(7.9)}); err == nil {
		t.Fatal("github.pr_checkout must reject a non-integral pr")
	}
}

func TestBuildNotificationsArgv(t *testing.T) {
	op, err := buildToolOp("github.notifications", map[string]any{
		"reasons":               []any{"review_requested"},
		"include_participating": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(op.argv, " ")
	if !strings.HasPrefix(joined, "gh api") || !strings.Contains(joined, "participating=true") {
		t.Fatalf("argv = %q", joined)
	}
	if len(op.network) != 1 || op.network[0].Host != "api.github.com" {
		t.Fatalf("network = %+v", op.network)
	}
}

func TestParseNotificationsFiltersByReason(t *testing.T) {
	out := `[
	  {"id":"1","reason":"review_requested","subject":{"title":"Fix","type":"PullRequest","url":"https://api.github.com/repos/o/r/pulls/12"},"repository":{"full_name":"o/r"}},
	  {"id":"2","reason":"mention","subject":{"title":"Hi","type":"Issue","url":"https://api.github.com/repos/o/r/issues/5"},"repository":{"full_name":"o/r"}}
	]`
	items, err := parseNotifications(out, []string{"review_requested"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (filtered)", len(items))
	}
	m := items[0].(map[string]any)
	if m["pr"] != 12 || m["repository"] != "o/r" {
		t.Fatalf("item = %+v", m)
	}
}

func TestParseNotificationsEmpty(t *testing.T) {
	items, err := parseNotifications("", nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("empty = %v %v", items, err)
	}
}

func TestPRCheckoutArgv(t *testing.T) {
	op, err := buildToolOp("github.pr_checkout", map[string]any{
		"notification": map[string]any{"repository": "o/r", "pr": float64(42)},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(op.argv, " ")
	if !strings.Contains(joined, "gh repo clone 'o/r'") || !strings.Contains(joined, "gh pr checkout 42") {
		t.Fatalf("argv = %q", joined)
	}
	out, _ := op.parse("")
	m := out.(map[string]any)
	if m["workspace"] != "/workspace/pr-42" || m["pr"] != 42 {
		t.Fatalf("out = %+v", m)
	}
}

func TestPRCommentBodyViaStdin(t *testing.T) {
	op, err := buildToolOp("github.pr_comment", map[string]any{
		"repo": "o/r", "pr": float64(7), "body": "looks good",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(op.stdin) != "looks good" {
		t.Fatalf("stdin = %q", op.stdin)
	}
	// The body must NOT appear on the argv (it would hit the host command line).
	if strings.Contains(strings.Join(op.argv, " "), "looks good") {
		t.Fatal("body leaked onto argv")
	}
	if !strings.Contains(strings.Join(op.argv, " "), "--body-file -") {
		t.Fatalf("argv = %q", op.argv)
	}
}

func TestHTTPRequestArgvAndNetwork(t *testing.T) {
	op, err := buildToolOp("http.request", map[string]any{
		"method": "post", "url": "https://example.com:8443/x", "body": "hi",
		"headers": []any{"Accept: application/json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.network[0].Host != "example.com" || op.network[0].Port != 8443 {
		t.Fatalf("network = %+v", op.network)
	}
	if string(op.stdin) != "hi" {
		t.Fatalf("stdin = %q", op.stdin)
	}
}

func TestShellExecArgvFirst(t *testing.T) {
	op, err := buildToolOp("shell.exec", map[string]any{"argv": []any{"ls", "-la"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.argv) != 2 || op.argv[0] != "ls" {
		t.Fatalf("argv = %+v", op.argv)
	}
	if shellExecNeedsUnrestricted(map[string]any{"argv": []any{"ls"}}) {
		t.Error("argv form should not need unrestricted")
	}
	if !shellExecNeedsUnrestricted(map[string]any{"command": "ls | wc"}) {
		t.Error("command string should need unrestricted")
	}
}

func TestMCPCallUnavailable(t *testing.T) {
	if _, err := buildToolOp("mcp.call", map[string]any{}); err == nil {
		t.Fatal("mcp.call should report unavailable in v1")
	}
}

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{"github_token": "GITHUB_TOKEN", "my-key": "MY_KEY", "1abc": "SECRET_1ABC"}
	for in, want := range cases {
		if got := envVarName(in); got != want {
			t.Errorf("envVarName(%q) = %q, want %q", in, got, want)
		}
	}
}
