package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestResponseInboundAcceptsChatStyleToolChoice(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":false
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil {
		t.Fatalf("expected named tool choice, got %#v", req.ToolChoice)
	}
	if req.ToolChoice.NamedToolChoice.Type != "function" || req.ToolChoice.NamedToolChoice.Function.Name != "lookup" {
		t.Fatalf("unexpected tool choice: %#v", req.ToolChoice.NamedToolChoice)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Fatalf("expected parallel_tool_calls=false, got %#v", req.ParallelToolCalls)
	}
}

func TestResponseInboundPreservesExplicitToolStreaming(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"glm-5.2",
		"input":"inspect the repository",
		"stream":true,
		"tool_stream":false,
		"tools":[{"type":"function","name":"ReadFile","parameters":{"type":"object"}}]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.ToolStream == nil || *req.ToolStream {
		t.Fatalf("expected explicit tool_stream=false to survive Responses conversion, got %#v", req.ToolStream)
	}
}

func TestResponseInboundKeepsMissingFunctionCallOutput(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %#v", req.Messages)
	}
	tool := req.Messages[1]
	if tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_1" {
		t.Fatalf("unexpected tool output message: %#v", tool)
	}
	if tool.Content.Content == nil || *tool.Content.Content != "" {
		t.Fatalf("expected missing output to become empty string content, got %#v", tool.Content)
	}
}

func TestResponseInboundKeepsStringFunctionCallOutput(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"previous_response_id":"resp_tool_parent",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":"tool result text"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.PreviousResponseID == nil || *req.PreviousResponseID != "resp_tool_parent" {
		t.Fatalf("expected previous_response_id to be preserved, got %#v", req.PreviousResponseID)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %#v", req.Messages)
	}
	tool := req.Messages[0]
	if tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_1" {
		t.Fatalf("unexpected tool output message: %#v", tool)
	}
	if tool.Content.Content == nil || *tool.Content.Content != "tool result text" {
		t.Fatalf("expected string output to be preserved, got %#v", tool.Content)
	}
}

func TestResponseInboundAcceptsCodexToolOutputTypes(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"input":[
			{"type":"mcp_tool_call","call_id":"call_mcp","name":"browser_snapshot","arguments":"{\"url\":\"https://example.com\"}"},
			{"type":"mcp_tool_call_output","call_id":"call_mcp","name":"browser_snapshot","output":"page ok"},
			{"type":"tool_search_call","id":"call_search","name":"web_search"},
			{"type":"tool_search_output","call_id":"call_search","output":"search ok"},
			{"type":"custom_tool_call","call_id":"call_custom","name":"terminal"},
			{"type":"custom_tool_call_output","call_id":"call_custom","output":"done"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Messages) != 6 {
		t.Fatalf("expected 6 messages, got %#v", req.Messages)
	}
	if got := req.Messages[0].ToolCalls[0]; got.ID != "call_mcp" || got.Function.Name != "browser_snapshot" {
		t.Fatalf("unexpected mcp tool call: %#v", got)
	}
	if tool := req.Messages[1]; tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_mcp" || tool.Content.Content == nil || *tool.Content.Content != "page ok" {
		t.Fatalf("unexpected mcp tool output: %#v", tool)
	}
	if got := req.Messages[2].ToolCalls[0]; got.ID != "call_search" || got.Function.Name != "web_search" {
		t.Fatalf("unexpected search tool call: %#v", got)
	}
	if tool := req.Messages[3]; tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_search" || tool.Content.Content == nil || *tool.Content.Content != "search ok" {
		t.Fatalf("unexpected search tool output: %#v", tool)
	}
	if got := req.Messages[4].ToolCalls[0]; got.ID != "call_custom" || got.Function.Name != "terminal" {
		t.Fatalf("unexpected custom tool call: %#v", got)
	}
	if tool := req.Messages[5]; tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_custom" || tool.Content.Content == nil || *tool.Content.Content != "done" {
		t.Fatalf("unexpected custom tool output: %#v", tool)
	}
}

func TestResponseInboundNormalizesInputRolesForProtocolBridge(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"follow policy"}]},
			{"type":"input_text","text":"hello"},
			{"type":"input_image","image_url":"data:image/png;base64,AAA"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %#v", req.Messages)
	}
	if req.Messages[0].Role != "system" {
		t.Fatalf("expected developer role to normalize to system, got %#v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[2].Role != "user" {
		t.Fatalf("expected missing input roles to default to user, got %#v", req.Messages)
	}
}

func TestResponseInboundPreservesRawCodexRequestFields(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-5.5",
		"instructions":"base instructions",
		"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep as developer input"}]}],
		"tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}},{"type":"web_search","name":"web_search","custom":"kept"}],
		"tool_choice":"auto",
		"text":{"verbosity":"low"},
		"client_metadata":{"x-codex-installation-id":"install-1"}
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.ResponsesInstructions == nil || *req.ResponsesInstructions != "base instructions" {
		t.Fatalf("expected raw instructions, got %#v", req.ResponsesInstructions)
	}
	if !strings.Contains(string(req.ResponsesInputRaw), `"role":"developer"`) {
		t.Fatalf("expected raw input to preserve developer role, got %s", string(req.ResponsesInputRaw))
	}
	if len(req.ResponsesToolsRaw) != 2 || !strings.Contains(string(req.ResponsesToolsRaw[1]), `"custom":"kept"`) {
		t.Fatalf("expected raw tools to be preserved, got %#v", req.ResponsesToolsRaw)
	}
	if string(req.ResponsesToolChoiceRaw) != `"auto"` {
		t.Fatalf("expected raw tool_choice, got %s", string(req.ResponsesToolChoiceRaw))
	}
	if !strings.Contains(string(req.ResponsesTextRaw), `"verbosity":"low"`) {
		t.Fatalf("expected raw text options, got %s", string(req.ResponsesTextRaw))
	}
	if !strings.Contains(string(req.ClientMetadata), `"x-codex-installation-id":"install-1"`) {
		t.Fatalf("expected client metadata, got %s", string(req.ClientMetadata))
	}
}

func TestResponseInboundPreservesPreviousResponseID(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"previous_response_id":"resp_previous",
		"input":"continue"
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.PreviousResponseID == nil || *req.PreviousResponseID != "resp_previous" {
		t.Fatalf("expected previous_response_id to be preserved, got %#v", req.PreviousResponseID)
	}
}

func TestResponseInboundCompletesStreamOnDoneWithoutUsageChunk(t *testing.T) {
	inbound := &ResponseInbound{}
	content := "hello"
	finishReason := "stop"

	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_1",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role: "assistant",
				Content: model.MessageContent{
					Content: &content,
				},
			},
		}},
	}); err != nil {
		t.Fatalf("TransformStream content chunk returned error: %v", err)
	}

	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_1",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}

	got := string(done)
	if !strings.Contains(got, `"type":"response.completed"`) {
		t.Fatalf("expected response.completed before DONE, got %s", got)
	}
	if !strings.Contains(got, `"input_tokens_details":{"cached_tokens":0`) {
		t.Fatalf("expected zero cached_tokens in synthesized response.completed, got %s", got)
	}
	if !strings.Contains(got, `"output_tokens_details":{"reasoning_tokens":0`) {
		t.Fatalf("expected zero reasoning_tokens in synthesized response.completed, got %s", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Fatalf("expected SSE DONE suffix, got %q", got)
	}
}

func TestResponseInboundCompletedUsageIncludesZeroCacheFields(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "stop"

	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_1",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	completed, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "chatcmpl_1",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Usage: &model.Usage{
			PromptTokens:     2,
			CompletionTokens: 1,
			TotalTokens:      3,
		},
	})
	if err != nil {
		t.Fatalf("TransformStream usage chunk returned error: %v", err)
	}

	got := string(completed)
	if !strings.Contains(got, `"input_tokens_details":{"cached_tokens":0`) {
		t.Fatalf("expected zero cached_tokens in response.completed, got %s", got)
	}
	if !strings.Contains(got, `"output_tokens_details":{"reasoning_tokens":0`) {
		t.Fatalf("expected zero reasoning_tokens in response.completed, got %s", got)
	}
}

func TestResponseInboundDoesNotMarkIncompleteStreamCompleted(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "length"

	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	got := string(done)
	if !strings.Contains(got, `"type":"response.incomplete"`) || !strings.Contains(got, `"status":"incomplete"`) {
		t.Fatalf("expected incomplete terminal response, got %s", got)
	}
	if strings.Contains(got, `"type":"response.completed"`) || strings.Contains(got, `"status":"completed"`) {
		t.Fatalf("incomplete response must not be reported completed, got %s", got)
	}
	if !strings.Contains(got, `"id":"resp_`) {
		t.Fatalf("expected synthesized non-empty response id, got %s", got)
	}
}

func TestResponseInboundDoesNotMarkFailedStreamCompleted(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "error"

	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "resp_failed",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	got := string(done)
	if !strings.Contains(got, `"type":"response.failed"`) || !strings.Contains(got, `"status":"failed"`) {
		t.Fatalf("expected failed terminal response, got %s", got)
	}
	if strings.Contains(got, `"type":"response.completed"`) || strings.Contains(got, `"status":"completed"`) {
		t.Fatalf("failed response must not be reported completed, got %s", got)
	}
}

// Cursor (a plain responses client) relies on the response id streamed in
// response.created matching the id octopus records as the responses-session
// owner. When the first stream chunk carries no upstream id, octopus generates
// one and commits it to the client; a later chunk that does carry an id must NOT
// override the aggregated result id, or the next turn's previous_response_id can
// never resolve the recorded owner and the conversation silently drops history.
func TestResponseInboundStreamIDStaysClientVisibleWhenLateChunkHasID(t *testing.T) {
	inbound := &ResponseInbound{}
	content := "hi"
	finishReason := "stop"

	// First chunk has no id -> octopus generates i.responseID and emits it in
	// response.created (the id the client echoes back as previous_response_id).
	created, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		Object:  "chat.completion.chunk",
		Model:   "gpt-5.5",
		Created: 123,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{Role: "assistant", Content: model.MessageContent{Content: &content}},
		}},
	})
	if err != nil {
		t.Fatalf("TransformStream first chunk returned error: %v", err)
	}
	clientID := inbound.responseID
	if clientID == "" {
		t.Fatalf("expected octopus to generate a response id for the id-less first chunk")
	}
	if !strings.Contains(string(created), `"id":"`+clientID+`"`) {
		t.Fatalf("expected response.created to carry client id %q, got %s", clientID, string(created))
	}

	// A later chunk now carries a different upstream id.
	if _, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{
		ID:      "upstream_late_id",
		Object:  "chat.completion.chunk",
		Model:   "gpt-5.5",
		Created: 123,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
		Usage:   &model.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}); err != nil {
		t.Fatalf("TransformStream late chunk returned error: %v", err)
	}

	// The aggregated internal response feeds responses-session ownership; it must
	// keep the client-visible id, not adopt the late upstream id.
	result, err := inbound.GetInternalResponse(context.Background())
	if err != nil {
		t.Fatalf("GetInternalResponse returned error: %v", err)
	}
	if result == nil {
		t.Fatalf("expected aggregated internal response, got nil")
	}
	if result.ID != clientID {
		t.Fatalf("aggregated id %q diverged from client-visible id %q (late upstream id leaked into session owner)", result.ID, clientID)
	}
}

func TestResponseInboundPreservesJSONSchemaResponseFormat(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-4o",
		"input":"return structured data",
		"text":{
			"format":{
				"type":"json_schema",
				"name":"result",
				"schema":{
					"type":"object",
					"properties":{"ok":{"type":"boolean"}},
					"required":["ok"]
				}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if req.ResponseFormat == nil {
		t.Fatalf("expected response format")
	}
	if req.ResponseFormat.Type != "json_schema" {
		t.Fatalf("expected json_schema response format, got %q", req.ResponseFormat.Type)
	}
	if !strings.Contains(string(req.ResponseFormat.JSONSchema), `"ok"`) {
		t.Fatalf("expected schema to be preserved, got %s", string(req.ResponseFormat.JSONSchema))
	}
}

func TestConvertUsageToResponsesPreservesTokenDetails(t *testing.T) {
	usage := &model.Usage{
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		PromptTokensDetails: &model.PromptTokensDetails{
			CachedTokens: 7,
			AudioTokens:  2,
		},
		CompletionTokensDetails: &model.CompletionTokensDetails{
			ReasoningTokens:          11,
			AudioTokens:              3,
			AcceptedPredictionTokens: 5,
			RejectedPredictionTokens: 6,
		},
	}

	got := convertUsageToResponses(usage)
	if got.InputTokenDetails.CachedTokens != 7 || got.InputTokenDetails.AudioTokens != 2 {
		t.Fatalf("expected input token details to round-trip, got %#v", got.InputTokenDetails)
	}
	if got.OutputTokenDetails.ReasoningTokens != 11 ||
		got.OutputTokenDetails.AudioTokens != 3 ||
		got.OutputTokenDetails.AcceptedPredictionTokens != 5 ||
		got.OutputTokenDetails.RejectedPredictionTokens != 6 {
		t.Fatalf("expected output token details to round-trip, got %#v", got.OutputTokenDetails)
	}
}

func TestConvertUsageToResponsesAddsAnthropicCacheTokensToInput(t *testing.T) {
	usage := &model.Usage{
		PromptTokens:             10,
		CompletionTokens:         20,
		TotalTokens:              45,
		CacheCreationInputTokens: 4,
		AnthropicUsage:           true,
		PromptTokensDetails: &model.PromptTokensDetails{
			CachedTokens: 11,
		},
	}

	got := convertUsageToResponses(usage)
	if got.InputTokens != 25 {
		t.Fatalf("expected Anthropic cache tokens to be included in input_tokens, got %d", got.InputTokens)
	}
}

// parseResponsesStreamEvents splits concatenated SSE output into the decoded
// stream events, ignoring the [DONE] sentinel.
func parseResponsesStreamEvents(t *testing.T, raw string) []ResponsesStreamEvent {
	t.Helper()
	var events []ResponsesStreamEvent
	for _, block := range strings.Split(raw, "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var ev ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("failed to decode SSE event %q: %v", payload, err)
		}
		events = append(events, ev)
	}
	return events
}

// Codex issues parallel tool calls whose argument fragments interleave across
// stream chunks. When a second tool-call index first appears, the inbound must
// NOT finalize the already-open sibling: doing so truncated the first tool's
// arguments and emitted a wrong output_index. Both tools must finalize together
// at the finish boundary with complete arguments, and every event's output_index
// must agree with its item_id regardless of arrival order.
func TestResponseInboundKeepsInterleavedParallelToolCallsIntact(t *testing.T) {
	inbound := &ResponseInbound{}
	finishReason := "tool_calls"

	chunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		return &model.InternalLLMResponse{
			ID:      "chatcmpl_parallel",
			Model:   "gpt-4o",
			Object:  "chat.completion.chunk",
			Created: 123,
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{Role: "assistant", ToolCalls: tcs},
			}},
		}
	}

	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{
			Index:    index,
			ID:       id,
			Type:     "function",
			Function: model.FunctionCall{Name: name, Arguments: args},
		}
	}

	var raw strings.Builder
	feed := func(stream *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		raw.Write(out)
	}

	// Interleave the two tool calls' argument fragments across chunks.
	feed(chunk(tc(0, "call_alpha", "get_weather", `{"city":`)))
	feed(chunk(tc(1, "call_beta", "get_time", `{"zone":`)))
	feed(chunk(tc(0, "", "", `"paris"}`)))
	feed(chunk(tc(1, "", "", `"utc"}`)))

	finish := &model.InternalLLMResponse{
		ID:      "chatcmpl_parallel",
		Model:   "gpt-4o",
		Object:  "chat.completion.chunk",
		Created: 123,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	}
	feed(finish)

	done, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	raw.Write(done)

	events := parseResponsesStreamEvents(t, raw.String())

	// Reconstruct the final arguments from the terminal *.done events and verify
	// neither tool's arguments were truncated.
	finalArgs := map[string]string{}
	itemOutputIndex := map[string]int{}
	for _, ev := range events {
		switch ev.Type {
		case "response.function_call_arguments.done":
			if ev.ItemID == nil || ev.OutputIndex == nil {
				t.Fatalf("function_call_arguments.done missing item_id/output_index: %#v", ev)
			}
			finalArgs[*ev.ItemID] = ev.Arguments
			assertOutputIndexAgrees(t, itemOutputIndex, *ev.ItemID, *ev.OutputIndex)
		case "response.output_item.added", "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			if ev.OutputIndex == nil {
				t.Fatalf("function_call %s missing output_index: %#v", ev.Type, ev)
			}
			assertOutputIndexAgrees(t, itemOutputIndex, ev.Item.ID, *ev.OutputIndex)
			if ev.Type == "response.output_item.done" {
				finalArgs[ev.Item.ID] = derefString(ev.Item.Arguments)
			}
		case "response.function_call_arguments.delta":
			if ev.ItemID == nil || ev.OutputIndex == nil {
				t.Fatalf("function_call_arguments.delta missing item_id/output_index: %#v", ev)
			}
			assertOutputIndexAgrees(t, itemOutputIndex, *ev.ItemID, *ev.OutputIndex)
		}
	}

	if got := finalArgs["call_alpha"]; got != `{"city":"paris"}` {
		t.Fatalf("first tool call arguments truncated/corrupted: got %q", got)
	}
	if got := finalArgs["call_beta"]; got != `{"zone":"utc"}` {
		t.Fatalf("second tool call arguments truncated/corrupted: got %q", got)
	}

	// The two parallel tool items must occupy distinct, stable output indices.
	if itemOutputIndex["call_alpha"] == itemOutputIndex["call_beta"] {
		t.Fatalf("parallel tool calls collided on output_index %d", itemOutputIndex["call_alpha"])
	}
}

// assertOutputIndexAgrees records the first output_index seen for an item_id and
// fails if any later event reports a different output_index for that same item.
func assertOutputIndexAgrees(t *testing.T, seen map[string]int, itemID string, outputIndex int) {
	t.Helper()
	if prev, ok := seen[itemID]; ok {
		if prev != outputIndex {
			t.Fatalf("output_index for item_id %q disagrees: saw %d then %d", itemID, prev, outputIndex)
		}
		return
	}
	seen[itemID] = outputIndex
}

// TestResponseInboundInjectsReasoningIntoToolCallMessage verifies that a
// reasoning item immediately preceding a function_call item in the Responses
// input is injected as ReasoningContent onto the same assistant tool-call
// message rather than emitted as a separate predecessor assistant message.
//
// DeepSeek V4 requires reasoning_content on the same assistant message as
// tool_calls; splitting them causes a 400 on every multi-turn tool-call
// continuation.
func TestResponseInboundInjectsReasoningIntoToolCallMessage(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"deepseek-reasoner",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"whats the weather?"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"I should call get_weather"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny 25C"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	// Expect 3 messages: user, assistant(reasoning+tool_calls), tool
	if len(req.Messages) != 3 {
		var roles []string
		for _, m := range req.Messages {
			roles = append(roles, m.Role)
		}
		t.Fatalf("expected 3 messages (user, assistant, tool), got %d with roles %v", len(req.Messages), roles)
	}

	user := req.Messages[0]
	if user.Role != "user" {
		t.Fatalf("expected first message to be user, got %q", user.Role)
	}

	assistant := req.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("expected second message to be assistant, got %q", assistant.Role)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected assistant message to have 1 tool call, got %d", len(assistant.ToolCalls))
	}
	if got := assistant.ToolCalls[0]; got.ID != "call_1" || got.Function.Name != "get_weather" {
		t.Fatalf("unexpected tool call: %#v", got)
	}
	if got := assistant.GetReasoningContent(); got != "I should call get_weather" {
		t.Fatalf("expected reasoning_content on assistant tool-call message, got %q", got)
	}

	tool := req.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_1" {
		t.Fatalf("unexpected tool output message: %#v", tool)
	}
	if tool.Content.Content == nil || *tool.Content.Content != "sunny 25C" {
		t.Fatalf("expected tool output content %q, got %#v", "sunny 25C", tool.Content)
	}
}

// TestResponseInboundReasoningBeforeTextFlushesStandalone verifies that a
// reasoning item preceding a plain text assistant response is still emitted as
// a standalone assistant message (preserving backward-compatible behaviour for
// non-tool-call turns).
func TestResponseInboundReasoningBeforeTextFlushesStandalone(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"deepseek-reasoner",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"just a greeting"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi there"}]}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	// Expect 3 messages: user, standalone-reasoning assistant, text assistant
	if len(req.Messages) != 3 {
		var roles []string
		for _, m := range req.Messages {
			roles = append(roles, m.Role)
		}
		t.Fatalf("expected 3 messages, got %d with roles %v", len(req.Messages), roles)
	}

	reasoning := req.Messages[1]
	if reasoning.Role != "assistant" {
		t.Fatalf("expected standalone reasoning message to be assistant, got %q", reasoning.Role)
	}
	if reasoning.GetReasoningContent() != "just a greeting" {
		t.Fatalf("expected standalone reasoning content, got %q", reasoning.GetReasoningContent())
	}
	if len(reasoning.ToolCalls) != 0 {
		t.Fatalf("standalone reasoning message must not have tool calls, got %d", len(reasoning.ToolCalls))
	}

	text := req.Messages[2]
	if text.Role != "assistant" {
		t.Fatalf("expected text message to be assistant, got %q", text.Role)
	}
	if text.Content.Content == nil || *text.Content.Content != "hi there" {
		t.Fatalf("expected text content, got %#v", text.Content)
	}
}
