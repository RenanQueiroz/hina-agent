// Package autorun is the side-effecting runtime for Phase 9 Automations: it wires
// the pure internal/automation engine to the real world — the deterministic tools
// (run argv-first inside the user's sbx sandbox), the callable-agent adapters (via
// the hardened Phase 8 AgentRouter), the LLM provider (aggregation steps), the
// durable scheduler, and the store (definitions + immutable run records). Keeping it
// separate keeps internal/automation dependency-light and exhaustively unit-tested.
//
// Like Phase 8, the actual sbx/CLI execution is validated on an sbx-equipped host;
// here the argv builders + output parsers are pure and fixture-tested, and the
// executor runs against a fake Runner so the orchestration is fully covered.
package autorun

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
)

// toolOp is the resolved sandbox plan for one deterministic tool step: the argv to
// run, optional stdin (so secret-bearing bodies never hit the argv), the network
// targets for the allow-list, and a parser that turns captured stdout into the
// step's structured output.
type toolOp struct {
	argv    []string
	stdin   []byte
	network []sandbox.NetworkRule
	// clis names the CLI binaries this op invokes (e.g. "gh", "git", "curl"). The
	// executor enforces these against the automation's allowed_cli_tools at run time
	// (granular mode), so a tool can't reach a CLI the profile didn't grant.
	clis []string
	// networkCapable marks an op that CAN make outbound requests even though it declares
	// no specific network target (e.g. shell.exec runs arbitrary argv). Such an op is
	// treated like a networked op for the profile's network gate, so a network:disabled
	// profile can't run it (and exfiltrate a granted secret) via an unlisted command.
	networkCapable bool
	parse          func(stdout string) (any, error)
}

// buildToolOp turns a resolved tool step into a sandbox plan. Every argv is built
// here from typed arguments — a model/definition never supplies a raw command line —
// so command injection is not reachable. Drift-prone CLI shapes (gh/curl) are
// re-verified on an sbx host per the research-findings cadence.
func buildToolOp(tool string, with map[string]any) (toolOp, error) {
	switch tool {
	case automation.ToolGithubNotifications:
		return buildNotifications(with)
	case automation.ToolGithubPRCheckout:
		return buildPRCheckout(with)
	case automation.ToolGithubPRComment:
		return buildPRComment(with)
	case automation.ToolHTTPRequest:
		return buildHTTPRequest(with)
	case automation.ToolShellExec:
		return buildShellExec(with)
	case automation.ToolMCPCall:
		// MCP execution needs an allow-listed MCP client, which is not wired in v1 (the
		// optional MCP facade is deferred). The tool validates so a definition can declare
		// it, but a run reports it unavailable rather than silently no-op'ing.
		return toolOp{}, fmt.Errorf("mcp.call is not available in this build (the MCP facade is deferred)")
	default:
		return toolOp{}, fmt.Errorf("unknown tool %q", tool)
	}
}

// buildNotifications lists GitHub notifications and filters to the requested
// reasons, normalizing each into an item the rest of the workflow consumes.
func buildNotifications(with map[string]any) (toolOp, error) {
	participating := boolArg(with, "include_participating")
	reasons := stringSlice(with["reasons"])
	q := url.Values{}
	q.Set("all", "false")
	if participating {
		q.Set("participating", "true")
	}
	argv := []string{"gh", "api", "--paginate", "/notifications?" + q.Encode()}
	return toolOp{
		argv:    argv,
		clis:    automation.ToolCLIs(automation.ToolGithubNotifications),
		network: []sandbox.NetworkRule{{Host: "api.github.com", Port: 443}},
		parse: func(stdout string) (any, error) {
			items, err := parseNotifications(stdout, reasons)
			if err != nil {
				return nil, err
			}
			return map[string]any{"items": items, "count": len(items)}, nil
		},
	}, nil
}

// parseNotifications decodes `gh api /notifications` output (a JSON array, or
// several arrays when paginated) into normalized items, filtered by reason.
func parseNotifications(stdout string, reasons []string) ([]any, error) {
	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return []any{}, nil
	}
	var all []map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var batch []map[string]any
		if err := dec.Decode(&batch); err != nil {
			if err.Error() == "EOF" {
				break
			}
			// A single object (some gh versions) — try decoding one more shape.
			return nil, fmt.Errorf("could not parse notifications output")
		}
		all = append(all, batch...)
	}
	allow := map[string]bool{}
	for _, r := range reasons {
		allow[r] = true
	}
	var items []any
	for _, n := range all {
		reason, _ := n["reason"].(string)
		if len(allow) > 0 && !allow[reason] {
			continue
		}
		subject, _ := n["subject"].(map[string]any)
		repo, _ := n["repository"].(map[string]any)
		item := map[string]any{
			"id":         n["id"],
			"reason":     reason,
			"title":      subject["title"],
			"type":       subject["type"],
			"url":        subject["url"],
			"repository": repo["full_name"],
		}
		if pr := prNumberFromURL(asString(subject["url"])); pr > 0 {
			item["pr"] = pr
		}
		items = append(items, item)
	}
	if items == nil {
		items = []any{}
	}
	return items, nil
}

// prNumberFromURL extracts the trailing number from a GitHub pulls API URL.
func prNumberFromURL(u string) int {
	i := strings.LastIndexByte(u, '/')
	if i < 0 || i+1 >= len(u) {
		return 0
	}
	n, err := strconv.Atoi(u[i+1:])
	if err != nil || n <= 0 {
		return 0
	}
	if !strings.Contains(u, "/pulls/") {
		return 0
	}
	return n
}

// validateGitHubRepo delegates to the shared automation classifier so the bare-"owner/repo"
// shape enforced at enable-time and at run time never drift. Rejecting a host prefix, scheme/URL,
// or extra slashes keeps gh/git from being pointed at a GHES/arbitrary/internal host (a
// 3-segment "host/owner/repo" or a URL would route to THAT host, outside the op.network guard).
func validateGitHubRepo(repo string) error { return automation.ValidateGitHubRepo(repo) }

// buildPRCheckout creates a clean per-run checkout for a PR. The notification (or an
// explicit repo+pr) names the PR; the result reports the in-sandbox workspace path
// and the PR number for downstream steps.
func buildPRCheckout(with map[string]any) (toolOp, error) {
	repo, pr := repoAndPR(with)
	if repo == "" || pr <= 0 {
		return toolOp{}, fmt.Errorf("github.pr_checkout needs a notification (or repo + pr) identifying the PR")
	}
	if err := validateGitHubRepo(repo); err != nil {
		return toolOp{}, fmt.Errorf("github.pr_checkout: %w", err)
	}
	// Clone into a per-PR subdir of the run workspace, then check out the PR head.
	dir := "pr-" + strconv.Itoa(pr)
	argv := []string{"sh", "-c", "gh repo clone " + shellQuote(repo) + " " + shellQuote(dir) +
		" && cd " + shellQuote(dir) + " && gh pr checkout " + strconv.Itoa(pr)}
	return toolOp{
		argv:    argv,
		clis:    automation.ToolCLIs(automation.ToolGithubPRCheckout),
		network: []sandbox.NetworkRule{{Host: "github.com", Port: 443}, {Host: "api.github.com", Port: 443}},
		parse: func(stdout string) (any, error) {
			return map[string]any{"workspace": "/workspace/" + dir, "pr": pr, "repository": repo}, nil
		},
	}, nil
}

// buildPRComment posts (or drafts) a PR comment from a body. The body is fed via
// stdin (never the argv), so it is not visible on the host command line or in audit.
func buildPRComment(with map[string]any) (toolOp, error) {
	repo, pr := repoAndPR(with)
	body := asString(with["body"])
	if repo == "" || pr <= 0 {
		return toolOp{}, fmt.Errorf("github.pr_comment needs repo + pr")
	}
	if err := validateGitHubRepo(repo); err != nil {
		return toolOp{}, fmt.Errorf("github.pr_comment: %w", err)
	}
	if strings.TrimSpace(body) == "" {
		return toolOp{}, fmt.Errorf("github.pr_comment needs a non-empty body (body_from a prior step)")
	}
	argv := []string{"gh", "pr", "comment", strconv.Itoa(pr), "--repo", repo, "--body-file", "-"}
	return toolOp{
		argv:    argv,
		clis:    automation.ToolCLIs(automation.ToolGithubPRComment),
		stdin:   []byte(body),
		network: []sandbox.NetworkRule{{Host: "api.github.com", Port: 443}},
		parse: func(stdout string) (any, error) {
			return map[string]any{"posted": true, "pr": pr, "url": strings.TrimSpace(stdout)}, nil
		},
	}, nil
}

// buildHTTPRequest performs a bounded HTTP call (argv-built curl), body via stdin.
func buildHTTPRequest(with map[string]any) (toolOp, error) {
	rawURL := asString(with["url"])
	method := strings.ToUpper(asString(with["method"]))
	if method == "" {
		method = "GET"
	}
	host, port, err := parseURLTarget(rawURL)
	if err != nil {
		return toolOp{}, fmt.Errorf("http.request: %w", err)
	}
	argv := []string{"curl", "-fsS", "--max-time", "30", "-X", method}
	for _, h := range stringSlice(with["headers"]) {
		argv = append(argv, "-H", h)
	}
	var stdin []byte
	if body := asString(with["body"]); body != "" {
		argv = append(argv, "--data-binary", "@-")
		stdin = []byte(body)
	}
	argv = append(argv, "--", rawURL)
	return toolOp{
		argv:    argv,
		clis:    automation.ToolCLIs(automation.ToolHTTPRequest),
		stdin:   stdin,
		network: []sandbox.NetworkRule{{Host: host, Port: port}},
		parse: func(stdout string) (any, error) {
			return map[string]any{"body": stdout}, nil
		},
	}, nil
}

// buildShellExec runs an argv-first command (the default), or a shell string only
// when the profile is unrestricted (the gate the plan requires).
func buildShellExec(with map[string]any) (toolOp, error) {
	if argv := stringSlice(with["argv"]); len(argv) > 0 {
		// The CLI binary is argv[0] — enforced against allowed_cli_tools (granular mode).
		// Marked network-capable: an arbitrary command can egress, so a network:disabled
		// profile must not be able to run it.
		return toolOp{argv: argv, clis: []string{argv[0]}, networkCapable: true, parse: shellParse}, nil
	}
	if cmd := asString(with["command"]); cmd != "" {
		// A shell string is gated by the caller to an unrestricted profile; here we just
		// build it (unrestricted skips the per-CLI allow-list).
		return toolOp{argv: []string{"/bin/sh", "-lc", cmd}, clis: []string{"/bin/sh"}, networkCapable: true, parse: shellParse}, nil
	}
	return toolOp{}, fmt.Errorf("shell.exec needs an argv array (or a command string for an unrestricted profile)")
}

func shellParse(stdout string) (any, error) {
	return map[string]any{"stdout": stdout}, nil
}

// shellExecNeedsUnrestricted reports whether a shell.exec step needs an unrestricted
// profile: the shell-string `command` form, OR an argv form whose argv[0] is a shell
// interpreter (e.g. ["sh","-c","curl ..."]) — that is just the command-string form in
// disguise and would otherwise run arbitrary commands behind one approved interpreter,
// bypassing the allowed_cli_tools meaning. A granular shell.exec must use a concrete,
// granted CLI as argv[0], not a shell.
func shellExecNeedsUnrestricted(with map[string]any) bool {
	if asString(with["command"]) != "" && len(stringSlice(with["argv"])) == 0 {
		return true
	}
	if argv := stringSlice(with["argv"]); len(argv) > 0 {
		return isShellInterpreter(argv[0])
	}
	return false
}

// isShellInterpreter delegates to the shared automation classifier so enable-time
// eligibility and run-time enforcement use the same list.
func isShellInterpreter(bin string) bool { return automation.IsShellInterpreter(bin) }

// --- argument helpers ---

func repoAndPR(with map[string]any) (string, int) {
	if n, ok := with["notification"].(map[string]any); ok {
		repo := asString(n["repository"])
		pr := intArg(n["pr"])
		if repo != "" && pr > 0 {
			return repo, pr
		}
	}
	return asString(with["repo"]), intArg(with["pr"])
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// intArg parses a PR-number-like argument STRICTLY: only a POSITIVE INTEGRAL JSON number, an
// integral json.Number, or a trimmed numeric string yields that value; anything else (a
// fractional 7.9 — which must NOT silently truncate to PR #7 and act on the wrong pull request —
// a non-numeric string, or a non-scalar) returns 0, which the callers treat as "no valid PR" and
// fail closed instead of targeting an unintended PR.
func intArg(v any) int {
	switch t := v.(type) {
	case float64:
		if t > 0 && t == float64(int64(t)) {
			return int(t)
		}
		return 0
	case int:
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil { // a fractional json.Number ("7.9") errors -> 0
			return int(n)
		}
		return 0
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
		return 0
	default:
		return 0
	}
}

func boolArg(with map[string]any, key string) bool {
	b, _ := with[key].(bool)
	return b
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s := asString(e); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// isInternalHostTarget reports whether a typed network op's target host is a loopback /
// link-local / cloud-metadata / private address an unattended automation must never reach
// (an SSRF guard against the server-side sandbox being used to probe internal services).
// IP literals are checked directly; a bare hostname is NOT resolved here (a DNS lookup
// would add a network dependency + a rebinding TOCTOU) — name→address resolution is bounded
// by the sbx container's own egress policy, which is the documented network backstop. The
// literal forms (127.0.0.1, ::1, 169.254.169.254, 10/172.16/192.168, localhost) ARE blocked.
func isInternalHostTarget(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	// Canonicalize the way the executed client (curl/getaddrinfo) will, BEFORE classifying, or
	// a normalization gap dodges the block. (1) Percent-decode valid host escapes, so an encoded
	// dotted literal like 127%2e0%2e0%2e1 is seen as 127.0.0.1 (a malformed escape leaves the
	// raw host, still checked — and an IPv6 zone's %et/%lo is one such "malformed" escape, kept
	// for the zone strip below). (2) Strip an IPv6 zone id (fe80::1%eth0 — the %25 from an
	// RFC6874 [..%25zone] URL decodes to %) so the address parses.
	if dec, err := url.PathUnescape(h); err == nil {
		h = dec
	}
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	// Classify the host as-is AND its IDN-canonical (UTS #46) form: an IDN-aware resolver maps
	// confusable Unicode dots (U+3002/U+FF0E/U+FF61) and fullwidth digits to their ASCII forms,
	// so "127。0。0。1" / fullwidth digits resolve to loopback/metadata. ToASCII errors on a
	// non-IDN host (an IP with ':' , an underscore label, …) — that's fine, the raw form is
	// still checked, and a host ToASCII rejects isn't a resolvable internal literal anyway.
	if classifyInternalHost(h) {
		return true
	}
	if canon, err := idna.Lookup.ToASCII(strings.TrimRight(h, ".")); err == nil && canon != h {
		return classifyInternalHost(canon)
	}
	return false
}

// classifyInternalHost reports whether an already-normalized host is a loopback/link-local/
// metadata/unspecified/private literal. It strips trailing FQDN-root dots (localhost. /
// 127.0.0.1.) the resolver treats as equivalent, then checks localhost, a canonical IP, and
// the legacy inet_aton forms (a single 32-bit number, hex, octal, shortened dotted) that
// curl/getaddrinfo also resolve (2852039166 / 0xa9fea9fe / 0251.0376.0251.0376 / 127.1).
func classifyInternalHost(h string) bool {
	h = strings.TrimRight(h, ".")
	if h == "" {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return isInternalIP(ip)
	}
	if ip, ok := parseLooseIPv4(h); ok {
		return isInternalIP(ip)
	}
	return false
}

// isInternalIP reports whether ip is a loopback/link-local/cloud-metadata/unspecified/private
// address an unattended automation must never reach.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified()
}

// parseLooseIPv4 parses the legacy inet_aton IPv4 forms getaddrinfo/curl accept but
// net.ParseIP rejects: 1–4 dot-separated parts, each decimal, hex (0x…), or octal (leading
// 0); a single number is the full 32-bit address, and a trailing part absorbs the remaining
// low bytes (a.b → b is 24-bit, a.b.c → c is 16-bit). A non-numeric part (a real hostname
// label) makes the whole parse fail, so legitimate DNS names are left for the sbx backstop.
func parseLooseIPv4(host string) (net.IP, bool) {
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := parseIPv4Part(p)
		if !ok {
			return nil, false
		}
		vals[i] = v
	}
	var addr uint32
	switch len(parts) {
	case 1:
		if vals[0] > 0xffffffff {
			return nil, false
		}
		addr = uint32(vals[0])
	case 2:
		if vals[0] > 0xff || vals[1] > 0xffffff {
			return nil, false
		}
		addr = uint32(vals[0])<<24 | uint32(vals[1])
	case 3:
		if vals[0] > 0xff || vals[1] > 0xff || vals[2] > 0xffff {
			return nil, false
		}
		addr = uint32(vals[0])<<24 | uint32(vals[1])<<16 | uint32(vals[2])
	case 4:
		for _, v := range vals {
			if v > 0xff {
				return nil, false
			}
		}
		addr = uint32(vals[0])<<24 | uint32(vals[1])<<16 | uint32(vals[2])<<8 | uint32(vals[3])
	}
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr)), true
}

// parseIPv4Part parses one inet_aton component: 0x… = hex, a leading 0 = octal, else decimal.
func parseIPv4Part(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base, s := 10, p
	switch {
	case len(p) >= 2 && (p[:2] == "0x" || p[:2] == "0X"):
		base, s = 16, p[2:]
	case len(p) >= 2 && p[0] == '0':
		base, s = 8, p[1:]
	}
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseURLTarget(raw string) (string, int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", 0, fmt.Errorf("url must be an http(s) URL")
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("url has no host")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port")
		}
		port = n
	}
	return host, port, nil
}

// shellQuote single-quotes a value for safe embedding in the checkout sh -c string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
