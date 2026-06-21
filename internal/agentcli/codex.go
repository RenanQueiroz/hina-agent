package agentcli

import (
	"fmt"
	"regexp"
	"strings"
)

// codexAdapter wraps the OpenAI Codex CLI (`codex`). Flag surface from B7:
//   - run:   codex exec --json [--output-schema <f>] --cd <ws> --skip-git-repo-check
//     --sandbox workspace-write --ask-for-approval never [-m <model>] <prompt>
//   - auth:  codex login [--device-auth] ; codex login status ; codex logout
//   - state: CODEX_HOME (per-user credential store)
//
// Corrections applied (B7): CODEX_API_KEY is dropped — API-key auth uses
// OPENAI_API_KEY; --full-auto is deprecated, so autonomy is expressed via
// --sandbox workspace-write --ask-for-approval never (running already inside `sbx`,
// the CLI's own sandbox just needs its prompts removed). Re-verify before trusting.
type codexAdapter struct{}

func (codexAdapter) Provider() Provider { return ProviderCodex }

func (codexAdapter) Capability() Capability {
	return Capability{
		Provider:     ProviderCodex,
		DisplayName:  "Codex",
		AuthTypes:    []AuthType{AuthBrowserState, AuthAPIKey},
		BrowserAuth:  true,
		ToolName:     ToolName(ProviderCodex),
		CredStoreEnv: "CODEX_HOME",
	}
}

func (codexAdapter) CredStore() CredStore {
	return CredStore{EnvVar: "CODEX_HOME", ContainerDir: "/agent/codex"}
}

func (codexAdapter) VersionArgs() []string { return []string{"codex", "--version"} }

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func (codexAdapter) ParseVersion(out string) string { return semverRe.FindString(out) }

func (codexAdapter) LoginArgs(opt LoginOptions) []string {
	if opt.DeviceAuth {
		return []string{"codex", "login", "--device-auth"}
	}
	return []string{"codex", "login"}
}

func (codexAdapter) LoginEnv() []string   { return nil }
func (codexAdapter) StatusArgs() []string { return []string{"codex", "login", "status"} }
func (codexAdapter) LogoutArgs() []string { return []string{"codex", "logout"} }

func (codexAdapter) AuthOK(statusOut string) bool {
	s := strings.ToLower(statusOut)
	if strings.Contains(s, "not logged in") || strings.Contains(s, "not authenticated") {
		return false
	}
	return strings.Contains(s, "logged in") || strings.Contains(s, "authenticated") || strings.Contains(s, "account")
}

func (codexAdapter) BuildRun(req RunRequest) (RunPlan, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return RunPlan{}, fmt.Errorf("codex: prompt is required")
	}
	argv := []string{
		"codex", "exec", "--json",
		"--cd", "/workspace",
		"--skip-git-repo-check",
		"--sandbox", "workspace-write",
		"--ask-for-approval", "never",
	}
	if req.Model != "" {
		argv = append(argv, "-m", req.Model)
	}
	var files []StagedFile
	if req.Structured && len(req.SchemaJSON) > 0 {
		const schemaName = "output-schema.json"
		argv = append(argv, "--output-schema", StagingDir+"/"+schemaName)
		files = append(files, StagedFile{RelPath: schemaName, Content: req.SchemaJSON})
	}
	// The prompt is the trailing positional argument (never a flag value), so a
	// prompt that starts with "-" can't be mistaken for an option.
	argv = append(argv, "--", req.Prompt)

	plan := RunPlan{
		Argv:  argv,
		Env:   []string{"CODEX_HOME=/agent/codex"},
		Files: files,
	}
	// API-key auth forwards OPENAI_API_KEY (NOT CODEX_API_KEY — dropped per B7);
	// browser-state auth uses the mounted CODEX_HOME and injects nothing.
	if req.AuthType == AuthAPIKey {
		plan.SecretNames = []string{"OPENAI_API_KEY"}
	}
	return plan, nil
}

func (codexAdapter) Parse(p Provider, raw RawResult) AgentRunResult {
	res := baseResult(p, raw)
	var finalText string
	seenFile := map[string]struct{}{}
	addFile := func(path string) {
		if path == "" {
			return
		}
		if _, dup := seenFile[path]; dup {
			return
		}
		seenFile[path] = struct{}{}
		res.ChangedFiles = append(res.ChangedFiles, path)
	}

	for _, o := range jsonObjects(raw.Stdout) {
		// Newer schema: {"type":"item.completed","item":{"type":"agent_message","text":...}}
		if item := obj(o, "item"); item != nil {
			itemType := str(item, "type")
			switch {
			case strings.Contains(itemType, "agent_message"), strings.Contains(itemType, "assistant"):
				if t := firstNonEmpty(str(item, "text"), str(item, "message")); t != "" {
					finalText = t
				}
			case strings.Contains(itemType, "file_change"), strings.Contains(itemType, "patch"):
				for _, ch := range arrOfObj(item, "changes") {
					addFile(str(ch, "path"))
				}
				addFile(str(item, "path"))
			}
		}
		// Older schema: {"msg":{"type":"agent_message","message":...}}
		if msg := obj(o, "msg"); msg != nil {
			switch str(msg, "type") {
			case "agent_message":
				if t := firstNonEmpty(str(msg, "message"), str(msg, "text")); t != "" {
					finalText = t
				}
			case "token_count", "token_usage":
				if in, out := usageTokens(msg); in+out > 0 {
					res.InputTokens, res.OutputTokens = in, out
				}
			}
		}
		// turn.completed carries usage at the top level.
		if u := obj(o, "usage"); u != nil {
			if in, out := usageTokens(u); in+out > 0 {
				res.InputTokens, res.OutputTokens = in, out
			}
		}
		// Flat schema: {"type":"agent_message","message":...}
		if str(o, "type") == "agent_message" {
			if t := firstNonEmpty(str(o, "message"), str(o, "text")); t != "" {
				finalText = t
			}
		}
	}
	if finalText == "" {
		finalText = lastNonEmptyLine(raw.Stdout)
	}
	res.FinalText = finalText
	finalizeStatus(&res, raw, false)
	return res
}

// firstNonEmpty returns the first non-empty string among its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// arrOfObj returns m[key] as a slice of objects (skipping non-object elements).
func arrOfObj(m map[string]any, key string) []map[string]any {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, e := range arr {
		if o, ok := e.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}
