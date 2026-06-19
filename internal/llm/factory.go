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
func Build(c Config) (Provider, error) {
	switch c.Provider {
	case "", "mock":
		return NewMockProvider(), nil
	case "openai":
		if c.Model == "" {
			return nil, fmt.Errorf("llm.model is required when llm.provider=openai")
		}
		return NewOpenAICompatProvider(c.BaseURL, c.APIKey, c.Model), nil
	default:
		return nil, fmt.Errorf("unknown llm.provider %q (want mock|openai)", c.Provider)
	}
}
