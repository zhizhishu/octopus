package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// Octopus no longer injects a reasoning.context default. The real Codex CLI 0.163 sends no
// X-OpenAI-Internal-Codex-Responses-Lite header and oct no longer synthesizes one, so there is
// no header/body pairing rule that would demand reasoning.context="all_turns". Whatever the
// client sent is forwarded faithfully, and an absent field stays absent.
//
// These tests drive the REAL codex shaper (prepareCodexRequestShape), not a deleted helper.

// codexShapeAttempt builds the minimal relayAttempt that makes prepareCodexRequestShape run the
// codex branch for an OpenAI-Response channel.
func codexShapeAttempt(req *model.InternalLLMRequest) *relayAttempt {
	return &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}
}

// Absent reasoning.context is NOT injected by the shaper.
func TestPrepareCodexRequestShapeDoesNotInjectReasoningContext(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-5.6-sol",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context injection, got %q", req.ReasoningContext)
	}
}

// A request that carries no reasoning at all also never gains a reasoning.context, and the
// effort normalization (which stays) still defaults a gpt-5.6 empty effort to high.
func TestPrepareCodexRequestShapeDoesNotInjectReasoningContextWithoutEffort(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-5.6-sol",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ReasoningContext != "" {
		t.Fatalf("expected reasoning.context to stay absent, got %q", req.ReasoningContext)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("precondition: gpt-5.6 empty effort should still default to high, got %q", req.ReasoningEffort)
	}
}

// A client that explicitly chose a reasoning.context keeps it verbatim — the shaper no longer
// overrides it (the old forced all_turns value is gone).
func TestPrepareCodexRequestShapePreservesExplicitReasoningContext(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		RawAPIFormat:     model.APIFormatOpenAIResponse,
		ReasoningContext: "client_chosen",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ReasoningContext != "client_chosen" {
		t.Fatalf("expected explicit reasoning.context preserved, got %q", req.ReasoningContext)
	}
}

// A NON-codex channel never runs the codex shaper, so it must never gain a reasoning.context.
// This keeps every other Responses upstream untouched.
func TestPrepareCodexRequestShapeSkipsReasoningContextForNonCodexChannel(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-4o",
		RawAPIFormat: model.APIFormatOpenAIChatCompletion,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIChat,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningContext != "" {
		t.Fatalf("expected no reasoning.context on a non-codex channel, got %q", req.ReasoningContext)
	}
}
