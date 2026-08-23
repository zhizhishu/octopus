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
// parallel_tool_calls=false:
//
//	400 X-OpenAI-Internal-Codex-Responses-Lite requires `parallel_tool_calls` to be false.
//
// applyCodexHeaderDefaultsWithFingerprint synthesizes that header on EVERY codex
// outbound, so oct owns the paired body field. These tests lock the pairing.

// A request that left parallel_tool_calls unset (the common case for a chat->codex
// request, or a codex client that simply omits the field) MUST come out of the shaper
// with parallel_tool_calls=false. Without this fix omitempty drops the field and the
// upstream 400s the same way it used to 400 on a missing reasoning.context.
func TestEnsureCodexParallelToolCallsDefaultsFalseWhenNil(t *testing.T) {
	req := &transformerModel.InternalLLMRequest{
		Model: "gpt-5.6-sol",
	}
	ensureCodexParallelToolCalls(req)
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected parallel_tool_calls to default to false, got %#v", req.ParallelToolCalls)
	}
}

// A request that explicitly sent parallel_tool_calls=true MUST be coerced to false,
// mirroring normalizeCodexReasoningEffort's override of unsupported effort values.
// The upstream demands a single hard value; an explicit client true is incoherent under
// the Lite header oct synthesizes, exactly like an explicit store=true is overridden
// elsewhere in this shaper.
func TestEnsureCodexParallelToolCallsOverridesExplicitTrue(t *testing.T) {
	trueVal := true
	req := &transformerModel.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		ParallelToolCalls: &trueVal,
	}
	ensureCodexParallelToolCalls(req)
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected explicit true to be coerced to false, got %#v", req.ParallelToolCalls)
	}
}

// A genuine codex CLI already sends parallel_tool_calls=false; echoing that back is what
// keeps oct faithful, so an explicit client false MUST survive untouched (no double-set).
func TestEnsureCodexParallelToolCallsKeepsExplicitFalse(t *testing.T) {
	falseVal := false
	req := &transformerModel.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		ParallelToolCalls: &falseVal,
	}
	ensureCodexParallelToolCalls(req)
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected explicit false to survive, got %#v", req.ParallelToolCalls)
	}
}

func TestEnsureCodexParallelToolCallsNilRequestIsNoop(t *testing.T) {
	ensureCodexParallelToolCalls(nil)
}

// End-to-end through the real codex shaper (not just the helper in isolation): a codex
// Responses request must come out of prepareCodexRequestShape carrying parallel_tool_calls=false,
// since that same path is what makes applyCodexHeaderDefaults stamp the Lite header. Header
// and body have to agree or the upstream 400s.
func TestPrepareCodexRequestShapeSetsParallelToolCallsFalse(t *testing.T) {
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
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected codex shape to set parallel_tool_calls=false, got %#v", req.ParallelToolCalls)
	}
}

// The mirror guard: a NON-codex channel never runs the codex shaper, so it must never gain
// a forced parallel_tool_calls. This is what keeps every other Responses upstream
// byte-identical to before the fix (an unknown field could itself be a 400 there, and
// even when known the value the upstream expects may differ).
func TestPrepareCodexRequestShapeSkipsParallelToolCallsForNonCodexChannel(t *testing.T) {
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
	if req.ParallelToolCalls != nil {
		t.Fatalf("expected no forced parallel_tool_calls on a non-codex channel, got %#v", req.ParallelToolCalls)
	}
}

// A genuine codex CLI's explicit parallel_tool_calls=false MUST survive the full shaper —
// the shaper is not allowed to overwrite a faithful value with its own default.
func TestPrepareCodexRequestShapePreservesExplicitFalseParallelToolCalls(t *testing.T) {
	content := "Say OK only"
	falseVal := false
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ParallelToolCalls: &falseVal,
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
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected explicit false to survive the shaper, got %#v", req.ParallelToolCalls)
	}
}

// A client that sent an incoherent parallel_tool_calls=true under the Lite header MUST
// come out of the full shaper with parallel_tool_calls=false — the shaper overrides the
// client's value to keep header and body consistent, exactly like store=true is overridden.
func TestPrepareCodexRequestShapeOverridesExplicitTrueParallelToolCalls(t *testing.T) {
	content := "Say OK only"
	trueVal := true
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ParallelToolCalls: &trueVal,
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
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls != false {
		t.Fatalf("expected explicit true to be overridden to false, got %#v", req.ParallelToolCalls)
	}
}
