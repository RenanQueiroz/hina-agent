package llm

import "testing"

// TestBuildCompatRequiresBaseURL proves the factory fails closed when the local
// openai-compat provider has no base_url, rather than defaulting to cloud.
func TestBuildCompatRequiresBaseURL(t *testing.T) {
	if _, err := Build(Config{Provider: "openai-compat", Model: "m"}); err == nil {
		t.Fatal("openai-compat without base_url must error, not default to cloud")
	}
	p, err := Build(Config{Provider: "openai-compat", Model: "m", BaseURL: "http://localhost:8080/v1"})
	if err != nil {
		t.Fatalf("openai-compat with base_url: %v", err)
	}
	if p.Name() != "openai-compat" {
		t.Fatalf("provider name = %q", p.Name())
	}
}
