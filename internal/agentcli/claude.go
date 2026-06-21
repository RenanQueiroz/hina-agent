package agentcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeAdapter wraps the Claude Code CLI (`claude`). Flag surface from B7:
//   - run:   claude -p <prompt> --output-format json [--json-schema <f>]
//     [--max-turns N] --dangerously-skip-permissions
//   - auth:  claude auth login ; claude auth status ; claude auth logout ;
//     claude setup-token (for an OAuth token)
//   - state: CLAUDE_CONFIG_DIR (per-user credential store)
//
// Auth precedence (B7): ANTHROPIC_API_KEY OVERRIDES subscription in -p, so a
// browser-state profile keeps it UNSET and relies on the mounted CLAUDE_CONFIG_DIR;
// --bare ignores OAuth, so it is never used for subscription. --dangerously-skip-
// permissions only removes the CLI's own prompts and is safe ONLY because the run
// is already inside `sbx`. Re-verify before trusting.
type claudeAdapter struct{}

func (claudeAdapter) Provider() Provider { return ProviderClaude }

func (claudeAdapter) Capability() Capability {
	return Capability{
		Provider:     ProviderClaude,
		DisplayName:  "Claude Code",
		AuthTypes:    []AuthType{AuthBrowserState, AuthAPIKey, AuthOAuthToken},
		BrowserAuth:  true,
		ToolName:     ToolName(ProviderClaude),
		CredStoreEnv: "CLAUDE_CONFIG_DIR",
	}
}

func (claudeAdapter) CredStore() CredStore {
	return CredStore{EnvVar: "CLAUDE_CONFIG_DIR", ContainerDir: "/agent/claude"}
}

func (claudeAdapter) VersionArgs() []string          { return []string{"claude", "--version"} }
func (claudeAdapter) ParseVersion(out string) string { return semverRe.FindString(out) }

func (claudeAdapter) LoginArgs(opt LoginOptions) []string {
	// Claude's interactive login is the same command regardless of device-code vs.
	// browser; inside a container with no reachable localhost callback it falls back
	// to the paste-a-code flow automatically.
	return []string{"claude", "auth", "login"}
}

func (claudeAdapter) LoginEnv() []string   { return nil }
func (claudeAdapter) StatusArgs() []string { return []string{"claude", "auth", "status"} }
func (claudeAdapter) LogoutArgs() []string { return []string{"claude", "auth", "logout"} }

func (claudeAdapter) AuthOK(statusOut string) bool {
	s := strings.ToLower(statusOut)
	if strings.Contains(s, "not logged in") || strings.Contains(s, "not authenticated") || strings.Contains(s, "logged out") {
		return false
	}
	return strings.Contains(s, "logged in") || strings.Contains(s, "authenticated") ||
		strings.Contains(s, "subscription") || strings.Contains(s, "@")
}

func (claudeAdapter) BuildRun(req RunRequest) (RunPlan, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return RunPlan{}, fmt.Errorf("claude: prompt is required")
	}
	argv := []string{
		"claude", "-p", req.Prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}
	var files []StagedFile
	if req.Structured && len(req.SchemaJSON) > 0 {
		const schemaName = "output-schema.json"
		argv = append(argv, "--json-schema", StagingDir+"/"+schemaName)
		files = append(files, StagedFile{RelPath: schemaName, Content: req.SchemaJSON})
	}
	if req.MaxTurns > 0 {
		argv = append(argv, "--max-turns", fmt.Sprintf("%d", req.MaxTurns))
	}
	plan := RunPlan{
		Argv:  argv,
		Env:   []string{"CLAUDE_CONFIG_DIR=/agent/claude"},
		Files: files,
	}
	switch req.AuthType {
	case AuthAPIKey:
		plan.SecretNames = []string{"ANTHROPIC_API_KEY"}
	case AuthOAuthToken:
		plan.SecretNames = []string{"CLAUDE_CODE_OAUTH_TOKEN"}
	case AuthBrowserState:
		// Keep ANTHROPIC_API_KEY UNSET so the mounted subscription state wins.
	}
	return plan, nil
}

func (claudeAdapter) Parse(p Provider, raw RawResult) AgentRunResult {
	res := baseResult(p, raw)
	isError := false
	// --output-format json emits a single result object; --output-format stream-json
	// emits many lines whose LAST type:"result" carries the summary. Scan all and
	// keep the last result-typed object.
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
		res.FinalText = str(result, "result")
		if str(result, "subtype") == "error" || boolField(result, "is_error") {
			isError = true
		}
		res.CostUSD = num(result, "total_cost_usd")
		if in, out := usageTokens(obj(result, "usage")); in+out > 0 {
			res.InputTokens, res.OutputTokens = in, out
		}
		// When a schema was requested, the result string is itself JSON — surface it
		// as Structured so callers don't re-parse.
		if strings.TrimSpace(res.FinalText) != "" {
			var probe any
			if json.Unmarshal([]byte(res.FinalText), &probe) == nil {
				if _, ok := probe.(map[string]any); ok {
					res.Structured = json.RawMessage(res.FinalText)
				}
			}
		}
	} else {
		res.FinalText = lastNonEmptyLine(raw.Stdout)
	}
	finalizeStatus(&res, raw, isError)
	return res
}

// boolField returns m[key] as a bool (false if absent or not a bool).
func boolField(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
