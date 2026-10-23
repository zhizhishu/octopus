package gemini

import (
	"testing"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// P2: Gemini outbound used to disguise an upstream "blocked" as "the model
// finished talking normally" in two places:
//
//  1. a prompt-level block answers with {"promptFeedback":{"blockReason":...}}
//     and an EMPTY candidates array; the candidates loop never ran, so
//     resp.Choices stayed empty and the client got HTTP 200 + choices: [].
//  2. SAFETY/PROHIBITED_CONTENT/IMAGE_SAFETY/BLOCKLIST/SPII and the
//     MALFORMED_FUNCTION_CALL/UNEXPECTED_TOOL_CALL terminations fell into the
//     `default: return "stop"` arm, marking a cut-off reply as complete.
func TestConvertGeminiToLLMResponsePromptBlocked(t *testing.T) {
	for _, isStream := range []bool{false, true} {
		name := "non_stream"
		if isStream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
				Candidates:     nil,
				PromptFeedback: &transformerModel.GeminiPromptFeedback{BlockReason: "SAFETY"},
			}, isStream)

			if len(resp.Choices) != 1 {
				t.Fatalf("blocked prompt must yield exactly 1 choice, got %d (%#v)", len(resp.Choices), resp.Choices)
			}
			choice := resp.Choices[0]
			if choice.FinishReason == nil {
				t.Fatalf("blocked prompt must carry a finish reason, got nil")
			}
			if *choice.FinishReason != "content_filter" {
				t.Fatalf("blocked prompt must report content_filter, got %q", *choice.FinishReason)
			}
			if choice.Index != 0 {
				t.Fatalf("synthetic blocked choice must use index 0, got %d", choice.Index)
			}
		})
	}
}

// A genuinely empty response (no candidates AND no block reason) must keep the
// old behaviour: no invented choice, no invented finish reason.
func TestConvertGeminiToLLMResponseNoCandidatesNoBlockReason(t *testing.T) {
	for _, pf := range []*transformerModel.GeminiPromptFeedback{
		nil,
		{},                // present but blockReason empty
		{BlockReason: ""}, // explicit empty
	} {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			PromptFeedback: pf,
		}, false)
		if len(resp.Choices) != 0 {
			t.Fatalf("no candidates and no blockReason must stay empty, got %#v", resp.Choices)
		}
	}
}

func TestConvertGeminiFinishReason(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		// Normal paths must not regress.
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		// Existing behaviour kept.
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		// Newly mapped: upstream refused / suppressed the output.
		{"PROHIBITED_CONTENT", "content_filter"},
		{"IMAGE_SAFETY", "content_filter"},
		{"BLOCKLIST", "content_filter"},
		{"SPII", "content_filter"},
		// The call itself died: must never look like a completed reply.
		{"MALFORMED_FUNCTION_CALL", "error"},
		{"UNEXPECTED_TOOL_CALL", "error"},
		// Genuinely unknown values keep the tolerant old fallback.
		{"SOMETHING_ELSE_ENTIRELY", "stop"},
		{"", "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			if got := convertGeminiFinishReason(tt.reason); got != tt.want {
				t.Fatalf("convertGeminiFinishReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// End-to-end through the candidate loop: a candidate that stopped for safety or
// because the function call was malformed must not surface as finish_reason=stop.
func TestConvertGeminiToLLMResponseTerminationReasons(t *testing.T) {
	reasonPtr := func(s string) *string { return &s }

	t.Run("safety_candidate_is_content_filter", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: []*transformerModel.GeminiCandidate{
				{Index: 0, FinishReason: reasonPtr("SAFETY")},
			},
		}, false)
		if len(resp.Choices) != 1 || resp.Choices[0].FinishReason == nil {
			t.Fatalf("expected 1 choice with finish reason, got %#v", resp.Choices)
		}
		if got := *resp.Choices[0].FinishReason; got != "content_filter" {
			t.Fatalf("finish reason = %q, want %q", got, "content_filter")
		}
	})

	t.Run("malformed_function_call_is_not_stop", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(&transformerModel.GeminiGenerateContentResponse{
			Candidates: []*transformerModel.GeminiCandidate{
				{Index: 0, FinishReason: reasonPtr("MALFORMED_FUNCTION_CALL")},
			},
		}, false)
		if len(resp.Choices) != 1 || resp.Choices[0].FinishReason == nil {
			t.Fatalf("expected 1 choice with finish reason, got %#v", resp.Choices)
		}
		if got := *resp.Choices[0].FinishReason; got == "stop" {
			t.Fatalf("MALFORMED_FUNCTION_CALL must not be reported as %q", got)
		}
	})
}
