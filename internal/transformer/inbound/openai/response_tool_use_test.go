package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samber/lo"

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

// TestResponseInboundDeferredAnnouncementWhenNameArrivesLate guards the P1-4
// "deferred announcement" fix in handleToolCalls. Some OpenAI-compatible upstreams
// split a tool call's id and function name across two stream chunks: the first
// chunk carries the id (call_x) but an empty function.name and a partial arguments
// fragment; the real name arrives in the second chunk together with the rest of
// the arguments.
//
// Before the fix octopus announced output_item.added eagerly on the first chunk
// with an empty name (the name field is omitempty, so the event lost it entirely)
// and a later *.done could not retroactively re-add it; a cursor / codex SDK that
// routes tool calls by name (AskQuestion / Cherry Studio) then dropped the call.
//
// The fix holds back output_item.added until the name arrives, then announces the
// item with the real name and replays the arguments that accumulated during the
// pending window so the codex client sees the correct lifecycle order
// (added -> delta -> done) instead of orphan deltas against an unannounced item.
//
// This test is the dedicated coverage for that deferred-announcement path. The
// existing TestResponseInboundToolCallNameBackfilledFromLaterFrame covers name
// backfill but exercises the OLD eager-announce behavior (added is emitted on the
// first chunk carrying the empty name); it does NOT cover the hold-back.
func TestResponseInboundDeferredAnnouncementWhenNameArrivesLate(t *testing.T) {
	inbound := &ResponseInbound{}
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_deferred", Model: "glm-4.6", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}

	// ---- Chunk 1: id present, name empty, partial arguments. The deferred path
	// must NOT emit output_item.added here. ----
	out1, err := inbound.TransformStream(context.Background(),
		toolChunk(tc(0, "call_x", "", `{"q":`)))
	if err != nil {
		t.Fatalf("TransformStream chunk1 returned error: %v", err)
	}
	eventsAfterChunk1 := parseResponsesStreamEvents(t, string(out1))
	for _, ev := range eventsAfterChunk1 {
		if ev.Type == "response.output_item.added" {
			t.Fatalf("first chunk (empty name) must NOT announce output_item.added (deferred announcement), got: %#v", ev)
		}
		// No arguments delta may sneak out either, or it would be an orphan delta
		// against an item the client never saw announced.
		if ev.Type == "response.function_call_arguments.delta" {
			t.Fatalf("first chunk (deferred announcement) must NOT emit arguments delta before output_item.added, got: %#v", ev)
		}
	}

	// ---- Chunk 2: real name arrives, plus the rest of the arguments. Now the
	// deferred announcement fires: output_item.added (with the real name) plus a
	// replay of the arguments accumulated during the pending window, then the
	// current chunk's arguments flow as a normal incremental delta. ----
	out2, err := inbound.TransformStream(context.Background(),
		toolChunk(tc(0, "", "AskQuestion", `"a?"}`)))
	if err != nil {
		t.Fatalf("TransformStream chunk2 returned error: %v", err)
	}

	// ---- Chunk 3: finish_reason. closeToolItem finalizes the function_call. ----
	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	out3, err := inbound.TransformStream(context.Background(), fin)
	if err != nil {
		t.Fatalf("TransformStream finish chunk returned error: %v", err)
	}

	// ---- Chunk The inbound synthesizes response.completed
	// with the finalized output (completedItems captured via output_item.done). ----
	doneOut, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}

	var raw strings.Builder
	raw.Write(out1)
	raw.Write(out2)
	raw.Write(out3)
	raw.Write(doneOut)

	events := parseResponsesStreamEvents(t, raw.String())
	// Core lifecycle invariant: no delta/done against an unannounced or finalized item.
	assertResponsesStreamItemLifecycle(t, events)

	// Locate the (single) function_call output_item.added event.
	addedIdx := -1
	for i, ev := range events {
		if ev.Type == "response.output_item.added" && ev.Item != nil && ev.Item.Type == "function_call" {
			if addedIdx != -1 {
				t.Fatalf("output_item.added for the function_call must fire exactly once, saw a second at index %d", i)
			}
			addedIdx = i
		}
	}
	if addedIdx == -1 {
		t.Fatalf("expected output_item.added for function_call after the name arrived, events: %#v", events)
	}

	// Assertion 2: the announced item carries the real name (not the empty placeholder).
	addedItem := events[addedIdx].Item
	if addedItem.Name != "AskQuestion" {
		t.Fatalf("output_item.added item.name must be %q (backfilled from the second chunk), got %q", "AskQuestion", addedItem.Name)
	}
	// call_id is the upstream pairing key; the announced item must already carry it
	// so the codex client can pair the tool result on the next turn.
	if addedItem.CallID != "call_x" {
		t.Fatalf("output_item.added call_id must be %q, got %q", "call_x", addedItem.CallID)
	}

	// Assertion 3: output_item.added is immediately followed by a
	// function_call_arguments.delta carrying the arguments that accumulated during
	// the deferred window. The current chunk's arguments then flow as a normal
	// incremental delta right after. Together they reconstruct the full arguments
	// string `{"q":"a?"}`.
	if addedIdx+1 >= len(events) {
		t.Fatalf("expected function_call_arguments.delta immediately after output_item.added, got end of stream")
	}
	replayEv := events[addedIdx+1]
	if replayEv.Type != "response.function_call_arguments.delta" {
		t.Fatalf("expected function_call_arguments.delta immediately after output_item.added, got %q at index %d", replayEv.Type, addedIdx+1)
	}
	// The deferred window accumulated ONLY chunk 1's arguments (`{"q":`); chunk 2's
	// arguments had not been accumulated yet when the replay fired.
	if replayEv.Delta != `{"q":` {
		t.Fatalf("replayed delta (deferred accumulation from chunk 1) must be %q, got %q", `{"q":`, replayEv.Delta)
	}
	if replayEv.ItemID == nil || *replayEv.ItemID != "call_x" {
		t.Fatalf("replayed delta must reference the announced item id %q, got %#v", "call_x", replayEv.ItemID)
	}
	// The current chunk's arguments then arrive as a normal incremental delta, so
	// the two deltas concatenate to the full arguments string.
	if addedIdx+2 >= len(events) || events[addedIdx+2].Type != "response.function_call_arguments.delta" {
		t.Fatalf("expected a second function_call_arguments.delta (chunk 2 incremental) after the replay, got %+v", events[addedIdx+2:])
	} else {
		incrementalEv := events[addedIdx+2]
		if incrementalEv.Delta != `"a?"}` {
			t.Fatalf("incremental delta (chunk 2) must be %q, got %q", `"a?"}`, incrementalEv.Delta)
		}
		// Sanity: the two deltas reconstruct the full, valid arguments JSON.
		if got := replayEv.Delta + incrementalEv.Delta; got != `{"q":"a?"}` {
			t.Fatalf("replay + incremental deltas must reconstruct the full arguments %q, got %q", `{"q":"a?"}`, got)
		}
	}

	// Assertion 4: after the deltas, function_call_arguments.done precedes
	// output_item.done in the correct lifecycle order.
	argsDoneIdx, itemDoneIdx := -1, -1
	for i := addedIdx + 1; i < len(events); i++ {
		switch events[i].Type {
		case "response.function_call_arguments.done":
			if argsDoneIdx == -1 {
				argsDoneIdx = i
			}
		case "response.output_item.done":
			if events[i].Item != nil && events[i].Item.Type == "function_call" && itemDoneIdx == -1 {
				itemDoneIdx = i
			}
		}
	}
	if argsDoneIdx == -1 {
		t.Fatalf("expected function_call_arguments.done event after the deltas")
	}
	if itemDoneIdx == -1 {
		t.Fatalf("expected output_item.done event for the function_call")
	}
	if argsDoneIdx >= itemDoneIdx {
		t.Fatalf("function_call_arguments.done (idx %d) must come before output_item.done (idx %d)", argsDoneIdx, itemDoneIdx)
	}

	// Assertion 5 (P1-5): function_call_arguments.done carries name + call_id so
	// SDK clients (cursor / Cherry Studio) that route by the done event's name
	// (not just output_item.added) can dispatch the call without a second lookup.
	argsDone := events[argsDoneIdx]
	if argsDone.Name != "AskQuestion" {
		t.Fatalf("function_call_arguments.done name must be %q, got %q", "AskQuestion", argsDone.Name)
	}
	if argsDone.CallID != "call_x" {
		t.Fatalf("function_call_arguments.done call_id must be %q, got %q", "call_x", argsDone.CallID)
	}
	if argsDone.Arguments != `{"q":"a?"}` {
		t.Fatalf("function_call_arguments.done arguments must be the normalized full JSON %q, got %q", `{"q":"a?"}`, argsDone.Arguments)
	}

	// Assertion 6: response.completed.response.output carries this function_call,
	// so a codex client that reconstructs the result from the terminal event sees
	// the call (an empty output array here made Cherry Studio stop after step 1).
	var completed *ResponsesStreamEvent
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "response.completed" {
			completed = &events[i]
			break
		}
	}
	if completed == nil || completed.Response == nil {
		t.Fatalf("expected response.completed event with a response payload, events: %#v", events)
	}
	var foundFunctionCall bool
	for _, item := range completed.Response.Output {
		if item.Type != "function_call" {
			continue
		}
		if item.Name != "AskQuestion" || item.CallID != "call_x" {
			continue
		}
		foundFunctionCall = true
		if derefString(item.Arguments) != `{"q":"a?"}` {
			t.Fatalf("response.completed.output function_call arguments must be %q, got %q", `{"q":"a?"}`, derefString(item.Arguments))
		}
	}
	if !foundFunctionCall {
		t.Fatalf("response.completed.output must contain the AskQuestion function_call (call_x), got %#v", completed.Response.Output)
	}
}

// TestResponseInboundMCPToolCallEmittedAsMCPItem guards the P1-6 minimal slice for
// mcp_tool_call. cursor / codex register an MCP server tool with type=mcp_tool_call
// in the request's tools array; the chat upstream only understands type=function,
// so octopus flattens that tool to a chat function tool (convertToolsToInternal) and
// records the original type by name (clientToolTypeByToolName) in TransformRequest.
// When the upstream chat tool_call comes back with that name, octopus must re-emit a
// Responses mcp_tool_call item (NOT a generic function_call) so the codex / cursor
// client can route the call to its MCP handler.
func TestResponseInboundMCPToolCallEmittedAsMCPItem(t *testing.T) {
	inbound := &ResponseInbound{}

	// Step 1: TransformRequest populates clientToolTypeByToolName from a request
	// whose tools array declares an mcp_tool_call tool. The chat upstream sees
	// this as a normal type=function tool (convertToolsToInternal), but the
	// original type is recorded so the response side can re-type the call.
	req, err := inbound.TransformRequest(context.Background(), []byte(`{
		"model":"gpt-5.5",
		"input":"call the MCP tool",
		"tools":[
			{"type":"mcp_tool_call","name":"mcp_tool_1","description":"an MCP tool","parameters":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}},
			{"type":"function","name":"plain_func","parameters":{"type":"object"}}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("expected both tools to survive conversion, got %d", len(req.Tools))
	}
	for _, tool := range req.Tools {
		if tool.Type != "function" {
			t.Fatalf("expected mcp_tool_call to be flattened to type=function for chat upstream, got tool type %q (name=%q)", tool.Type, tool.Function.Name)
		}
	}
	if got := inbound.clientToolTypeByToolName["mcp_tool_1"]; got != "mcp_tool_call" {
		t.Fatalf("expected clientToolTypeByToolName[mcp_tool_1]=mcp_tool_call, got %q", got)
	}
	if _, present := inbound.clientToolTypeByToolName["plain_func"]; present {
		t.Fatalf("plain function tools must NOT be recorded in clientToolTypeByToolName")
	}

	// Step 2: stream a chat tool_call whose function.name is mcp_tool_1. The
	// response side must re-emit it as an mcp_tool_call item, NOT a function_call.
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{ID: "chatcmpl_mcp", Model: "gpt-5.5", Object: "chat.completion.chunk", Created: 1}
	}
	toolChunk := func(tcs ...model.ToolCall) *model.InternalLLMResponse {
		c := base()
		c.Choices = []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ToolCalls: tcs}}}
		return c
	}
	tc := func(index int, id, name, args string) model.ToolCall {
		return model.ToolCall{Index: index, ID: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}
	}

	var raw strings.Builder
	feed := func(stream *model.InternalLLMResponse) {
		out, err := inbound.TransformStream(context.Background(), stream)
		if err != nil {
			t.Fatalf("TransformStream returned error: %v", err)
		}
		raw.Write(out)
	}

	// Chunk 1: id + name + partial args arrive together (eager-announce path).
	feed(toolChunk(tc(0, "call_mcp_1", "mcp_tool_1", `{"x":`)))
	// Chunk 2: rest of the arguments.
	feed(toolChunk(tc(0, "", "", `"hi"}`)))

	// Chunk 3: finish_reason. closeToolItem finalizes the mcp_tool_call.
	finishReason := "tool_calls"
	fin := base()
	fin.Choices = []model.Choice{{Index: 0, FinishReason: &finishReason}}
	feed(fin)

	// Chunk 4: [DONE]. The inbound synthesizes response.completed with the
	// finalized output (completedItems captured via output_item.done).
	doneOut, err := inbound.TransformStream(context.Background(), &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("TransformStream done chunk returned error: %v", err)
	}
	raw.Write(doneOut)

	events := parseResponsesStreamEvents(t, raw.String())

	// Core lifecycle invariant: no delta/done against an unannounced or finalized item.
	assertResponsesStreamItemLifecycle(t, events)

	// Locate the (single) mcp_tool_call output_item.added event. There must be NO
	// function_call item for the mcp_tool_call (it MUST be re-typed).
	addedIdx := -1
	for i, ev := range events {
		if ev.Type == "response.output_item.added" && ev.Item != nil {
			switch ev.Item.Type {
			case "mcp_tool_call":
				if addedIdx != -1 {
					t.Fatalf("output_item.added for mcp_tool_call must fire exactly once, saw a second at index %d", i)
				}
				if ev.Item.CallID != "call_mcp_1" {
					t.Fatalf("mcp_tool_call added event call_id must be call_mcp_1, got %q", ev.Item.CallID)
				}
				if ev.Item.Name != "mcp_tool_1" {
					t.Fatalf("mcp_tool_call added event name must be mcp_tool_1, got %q", ev.Item.Name)
				}
				addedIdx = i
			case "function_call":
				t.Fatalf("MCP tool call must NOT be emitted as a function_call item, but saw function_call at event index %d (name=%q)", i, ev.Item.Name)
			}
		}
	}
	if addedIdx == -1 {
		t.Fatalf("expected output_item.added for mcp_tool_call, events: %#v", events)
	}
	if addedItem := events[addedIdx].Item; addedItem.Status == nil || *addedItem.Status != "in_progress" {
		t.Fatalf("mcp_tool_call added event status must be in_progress, got %#v", addedItem.Status)
	}

	// Locate the (single) mcp_tool_call output_item.done event and verify the
	// full arguments string survived.
	var doneItem *ResponsesItem
	argsDoneIdx := -1
	itemDoneIdx := -1
	for i := addedIdx + 1; i < len(events); i++ {
		switch events[i].Type {
		case "response.function_call_arguments.done":
			if argsDoneIdx == -1 {
				argsDoneIdx = i
				if events[i].Name != "mcp_tool_1" {
					t.Fatalf("function_call_arguments.done name must be mcp_tool_1, got %q", events[i].Name)
				}
				if events[i].CallID != "call_mcp_1" {
					t.Fatalf("function_call_arguments.done call_id must be call_mcp_1, got %q", events[i].CallID)
				}
				if events[i].Arguments != `{"x":"hi"}` {
					t.Fatalf("function_call_arguments.done arguments must be %q, got %q", `{"x":"hi"}`, events[i].Arguments)
				}
			}
		case "response.output_item.done":
			if events[i].Item != nil && events[i].Item.Type == "mcp_tool_call" && itemDoneIdx == -1 {
				itemDoneIdx = i
				doneItem = events[i].Item
			}
		}
	}
	if argsDoneIdx == -1 {
		t.Fatalf("expected function_call_arguments.done event for the mcp_tool_call")
	}
	if itemDoneIdx == -1 {
		t.Fatalf("expected output_item.done event for the mcp_tool_call")
	}
	if argsDoneIdx >= itemDoneIdx {
		t.Fatalf("function_call_arguments.done (idx %d) must come before output_item.done (idx %d)", argsDoneIdx, itemDoneIdx)
	}
	if doneItem == nil {
		t.Fatalf("expected non-nil mcp_tool_call done item")
	}
	if doneItem.CallID != "call_mcp_1" || doneItem.Name != "mcp_tool_1" {
		t.Fatalf("mcp_tool_call done item must carry call_id+name, got CallID=%q Name=%q", doneItem.CallID, doneItem.Name)
	}
	if derefString(doneItem.Arguments) != `{"x":"hi"}` {
		t.Fatalf("mcp_tool_call done item arguments must be %q, got %q", `{"x":"hi"}`, derefString(doneItem.Arguments))
	}
	if doneItem.Status == nil || *doneItem.Status != "completed" {
		t.Fatalf("mcp_tool_call done item status must be completed, got %#v", doneItem.Status)
	}

	// response.completed.output must carry this mcp_tool_call so a codex client
	// that reconstructs the result from the terminal event sees the call.
	var completed *ResponsesStreamEvent
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "response.completed" {
			completed = &events[i]
			break
		}
	}
	if completed == nil || completed.Response == nil {
		t.Fatalf("expected response.completed event with a response payload, events: %#v", events)
	}
	var foundMCPItem bool
	for _, item := range completed.Response.Output {
		if item.Type != "mcp_tool_call" {
			continue
		}
		if item.Name != "mcp_tool_1" || item.CallID != "call_mcp_1" {
			continue
		}
		foundMCPItem = true
		if derefString(item.Arguments) != `{"x":"hi"}` {
			t.Fatalf("response.completed.output mcp_tool_call arguments must be %q, got %q", `{"x":"hi"}`, derefString(item.Arguments))
		}
	}
	if !foundMCPItem {
		t.Fatalf("response.completed.output must contain the mcp_tool_call (mcp_tool_1 / call_mcp_1), got %#v", completed.Response.Output)
	}
}

// TestResponseInboundMCPToolCallNonStreamEmittedAsMCPItem covers the non-streaming
// path: TransformResponse must convert the chat tool_call into a Responses
// mcp_tool_call item (not a function_call) when the request declared the tool
// with type=mcp_tool_call.
func TestResponseInboundMCPToolCallNonStreamEmittedAsMCPItem(t *testing.T) {
	inbound := &ResponseInbound{}
	if _, err := inbound.TransformRequest(context.Background(), []byte(`{
		"model":"gpt-5.5",
		"input":"call the MCP tool",
		"tools":[{"type":"mcp_tool_call","name":"mcp_tool_1","parameters":{"type":"object"}}]
	}`)); err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	args := `{"x":"hello"}`
	resp := &model.InternalLLMResponse{
		ID:      "chatcmpl_nonstream",
		Model:   "gpt-5.5",
		Object:  "chat.completion",
		Created: 1,
		Choices: []model.Choice{{
			Index:        0,
			FinishReason: lo.ToPtr("tool_calls"),
			Message: &model.Message{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					ID:       "call_nonstream",
					Type:     "function",
					Index:    0,
					Function: model.FunctionCall{Name: "mcp_tool_1", Arguments: args},
				}},
			},
		}},
	}
	body, err := inbound.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse returned error: %v", err)
	}
	got := string(body)

	// The response MUST contain an mcp_tool_call output item, NOT a function_call.
	if !strings.Contains(got, `"type":"mcp_tool_call"`) {
		t.Fatalf("non-stream response must contain a mcp_tool_call output item, got %s", got)
	}
	if strings.Contains(got, `"type":"function_call"`) {
		t.Fatalf("non-stream response must NOT contain a function_call item for an MCP tool, got %s", got)
	}
	if !strings.Contains(got, `"name":"mcp_tool_1"`) {
		t.Fatalf("non-stream response must carry the tool name mcp_tool_1, got %s", got)
	}
	if !strings.Contains(got, `"call_id":"call_nonstream"`) {
		t.Fatalf("non-stream response must carry the call_id call_nonstream, got %s", got)
	}
	if !strings.Contains(got, `"arguments":"{\"x\":\"hello\"}"`) {
		t.Fatalf("non-stream response must carry the normalized JSON arguments, got %s", got)
	}
}

// TestResponseInboundMCPToolCallRoundTripsThroughInputItem guards the inbound
// side: a request whose input array contains an mcp_tool_call item from a
// previous turn must convert to an internal ToolCall tagged with
// ToolCallTypeMCP so the outbound->internal->inbound round-trip preserves the MCP
// nature (otherwise the next response would emit a generic function_call).
func TestResponseInboundMCPToolCallRoundTripsThroughInputItem(t *testing.T) {
	req, err := (&ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"prev turn had an mcp call"}]},
			{"type":"mcp_tool_call","call_id":"call_prev","name":"mcp_tool_1","arguments":"{\"x\":\"y\"}"},
			{"type":"mcp_tool_call_output","call_id":"call_prev","name":"mcp_tool_1","output":"ok"}
		]
	}`))
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	// Expect 3 messages: user, assistant(mcp tool call), tool(output).
	if len(req.Messages) != 3 {
		var roles []string
		for _, m := range req.Messages {
			roles = append(roles, m.Role)
		}
		t.Fatalf("expected 3 messages, got %d with roles %v", len(req.Messages), roles)
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected assistant message with 1 tool call, got %#v", assistant)
	}
	got := assistant.ToolCalls[0]
	if got.Type != model.ToolCallTypeMCP {
		t.Fatalf("expected ToolCallTypeMCP, got %q", got.Type)
	}
	if got.ID != "call_prev" || got.Function.Name != "mcp_tool_1" || got.Function.Arguments != `{"x":"y"}` {
		t.Fatalf("unexpected tool call payload: %#v", got)
	}
}
