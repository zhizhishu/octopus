package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// ConvertToResponsesRequest must project InternalLLMRequest.ReasoningContext onto the
// outbound reasoning.context. The upstream 400s a codex request that carries the
// X-OpenAI-Internal-Codex-Responses-Lite header without it.
func TestConvertToResponsesRequestCarriesReasoningContext(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "high",
		ReasoningSummary: "auto",
		ReasoningContext: "all_turns",
	}

	result := ConvertToResponsesRequest(req)

	if result.Reasoning == nil {
		t.Fatal("expected reasoning object on the outbound request")
	}
	if result.Reasoning.Context != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns, got %q", result.Reasoning.Context)
	}

	body, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal outbound request: %v", err)
	}
	if !strings.Contains(string(body), `"context":"all_turns"`) {
		t.Fatalf("expected reasoning.context on the wire, got %s", body)
	}
}

// A context-only request must still materialize a reasoning object. The old guard only
// created one when effort/summary/budget was set, so a codex turn with no reasoning
// requested shipped no reasoning at all — and kept 400ing despite the header.
func TestConvertToResponsesRequestMaterializesReasoningForContextOnly(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningContext: "all_turns",
	}

	result := ConvertToResponsesRequest(req)

	if result.Reasoning == nil {
		t.Fatal("expected a context-only request to still carry a reasoning object")
	}
	if result.Reasoning.Context != "all_turns" {
		t.Fatalf("expected reasoning.context=all_turns, got %q", result.Reasoning.Context)
	}
	if result.Reasoning.Effort != "" {
		t.Fatalf("expected no synthesized effort, got %q", result.Reasoning.Effort)
	}
	if result.Reasoning.Summary != "" {
		t.Fatalf("expected no synthesized summary, got %q", result.Reasoning.Summary)
	}
}

// The reasoning.context requirement is codex-only: it is filled by the codex shaper,
// which never runs for a plain (non-codex) Responses channel. Such a channel's upstream
// may reject the unknown field, so its bytes must stay exactly as before — no context
// key anywhere on the wire, whether or not reasoning is otherwise requested.
func TestConvertToResponsesRequestOmitsContextWhenUnset(t *testing.T) {
	testCases := []struct {
		name string
		req  *model.InternalLLMRequest
	}{
		{
			name: "reasoning requested without context",
			req: &model.InternalLLMRequest{
				Model:            "o3",
				ReasoningEffort:  "medium",
				ReasoningSummary: "auto",
			},
		},
		{
			name: "no reasoning at all",
			req: &model.InternalLLMRequest{
				Model: "gpt-4o",
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			result := ConvertToResponsesRequest(testCase.req)

			if result.Reasoning != nil && result.Reasoning.Context != "" {
				t.Fatalf("expected no reasoning.context, got %q", result.Reasoning.Context)
			}

			body, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal outbound request: %v", err)
			}
			// omitempty must drop the key entirely: a non-codex upstream that does not
			// know the field should never even see it.
			if strings.Contains(string(body), `"context"`) {
				t.Fatalf("expected no context key on a non-codex outbound, got %s", body)
			}
		})
	}
}
