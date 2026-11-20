package relay

import (
	"encoding/json"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// Octopus no longer force-sets parallel_tool_calls=false to pair with the
// X-OpenAI-Internal-Codex-Responses-Lite header (oct no longer synthesizes that header, and the
// real Codex CLI 0.163 sends no such header). An explicit client true is preserved; a false
// is preserved; and on turns where ensureCodexAgentContext introduces no default Codex tools an
// absent field stays absent. The only remaining default that sets false is ensureCodexAgentContext
// itself when it injects the default Codex tools (kept unchanged).

// An explicit parallel_tool_calls=true is preserved — the old shaper override to false is gone.
func TestPrepareCodexRequestShapePreservesExplicitTrueParallelToolCalls(t *testing.T) {
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

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("expected explicit parallel_tool_calls=true to survive the shaper, got %#v", req.ParallelToolCalls)
	}
}

// A genuine codex CLI's explicit parallel_tool_calls=false survives the full shaper untouched.
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

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("expected explicit parallel_tool_calls=false preserved, got %#v", req.ParallelToolCalls)
	}
}

// On a turn where ensureCodexAgentContext injects the default Codex tools, parallel_tool_calls is
// still defaulted to false as part of that tool round-trip (the AgentContext default is kept; only
// the separate Lite-pairing force-set was removed).
func TestPrepareCodexRequestShapeDefaultsParallelToolCallsFalseViaAgentContext(t *testing.T) {
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

	if len(req.Tools) == 0 {
		t.Fatalf("precondition: default Codex tools should have been injected, got none")
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("expected AgentContext default parallel_tool_calls=false alongside the tools, got %#v", req.ParallelToolCalls)
	}
}

// On a tool-output continuation ensureCodexAgentContext introduces no default tools, so an absent
// parallel_tool_calls stays absent instead of being forced (the old forced-false is gone).
func TestPrepareCodexRequestShapeDoesNotForceParallelToolCallsOnToolOutputTurn(t *testing.T) {
	callID := "call_1"
	output := "result"
	req := &model.InternalLLMRequest{
		Model:             "gpt-5.6-sol",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ResponsesInputRaw: json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"result"}]`),
		Messages: []model.Message{
			{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID:       callID,
				Type:     "function",
				Function: model.FunctionCall{Name: "f", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: &callID, Content: model.MessageContent{Content: &output}},
		},
	}

	codexShapeAttempt(req).prepareCodexRequestShape()

	if req.ParallelToolCalls != nil {
		t.Fatalf("expected absent parallel_tool_calls to stay absent on a tool-output turn, got %#v", req.ParallelToolCalls)
	}
}

// A NON-codex channel never runs the codex shaper, so it must never gain a forced
// parallel_tool_calls even when the client sent true.
func TestPrepareCodexRequestShapeSkipsParallelToolCallsForNonCodexChannel(t *testing.T) {
	content := "Say OK only"
	trueVal := true
	req := &model.InternalLLMRequest{
		Model:             "gpt-4o",
		RawAPIFormat:      model.APIFormatOpenAIChatCompletion,
		ParallelToolCalls: &trueVal,
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

	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("expected explicit true to survive on a non-codex channel, got %#v", req.ParallelToolCalls)
	}
}
