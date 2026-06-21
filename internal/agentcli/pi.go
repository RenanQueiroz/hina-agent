package agentcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// piAdapter wraps the local-only Pi coding agent (@earendil-works/pi-coding-agent).
// Pi is the account-free path: it talks ONLY to Hina's host-inference proxy in
// front of the Phase 11 managed llama.cpp backend — never a cloud provider. Surface
// from B7:
//   - models: a generated models.json custom provider, api="openai-completions",
//     baseUrl=<host-inference proxy>/v1, dummy key → local llama.cpp.
//   - lockdown: PI_OFFLINE=1 and --no-extensions/--no-skills/--no-context-files/
//     --no-tools; Pi has no built-in sandbox, so it runs inside `sbx`.
//
// Until Phase 11 provides that proxy there is no endpoint to target, so BuildRun
// fails closed when LocalEndpoint is empty and the orchestrator reports Pi
// unavailable (like a missing CLI). Pi's exact one-shot/RPC invocation is re-verified
// when Phase 11 wires the backend.
type piAdapter struct{}

func (piAdapter) Provider() Provider { return ProviderPi }

func (piAdapter) Capability() Capability {
	return Capability{
		Provider:     ProviderPi,
		DisplayName:  "Pi (local)",
		AuthTypes:    []AuthType{AuthLocalLlamaCpp},
		BrowserAuth:  false,
		LocalOnly:    true,
		ToolName:     ToolName(ProviderPi),
		CredStoreEnv: "",
	}
}

func (piAdapter) CredStore() CredStore {
	// Pi has no account credentials; HOME is relocated only so its config/cache stay
	// inside the per-user scratch rather than leaking into the container image.
	return CredStore{EnvVar: "HOME", ContainerDir: "/agent/pi"}
}

func (piAdapter) VersionArgs() []string          { return []string{"pi", "--version"} }
func (piAdapter) ParseVersion(out string) string { return semverRe.FindString(out) }

// Pi is account-free, so there is no interactive login: these are no-ops the broker
// never invokes (BrowserAuth is false and the only auth type is local_llamacpp).
func (piAdapter) LoginArgs(opt LoginOptions) []string { return nil }
func (piAdapter) LoginEnv() []string                  { return nil }
func (piAdapter) StatusArgs() []string                { return []string{"pi", "--version"} }
func (piAdapter) LogoutArgs() []string                { return nil }
func (piAdapter) AuthOK(string) bool                  { return true }

// piModelsFile is the StagingDir-relative path Pi's generated custom-provider config
// is staged at; PI_CONFIG_DIR points Pi at the directory holding it (in the staging
// mount, not the durable workspace).
const piModelsFile = "agent/models.json"

func (piAdapter) BuildRun(req RunRequest) (RunPlan, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return RunPlan{}, fmt.Errorf("pi: prompt is required")
	}
	// Fail closed: Pi must never reach a cloud provider, so without the host-inference
	// proxy endpoint there is nothing valid to target.
	if strings.TrimSpace(req.LocalEndpoint) == "" {
		return RunPlan{}, fmt.Errorf("pi: no local inference endpoint configured (needs the Phase 11 managed llama.cpp backend)")
	}
	// Reject a non-local endpoint (a typo/misconfig) so a "local-only" Pi run can't be
	// pointed at an arbitrary remote OpenAI-compatible server.
	if !IsLocalEndpoint(req.LocalEndpoint) {
		return RunPlan{}, fmt.Errorf("pi: refusing a non-local inference endpoint %q (Pi reaches only the host-inference proxy: host.docker.internal or loopback)", req.LocalEndpoint)
	}
	models, err := piModelsJSON(req.LocalEndpoint, req.Model)
	if err != nil {
		return RunPlan{}, err
	}
	argv := []string{
		"pi",
		"--no-extensions", "--no-skills", "--no-context-files", "--no-tools",
		"-p", req.Prompt,
	}
	plan := RunPlan{
		Argv: argv,
		Env: []string{
			"HOME=/agent/pi",
			"PI_OFFLINE=1",
			"PI_CONFIG_DIR=" + StagingDir + "/agent",
		},
		Files: []StagedFile{{RelPath: piModelsFile, Content: models}},
	}
	return plan, nil
}

// piModelsJSON builds Pi's custom-provider config pointing at the host-inference
// proxy with a dummy key (the local backend ignores it). model selects which served
// model id to use ("local" when unspecified).
func piModelsJSON(endpoint, model string) ([]byte, error) {
	if model == "" {
		model = "local"
	}
	cfg := map[string]any{
		"providers": map[string]any{
			"local": map[string]any{
				"api":     "openai-completions",
				"baseUrl": endpoint,
				"apiKey":  "local-llamacpp",
			},
		},
		"defaultModel": map[string]any{"provider": "local", "model": model},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func (piAdapter) Parse(p Provider, raw RawResult) AgentRunResult {
	res := baseResult(p, raw)
	var finalText string
	for _, o := range jsonObjects(raw.Stdout) {
		// Pi rpc/JSONL: assistant messages carry role+content; a final/result event
		// closes the turn. Keep the last assistant content as the final text.
		if str(o, "role") == "assistant" {
			if t := firstNonEmpty(str(o, "content"), str(o, "text"), str(o, "message")); t != "" {
				finalText = t
			}
		}
		if t := str(o, "type"); t == "message" || t == "assistant_message" || t == "result" {
			if c := firstNonEmpty(str(o, "content"), str(o, "text"), str(o, "result")); c != "" {
				finalText = c
			}
		}
		if u := obj(o, "usage"); u != nil {
			if in, out := usageTokens(u); in+out > 0 {
				res.InputTokens, res.OutputTokens = in, out
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
