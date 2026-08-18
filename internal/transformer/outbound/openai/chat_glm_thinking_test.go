package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func chatRequestBody(t *testing.T, request *model.InternalLLMRequest) map[string]any {
	t.Helper()

	req, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, "https://upstream.example", "key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return payload
}

func userMessages() []model.Message {
	content := "hi"
	return []model.Message{{
		Role:    "user",
		Content: model.MessageContent{Content: &content},
	}}
}

func thinkingType(t *testing.T, payload map[string]any) (string, bool) {
	t.Helper()

	raw, ok := payload["thinking"]
	if !ok {
		return "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("thinking field is not an object: %#v", raw)
	}
	typ, _ := obj["type"].(string)
	return typ, true
}

func TestChatOutboundGLMEnablesThinkingFromReasoningEffort(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-4.6",
		ReasoningEffort: "high",
		Messages:        userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok {
		t.Fatalf("expected thinking field for GLM with reasoning_effort, got %#v", payload)
	}
	if typ != "enabled" {
		t.Fatalf("expected thinking type enabled, got %q", typ)
	}
}

func TestChatOutboundGLMEnablesThinkingFromAdaptiveThinking(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:            "glm-4.5",
		AdaptiveThinking: true,
		Messages:         userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok || typ != "enabled" {
		t.Fatalf("expected thinking type enabled for adaptive thinking, got %q ok=%t (%#v)", typ, ok, payload)
	}
}

func TestChatOutboundGLMEnablesThinkingFromReasoningBudget(t *testing.T) {
	budget := int64(2048)
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-4.6",
		ReasoningBudget: &budget,
		Messages:        userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok || typ != "enabled" {
		t.Fatalf("expected thinking type enabled for reasoning budget, got %q ok=%t (%#v)", typ, ok, payload)
	}
}

func TestChatOutboundZaiEnablesThinking(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "zai-org/glm-4.6", // also covered by glm, but exercise the zai token
		ReasoningEffort: "medium",
		Messages:        userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok || typ != "enabled" {
		t.Fatalf("expected thinking type enabled for zai model, got %q ok=%t (%#v)", typ, ok, payload)
	}
}

func TestChatOutboundGLMDisablesThinkingForMinimalEffort(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-4.6",
		ReasoningEffort: "minimal",
		Messages:        userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok {
		t.Fatalf("expected thinking field for explicit disable, got %#v", payload)
	}
	if typ != "disabled" {
		t.Fatalf("expected thinking type disabled, got %q", typ)
	}
}

func TestChatOutboundGLMDoesNotOverrideClientThinking(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-4.6",
		ReasoningEffort: "high",
		Thinking:        json.RawMessage(`{"type":"disabled"}`),
		Messages:        userMessages(),
	})

	typ, ok := thinkingType(t, payload)
	if !ok {
		t.Fatalf("expected client thinking to be preserved, got %#v", payload)
	}
	if typ != "disabled" {
		t.Fatalf("expected client-provided thinking type disabled to be respected, got %q", typ)
	}
}

func TestChatOutboundGLMWithoutReasoningIntentOmitsThinking(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:    "glm-4.6",
		Messages: userMessages(),
	})

	if _, ok := payload["thinking"]; ok {
		t.Fatalf("did not expect thinking field when no reasoning intent is present: %#v", payload)
	}
}

func TestChatOutboundNonGLMNeverInjectsThinking(t *testing.T) {
	for _, modelName := range []string{"gpt-4o", "deepseek-chat", "deepseek-reasoner"} {
		payload := chatRequestBody(t, &model.InternalLLMRequest{
			Model:           modelName,
			ReasoningEffort: "high",
			Messages:        userMessages(),
		})
		if _, ok := payload["thinking"]; ok {
			t.Fatalf("non-GLM model %q must not be injected with thinking: %#v", modelName, payload)
		}
	}
}

func TestChatOutboundGLMEnablesToolStreamingForStreamedTools(t *testing.T) {
	stream := true
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:    "glm-5.2",
		Stream:   &stream,
		Messages: userMessages(),
		Tools: []model.Tool{{
			Type: "function",
			Function: model.Function{
				Name:       "ReadFile",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
	})

	if enabled, ok := payload["tool_stream"].(bool); !ok || !enabled {
		t.Fatalf("expected tool_stream=true for a streamed GLM tool request, got %#v", payload["tool_stream"])
	}
}

func TestChatOutboundGLMRespectsExplicitToolStreamingValue(t *testing.T) {
	stream := true
	toolStream := false
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:      "glm-5.2",
		Stream:     &stream,
		ToolStream: &toolStream,
		Messages:   userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	})

	if enabled, ok := payload["tool_stream"].(bool); !ok || enabled {
		t.Fatalf("expected explicit tool_stream=false to be preserved, got %#v", payload["tool_stream"])
	}
}

func TestChatOutboundToolStreamingProjectionDoesNotMutateRetryRequest(t *testing.T) {
	stream := true
	request := &model.InternalLLMRequest{
		Model:    "glm-5.2",
		Stream:   &stream,
		Messages: userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	}

	firstPayload := chatRequestBody(t, request)
	if enabled, ok := firstPayload["tool_stream"].(bool); !ok || !enabled {
		t.Fatalf("expected GLM attempt to enable tool streaming, got %#v", firstPayload["tool_stream"])
	}
	if request.ToolStream != nil {
		t.Fatalf("GLM projection leaked into the shared retry request: %#v", request.ToolStream)
	}

	request.Model = "deepseek-chat"
	secondPayload := chatRequestBody(t, request)
	if _, present := secondPayload["tool_stream"]; present {
		t.Fatalf("tool_stream leaked from the GLM attempt into a non-GLM retry: %#v", secondPayload)
	}
}

func TestChatOutboundGLMThinkingProjectionDoesNotMutateRetryRequest(t *testing.T) {
	// GLM projects reasoning_effort onto Thinking for the attempt body only.
	// A later non-GLM failover must not inherit that provider-specific field.
	request := &model.InternalLLMRequest{
		Model:           "glm-5.2",
		ReasoningEffort: "high",
		Messages:        userMessages(),
	}

	firstPayload := chatRequestBody(t, request)
	if typ, ok := thinkingType(t, firstPayload); !ok || typ != "enabled" {
		t.Fatalf("expected GLM attempt to project thinking=enabled, got %q ok=%t (%#v)", typ, ok, firstPayload)
	}
	if request.Thinking != nil {
		t.Fatalf("GLM thinking projection leaked into the shared retry request: %#v", request.Thinking)
	}

	request.Model = "deepseek-chat"
	secondPayload := chatRequestBody(t, request)
	if _, present := secondPayload["thinking"]; present {
		t.Fatalf("thinking leaked from the GLM attempt into a non-GLM retry: %#v", secondPayload)
	}
}

func TestChatOutboundNonGLMAttemptPreservesExplicitValueForLaterGLMRetry(t *testing.T) {
	stream := true
	toolStream := false
	request := &model.InternalLLMRequest{
		Model:      "deepseek-chat",
		Stream:     &stream,
		ToolStream: &toolStream,
		Messages:   userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	}

	firstPayload := chatRequestBody(t, request)
	if _, present := firstPayload["tool_stream"]; present {
		t.Fatalf("non-GLM attempt must not receive tool_stream: %#v", firstPayload)
	}
	if request.ToolStream == nil || *request.ToolStream {
		t.Fatalf("non-GLM attempt lost the client's explicit false value: %#v", request.ToolStream)
	}

	request.Model = "glm-5.2"
	secondPayload := chatRequestBody(t, request)
	if enabled, ok := secondPayload["tool_stream"].(bool); !ok || enabled {
		t.Fatalf("later GLM retry must preserve explicit tool_stream=false, got %#v", secondPayload["tool_stream"])
	}
}

func TestChatOutboundToolStreamingCompatibilityIsGLMOnly(t *testing.T) {
	stream := true
	toolStream := true
	for _, testCase := range []struct {
		modelName      string
		expectInjected bool
	}{
		{modelName: "glm-4.5", expectInjected: false},
		{modelName: "glm-4.6", expectInjected: true},
		{modelName: "glm-4.6-air", expectInjected: true},
		{modelName: "glm-4.7", expectInjected: true},
		{modelName: "glm-4.7-flash", expectInjected: true},
		{modelName: "glm-5", expectInjected: true},
		{modelName: "glm-5.2", expectInjected: true},
		{modelName: "glm-5-air", expectInjected: true},
		{modelName: "vendor/glm-5.2", expectInjected: true},
		{modelName: "glm-4.60", expectInjected: false},
		{modelName: "glm-4.6xxxx", expectInjected: false},
		{modelName: "glm-50", expectInjected: false},
		{modelName: "glm-5x", expectInjected: false},
		{modelName: "deepseek-chat", expectInjected: false},
	} {
		t.Run(testCase.modelName, func(t *testing.T) {
			request := &model.InternalLLMRequest{
				Model:    testCase.modelName,
				Stream:   &stream,
				Messages: userMessages(),
				Tools: []model.Tool{{
					Type:     "function",
					Function: model.Function{Name: "ReadFile"},
				}},
			}
			if testCase.modelName == "deepseek-chat" {
				request.ToolStream = &toolStream
			}

			payload := chatRequestBody(t, request)
			_, present := payload["tool_stream"]
			if present != testCase.expectInjected {
				t.Fatalf("tool_stream presence=%t, want %t: %#v", present, testCase.expectInjected, payload)
			}
		})
	}
}

func TestChatOutboundGLMDropsReasoningEffortAfterThinkingProjection(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-5.2",
		ReasoningEffort: "high",
		Messages:        userMessages(),
	})

	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("GLM body must not dual-send reasoning_effort after thinking projection: %#v", payload)
	}
	typ, ok := thinkingType(t, payload)
	if !ok || typ != "enabled" {
		t.Fatalf("expected thinking=enabled after projection, got %q ok=%t (%#v)", typ, ok, payload)
	}
}

func TestChatOutboundGLMDropsReasoningEffortWhenClientSuppliesThinking(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-5.2",
		ReasoningEffort: "high",
		Thinking:        json.RawMessage(`{"type":"enabled"}`),
		Messages:        userMessages(),
	})

	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("GLM body must drop reasoning_effort even when client thinking is preserved: %#v", payload)
	}
}

func TestChatOutboundGLMReasoningEffortProjectionDoesNotMutateRetryRequest(t *testing.T) {
	request := &model.InternalLLMRequest{
		Model:           "glm-5.2",
		ReasoningEffort: "high",
		Messages:        userMessages(),
	}

	_ = chatRequestBody(t, request)
	if request.ReasoningEffort != "high" {
		t.Fatalf("GLM projection must restore ReasoningEffort on shared retry request, got %q", request.ReasoningEffort)
	}
}

func TestChatOutboundGLMXHighMapsToThinkingAndDropsEffort(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:           "glm-5.2",
		ReasoningEffort: "xhigh",
		Messages:        userMessages(),
	})
	if _, ok := payload["reasoning_effort"]; ok {
		t.Fatalf("xhigh must not leak as reasoning_effort: %#v", payload)
	}
	typ, ok := thinkingType(t, payload)
	if !ok || typ != "enabled" {
		t.Fatalf("xhigh must project thinking=enabled, got %q ok=%t (%#v)", typ, ok, payload)
	}
}

func TestChatOutboundThirdPartyStripsServiceTier(t *testing.T) {
	tier := "priority"
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:       "glm-5.2",
		ServiceTier: &tier,
		Messages:    userMessages(),
	})
	if _, ok := payload["service_tier"]; ok {
		t.Fatalf("third-party chat must strip service_tier: %#v", payload)
	}
}

func TestChatOutboundGLMRemapsMaxCompletionTokens(t *testing.T) {
	n := int64(4096)
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model:               "glm-5.2",
		MaxCompletionTokens: &n,
		Messages:            userMessages(),
	})
	if _, ok := payload["max_completion_tokens"]; ok {
		t.Fatalf("GLM chat must not send max_completion_tokens: %#v", payload)
	}
	if got, ok := payload["max_tokens"].(float64); !ok || int64(got) != 4096 {
		t.Fatalf("expected max_tokens=4096, got %#v", payload["max_tokens"])
	}
}
