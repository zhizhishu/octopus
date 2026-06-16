package anthropic

import (
	"context"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestConvertAnthropicClientUsageKeepsProviderNativeInputTokens(t *testing.T) {
	usage := convertAnthropicClientUsage(&transformerModel.Usage{
		PromptTokens:             60,
		CompletionTokens:         12,
		AnthropicUsage:           true,
		CacheCreationInputTokens: 10,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 30,
		},
	})

	if usage.InputTokens != 60 || usage.OutputTokens != 12 || usage.CacheReadInputTokens != 30 || usage.CacheCreationInputTokens != 10 {
		t.Fatalf("unexpected provider-native usage: %#v", usage)
	}
}

func TestConvertAnthropicClientUsageSubtractsOpenAIStyleCachedInput(t *testing.T) {
	usage := convertAnthropicClientUsage(&transformerModel.Usage{
		PromptTokens:     60,
		CompletionTokens: 12,
		PromptTokensDetails: &transformerModel.PromptTokensDetails{
			CachedTokens: 30,
		},
	})

	if usage.InputTokens != 30 || usage.OutputTokens != 12 || usage.CacheReadInputTokens != 30 {
		t.Fatalf("unexpected OpenAI-style usage: %#v", usage)
	}
}

func TestTransformRequestKeepsThinkingOnlyMessageEffective(t *testing.T) {
	inbound := &MessagesInbound{}
	req, err := inbound.TransformRequest(context.Background(), []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":256,
		"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":"sig"}]}]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 1 || strings.TrimSpace(req.Messages[0].GetReasoningContent()) == "" {
		t.Fatalf("expected reasoning-only message to be preserved, got %#v", req.Messages)
	}
}

func TestTransformRequestKeepsServerToolUseEffective(t *testing.T) {
	inbound := &MessagesInbound{}
	req, err := inbound.TransformRequest(context.Background(), []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":256,
		"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"srvu_1","name":"web_search","input":{"query":"octopus"}}]}]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("expected server_tool_use to be preserved as a tool call, got %#v", req.Messages)
	}
}

func TestTransformRequestPreservesClaudeContextManagement(t *testing.T) {
	inbound := &MessagesInbound{}
	req, err := inbound.TransformRequest(context.Background(), []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64000,
		"service_tier":"auto",
		"betas":["custom-beta-2026-06-08"," "],
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"high"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"tools":[],
		"messages":[{"role":"user","content":"ping"}]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if req.ServiceTier == nil || *req.ServiceTier != "auto" {
		t.Fatalf("expected service_tier auto, got %#v", req.ServiceTier)
	}
	if !req.AdaptiveThinking || req.ReasoningEffort != EffortHigh {
		t.Fatalf("expected adaptive high thinking, effort=%q adaptive=%t", req.ReasoningEffort, req.AdaptiveThinking)
	}
	if !strings.Contains(string(req.AnthropicContextManagement), "clear_thinking_20251015") {
		t.Fatalf("expected context_management to be preserved, got %s", string(req.AnthropicContextManagement))
	}
	if !strings.Contains(string(req.AnthropicThinking), `"adaptive"`) {
		t.Fatalf("expected raw thinking to be preserved, got %s", string(req.AnthropicThinking))
	}
	if !strings.Contains(string(req.AnthropicOutputConfig), `"effort"`) {
		t.Fatalf("expected raw output_config to be preserved, got %s", string(req.AnthropicOutputConfig))
	}
	if !req.AnthropicToolsPresent {
		t.Fatalf("expected empty Anthropic tools field presence to be preserved")
	}
	if len(req.TransformOptions.AnthropicBetas) != 1 || req.TransformOptions.AnthropicBetas[0] != "custom-beta-2026-06-08" {
		t.Fatalf("expected body betas to be lifted, got %#v", req.TransformOptions.AnthropicBetas)
	}
}

func TestTransformStreamFinalizesMessageStopOnDoneWithoutUsage(t *testing.T) {
	inbound := &MessagesInbound{}
	content := "hello"
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_no_usage",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				Role:    "assistant",
				Content: transformerModel.MessageContent{Content: &content},
			},
		}},
	}); err != nil {
		t.Fatalf("content stream: %v", err)
	}

	finishReason := "stop"
	finish, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_no_usage",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	})
	if err != nil {
		t.Fatalf("finish stream: %v", err)
	}
	if !strings.Contains(string(finish), "event:content_block_stop") {
		t.Fatalf("expected content block stop before done, got %s", string(finish))
	}

	done, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("done stream: %v", err)
	}
	got := string(done)
	for _, want := range []string{"event:message_delta", `"stop_reason":"end_turn"`, "event:message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected done stream to contain %q, got %s", want, got)
		}
	}

	again, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("second done stream: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("expected second done to avoid duplicate message_stop, got %s", string(again))
	}
}

func TestTransformStreamUsageChunkStillFinalizesBeforeDone(t *testing.T) {
	inbound := &MessagesInbound{}
	content := "hello"
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_usage",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index: 0,
			Delta: &transformerModel.Message{
				Role:    "assistant",
				Content: transformerModel.MessageContent{Content: &content},
			},
		}},
	}); err != nil {
		t.Fatalf("content stream: %v", err)
	}

	finishReason := "stop"
	if _, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_usage",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Choices: []transformerModel.Choice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
	}); err != nil {
		t.Fatalf("finish stream: %v", err)
	}

	usage, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{
		ID:     "chatcmpl_usage",
		Object: "chat.completion.chunk",
		Model:  "gpt-5.5",
		Usage: &transformerModel.Usage{
			PromptTokens:     12,
			CompletionTokens: 3,
		},
	})
	if err != nil {
		t.Fatalf("usage stream: %v", err)
	}
	got := string(usage)
	for _, want := range []string{"event:message_delta", `"input_tokens":12`, `"output_tokens":3`, "event:message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected usage stream to contain %q, got %s", want, got)
		}
	}

	done, err := inbound.TransformStream(context.Background(), &transformerModel.InternalLLMResponse{Object: "[DONE]"})
	if err != nil {
		t.Fatalf("done stream: %v", err)
	}
	if len(done) != 0 {
		t.Fatalf("expected done after usage finalization to be empty, got %s", string(done))
	}
}
