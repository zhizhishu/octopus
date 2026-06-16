package authropic

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestMessageOutboundTransformStreamSkipsEmptyEventData(t *testing.T) {
	outbound := &MessageOutbound{}

	resp, err := outbound.TransformStream(context.Background(), []byte(" \n\t "))
	if err != nil {
		t.Fatalf("expected empty stream data to be skipped, got error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected empty stream data to return nil response, got %#v", resp)
	}
}

func TestConvertToAnthropicRequestPreservesNamedToolChoice(t *testing.T) {
	parallel := false
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: stringPtr("use the lookup tool")},
		}},
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:        "lookup",
				Description: "lookup data",
				Parameters:  []byte(`{"type":"object"}`),
			},
		}},
		ToolChoice: &model.ToolChoice{
			NamedToolChoice: &model.NamedToolChoice{
				Type:     "function",
				Function: model.ToolFunction{Name: "lookup"},
			},
		},
		ParallelToolCalls: &parallel,
	}

	converted := convertToAnthropicRequest(req)

	if converted.ToolChoice == nil {
		t.Fatalf("expected anthropic tool_choice to be set")
	}
	if converted.ToolChoice.Type != "tool" || converted.ToolChoice.Name == nil || *converted.ToolChoice.Name != "lookup" {
		t.Fatalf("unexpected anthropic tool_choice: %#v", converted.ToolChoice)
	}
	if converted.ToolChoice.DisableParallelToolUse == nil || !*converted.ToolChoice.DisableParallelToolUse {
		t.Fatalf("expected disable_parallel_tool_use=true, got %#v", converted.ToolChoice.DisableParallelToolUse)
	}
}

func TestConvertToAnthropicRequestMapsRequiredToolChoiceToAny(t *testing.T) {
	required := "required"
	parallel := true
	req := &model.InternalLLMRequest{
		Model: "claude-sonnet-4-5",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: stringPtr("call any tool")},
		}},
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:       "lookup",
				Parameters: []byte(`{"type":"object"}`),
			},
		}},
		ToolChoice:        &model.ToolChoice{ToolChoice: &required},
		ParallelToolCalls: &parallel,
	}

	converted := convertToAnthropicRequest(req)

	if converted.ToolChoice == nil || converted.ToolChoice.Type != "any" {
		t.Fatalf("expected required to map to anthropic any, got %#v", converted.ToolChoice)
	}
	if converted.ToolChoice.DisableParallelToolUse == nil || *converted.ToolChoice.DisableParallelToolUse {
		t.Fatalf("expected disable_parallel_tool_use=false, got %#v", converted.ToolChoice.DisableParallelToolUse)
	}
}

func TestConvertToolResultBlockAlwaysHasContent(t *testing.T) {
	toolCallID := "toolu_empty"
	block := convertToolResultBlock(model.Message{
		Role:       "tool",
		ToolCallID: &toolCallID,
	})

	if block.Content == nil || block.Content.Content == nil {
		t.Fatalf("expected empty tool_result content to be emitted, got %#v", block)
	}
	if *block.Content.Content != "" {
		t.Fatalf("expected empty tool_result content, got %q", *block.Content.Content)
	}
}
