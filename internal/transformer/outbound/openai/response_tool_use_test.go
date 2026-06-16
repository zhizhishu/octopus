package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestResponseOutboundTransformStreamSkipsEmptyEventData(t *testing.T) {
	outbound := &ResponseOutbound{}

	resp, err := outbound.TransformStream(context.Background(), []byte(" \n\t "))
	if err != nil {
		t.Fatalf("expected empty stream data to be skipped, got error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected empty stream data to return nil response, got %#v", resp)
	}
}

func TestConvertToolChoiceToResponsesNormalizesToolType(t *testing.T) {
	tc := convertToolChoiceToResponses(&model.ToolChoice{
		NamedToolChoice: &model.NamedToolChoice{
			Type:     "tool",
			Function: model.ToolFunction{Name: "lookup"},
		},
	})

	if tc == nil || tc.Type == nil || *tc.Type != "function" {
		t.Fatalf("expected tool type to normalize to function, got %#v", tc)
	}
	if tc.Name == nil || *tc.Name != "lookup" {
		t.Fatalf("expected tool choice name lookup, got %#v", tc)
	}
}

func TestResponseOutboundCarriesToolMetadataAcrossStreamEvents(t *testing.T) {
	outbound := &ResponseOutbound{}

	added, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}
	}`))
	if err != nil {
		t.Fatalf("TransformStream output_item.added returned error: %v", err)
	}
	if added == nil || len(added.Choices) != 1 || added.Choices[0].Delta == nil || len(added.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected tool-call metadata chunk, got %#v", added)
	}

	delta, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.function_call_arguments.delta",
		"output_index":0,
		"delta":"{\"q\""
	}`))
	if err != nil {
		t.Fatalf("TransformStream function_call_arguments.delta returned error: %v", err)
	}
	if delta == nil || len(delta.Choices) != 1 || delta.Choices[0].Delta == nil || len(delta.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected tool-call argument chunk, got %#v", delta)
	}
	toolCall := delta.Choices[0].Delta.ToolCalls[0]
	if toolCall.ID != "call_1" {
		t.Fatalf("expected call_id from previous item, got %#v", toolCall)
	}
	if toolCall.Function.Name != "lookup" {
		t.Fatalf("expected function name from previous item, got %#v", toolCall)
	}
	if toolCall.Function.Arguments != `{"q"` {
		t.Fatalf("expected argument delta, got %#v", toolCall)
	}

	completed, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.completed",
		"response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"completed","output":[]}
	}`))
	if err != nil {
		t.Fatalf("TransformStream response.completed returned error: %v", err)
	}
	if completed == nil || len(completed.Choices) != 1 || completed.Choices[0].FinishReason == nil {
		t.Fatalf("expected finish chunk, got %#v", completed)
	}
	if *completed.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls finish reason after tool stream, got %#v", completed.Choices[0].FinishReason)
	}
}

func TestResponseOutboundMapsCodexNativeToolCallItems(t *testing.T) {
	outbound := &ResponseOutbound{}

	added, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"id":"ls_1","type":"local_shell_call","call_id":"call_shell","action":{"command":"pwd"}}
	}`))
	if err != nil {
		t.Fatalf("TransformStream native tool output_item.added returned error: %v", err)
	}
	if added == nil || len(added.Choices) != 1 || added.Choices[0].Delta == nil || len(added.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected native tool-call metadata chunk, got %#v", added)
	}
	tool := added.Choices[0].Delta.ToolCalls[0]
	if tool.ID != "call_shell" || tool.Function.Name != "local_shell" {
		t.Fatalf("unexpected native tool-call metadata: %#v", tool)
	}

	done, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.done",
		"output_index":0,
		"item":{"id":"ls_1","type":"local_shell_call","call_id":"call_shell","action":{"command":"pwd"}}
	}`))
	if err != nil {
		t.Fatalf("TransformStream native tool output_item.done returned error: %v", err)
	}
	if done == nil || len(done.Choices) != 1 || done.Choices[0].Delta == nil || len(done.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected native tool-call done chunk, got %#v", done)
	}
	tool = done.Choices[0].Delta.ToolCalls[0]
	if tool.ID != "call_shell" || tool.Function.Name != "local_shell" || !strings.Contains(tool.Function.Arguments, `"command":"pwd"`) {
		t.Fatalf("unexpected native tool-call done chunk: %#v", tool)
	}
}

func TestResponsesUsageMarshalIncludesZeroCacheFields(t *testing.T) {
	var usage ResponsesUsage
	usage.InputTokens = 2
	usage.OutputTokens = 1
	usage.TotalTokens = 3

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `"input_tokens_details":{"cached_tokens":0`) {
		t.Fatalf("expected zero cached_tokens in usage JSON, got %s", got)
	}
	if !strings.Contains(got, `"output_tokens_details":{"reasoning_tokens":0`) {
		t.Fatalf("expected zero reasoning_tokens in usage JSON, got %s", got)
	}
}

func TestResponsesUsageUnmarshalAcceptsOpenAIChatUsageAliases(t *testing.T) {
	var usage ResponsesUsage
	if err := json.Unmarshal([]byte(`{
		"prompt_tokens":12,
		"completion_tokens":4,
		"total_tokens":16,
		"prompt_tokens_details":{"cached_tokens":5},
		"cache_creation_input_tokens":2
	}`), &usage); err != nil {
		t.Fatalf("unmarshal usage aliases: %v", err)
	}

	got := convertResponsesUsage(&usage)
	if got.PromptTokens != 12 || got.CompletionTokens != 4 || got.TotalTokens != 16 {
		t.Fatalf("unexpected token counts: %#v", got)
	}
	if got.PromptTokensDetails == nil || got.PromptTokensDetails.CachedTokens != 5 {
		t.Fatalf("expected cached token alias to survive, got %#v", got.PromptTokensDetails)
	}
	if got.CacheCreationInputTokens != 2 {
		t.Fatalf("expected cache creation tokens to survive, got %d", got.CacheCreationInputTokens)
	}
}

func TestResponseOutboundCompletedEventAcceptsTopLevelUsageAliases(t *testing.T) {
	outbound := &ResponseOutbound{}
	resp, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.completed",
		"response":{"id":"resp_1","object":"response","model":"glm","status":"completed","output":[]},
		"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12,"cached_tokens":4}
	}`))
	if err != nil {
		t.Fatalf("TransformStream response.completed returned error: %v", err)
	}
	if resp == nil || resp.Usage == nil {
		t.Fatalf("expected usage on completed event, got %#v", resp)
	}
	if resp.Usage.PromptTokens != 9 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 12 {
		t.Fatalf("unexpected usage: %#v", resp.Usage)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("expected top-level cached_tokens alias, got %#v", resp.Usage.PromptTokensDetails)
	}
}

func TestResponseOutboundIncompleteEventMapsToLengthFinish(t *testing.T) {
	outbound := &ResponseOutbound{}
	resp, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.incomplete",
		"response":{"id":"resp_1","object":"response","model":"glm","status":"incomplete","output":[]}
	}`))
	if err != nil {
		t.Fatalf("TransformStream response.incomplete returned error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].FinishReason == nil {
		t.Fatalf("expected finish chunk, got %#v", resp)
	}
	if *resp.Choices[0].FinishReason != "length" {
		t.Fatalf("expected length finish reason, got %#v", resp.Choices[0].FinishReason)
	}
}

func TestResponseOutboundFailedEventAcceptsStringErrorCode(t *testing.T) {
	outbound := &ResponseOutbound{}
	resp, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.failed",
		"response":{
			"id":"resp_1",
			"object":"response",
			"model":"gpt-5.5",
			"status":"failed",
			"output":[],
			"error":{"code":"rate_limit_exceeded","message":"busy"}
		}
	}`))
	if err != nil {
		t.Fatalf("TransformStream response.failed with string error code returned error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].FinishReason == nil {
		t.Fatalf("expected error finish chunk, got %#v", resp)
	}
	if *resp.Choices[0].FinishReason != "error" {
		t.Fatalf("expected error finish reason, got %#v", resp.Choices[0].FinishReason)
	}
}

func TestResponsesErrorCodeAcceptsNumericCode(t *testing.T) {
	var event ResponsesStreamEvent
	if err := json.Unmarshal([]byte(`{
		"type":"response.failed",
		"response":{
			"id":"resp_1",
			"object":"response",
			"model":"gpt-5.5",
			"status":"failed",
			"output":[],
			"error":{"code":429,"message":"busy"}
		}
	}`), &event); err != nil {
		t.Fatalf("numeric error code must not fail ResponsesStreamEvent unmarshal: %v", err)
	}
	if event.Response == nil || event.Response.Error == nil || string(event.Response.Error.Code) != "429" {
		t.Fatalf("expected numeric code to be preserved as string, got %#v", event.Response)
	}
}

func TestConvertToResponsesRequestPreservesJSONSchemaResponseFormat(t *testing.T) {
	content := "return structured data"
	req := &model.InternalLLMRequest{
		Model: "gpt-4o",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
		ResponseFormat: &model.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"result","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}`),
		},
	}

	converted := ConvertToResponsesRequest(req)
	if converted.Text == nil || converted.Text.Format == nil {
		t.Fatalf("expected text format, got %#v", converted.Text)
	}
	if converted.Text.Format.Type != "json_schema" {
		t.Fatalf("expected json_schema format, got %q", converted.Text.Format.Type)
	}
	if string(converted.Text.Format.Schema) == "" {
		t.Fatalf("expected schema to be preserved")
	}
}

func TestConvertToResponsesRequestPreservesPreviousResponseID(t *testing.T) {
	content := "continue"
	previous := "resp_previous"
	req := &model.InternalLLMRequest{
		Model:              "gpt-4o",
		PreviousResponseID: &previous,
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	converted := ConvertToResponsesRequest(req)
	if converted.PreviousResponseID == nil || *converted.PreviousResponseID != previous {
		t.Fatalf("expected previous_response_id to be preserved, got %#v", converted.PreviousResponseID)
	}
}

func TestConvertToResponsesRequestUsesCodexMessageInputShape(t *testing.T) {
	content := "Reply with exactly OK."
	req := &model.InternalLLMRequest{
		Model: "gpt-5.5",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	converted := ConvertToResponsesRequest(req)
	if converted.Input.Text != nil {
		t.Fatalf("expected array input, got text shorthand %q", *converted.Input.Text)
	}
	if len(converted.Input.Items) != 1 {
		t.Fatalf("expected one input item, got %#v", converted.Input.Items)
	}
	item := converted.Input.Items[0]
	if item.Type != "message" || item.Role != "user" {
		t.Fatalf("expected user message input item, got %#v", item)
	}
	if item.Content == nil || len(item.Content.Items) != 1 || item.Content.Items[0].Type != "input_text" {
		t.Fatalf("expected input_text content item, got %#v", item.Content)
	}
}

func TestConvertToResponsesRequestReusesRawCodexRequestShape(t *testing.T) {
	sessionID := "019e8d7b-0690-7a91-a60f-b642269c3439"
	stream := true
	req := &model.InternalLLMRequest{
		Model:                 "mapped-gpt-5.5",
		RawAPIFormat:          model.APIFormatOpenAIResponse,
		Stream:                &stream,
		PromptCacheKey:        &sessionID,
		ResponsesInstructions: stringPtr("base instructions"),
		ResponsesInputRaw:     json.RawMessage(`[{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep developer item"}]}]`),
		ResponsesToolsRaw: []json.RawMessage{
			json.RawMessage(`{"type":"function","name":"shell_command","parameters":{"type":"object"}}`),
			json.RawMessage(`{"type":"web_search","name":"web_search","custom":"kept"}`),
		},
		ResponsesToolChoiceRaw: json.RawMessage(`"auto"`),
		ResponsesTextRaw:       json.RawMessage(`{"verbosity":"low"}`),
		ClientMetadata:         json.RawMessage(`{"x-codex-installation-id":"install-1"}`),
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: stringPtr("fallback should not rewrite raw input")},
		}},
	}

	converted := ConvertToResponsesRequest(req)
	data, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("marshal converted request: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"model":"mapped-gpt-5.5"`,
		`"stream":true`,
		`"prompt_cache_key":"019e8d7b-0690-7a91-a60f-b642269c3439"`,
		`"role":"developer"`,
		`"custom":"kept"`,
		`"tool_choice":"auto"`,
		`"text":{"verbosity":"low"}`,
		`"client_metadata":{"x-codex-installation-id":"install-1"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected converted request to contain %s, got %s", want, got)
		}
	}
	if strings.Contains(got, "fallback should not rewrite raw input") {
		t.Fatalf("expected raw input to win, got %s", got)
	}
}

func stringPtr(value string) *string {
	return &value
}

func TestConvertToResponsesRequestPreservesReasoningEncryptedContent(t *testing.T) {
	encrypted := "gAAAAABencrypted"
	reasoning := "kept summary"
	content := "continue"
	req := &model.InternalLLMRequest{
		Model:   "gpt-5.5",
		Include: []string{"reasoning.encrypted_content"},
		Messages: []model.Message{{
			Role:               "assistant",
			ReasoningContent:   &reasoning,
			ReasoningSignature: &encrypted,
		}, {
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
	}

	converted := ConvertToResponsesRequest(req)
	if len(converted.Include) != 1 || converted.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("expected include to be preserved, got %#v", converted.Include)
	}
	if len(converted.Input.Items) == 0 || converted.Input.Items[0].Type != "reasoning" {
		t.Fatalf("expected first input item to be reasoning, got %#v", converted.Input.Items)
	}
	if converted.Input.Items[0].EncryptedContent == nil || *converted.Input.Items[0].EncryptedContent != encrypted {
		t.Fatalf("expected encrypted content to be preserved, got %#v", converted.Input.Items[0].EncryptedContent)
	}
}

func TestConvertResponsesUsagePreservesTokenDetails(t *testing.T) {
	var usage ResponsesUsage
	usage.InputTokens = 10
	usage.OutputTokens = 20
	usage.TotalTokens = 30
	usage.InputTokenDetails.CachedTokens = 7
	usage.InputTokenDetails.AudioTokens = 2
	usage.OutputTokenDetails.ReasoningTokens = 11
	usage.OutputTokenDetails.AudioTokens = 3
	usage.OutputTokenDetails.AcceptedPredictionTokens = 5
	usage.OutputTokenDetails.RejectedPredictionTokens = 6

	got := convertResponsesUsage(&usage)
	if got.PromptTokensDetails == nil ||
		got.PromptTokensDetails.CachedTokens != 7 ||
		got.PromptTokensDetails.AudioTokens != 2 {
		t.Fatalf("expected prompt token details to round-trip, got %#v", got.PromptTokensDetails)
	}
	if got.CompletionTokensDetails == nil ||
		got.CompletionTokensDetails.ReasoningTokens != 11 ||
		got.CompletionTokensDetails.AudioTokens != 3 ||
		got.CompletionTokensDetails.AcceptedPredictionTokens != 5 ||
		got.CompletionTokensDetails.RejectedPredictionTokens != 6 {
		t.Fatalf("expected completion token details to round-trip, got %#v", got.CompletionTokensDetails)
	}
}
