package llm

import "fmt"

// Config selects and configures a provider (admin-owned; users never choose).
type Config struct {
	Provider     string // mock | openai
	Model        string
	BaseURL      string
	APIKey       string
	SystemPrompt string
}

// Build constructs a Provider from config, defaulting to the mock provider so
// the server runs with no setup.
//
//   - mock          credential-free streamed echo (default)
//   - openai        cloud OpenAI via the official SDK's Responses API
//   - openai-compat any OpenAI-compatible /chat/completions endpoint
//     (e.g. a local llama.cpp server via base_url)
func Build(c Config) (Provider, error) {
	switch c.Provider {
	case "", "mock":
		return NewMockProvider(), nil
	case "openai":
		if c.Model == "" {
			return nil, fmt.Errorf("llm.model is required when llm.provider=openai")
		}
		return NewOpenAIResponsesProvider(c.APIKey, c.Model, c.BaseURL), nil
	case "openai-compat":
		if c.Model == "" {
			return nil, fmt.Errorf("llm.model is required when llm.provider=openai-compat")
		}
		// Fail closed on a missing local endpoint rather than defaulting to cloud.
		if c.BaseURL == "" {
			return nil, fmt.Errorf("llm.base_url is required when llm.provider=openai-compat (the local endpoint); refusing to default to cloud OpenAI")
		}
		return NewOpenAICompatProvider(c.BaseURL, c.APIKey, c.Model), nil
	default:
		return nil, fmt.Errorf("unknown llm.provider %q (want mock|openai|openai-compat)", c.Provider)
	}
}
