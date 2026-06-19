package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatProvider is a thin streaming client for any OpenAI-compatible
// /chat/completions endpoint — cloud OpenAI (default base URL) or a local
// llama.cpp server (set BaseURL to e.g. http://localhost:8080/v1). It owns
// cancellation and SSE parsing directly rather than depending on an SDK, which
// keeps the control plane dependency-light; an SDK can replace it later if
// Responses-only features (hosted tools) are needed.
type OpenAICompatProvider struct {
	BaseURL string // e.g. https://api.openai.com/v1 or http://localhost:8080/v1
	APIKey  string
	Model   string
	client  *http.Client
}

// NewOpenAICompatProvider builds the provider. baseURL defaults to the OpenAI API.
func NewOpenAICompatProvider(baseURL, apiKey, model string) *OpenAICompatProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompatProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// Name implements Provider.
func (p *OpenAICompatProvider) Name() string { return "openai-compat" }

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Stream implements Provider.
func (p *OpenAICompatProvider) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	body, err := json.Marshal(chatRequest{Model: p.Model, Messages: req.Messages, Stream: true})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		snippet := make([]byte, 512)
		n, _ := resp.Body.Read(snippet)
		return nil, fmt.Errorf("llm backend returned %s: %s", resp.Status, strings.TrimSpace(string(snippet[:n])))
	}

	out := make(chan Delta)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk chatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue // tolerate keep-alive / non-JSON lines
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content == "" {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- Delta{Text: c.Delta.Content}:
				}
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			out <- Delta{Err: err}
			return
		}
		select {
		case <-ctx.Done():
		case out <- Delta{Done: true}:
		}
	}()
	return out, nil
}
