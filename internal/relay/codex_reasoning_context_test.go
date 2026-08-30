package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// The upstream rejects any request carrying the header
// X-OpenAI-Internal-Codex-Responses-Lite: true unless the body also sets
// reasoning.context="all_turns":
//
//	400 X-OpenAI-Internal-Codex-Responses-Lite requires `reasoning.context` to be `all_turns`.
//
// applyCodexHeaderDefaultsWithFingerprint synthesizes that header on EVERY codex
// outbound, so oct owns the paired body field. These tests lock the pairing.

func TestEnsureCodexReasoningContextDefaultsAllTurns(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "high",
	}

	ensureCodexReasoningContext(req)

	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context to default to all_turns, got %q", req.ReasoningContext)
	}
}

// A request with NO reasoning at all still needs the field: the upstream ties the
// requirement to the header, not to reasoning being requested. This is the case a
// naive fix (mirroring ensureCodexReasoningSummary's effort gate) would miss, leaving
// those requests still 400ing.
func TestEnsureCodexReasoningContextAppliesWithoutAnyReasoning(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model: "gpt-5.6-sol",
	}

	ensureCodexReasoningContext(req)

	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected reasoning.context on a request with no effort/summary, got %q", req.ReasoningContext)
	}
	// Must not invent an effort/summary as a side effect — that would change the
	// codex shape beyond the one field this fix owns.
	if req.ReasoningEffort != "" {
		t.Fatalf("expected reasoning.effort untouched, got %q", req.ReasoningEffort)
	}
	if req.ReasoningSummary != "" {
		t.Fatalf("expected reasoning.summary untouched, got %q", req.ReasoningSummary)
	}
}

// Lite's upstream contract is stricter than a client preference. Once the header is
// present, an explicit incompatible value must be corrected or the request 400s.
func TestEnsureCodexReasoningContextOverridesIncompatibleClientValue(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model:            "gpt-5.6-sol",
		ReasoningEffort:  "high",
		ReasoningContext: "client_chosen",
	}

	ensureCodexReasoningContext(req)

	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected Lite-compatible reasoning.context, got %q", req.ReasoningContext)
	}
}

func TestEnsureCodexReasoningContextNilRequestIsNoop(t *testing.T) {
	ensureCodexReasoningContext(nil)
}

// Ghost-pairing guard: a denylisted model (gpt-5.5 family) gets NO Lite header on the
// wire (applyCodexResponsesLiteHeader deletes it), so the body-side pairing field must
// not be forced either — a headless constraint has no justification and may itself be
// rejected by the upstream.
func TestEnsureCodexReasoningContextSkipsLiteDeniedModels(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model: "gpt-5.5",
	}

	ensureCodexReasoningContext(req)

	if req.ReasoningContext != "" {
		t.Fatalf("expected no forced reasoning.context for a Lite-denied model, got %q", req.ReasoningContext)
	}
}

// End-to-end through the real codex shaper (not just the helper in isolation): a codex
// Responses request must come out of prepareCodexRequestShape carrying the context, since
// that same path is what makes applyCodexHeaderDefaults stamp the Lite header. Header and
// body have to agree or the upstream 400s.
func TestPrepareCodexRequestShapeSetsReasoningContext(t *testing.T) {
	content := "Say OK only"
	req := &model.InternalLLMRequest{
		Model:        "gpt-5.6-sol",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse},
	}

	ra.prepareCodexRequestShape()

	if req.ReasoningContext != "all_turns" {
		t.Fatalf("expected codex shape to set reasoning.context=all_turns, got %q", req.ReasoningContext)
	}
}

// The mirror guard: a NON-codex channel never runs the codex shaper, so it must never gain
// a reasoning.context. This is what keeps every other Responses upstream byte-identical to
// before the fix (an unknown field could itself be a 400 there).
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
