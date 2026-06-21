package agentcli

import (
	"fmt"
	"strings"
)

// cursorAdapter wraps the Cursor CLI. Flag surface from B7:
//   - run:   agent -p <prompt> --output-format json [--force] [--model <m>]
//   - auth:  agent login ; agent status ; agent logout (NO_OPEN_BROWSER=1 for login)
//   - key:   CURSOR_API_KEY (per-user injection)
//
// B7 corrections: the Cursor CLI cannot target a custom provider/endpoint (it is
// IDE-only and --model only selects Cursor-hosted models), so Cursor stays an
// account-backed agent — Pi, not Cursor, is the local path. Headless cancellation
// is UNDOCUMENTED, so the orchestrator relies on its own timeout + process-tree
// kill rather than a CLI flag. The binary name (`agent` here per B7) and the
// relocation of persistent state via HOME MUST be re-verified on a host with the
// CLI before this adapter is trusted.
type cursorAdapter struct{}

func (cursorAdapter) Provider() Provider { return ProviderCursor }

func (cursorAdapter) Capability() Capability {
	return Capability{
		Provider:     ProviderCursor,
		DisplayName:  "Cursor",
		AuthTypes:    []AuthType{AuthBrowserState, AuthAPIKey},
		BrowserAuth:  true,
		ToolName:     ToolName(ProviderCursor),
		CredStoreEnv: "HOME",
	}
}

func (cursorAdapter) CredStore() CredStore {
	// The Cursor CLI has no documented config-dir env var, so its persistent state
	// (~/.cursor, ~/.config) is relocated into the per-user encrypted store by
	// pointing HOME at the mounted dir. Re-verify on a host with the CLI.
	return CredStore{EnvVar: "HOME", ContainerDir: "/agent/cursor"}
}

func (cursorAdapter) VersionArgs() []string          { return []string{"agent", "--version"} }
func (cursorAdapter) ParseVersion(out string) string { return semverRe.FindString(out) }

func (cursorAdapter) LoginArgs(opt LoginOptions) []string { return []string{"agent", "login"} }

// LoginEnv disables Cursor's attempt to open a host browser (B7: NO_OPEN_BROWSER=1)
// — inside the container that browser can't reach the user anyway, so the broker
// streams the URL/code instead.
func (cursorAdapter) LoginEnv() []string   { return []string{"NO_OPEN_BROWSER=1"} }
func (cursorAdapter) StatusArgs() []string { return []string{"agent", "status"} }
func (cursorAdapter) LogoutArgs() []string { return []string{"agent", "logout"} }

func (cursorAdapter) AuthOK(statusOut string) bool {
	s := strings.ToLower(statusOut)
	if strings.Contains(s, "not logged in") || strings.Contains(s, "not authenticated") || strings.Contains(s, "logged out") {
		return false
	}
	return strings.Contains(s, "logged in") || strings.Contains(s, "authenticated") || strings.Contains(s, "@")
}

func (cursorAdapter) BuildRun(req RunRequest) (RunPlan, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return RunPlan{}, fmt.Errorf("cursor: prompt is required")
	}
	argv := []string{"agent", "-p", req.Prompt, "--output-format", "json", "--force"}
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	plan := RunPlan{
		Argv: argv,
		Env:  []string{"HOME=/agent/cursor"},
	}
	if req.AuthType == AuthAPIKey {
		plan.SecretNames = []string{"CURSOR_API_KEY"}
	}
	return plan, nil
}

func (cursorAdapter) Parse(p Provider, raw RawResult) AgentRunResult {
	res := baseResult(p, raw)
	isError := false
	var result map[string]any
	for _, o := range jsonObjects(raw.Stdout) {
		if str(o, "type") == "result" || o["result"] != nil {
			result = o
		}
	}
	if result == nil {
		result = firstJSONObject(raw.Stdout)
	}
	if result != nil {
		res.FinalText = firstNonEmpty(str(result, "result"), str(result, "text"), str(result, "response"))
		if boolField(result, "is_error") || str(result, "subtype") == "error" {
			isError = true
		}
		res.CostUSD = num(result, "total_cost_usd")
		if in, out := usageTokens(obj(result, "usage")); in+out > 0 {
			res.InputTokens, res.OutputTokens = in, out
		}
	} else {
		res.FinalText = lastNonEmptyLine(raw.Stdout)
	}
	finalizeStatus(&res, raw, isError)
	return res
}
