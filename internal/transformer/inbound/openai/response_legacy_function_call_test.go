package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestResponseInboundNormalizesLegacyStreamFunctionCall(t *testing.T) {
	inbound := &ResponseInbound{}
	legacyCall := &model.FunctionCall{
		Name:      "ReadFile",
		Arguments: `{"path":"main.go"}`,
	}

	streamData, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_legacy",
		Object:  "chat.completion.chunk",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role:         "assistant",
				FunctionCall: legacyCall,
			},
		}},
	})
	if err != nil {
		t.Fatalf("TransformStream returned error: %v", err)
	}

	events := parseResponsesStreamEvents(t, string(streamData))
	var addedFunctionCall *ResponsesItem
	for _, event := range events {
		if event.Type == "response.output_item.added" && event.Item != nil && event.Item.Type == "function_call" {
			addedFunctionCall = event.Item
			break
		}
	}
	if addedFunctionCall == nil {
		t.Fatalf("expected a Responses function_call item, got %s", string(streamData))
	}
	if addedFunctionCall.Name != "ReadFile" || addedFunctionCall.CallID == "" {
		t.Fatalf("unexpected normalized function call item: %#v", addedFunctionCall)
	}
	finishReason := "function_call"
	finishedData, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_legacy",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	})
	if err != nil {
		t.Fatalf("TransformStream finish returned error: %v", err)
	}
	var finishedFunctionCall *ResponsesItem
	for _, event := range parseResponsesStreamEvents(t, string(finishedData)) {
		if event.Type == "response.output_item.done" && event.Item != nil && event.Item.Type == "function_call" {
			finishedFunctionCall = event.Item
			break
		}
	}
	if finishedFunctionCall == nil || finishedFunctionCall.CallID != addedFunctionCall.CallID {
		t.Fatalf("terminal function call did not preserve call_id %q: %#v", addedFunctionCall.CallID, finishedFunctionCall)
	}

	aggregated, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if aggregated == nil || len(aggregated.Choices) != 1 || aggregated.Choices[0].Message == nil || len(aggregated.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one aggregated legacy tool call, got %#v", aggregated)
	}
	if aggregated.Choices[0].Message.ToolCalls[0].ID != addedFunctionCall.CallID {
		t.Fatalf("stream event call_id %q differs from aggregated transcript id %q", addedFunctionCall.CallID, aggregated.Choices[0].Message.ToolCalls[0].ID)
	}
}

func TestResponseInboundNormalizesLegacyNonStreamFunctionCall(t *testing.T) {
	inbound := &ResponseInbound{}
	responseData, err := inbound.TransformResponse(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_legacy",
		Object:  "chat.completion",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role: "assistant",
				FunctionCall: &model.FunctionCall{
					Name:      "ReadFile",
					Arguments: `{"path":"main.go"}`,
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("TransformResponse returned error: %v", err)
	}

	var response ResponsesResponse
	if err := json.Unmarshal(responseData, &response); err != nil {
		t.Fatalf("failed to decode Responses output: %v", err)
	}
	if len(response.Output) != 1 || response.Output[0].Type != "function_call" {
		t.Fatalf("expected one function_call output item, got %s", string(responseData))
	}
	if response.Output[0].Name != "ReadFile" || response.Output[0].CallID == "" {
		t.Fatalf("unexpected normalized output item: %#v", response.Output[0])
	}
	stored, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if stored == nil || len(stored.Choices) != 1 || stored.Choices[0].Message == nil || len(stored.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one stored legacy tool call, got %#v", stored)
	}
	if stored.Choices[0].Message.ToolCalls[0].ID != response.Output[0].CallID {
		t.Fatalf("response call_id %q differs from stored transcript id %q", response.Output[0].CallID, stored.Choices[0].Message.ToolCalls[0].ID)
	}
}

func TestResponseInboundKeepsModernToolCallsWhenLegacyFieldAlsoExists(t *testing.T) {
	inbound := &ResponseInbound{}
	streamData, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_both",
		Object:  "chat.completion.chunk",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				FunctionCall: &model.FunctionCall{Name: "legacy", Arguments: "{}"},
				ToolCalls: []model.ToolCall{{
					Index: 0,
					ID:    "call_modern",
					Type:  "function",
					Function: model.FunctionCall{
						Name:      "modern",
						Arguments: "{}",
					},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("TransformStream returned error: %v", err)
	}

	if strings.Contains(string(streamData), `"name":"legacy"`) {
		t.Fatalf("legacy function_call must not replace a modern tool call: %s", string(streamData))
	}
	if !strings.Contains(string(streamData), `"name":"modern"`) {
		t.Fatalf("modern tool call was not preserved: %s", string(streamData))
	}
}

func TestResponseInboundIgnoresLegacyFunctionCallAfterModernToolCallAcrossChunks(t *testing.T) {
	inbound := &ResponseInbound{}
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_cross",
		Object:  "chat.completion.chunk",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				ToolCalls: []model.ToolCall{{
					Index: 0,
					ID:    "call_modern",
					Type:  "function",
					Function: model.FunctionCall{
						Name:      "modern",
						Arguments: `{"path":`,
					},
				}},
			},
		}},
	}); err != nil {
		t.Fatalf("modern chunk failed: %v", err)
	}

	legacyChunk, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_cross",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				FunctionCall: &model.FunctionCall{
					Name:      "legacy",
					Arguments: `"main.go"}`,
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("legacy chunk failed: %v", err)
	}
	if strings.Contains(string(legacyChunk), `"name":"legacy"`) {
		t.Fatalf("later legacy function_call must not inject a competing tool call: %s", string(legacyChunk))
	}

	finishReason := "tool_calls"
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_cross",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("finish chunk failed: %v", err)
	}

	aggregated, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if aggregated == nil || len(aggregated.Choices) != 1 || aggregated.Choices[0].Message == nil {
		t.Fatalf("expected one aggregated choice, got %#v", aggregated)
	}
	toolCalls := aggregated.Choices[0].Message.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one modern tool call, got %#v", toolCalls)
	}
	if toolCalls[0].ID != "call_modern" || toolCalls[0].Function.Name != "modern" {
		t.Fatalf("modern tool call was clobbered: %#v", toolCalls[0])
	}
	if toolCalls[0].Function.Arguments != `{"path":` {
		t.Fatalf("legacy arguments must not append onto modern tool call: %#v", toolCalls[0].Function.Arguments)
	}
}

func TestResponseInboundAggregatesLegacyFunctionCallOnSecondaryChoice(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "function_call"
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_multi",
		Object:  "chat.completion.chunk",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: stringPointer("choice0")}},
			},
			{
				Index: 1,
				Delta: &model.Message{
					Role: "assistant",
					FunctionCall: &model.FunctionCall{
						Name:      "ReadFile",
						Arguments: `{"path":"main.go"}`,
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("multi-choice chunk failed: %v", err)
	}
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_multi",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{
			{Index: 0, FinishReason: stringPointer("stop")},
			{Index: 1, FinishReason: &finishReason},
		},
	}); err != nil {
		t.Fatalf("finish chunk failed: %v", err)
	}

	aggregated, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if aggregated == nil || len(aggregated.Choices) < 2 {
		t.Fatalf("expected two aggregated choices, got %#v", aggregated)
	}
	secondary := aggregated.Choices[1]
	if secondary.Message == nil || len(secondary.Message.ToolCalls) != 1 {
		t.Fatalf("choice 1 legacy function_call was lost: %#v", secondary)
	}
	if secondary.Message.ToolCalls[0].Function.Name != "ReadFile" {
		t.Fatalf("unexpected secondary tool call: %#v", secondary.Message.ToolCalls[0])
	}
	if secondary.Message.ToolCalls[0].ID == "" {
		t.Fatalf("secondary legacy tool call missing call_id")
	}
}

func TestResponseInboundReusesLegacyCallIDAcrossArgumentChunks(t *testing.T) {
	inbound := &ResponseInbound{}
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_frag",
		Object:  "chat.completion.chunk",
		Model:   "glm-5.2",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role: "assistant",
				FunctionCall: &model.FunctionCall{
					Name:      "ReadFile",
					Arguments: `{"path":`,
				},
			},
		}},
	}); err != nil {
		t.Fatalf("name/args head chunk failed: %v", err)
	}
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_frag",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				FunctionCall: &model.FunctionCall{
					Arguments: `"main.go"}`,
				},
			},
		}},
	}); err != nil {
		t.Fatalf("args tail chunk failed: %v", err)
	}

	finishReason := "function_call"
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:     "chatcmpl_frag",
		Object: "chat.completion.chunk",
		Model:  "glm-5.2",
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("finish chunk failed: %v", err)
	}

	aggregated, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if aggregated == nil || len(aggregated.Choices) != 1 || aggregated.Choices[0].Message == nil {
		t.Fatalf("expected aggregated choice, got %#v", aggregated)
	}
	toolCalls := aggregated.Choices[0].Message.ToolCalls
	if len(toolCalls) != 1 {
		t.Fatalf("expected one tool call, got %#v", toolCalls)
	}
	if toolCalls[0].Function.Name != "ReadFile" {
		t.Fatalf("unexpected tool name: %#v", toolCalls[0])
	}
	if toolCalls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Fatalf("arguments were not concatenated across legacy chunks: %#v", toolCalls[0].Function.Arguments)
	}
	if toolCalls[0].ID == "" {
		t.Fatalf("missing stable call_id for fragmented legacy call")
	}
	if inbound.legacyFunctionCallIDByChoice[0] != toolCalls[0].ID {
		t.Fatalf("call_id was not reused across fragments: map=%q tool=%q", inbound.legacyFunctionCallIDByChoice[0], toolCalls[0].ID)
	}
}

func stringPointer(value string) *string {
	return &value
}
