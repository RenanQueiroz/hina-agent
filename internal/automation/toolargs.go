package automation

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// githubRepoPattern matches a BARE GitHub "owner/repo" full name — the only repo form v1
// accepts (no host, URL, or extra path), so a repo value can't smuggle a different host/path.
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateGitHubRepo rejects any repo value that isn't a bare github.com "owner/repo" name. It
// is the SINGLE source shared by enable-time validation and the runtime github.* tool builders,
// so the accepted shape can never drift between the two.
func ValidateGitHubRepo(repo string) error {
	if !githubRepoPattern.MatchString(repo) {
		return fmt.Errorf("repo %q must be a bare GitHub \"owner/repo\" name (no host, URL, or extra path)", repo)
	}
	return nil
}

// ValidateLiteralURL checks a LITERAL http.request url at enable-time: it must be a well-formed
// http(s) URL with a host, and must not target an obvious internal/loopback address. This is a
// best-effort EARLY rejection so a hardcoded bad or internal URL fails at enable rather than
// mid-run after earlier side effects. The authoritative SSRF guard — which also handles IDNA
// confusables, inet_aton numeric forms, and DYNAMIC (resolved) values — stays at run time,
// because DNS resolution and rebinding are inherently runtime concerns.
func ValidateLiteralURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("url %q must be an http(s) URL", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url %q has no host", raw)
	}
	if isLiteralInternalHost(u.Hostname()) {
		return fmt.Errorf("url %q targets an internal/loopback host", raw)
	}
	return nil
}

// isLiteralInternalHost reports whether a LITERAL host is loopback/private/link-local/etc. A DNS
// hostname (not a parseable IP literal) returns false here — the runtime SSRF guard resolves and
// checks those (this enable-time pass only catches the obvious hardcoded cases).
func isLiteralInternalHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
