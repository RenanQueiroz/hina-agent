package sandbox

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/agentcli"
)

// StateKind is the sandbox_state.kind value under which a user's Sandbox
// Environment policy is stored.
const StateKind = "environment"

// BuiltinTools are the sandbox-backed tools the agent loop can route. Every one
// runs inside the user's `sbx` sandbox; the per-user Environment allow-list
// decides which are offered.
var BuiltinTools = []string{ToolShell, ToolFSRead, ToolFSWrite, ToolHTTP}

// agentToolNames are the callable-agent run tools (agent.<provider>.run). They are
// part of the same per-user Environment tool allow-list as the built-ins, so a user
// who removes one stops the model from invoking that agent (the auth profile is a
// separate, additional requirement).
func agentToolNames() []string {
	caps := agentcli.Capabilities()
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.ToolName)
	}
	return out
}

// AllToolNames is every tool name the Environment governs: the built-ins plus the
// callable-agent run tools. It is the universe the UI offers and Validate accepts.
func AllToolNames() []string {
	out := make([]string, 0, len(BuiltinTools)+4)
	out = append(out, BuiltinTools...)
	out = append(out, agentToolNames()...)
	return out
}

// envNameRe validates a granted secret's injected env-var name.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dangerousEnvPrefixes are env-var name prefixes the host dynamic loader / Docker
// client interpret. A secret forwarded under such a name could alter HOST-side
// execution or Docker targeting before the container exists, so they are never
// allowed as a grant name.
var dangerousEnvPrefixes = []string{"LD_", "DYLD_", "DOCKER_", "BASH_FUNC_"}

// dangerousEnvExact are exact env-var names that influence host process resolution
// or the shell — also forbidden as grant names.
var dangerousEnvExact = map[string]struct{}{
	"PATH": {}, "HOME": {}, "IFS": {}, "ENV": {}, "BASH_ENV": {}, "SHELLOPTS": {},
	"PS4": {}, "PROMPT_COMMAND": {},
	// gh routing vars: GH_HOST/GH_REPO redirect a bare owner/repo off github.com, and a config
	// dir points gh at a hosts.yml defining arbitrary hosts — gh resolves that dir from
	// GH_CONFIG_DIR, else XDG_CONFIG_HOME/gh (else APPDATA on Windows), so all three are
	// forbidden; any would reroute a typed github.* tool past its validated github.com target.
	// (The auth tokens GH_TOKEN/GITHUB_TOKEN are NOT here — those are the intended credential.)
	"GH_HOST": {}, "GH_REPO": {}, "GH_CONFIG_DIR": {}, "XDG_CONFIG_HOME": {}, "APPDATA": {},
}

// DangerousEnvName reports whether name would be interpreted by the host loader /
// Docker client / shell / proxy resolution and so must not be used as a secret
// grant's injected name. It uppercases first (loaders/shells match the upper form).
func DangerousEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range dangerousEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	// The whole *_PROXY suffix class (HTTP_PROXY, HTTPS_PROXY, FTP_PROXY, …) redirects
	// host-side network resolution; the GIT_SSH* / GIT_CONFIG* prefix classes redirect git's
	// SSH transport / inject git config (url.insteadOf can reroute an HTTPS fetch).
	if strings.HasSuffix(upper, "_PROXY") || strings.HasPrefix(upper, "GIT_SSH") || strings.HasPrefix(upper, "GIT_CONFIG") {
		return true
	}
	_, bad := dangerousEnvExact[upper]
	return bad
}

// Environment is a user's Sandbox Environment policy: what that user's sandboxes
// may do, independent of any one session or Automation. It is the single place a
// user (and admin) configures allowed tools, MCP servers, the default network
// posture, writable mounts, and which vaulted secrets are injected as env vars.
type Environment struct {
	AllowedTools   []string      `json:"allowed_tools"`
	MCPServers     []MCPServer   `json:"mcp_servers"`
	Network        NetworkPolicy `json:"network"`
	WritableMounts []string      `json:"writable_mounts"`
	SecretGrants   []SecretGrant `json:"secret_grants"`
}

// MCPServer is a configured Model Context Protocol server. Phase 7 stores and
// validates the allow-list; the actual MCP tool invocation (through the sandbox
// network allow-list) is wired with the callable-agent work in Phase 8.
type MCPServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// NetworkPolicy is the default network posture plus the explicit host:port
// allow-list. Default is "deny": a tool reaches a host service only via an Allow
// entry, which the runner turns into an `sbx policy allow network` grant.
type NetworkPolicy struct {
	Default string        `json:"default"` // "deny" | "allow"
	Allow   []NetworkRule `json:"allow"`
}

// SecretGrant binds a vaulted secret id to the env-var name it is injected as.
type SecretGrant struct {
	SecretID string `json:"secret_id"`
	EnvName  string `json:"env_name"`
}

// DefaultEnvironment is the conservative starting policy: every tool (built-in +
// callable agent) is allowed (each already runs inside the isolated sandbox, and an
// agent run additionally requires a configured auth profile), but the network is
// default-deny and no secrets are granted until the user opts in. A user who saves a
// policy with a tool removed blocks it — including an agent tool.
func DefaultEnvironment() Environment {
	return Environment{
		AllowedTools: AllToolNames(),
		Network:      NetworkPolicy{Default: "deny"},
	}
}

// ToolAllowed reports whether tool is in the user's allow-list.
func (e Environment) ToolAllowed(tool string) bool {
	for _, t := range e.AllowedTools {
		if t == tool {
			return true
		}
	}
	return false
}

// NetworkAllowed reports whether the policy permits reaching host:port (an
// explicit Allow entry, or a fully-open "allow" default).
func (e Environment) NetworkAllowed(host string, port int) bool {
	if strings.EqualFold(e.Network.Default, "allow") {
		return true
	}
	for _, rule := range e.Network.Allow {
		if strings.EqualFold(rule.Host, host) && rule.Port == port {
			return true
		}
	}
	return false
}

// Validate checks the policy is well-formed before it is stored. It fails closed
// on anything the runtime would otherwise have to guess about.
func (e Environment) Validate() error {
	for _, t := range e.AllowedTools {
		if !isKnownTool(t) {
			return fmt.Errorf("unknown tool %q (allowed: %s)", t, strings.Join(AllToolNames(), ", "))
		}
	}
	switch strings.ToLower(e.Network.Default) {
	case "", "deny", "allow":
	default:
		return fmt.Errorf("network.default %q must be deny|allow", e.Network.Default)
	}
	for _, rule := range e.Network.Allow {
		if rule.Host == "" {
			return fmt.Errorf("network allow entry has an empty host")
		}
		if rule.Port < 1 || rule.Port > 65535 {
			return fmt.Errorf("network allow port %d out of range", rule.Port)
		}
	}
	for _, m := range e.MCPServers {
		if m.Name == "" || m.URL == "" {
			return fmt.Errorf("mcp server needs a name and url")
		}
	}
	for _, m := range e.WritableMounts {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("writable mount path is empty")
		}
	}
	seen := make(map[string]struct{}, len(e.SecretGrants))
	for _, g := range e.SecretGrants {
		if g.SecretID == "" {
			return fmt.Errorf("secret grant has an empty secret id")
		}
		if !envNameRe.MatchString(g.EnvName) {
			return fmt.Errorf("secret grant env name %q is not a valid environment variable name", g.EnvName)
		}
		if DangerousEnvName(g.EnvName) {
			return fmt.Errorf("secret grant env name %q is reserved (the host loader/Docker client/shell interprets it) and cannot be used", g.EnvName)
		}
		if _, dup := seen[g.EnvName]; dup {
			return fmt.Errorf("secret grant env name %q is used more than once", g.EnvName)
		}
		seen[g.EnvName] = struct{}{}
	}
	return nil
}

// Normalize fills defaults so a partially-specified policy is consistent (e.g. an
// empty network default becomes "deny").
func (e Environment) Normalize() Environment {
	if e.Network.Default == "" {
		e.Network.Default = "deny"
	}
	e.Network.Default = strings.ToLower(e.Network.Default)
	return e
}

func isKnownTool(t string) bool {
	for _, b := range AllToolNames() {
		if b == t {
			return true
		}
	}
	return false
}
