package openai

import (
	"context"
	"encoding/json"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"io"
	"testing"
)

func TestGPT62ChatDownstreamToResponsesUpstreamKeepsTools(t *testing.T) {
	text := "find recent commits"
	req := &model.InternalLLMRequest{Model: "gpt-6.2-astra", ReasoningEffort: "high",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
		Tools:    []model.Tool{{Type: "function", Function: model.Function{Name: "search", Description: "search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}},
	}
	outbound, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://api.openai.com/v1", "dummy")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(outbound.Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["model"] != "gpt-6.2-astra" {
		t.Fatalf("model changed: %s", body)
	}
	if got, ok := wire["tools"].([]any); !ok || len(got) != 1 {
		t.Fatalf("tool schema missing from Chat->Responses: %s", body)
	}
	if _, ok := wire["input"]; !ok {
		t.Fatalf("input missing: %s", body)
	}
	if wire["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("effort altered: %s", body)
	}
}
