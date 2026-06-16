package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestChatOutboundTransformStreamSkipsEmptyEventData(t *testing.T) {
	outbound := &ChatOutbound{}

	resp, err := outbound.TransformStream(context.Background(), []byte(" \n\t "))
	if err != nil {
		t.Fatalf("expected empty stream data to be skipped, got error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected empty stream data to return nil response, got %#v", resp)
	}
}

func TestChatOutboundTransformResponseRecoversOpenAICompatibleCacheAliases(t *testing.T) {
	outbound := &ChatOutbound{}

	resp, err := outbound.TransformResponse(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl_1",
			"object":"chat.completion",
			"model":"moonshot",
			"choices":[
				{"index":0,"message":{"role":"assistant","content":"ok"},"usage":{"cached_tokens":77}}
			],
			"usage":{"prompt_tokens":120,"completion_tokens":10,"total_tokens":130}
		}`)),
	})
	if err != nil {
		t.Fatalf("TransformResponse returned error: %v", err)
	}
	if resp.Usage == nil || resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 77 {
		t.Fatalf("expected cached tokens from choices usage alias, got %#v", resp.Usage)
	}
}

func TestChatOutboundDropsResponsesPreviousResponseID(t *testing.T) {
	content := "continue"
	previous := "resp_previous"
	req, err := (&ChatOutbound{}).TransformRequest(context.Background(), &model.InternalLLMRequest{
		Model:              "gpt-4o",
		PreviousResponseID: &previous,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}, "https://upstream.example", "key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Fatalf("chat request must not forward previous_response_id: %s", string(body))
	}
}

func TestChatOutboundTransformStreamRecoversLlamaCacheN(t *testing.T) {
	outbound := &ChatOutbound{}

	resp, err := outbound.TransformStream(context.Background(), []byte(`{
		"id":"chatcmpl_1",
		"object":"chat.completion.chunk",
		"model":"llama",
		"choices":[],
		"usage":{"prompt_tokens":0,"completion_tokens":1},
		"timings":{"cache_n":33}
	}`))
	if err != nil {
		t.Fatalf("TransformStream returned error: %v", err)
	}
	if resp.Usage == nil || resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 33 {
		t.Fatalf("expected cached tokens from timings.cache_n, got %#v", resp.Usage)
	}
	if resp.Usage.PromptTokens != 33 {
		t.Fatalf("expected prompt tokens to cover recovered cache tokens, got %d", resp.Usage.PromptTokens)
	}
}
