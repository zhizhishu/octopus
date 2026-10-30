package authropic

import (
	"encoding/json"
	"testing"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

const injectedClearThinking = `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`

func injectedCMRequest() *transformerModel.InternalLLMRequest {
	content := "ping"
	maxTokens := int64(8192)
	return &transformerModel.InternalLLMRequest{
		Model:                      "claude-opus-4-7",
		MaxTokens:                  &maxTokens,
		AnthropicContextManagement: json.RawMessage(injectedClearThinking),
		TransformOptions:           transformerModel.TransformOptions{ClaudeContextManagementInjected: true},
		Messages:                   []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &content}}},
	}
}

// Anthropic 400: "`clear_thinking_20251015` strategy requires `thinking` to be enabled or adaptive".
func TestInjectedContextManagementDroppedWhenThinkingOff(t *testing.T) {
	// no thinking at all
	if got := convertToAnthropicRequest(injectedCMRequest()); got.ContextManagement != nil {
		t.Fatalf("injected context_management kept without thinking: %s", got.ContextManagement)
	}
	// client turned thinking off
	req := injectedCMRequest()
	req.AnthropicThinking = json.RawMessage(`{"type":"disabled"}`)
	if got := convertToAnthropicRequest(req); got.ContextManagement != nil {
		t.Fatalf("injected context_management kept with thinking disabled: %s", got.ContextManagement)
	}
	req = injectedCMRequest()
	req.ReasoningEffort = "none"
	if got := convertToAnthropicRequest(req); got.ContextManagement != nil {
		t.Fatalf("injected context_management kept with reasoning_effort=none: %s", got.ContextManagement)
	}
}

func TestInjectedContextManagementKeptWhenThinkingOn(t *testing.T) {
	req := injectedCMRequest()
	req.AnthropicThinking = json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
	if got := convertToAnthropicRequest(req); string(got.ContextManagement) != injectedClearThinking {
		t.Fatalf("adaptive: context_management = %s", got.ContextManagement)
	}
	req = injectedCMRequest()
	req.ReasoningEffort = "high"
	req.AdaptiveThinking = true
	if got := convertToAnthropicRequest(req); string(got.ContextManagement) != injectedClearThinking {
		t.Fatalf("adaptive (internal form): context_management = %s", got.ContextManagement)
	}
	req = injectedCMRequest()
	req.ReasoningEffort = "medium" // budget-based enabled thinking
	if got := convertToAnthropicRequest(req); string(got.ContextManagement) != injectedClearThinking {
		t.Fatalf("enabled: context_management = %s", got.ContextManagement)
	}
}

// The client's own context_management is the client's call — never removed.
func TestClientContextManagementNeverDropped(t *testing.T) {
	req := injectedCMRequest()
	req.TransformOptions.ClaudeContextManagementInjected = false
	req.AnthropicThinking = json.RawMessage(`{"type":"disabled"}`)
	if got := convertToAnthropicRequest(req); string(got.ContextManagement) != injectedClearThinking {
		t.Fatalf("client context_management changed: %s", got.ContextManagement)
	}
}
