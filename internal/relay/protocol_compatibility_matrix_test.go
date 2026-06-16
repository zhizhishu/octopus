package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	anthropicOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/authropic"
	geminiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/gemini"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

func TestProtocolCompatibilityMatrix(t *testing.T) {
	type matrixCase struct {
		name   string
		verify func(t *testing.T)
	}

	cases := []matrixCase{
		{
			name: "openai chat legacy function_call",
			verify: func(t *testing.T) {
				req, err := (&openaiInbound.ChatInbound{}).TransformRequest(context.Background(), []byte(`{
					"model":"gpt-4o",
					"messages":[
						{"role":"user","content":"lookup"},
						{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{\"q\":\"octopus\"}"}},
						{"role":"function","name":"lookup","content":"{\"ok\":true}"}
					]
				}`))
				if err != nil {
					t.Fatalf("transform request: %v", err)
				}
				if len(req.Messages) != 3 || len(req.Messages[1].ToolCalls) != 1 || req.Messages[2].Role != "tool" {
					t.Fatalf("legacy function_call matrix failed: %#v", req.Messages)
				}
				if req.Messages[2].ToolCallID == nil || *req.Messages[2].ToolCallID != req.Messages[1].ToolCalls[0].ID {
					t.Fatalf("function result did not bind to generated tool call: %#v", req.Messages[2])
				}
			},
		},
		{
			name: "openai responses developer role and empty tool output",
			verify: func(t *testing.T) {
				req, err := (&openaiInbound.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{
					"model":"gpt-4o",
					"input":[
						{"type":"message","role":"developer","content":[{"type":"input_text","text":"follow policy"}]},
						{"type":"input_text","text":"hello"},
						{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
						{"type":"function_call_output","call_id":"call_1"}
					],
					"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
					"tool_choice":{"type":"function","function":{"name":"lookup"}}
				}`))
				if err != nil {
					t.Fatalf("transform request: %v", err)
				}
				if len(req.Messages) != 4 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
					t.Fatalf("responses role normalization failed: %#v", req.Messages)
				}
				tool := req.Messages[3]
				if tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "call_1" || tool.Content.Content == nil || *tool.Content.Content != "" {
					t.Fatalf("responses empty function_call_output failed: %#v", tool)
				}
				if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil || req.ToolChoice.NamedToolChoice.Function.Name != "lookup" {
					t.Fatalf("responses tool_choice failed: %#v", req.ToolChoice)
				}
			},
		},
		{
			name: "anthropic messages tool_use and empty tool_result",
			verify: func(t *testing.T) {
				req, err := (&anthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(`{
					"model":"claude-sonnet-4.5",
					"max_tokens":256,
					"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},
					"messages":[
						{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"octopus"}}]},
						{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1"}]}
					]
				}`))
				if err != nil {
					t.Fatalf("transform request: %v", err)
				}
				if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil || req.ParallelToolCalls == nil || *req.ParallelToolCalls {
					t.Fatalf("anthropic tool choice/parallel preference failed: choice=%#v parallel=%#v", req.ToolChoice, req.ParallelToolCalls)
				}
				if len(req.Messages) != 2 || len(req.Messages[0].ToolCalls) != 1 {
					t.Fatalf("anthropic tool_use failed: %#v", req.Messages)
				}
				tool := req.Messages[1]
				if tool.Role != "tool" || tool.ToolCallID == nil || *tool.ToolCallID != "toolu_1" || tool.Content.Content == nil || *tool.Content.Content != "" {
					t.Fatalf("anthropic empty tool_result failed: %#v", tool)
				}
			},
		},
		{
			name: "gemini generateContent tool call and response",
			verify: func(t *testing.T) {
				ctx := gemini.WithRequestOptions(context.Background(), "gemini-request", false)
				req, err := (&gemini.GenerateContentInbound{}).TransformRequest(ctx, []byte(`{
					"contents":[
						{"role":"user","parts":[{"text":"hello"}]},
						{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"octopus"}}}]},
						{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}
					],
					"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],
					"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}
				}`))
				if err != nil {
					t.Fatalf("transform request: %v", err)
				}
				if req.Model != "gemini-request" || req.RawAPIFormat != transformerModel.APIFormatGeminiContents {
					t.Fatalf("gemini metadata failed: model=%q format=%s", req.Model, req.RawAPIFormat)
				}
				if len(req.Messages) != 3 || len(req.Messages[1].ToolCalls) != 1 || req.Messages[2].Role != "tool" {
					t.Fatalf("gemini tool conversion failed: %#v", req.Messages)
				}
			},
		},
		{
			name: "stream adapters skip blank keepalive data",
			verify: func(t *testing.T) {
				streamAdapters := []struct {
					name string
					fn   func([]byte) (*transformerModel.InternalLLMResponse, error)
				}{
					{name: "openai chat", fn: func(data []byte) (*transformerModel.InternalLLMResponse, error) {
						return (&openaiOutbound.ChatOutbound{}).TransformStream(context.Background(), data)
					}},
					{name: "openai responses", fn: func(data []byte) (*transformerModel.InternalLLMResponse, error) {
						return (&openaiOutbound.ResponseOutbound{}).TransformStream(context.Background(), data)
					}},
					{name: "anthropic", fn: func(data []byte) (*transformerModel.InternalLLMResponse, error) {
						return (&anthropicOutbound.MessageOutbound{}).TransformStream(context.Background(), data)
					}},
					{name: "gemini", fn: func(data []byte) (*transformerModel.InternalLLMResponse, error) {
						return (&geminiOutbound.MessagesOutbound{}).TransformStream(context.Background(), data)
					}},
				}
				for _, adapter := range streamAdapters {
					got, err := adapter.fn([]byte(" \n\t "))
					if err != nil {
						t.Fatalf("%s blank stream data returned error: %v", adapter.name, err)
					}
					if got != nil {
						t.Fatalf("%s blank stream data should be ignored, got %#v", adapter.name, got)
					}
				}
			},
		},
		{
			name: "stream keepalive and timeout semantics",
			verify: func(t *testing.T) {
				if !isStreamKeepaliveEvent("ping", `{"type":"message_start"}`) {
					t.Fatalf("expected Anthropic ping event type to be treated as keepalive")
				}
				if !isStreamKeepaliveEvent("", `{"type":"ping"}`) {
					t.Fatalf("expected JSON ping envelope to be treated as keepalive")
				}
				if isStreamKeepaliveEvent("", `{"type":"content_block_stop"}`) {
					t.Fatalf("content_block_stop should not be treated as keepalive")
				}
				anthropicAttempt := &relayAttempt{relayRequest: &relayRequest{inboundType: inbound.InboundTypeAnthropic}}
				if got := string(anthropicAttempt.streamKeepaliveData()); !strings.Contains(got, "event: ping") || !strings.Contains(got, `"type":"ping"`) {
					t.Fatalf("expected Anthropic keepalive ping, got %q", got)
				}
				openAIAttempt := &relayAttempt{relayRequest: &relayRequest{inboundType: inbound.InboundTypeOpenAIChat}}
				if got := string(openAIAttempt.streamKeepaliveData()); got != ":\n\n" {
					t.Fatalf("expected OpenAI-compatible SSE comment keepalive, got %q", got)
				}
				if got := streamSecondsDuration(0); got != 0 {
					t.Fatalf("0 seconds should disable stream timers, got %s", got)
				}
				if got := streamSecondsDuration(15); got != 15*time.Second {
					t.Fatalf("15 seconds should become a 15s timer, got %s", got)
				}
			},
		},
		{
			name: "openai cache usage aliases",
			verify: func(t *testing.T) {
				var usage transformerModel.Usage
				if err := json.Unmarshal([]byte(`{
					"input_tokens":60,
					"output_tokens":20,
					"cache_read_input_tokens":30,
					"cache_creation_input_tokens":10,
					"cached_tokens":30
				}`), &usage); err != nil {
					t.Fatalf("unmarshal usage: %v", err)
				}
				hit, write, input := usageCacheStats(&usage)
				if hit != 30 || write != 10 || input != 100 {
					t.Fatalf("cache usage alias matrix failed: hit=%d write=%d input=%d usage=%#v", hit, write, input, usage)
				}
			},
		},
		{
			name: "responses stream emits completed before done",
			verify: func(t *testing.T) {
				inbound := &openaiInbound.ResponseInbound{}
				content := "ok"
				finishReason := "stop"
				if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
					ID: "chatcmpl_1", Object: "chat.completion.chunk", Model: "gpt-4o",
					Choices: []transformerModel.Choice{{Index: 0, Delta: &transformerModel.Message{Role: "assistant", Content: transformerModel.MessageContent{Content: &content}}}},
				}); err != nil {
					t.Fatalf("content stream: %v", err)
				}
				if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
					ID: "chatcmpl_1", Object: "chat.completion.chunk", Model: "gpt-4o",
					Choices: []transformerModel.Choice{{Index: 0, FinishReason: &finishReason}},
				}); err != nil {
					t.Fatalf("finish stream: %v", err)
				}
				done, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
				if err != nil {
					t.Fatalf("done stream: %v", err)
				}
				got := string(done)
				if !strings.Contains(got, `"type":"response.completed"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
					t.Fatalf("responses completed-before-done matrix failed: %s", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, tc.verify)
	}
}
