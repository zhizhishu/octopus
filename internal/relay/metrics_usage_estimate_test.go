package relay

import (
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func estTestPtr(s string) *string { return &s }

// TestSetInternalResponseEstimatesUsageWhenUpstreamOmitsIt verifies that a
// successful response which delivered content but carries no upstream usage gets
// non-zero token counts estimated locally (so it is not logged/billed as 0),
// while resp.Usage stays untouched so the usage-missing flag is preserved.
func TestSetInternalResponseEstimatesUsageWhenUpstreamOmitsIt(t *testing.T) {
	m := NewRelayMetrics(0, 0, "", "gpt-4", &transformerModel.InternalLLMRequest{
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: estTestPtr("What is the capital of France? Please explain in some detail.")},
		}},
	})
	resp := &transformerModel.InternalLLMResponse{
		Choices: []transformerModel.Choice{{
			Index: 0,
			Message: &transformerModel.Message{
				Role:    "assistant",
				Content: transformerModel.MessageContent{Content: estTestPtr("The capital of France is Paris, a major European city known for art, fashion and cuisine.")},
			},
		}},
		// Usage intentionally nil — upstream omitted it.
	}

	m.SetInternalResponse(resp, "gpt-4")

	if m.Stats.OutputToken == 0 {
		t.Error("output tokens should be estimated (>0) when upstream omits usage but content was delivered")
	}
	if m.Stats.InputToken == 0 {
		t.Error("input tokens should be estimated from the request when upstream omits usage")
	}
	if resp.Usage != nil {
		t.Errorf("resp.Usage must stay nil so the usage-missing flag is preserved, got %+v", resp.Usage)
	}
}

// TestSetInternalResponseNoEstimateWhenEmpty verifies an empty response (no
// delivered content) is not given phantom tokens.
func TestSetInternalResponseNoEstimateWhenEmpty(t *testing.T) {
	m := NewRelayMetrics(0, 0, "", "gpt-4", nil)
	resp := &transformerModel.InternalLLMResponse{
		Choices: []transformerModel.Choice{{Index: 0, Message: &transformerModel.Message{Role: "assistant"}}},
	}
	m.SetInternalResponse(resp, "gpt-4")
	if m.Stats.OutputToken != 0 {
		t.Errorf("empty response must stay 0 output tokens, got %d", m.Stats.OutputToken)
	}
}

// TestSetInternalResponseEstimatesOnZeroCompletion covers the main Gemini-style
// case: usage IS present but reports zero completion tokens while content was
// delivered — completion is estimated locally, and an already-present prompt count
// is preserved (not re-estimated).
func TestSetInternalResponseEstimatesOnZeroCompletion(t *testing.T) {
	m := NewRelayMetrics(0, 0, "", "gpt-4", nil)
	resp := &transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{PromptTokens: 5, CompletionTokens: 0},
		Choices: []transformerModel.Choice{{
			Index:   0,
			Message: &transformerModel.Message{Role: "assistant", Content: transformerModel.MessageContent{Content: estTestPtr("A reasonably long completion that clearly carries more than zero tokens of content.")}},
		}},
	}
	m.SetInternalResponse(resp, "gpt-4")
	if m.Stats.OutputToken == 0 {
		t.Error("zero-completion-with-content should be estimated (>0)")
	}
	if m.Stats.InputToken != 5 {
		t.Errorf("existing prompt tokens should be preserved (5), got %d", m.Stats.InputToken)
	}
}

// TestSetInternalResponseKeepsUpstreamUsage verifies a real upstream usage is used
// verbatim (estimate does not override a non-zero completion count).
func TestSetInternalResponseKeepsUpstreamUsage(t *testing.T) {
	m := NewRelayMetrics(0, 0, "", "gpt-4", nil)
	resp := &transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{PromptTokens: 11, CompletionTokens: 22},
		Choices: []transformerModel.Choice{{
			Index:   0,
			Message: &transformerModel.Message{Role: "assistant", Content: transformerModel.MessageContent{Content: estTestPtr("hello world this is a longer reply than 22 tokens would suggest maybe")}},
		}},
	}
	m.SetInternalResponse(resp, "gpt-4")
	if m.Stats.OutputToken != 22 || m.Stats.InputToken != 11 {
		t.Errorf("real upstream usage must be used verbatim, got in=%d out=%d", m.Stats.InputToken, m.Stats.OutputToken)
	}
}
