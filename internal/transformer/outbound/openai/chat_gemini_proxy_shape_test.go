package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// Chat-protocol Gemini proxy models are not native Gemini channels.
// The model's -high suffix already selects its reasoning variant. Keep their
// upstream body in OpenAI Chat format, without Gemini thinkingConfig or
// Claude/Codex identity changes, when an agent sends a large tool-bearing turn.
func TestGeminiChatProxyPreservesAgentRequestShape(t *testing.T) {
	for _, name := range []string{"gemini-4-carbon-high", "gemini-4-hydrogen-high"} {
		t.Run(name, func(t *testing.T) {
			maxTokens := int64(65536)
			stream := true
			roles := []model.Message{
				{Role: "developer", Content: model.MessageContent{Content: stringPtrForProxyTest("agent instructions")}},
				{Role: "user", Content: model.MessageContent{Content: stringPtrForProxyTest("context A")}},
				{Role: "user", Content: model.MessageContent{Content: stringPtrForProxyTest("context B")}},
				{Role: "user", Content: model.MessageContent{Content: stringPtrForProxyTest("execute a tool")}},
			}
			tools := make([]model.Tool, 11)
			for i := range tools {
				tools[i] = model.Tool{Type: "function", Function: model.Function{
					Name:       fmt.Sprintf("action_%d", i),
					Parameters: json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}}}`),
				}}
			}
			payload := chatBodyWithBase(t, &model.InternalLLMRequest{
				Model: name, Messages: roles, Tools: tools, Stream: &stream,
				ReasoningEffort: "high", MaxCompletionTokens: &maxTokens,
			}, "https://upstream.example/v1")

			if payload["model"] != name {
				t.Fatalf("model changed: %#v", payload["model"])
			}
			if payload["max_completion_tokens"] != float64(maxTokens) {
				t.Fatalf("token limit dropped: %#v", payload["max_completion_tokens"])
			}
			if payload["stream"] != true {
				t.Fatalf("stream flag lost: %#v", payload["stream"])
			}
			if v, ok := payload["tools"].([]any); !ok || len(v) != 11 {
				t.Fatalf("tool declarations lost: %#v", payload["tools"])
			}
			for _, k := range []string{"generationConfig", "thinkingConfig", "thinkingBudget", "thinkingLevel", "enable_thinking", "thinking_budget"} {
				if _, exists := payload[k]; exists {
					t.Fatalf("native Gemini / unrelated provider field %s leaked into Chat", k)
				}
			}
			// The target Chat proxy selects reasoning by the wire model name.
			// Unrecognized native controls must not be manufactured for it.
			if _, exists := payload["reasoning_effort"]; exists {
				t.Fatal("unexpected unsupported generic effort field on Chat proxy")
			}
			opts, ok := payload["stream_options"].(map[string]any)
			if !ok || opts["include_usage"] != true {
				t.Fatalf("missing stream usage: %#v", payload["stream_options"])
			}
			messages, ok := payload["messages"].([]any)
			if !ok || len(messages) < 2 {
				t.Fatalf("agent messages dropped: %#v", payload["messages"])
			}
			first := messages[0].(map[string]any)
			if first["role"] != "system" {
				t.Fatalf("third-party Chat expects system instruction: %#v", first)
			}
			merged := ""
			for _, entry := range messages {
				m := entry.(map[string]any)
				if m["role"] == "user" {
					merged += fmt.Sprint(m["content"])
				}
			}
			for _, piece := range []string{"context A", "context B", "execute a tool"} {
				if !strings.Contains(merged, piece) {
					t.Fatalf("agent content fragment lost: %s", piece)
				}
			}
		})
	}
}

func TestGeminiChatProxyKeepsCherryStudioParameters(t *testing.T) {
	stream := true
	temperature := 1.0
	topP := 0.9
	payload := chatBodyWithBase(t, &model.InternalLLMRequest{
		Model:    "gemini-4-carbon-high",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: stringPtrForProxyTest("hello")}}},
		Stream:   &stream, Temperature: &temperature, TopP: &topP,
	}, "https://upstream.example/v1")
	if payload["temperature"] != float64(1) || payload["top_p"] != topP {
		t.Fatalf("Cherry-style sampling parameters changed: %#v", payload)
	}
	if _, ok := payload["tools"]; ok {
		t.Fatalf("unexpected agent tools on plain Chat: %#v", payload)
	}
}

func stringPtrForProxyTest(value string) *string { return &value }
