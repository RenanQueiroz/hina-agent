package llm

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIResponsesProvider streams from cloud OpenAI via the official Go SDK's
// Responses API. This is the plan's cloud-OpenAI path (hosted tools live on
// /responses in later phases). Local OpenAI-compatible servers use
// OpenAICompatProvider (chat/completions) instead.
type OpenAIResponsesProvider struct {
	client openai.Client
	model  string
}

// NewOpenAIResponsesProvider builds the provider. baseURL is optional (defaults
// to the OpenAI API) and lets it target a /responses-compatible endpoint.
func NewOpenAIResponsesProvider(apiKey, model, baseURL string) *OpenAIResponsesProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAIResponsesProvider{client: openai.NewClient(opts...), model: model}
}

// Name implements Provider.
func (p *OpenAIResponsesProvider) Name() string { return "openai" }

// Stream implements Provider.
func (p *OpenAIResponsesProvider) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	var instructions string
	items := make(responses.ResponseInputParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if instructions != "" {
				instructions += "\n"
			}
			instructions += m.Content
			continue
		}
		items = append(items, responses.ResponseInputItemUnionParam{
			OfMessage: &responses.EasyInputMessageParam{
				Role:    responses.EasyInputMessageRole(m.Role),
				Content: responses.EasyInputMessageContentUnionParam{OfString: param.NewOpt(m.Content)},
			},
		})
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(p.model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
	}
	if instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	}

	stream := p.client.Responses.NewStreaming(ctx, params)
	out := make(chan Delta)
	go func() {
		defer close(out)
		for stream.Next() {
			ev := stream.Current()
			if ev.Type != "response.output_text.delta" || ev.Delta == "" {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- Delta{Text: ev.Delta}:
			}
		}
		if err := stream.Err(); err != nil && ctx.Err() == nil {
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
