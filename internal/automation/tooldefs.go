package automation

import "strings"

// Deterministic tool names an automation `tool` step may invoke. These run BEFORE
// any model/agent wakes (cheap filtering), inside the automation's sandbox, argv-
// first. The set is closed (a typed adapter owns each), so a definition can never
// name an arbitrary host command as a "tool".
const (
	ToolGithubNotifications = "github.notifications"
	ToolGithubPRCheckout    = "github.pr_checkout"
	ToolGithubPRComment     = "github.pr_comment"
	ToolHTTPRequest         = "http.request"
	ToolShellExec           = "shell.exec"
	ToolMCPCall             = "mcp.call"
)

// knownTools is the closed set of SELECTABLE deterministic tools. mcp.call is omitted:
// the MCP facade is deferred (the executor can't run it), so a definition must not be
// able to declare it and pass validation only to fail on its first run. Re-add it when
// MCP execution lands.
var knownTools = map[string]bool{
	ToolGithubNotifications: true,
	ToolGithubPRCheckout:    true,
	ToolGithubPRComment:     true,
	ToolHTTPRequest:         true,
	ToolShellExec:           true,
}

// KnownTools returns the selectable deterministic tool names, for the builder UI's
// tool picker (mcp.call is excluded until its execution path exists).
func KnownTools() []string {
	return []string{
		ToolGithubNotifications, ToolGithubPRCheckout, ToolGithubPRComment,
		ToolHTTPRequest, ToolShellExec,
	}
}

// toolCapability is the run-time surface a deterministic tool needs: the CLI binaries
// it invokes (which a granular profile must grant via allowed_cli_tools) and whether it
// can make outbound network requests (which a granular profile must allow via
// network=enabled). This is the SINGLE source the enable-time eligibility check AND the
// run-time executor (internal/autorun) both consult, so the displayed profile can't be
// weaker than what runs. shell.exec's CLIs are argv-dependent (checked at run time from
// argv[0]); it is networked (an arbitrary command can egress).
type toolCapability struct {
	clis      []string
	networked bool
}

var toolCaps = map[string]toolCapability{
	ToolGithubNotifications: {clis: []string{"gh"}, networked: true},
	ToolGithubPRCheckout:    {clis: []string{"sh", "gh", "git"}, networked: true},
	ToolGithubPRComment:     {clis: []string{"gh"}, networked: true},
	ToolHTTPRequest:         {clis: []string{"curl"}, networked: true},
	ToolShellExec:           {clis: nil, networked: true},
}

// ToolCLIs returns the CLI binaries a deterministic tool statically invokes (empty for
// shell.exec, whose binary comes from argv at run time).
func ToolCLIs(tool string) []string { return toolCaps[tool].clis }

// ToolNetworked reports whether a deterministic tool can make outbound requests.
func ToolNetworked(tool string) bool { return toolCaps[tool].networked }

// NetworkAllowed reports whether a profile permits outbound network for a network-
// capable op, by mode + the sandbox.network field. unrestricted defaults to allowed
// (it may make any permitted request) but still honors an EXPLICIT "disabled";
// granular is default-deny and requires "enabled". The same rule is enforced at enable
// time (eligibility) and at run time (the executor), so the displayed network posture
// always matches what runs.
func NetworkAllowed(mode, network string) bool {
	if mode == ModeUnrestricted {
		return network != "disabled"
	}
	return network == "enabled"
}

// shellInterpreters are program names that execute arbitrary command strings; a
// granular shell.exec may not use one as argv[0] (use an unrestricted profile for a
// shell string). Shared by enable-time eligibility AND the run-time executor.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true,
	"ksh": true, "fish": true, "csh": true, "tcsh": true, "busybox": true,
}

// IsShellInterpreter reports whether bin (a path or bare name) is a known shell
// interpreter.
func IsShellInterpreter(bin string) bool {
	base := bin
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	return shellInterpreters[strings.ToLower(base)]
}

// Callable-agent adapters an `agent_cli` step may use. Mirrors internal/agentcli;
// kept here so the automation package validates without importing it.
var knownAdapters = map[string]bool{
	"codex": true, "claude": true, "cursor": true, "pi": true,
}

// KnownAdapters returns the agent_cli adapter names, for the builder UI.
func KnownAdapters() []string { return []string{"codex", "claude", "cursor", "pi"} }

// AgentToolName maps an adapter to the routable agent tool name it runs as (the
// AgentRouter's `agent.<provider>.run`). Used for eligibility checks and execution.
func AgentToolName(adapter string) string { return "agent." + adapter + ".run" }

// knownHostServices is the closed allow-list of server-owned local endpoints a
// sandbox may be permitted to reach. v1 has only llamacpp (the Phase 11 gateway).
var knownHostServices = map[string]bool{"llamacpp": true}
