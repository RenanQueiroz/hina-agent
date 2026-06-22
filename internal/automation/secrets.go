package automation

import "strings"

// EnvVarName converts a vaulted secret's name to the environment-variable name it is
// injected under (e.g. "github_token" -> "GITHUB_TOKEN"). It is the SINGLE source used
// by both enable-time eligibility (here) and the run-time executor (internal/autorun),
// so the displayed grant can't differ from what runs.
func EnvVarName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "SECRET_" + out
	}
	return out
}

// dangerousEnvPrefixes / dangerousEnvExact MIRROR internal/sandbox.DangerousEnvName so
// the automation package can reject a host-dangerous grant name at enable time without
// importing sandbox (which would weigh down this stdlib-only package). A cross-package
// test asserts the two stay in lockstep.
var dangerousEnvPrefixes = []string{"LD_", "DYLD_", "DOCKER_", "BASH_FUNC_", "GIT_SSH", "GIT_CONFIG"}

var dangerousEnvExact = map[string]struct{}{
	"PATH": {}, "HOME": {}, "IFS": {}, "ENV": {}, "BASH_ENV": {}, "SHELLOPTS": {},
	"PS4": {}, "PROMPT_COMMAND": {},
	// gh routing vars: GH_HOST/GH_REPO redirect a bare owner/repo off github.com, and a config
	// dir points gh at a hosts.yml defining arbitrary hosts — gh resolves that dir from
	// GH_CONFIG_DIR, else XDG_CONFIG_HOME/gh (else APPDATA on Windows), so all three are
	// forbidden; any would reroute the typed github.* tools past their validated github.com
	// target. (The auth tokens GH_TOKEN/GITHUB_TOKEN are NOT here — the intended credential.)
	"GH_HOST": {}, "GH_REPO": {}, "GH_CONFIG_DIR": {}, "XDG_CONFIG_HOME": {}, "APPDATA": {},
}

// IsDangerousEnvName reports whether an injected env-var name would be interpreted by
// the host loader / Docker client / shell / proxy resolution — such a name must not be
// used for a secret grant (it could alter host-side execution, and the sandbox runner
// drops it, silently omitting the credential the definition claims to inject).
func IsDangerousEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range dangerousEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	if strings.HasSuffix(upper, "_PROXY") {
		return true
	}
	_, bad := dangerousEnvExact[upper]
	return bad
}
