package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// A genuine codex CLI sends reasoning:{effort,summary:"auto"}. ConvertToResponsesRequest must
// carry InternalLLMRequest.ReasoningSummary onto the outbound reasoning.summary so the upstream
// streams reasoning-summary deltas during a long reasoning turn. Dropping it (the historical
// bug: the reasoning struct had only an effort field) made a long max-effort turn stream
// nothing to the client until the final message — perceived as a hang.
func TestConvertToResponsesRequestCarriesReasoningSummary(t *testing.T) {
	content := "hi"
	req := &model.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "max",
		ReasoningSummary: "auto",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	out := ConvertToResponsesRequest(req)
	if out.Reasoning == nil {
		t.Fatalf("expected reasoning object to be present")
	}
	if out.Reasoning.Effort != "max" || out.Reasoning.Summary != "auto" {
		t.Fatalf("expected reasoning{effort:max,summary:auto}, got %#v", out.Reasoning)
	}

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"summary":"auto"`) {
		t.Fatalf("expected serialized reasoning.summary=auto, got %s", string(b))
	}
}

// When the client sent no summary and none was defaulted upstream (non-codex path), the
// outbound must NOT invent one — reasoning.summary stays absent so a plain OpenAI Responses
// upstream sees exactly what the client asked for.
func TestConvertToResponsesRequestOmitsAbsentReasoningSummary(t *testing.T) {
	content := "hi"
	req := &model.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "high",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	out := ConvertToResponsesRequest(req)
	if out.Reasoning == nil || out.Reasoning.Effort != "high" {
		t.Fatalf("expected reasoning.effort=high, got %#v", out.Reasoning)
	}
	if out.Reasoning.Summary != "" {
		t.Fatalf("expected no reasoning.summary when client sent none, got %q", out.Reasoning.Summary)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"summary"`) {
		t.Fatalf("expected reasoning.summary to be omitted, got %s", string(b))
	}
}
