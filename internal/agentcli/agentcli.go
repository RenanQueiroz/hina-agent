// Package agentcli is the typed, versioned adapter layer for account-backed coding
// agent CLIs (Codex, Claude Code, Cursor) and the local-only Pi agent. Each Adapter
// owns the drift-prone part — the exact command-line surface and the output parser
// — and produces a RunPlan (argv + env + staged files) the sandbox orchestration
// layer (internal/sandbox AgentRouter) executes inside the calling user's `sbx`
// sandbox. The Go server therefore owns process invocation, environment, timeouts,
// and artifact capture; the model only supplies structured RunRequest fields and
// never builds a raw CLI string.
//
// The four CLIs move fast and break flags/auth/output between releases (B7 in
// plans/research-findings.md records the verified surface plus 5 corrections), so
// every adapter is treated as a *versioned capability with a health check*: its
// VersionArgs probe + the parser are the drift guard, and the flag surface MUST be
// re-verified on a host that has the CLI installed before it is trusted in
// production (the Phase 8 plan's explicit instruction). Like `sbx` itself, these
// CLIs are not assumed present in CI; the adapters are unit-tested against captured
// output fixtures so the argv construction and parsing are fully covered without the
// real binaries, and `hina doctor` reports which are actually installed.
//
// Pi is the local/account-free path: it targets the Phase 11 managed llama.cpp
// backend through Hina's host-inference proxy and never a cloud provider. Until
// Phase 11 lands that proxy, the Pi adapter is built but reports itself unavailable
// (no endpoint to reach), exactly as a missing CLI does.
package agentcli

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"time"
)

// Provider identifies one callable agent CLI.
type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderCursor Provider = "cursor"
	ProviderPi     Provider = "pi"
)

// Valid reports whether p is a known provider.
func (p Provider) Valid() bool {
	switch p {
	case ProviderCodex, ProviderClaude, ProviderCursor, ProviderPi:
		return true
	default:
		return false
	}
}

// AuthType records HOW a configured provider profile authenticates, WITHOUT ever
// storing the credential itself. It is recorded on every run (the Phase 8 exit
// criterion: "runs record the auth-profile type without storing credential
// values") and shown coarsely in the admin UI — never the token/URL/code.
type AuthType string

const (
	// AuthBrowserState is subscription/browser login state captured in the
	// per-user encrypted credential store (CODEX_HOME / CLAUDE_CONFIG_DIR / Cursor
	// state). The powerful bearer material lives only in the vault boundary.
	AuthBrowserState AuthType = "browser_state"
	// AuthAPIKey is a provider API key injected as a run-scoped env var.
	AuthAPIKey AuthType = "api_key"
	// AuthOAuthToken is a long-lived OAuth/setup token (e.g. CLAUDE_CODE_OAUTH_TOKEN).
	AuthOAuthToken AuthType = "oauth_token"
	// AuthLocalLlamaCpp is Pi against the host llama.cpp endpoint — no account, no
	// cloud key.
	AuthLocalLlamaCpp AuthType = "local_llamacpp"
)

// Valid reports whether t is a known auth type.
func (t AuthType) Valid() bool {
	switch t {
	case AuthBrowserState, AuthAPIKey, AuthOAuthToken, AuthLocalLlamaCpp:
		return true
	default:
		return false
	}
}

// CredStore describes where a provider keeps its persistent credential/agent
// state and the env var that relocates it. The auth broker and the run path both
// mount the user's encrypted agent-state at ContainerDir and set EnvVar to it, so
// an authenticated login and a later run share the same per-user store.
type CredStore struct {
	EnvVar       string // env var that points the CLI at its store, e.g. "CODEX_HOME"
	ContainerDir string // container path the agent-state is mounted at, e.g. "/agent/codex"
}

// LoginOptions selects the interactive login flavor for the auth broker.
type LoginOptions struct {
	// DeviceAuth requests the device-code / paste-code flow (the mandatory fallback
	// when a localhost browser callback can't reach into the sandbox container).
	DeviceAuth bool
}

// RunRequest is the typed, model-supplied request for a headless agent run. The
// model never provides argv/flags — only these fields — so command injection is
// not reachable from a model response.
type RunRequest struct {
	Prompt   string   // the task/instruction to run (required)
	Model    string   // optional model id (provider-hosted for Cursor; see B7)
	MaxTurns int      // optional agent-turn budget (0 = adapter default / omit)
	AuthType AuthType // the profile type in use (affects env, e.g. browser profiles keep ANTHROPIC_API_KEY unset)
	// Structured requests schema-constrained output. When set with a non-empty
	// SchemaJSON, the adapter stages the schema as a workspace file and points the
	// CLI's structured-output flag at it.
	Structured bool
	SchemaJSON json.RawMessage // the JSON Schema to constrain output to
	// LocalEndpoint is SERVER-SET (never model-supplied): the base URL of Hina's
	// host-inference proxy that Pi must target (the Phase 11 path-filtered /v1
	// gateway, e.g. "http://host.docker.internal:8081/v1"). Empty for the
	// account-backed agents; empty for Pi means no local backend is available, so
	// Pi's BuildRun fails closed.
	LocalEndpoint string
}

// StagedFile is a file the orchestrator writes into the run workspace (or a
// mounted config dir) before launching the CLI — e.g. an output schema or Pi's
// generated models.json. Mode 0 means 0o600.
type StagedFile struct {
	RelPath string // path relative to the run workspace
	Content []byte
	Mode    uint32
}

// RunPlan is an adapter's resolved instructions for one headless run. The
// orchestrator runs Argv inside the sandbox with Env (non-secret) set, injects the
// values for SecretNames from the vault (gated on network isolation), and stages
// Files first. It is a pure value (no I/O) so it is fully unit-testable.
type RunPlan struct {
	Argv        []string     // in-sandbox command (argv-first, no shell)
	Env         []string     // NON-secret "K=V" env (e.g. CODEX_HOME, PI_OFFLINE=1)
	SecretNames []string     // env var NAMES whose secret values the orchestrator injects
	Files       []StagedFile // files to stage into the workspace before the run
}

// RawResult is the captured outcome of running a RunPlan, handed to Adapter.Parse.
type RawResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	TimedOut   bool
	Cancelled  bool
	Duration   time.Duration
	StdoutPath string // path to the full captured stdout (already secret-redacted)
	StderrPath string
}

// Run status values for a normalized AgentRunResult.
const (
	StatusOK        = "ok"
	StatusError     = "error"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// ToolCallInfo is a tool/command the agent invoked during a run, when the CLI's
// output makes it parseable (best-effort — not every CLI/format exposes it).
type ToolCallInfo struct {
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
}

// AgentRunResult is the normalized outcome of any adapter's run, so callers (chat,
// Automations) get one shape regardless of which CLI ran. Token/cost fields are
// best-effort (0 when the CLI doesn't report them).
type AgentRunResult struct {
	Provider     Provider        `json:"provider"`
	Status       string          `json:"status"` // StatusOK | StatusError | StatusTimeout | StatusCancelled
	FinalText    string          `json:"final_text"`
	Structured   json.RawMessage `json:"structured,omitempty"` // schema-constrained output, when requested + parseable
	ChangedFiles []string        `json:"changed_files,omitempty"`
	ToolCalls    []ToolCallInfo  `json:"tool_calls,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
	CostUSD      float64         `json:"cost_usd,omitempty"`
	ExitCode     int             `json:"exit_code"`
	DurationMs   int64           `json:"duration_ms"`
	StdoutPath   string          `json:"stdout_path,omitempty"`
	StderrPath   string          `json:"stderr_path,omitempty"`
	Err          string          `json:"err,omitempty"` // execution/parse error the model should see
}

// Capability is an adapter's static metadata for eligibility + the UI: which auth
// types it supports and whether it is the local-only path. It carries no per-user
// state.
type Capability struct {
	Provider     Provider   `json:"provider"`
	DisplayName  string     `json:"display_name"`
	AuthTypes    []AuthType `json:"auth_types"`     // profile types a user may configure
	BrowserAuth  bool       `json:"browser_auth"`   // supports the interactive PTY login broker
	LocalOnly    bool       `json:"local_only"`     // Pi: no account, host llama.cpp only
	ToolName     string     `json:"tool_name"`      // the routable tool name, e.g. "agent.codex.run"
	CredStoreEnv string     `json:"cred_store_env"` // env var that relocates the credential store ("" if none)
}

// Adapter is a versioned, typed wrapper around one agent CLI. Implementations are
// pure (no I/O): they construct argv/env and parse captured output, so the whole
// drift-prone surface is unit-tested without the real binary.
type Adapter interface {
	Provider() Provider
	Capability() Capability

	// CredStore returns where this CLI keeps persistent state and the env var that
	// relocates it. The zero CredStore (empty EnvVar) means the CLI has no
	// relocatable store (the orchestrator then mounts at the default ContainerDir).
	CredStore() CredStore

	// VersionArgs is the full health-check argv (CLI binary first, e.g.
	// {"codex","--version"}); ParseVersion extracts a version string from its output.
	VersionArgs() []string
	ParseVersion(out string) string

	// Auth-broker commands, run interactively in a short-lived auth container. Each
	// returns a full argv (the CLI binary first) so the broker can run it directly.
	// LoginArgs honors LoginOptions.DeviceAuth (the paste-code fallback); LoginEnv is
	// the non-secret env the login needs (e.g. Cursor's NO_OPEN_BROWSER=1).
	LoginArgs(opt LoginOptions) []string
	LoginEnv() []string
	StatusArgs() []string
	LogoutArgs() []string
	// AuthOK reports whether a status-command output indicates an authenticated
	// profile — used to confirm a login session succeeded.
	AuthOK(statusOut string) bool

	// BuildRun produces the headless run plan for req, or an error if the request
	// is malformed (e.g. an empty prompt).
	BuildRun(req RunRequest) (RunPlan, error)
	// Parse normalizes a captured RawResult into an AgentRunResult.
	Parse(p Provider, raw RawResult) AgentRunResult
}

// registry holds the built-in adapters keyed by provider, in a stable order.
var registry = []Adapter{
	codexAdapter{},
	claudeAdapter{},
	cursorAdapter{},
	piAdapter{},
}

// All returns every built-in adapter in a stable order (codex, claude, cursor, pi).
func All() []Adapter {
	out := make([]Adapter, len(registry))
	copy(out, registry)
	return out
}

// Get returns the adapter for p, or (nil, false) if p is unknown.
func Get(p Provider) (Adapter, bool) {
	for _, a := range registry {
		if a.Provider() == p {
			return a, true
		}
	}
	return nil, false
}

// Capabilities returns the static capability metadata for every adapter.
func Capabilities() []Capability {
	out := make([]Capability, 0, len(registry))
	for _, a := range registry {
		out = append(out, a.Capability())
	}
	return out
}

// IsLocalEndpoint reports whether raw is a safe LOCAL inference endpoint for Pi: an
// http(s) URL whose host is the container→host gateway (host.docker.internal) or a
// loopback address. Pi is the account-free, local-only agent that must reach ONLY
// Hina's host-inference proxy, so a non-local endpoint (a typo or misconfig) is
// refused rather than silently sending prompts/workspace context to an arbitrary
// remote OpenAI-compatible server.
func IsLocalEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "host.docker.internal" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// StagingDir is the container path the orchestrator mounts a FRESH per-run scratch
// at, holding adapter-staged files (an output schema, Pi's models.json). RelPath in a
// StagedFile is relative to this dir, and an adapter references a staged file by
// StagingDir+"/"+RelPath — so model-controlled files are never written into the
// durable workspace from the host (no symlink-escape, no pre-audit workspace mutation).
const StagingDir = "/hina"

// ToolName is the routable tool name for a provider's headless run, e.g.
// "agent.codex.run". The agent loop routes these to the AgentRouter.
func ToolName(p Provider) string { return "agent." + string(p) + ".run" }

// ProviderFromToolName extracts the provider from a routable tool name, reporting
// false when name is not an "agent.<provider>.run" tool.
func ProviderFromToolName(name string) (Provider, bool) {
	const prefix, suffix = "agent.", ".run"
	if len(name) <= len(prefix)+len(suffix) {
		return "", false
	}
	if name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return "", false
	}
	p := Provider(name[len(prefix) : len(name)-len(suffix)])
	if !p.Valid() {
		return "", false
	}
	return p, true
}
