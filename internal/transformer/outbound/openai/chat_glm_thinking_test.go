package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func chatRequestBodyWithBase(baseUrl string, t *testing.T, request *model.InternalLLMRequest) map[string]any {
	t.Helper()

	req, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, baseUrl, "key")
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

// chatRequestBody drives the generic upstream host, where tool_stream must NOT
// be auto-injected; tests that need a first-party GLM host use
// chatRequestBodyWithBase explicitly.
func chatRequestBody(t *testing.T, request *model.InternalLLMRequest) map[string]any {
	t.Helper()
	return chatRequestBodyWithBase("https://upstream.example", t, request)
}

const (
	glmNativeHostZAI   = "https://api.z.ai/v1"
	glmNativeHostGLM   = "https://open.bigmodel.cn/api/paas/v4"
	glmSpoofHostSuffix = "https://api.z.ai.evil.example/v1"
	glmSpoofHostPrefix = "https://notapi.z.ai/v1"
	glmSpoofPathOnly   = "https://example.com/api.z.ai/v1"
	glmSpoofUserinfo   = "https://api.z.ai@evil.example/v1"
	glmSpoofInvalid    = "https://%zz:bad"
)

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
	payload := chatRequestBodyWithBase(glmNativeHostZAI, t, &model.InternalLLMRequest{
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
		t.Fatalf("expected tool_stream=true for a streamed GLM tool request on %s, got %#v", glmNativeHostZAI, payload["tool_stream"])
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

	firstPayload := chatRequestBodyWithBase(glmNativeHostZAI, t, request)
	if enabled, ok := firstPayload["tool_stream"].(bool); !ok || !enabled {
		t.Fatalf("expected GLM attempt on %s to enable tool streaming, got %#v", glmNativeHostZAI, firstPayload["tool_stream"])
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

			payload := chatRequestBodyWithBase(glmNativeHostZAI, t, request)
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

func streamedGLMToolRequest() *model.InternalLLMRequest {
	stream := true
	return &model.InternalLLMRequest{
		Model:    "glm-5.2",
		Stream:   &stream,
		Messages: userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	}
}

func TestChatOutboundGenericUpstreamStreamedGLMToolsOmitsSyntheticField(t *testing.T) {
	// A generic (non first-party) gateway may serve a GLM model but reject the
	// tool_stream extension as an unknown field. It must receive no synthetic
	// value while the client's tools/stream are forwarded untouched.
	payload := chatRequestBody(t, streamedGLMToolRequest())
	if _, ok := payload["tool_stream"]; ok {
		t.Fatalf("generic upstream must not receive a synthetic tool_stream, got %#v", payload["tool_stream"])
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("generic upstream must keep tools even without tool_stream: %#v", payload)
	}
	if enabled, ok := payload["stream"].(bool); !ok || !enabled {
		t.Fatalf("generic upstream must keep stream=true even without tool_stream: %#v", payload)
	}
}

func TestChatOutboundNativeHostsEnableToolStreaming(t *testing.T) {
	for _, host := range []string{glmNativeHostZAI, glmNativeHostGLM} {
		t.Run(host, func(t *testing.T) {
			payload := chatRequestBodyWithBase(host, t, streamedGLMToolRequest())
			if enabled, ok := payload["tool_stream"].(bool); !ok || !enabled {
				t.Fatalf("expected tool_stream=true for supported GLM on %s, got %#v", host, payload["tool_stream"])
			}
		})
	}
}

func TestChatOutboundToolStreamingOmissionCases(t *testing.T) {
	stream := true
	// No tools.
	noTools := chatRequestBodyWithBase(glmNativeHostZAI, t, &model.InternalLLMRequest{
		Model:    "glm-5.2",
		Stream:   &stream,
		Messages: userMessages(),
	})
	if _, ok := noTools["tool_stream"]; ok {
		t.Fatalf("no-tools request must not get tool_stream: %#v", noTools)
	}
	// Non-stream.
	nonStream := false
	nonStreamPayload := chatRequestBodyWithBase(glmNativeHostZAI, t, &model.InternalLLMRequest{
		Model:    "glm-5.2",
		Stream:   &nonStream,
		Messages: userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	})
	if _, ok := nonStreamPayload["tool_stream"]; ok {
		t.Fatalf("non-stream request must not get tool_stream: %#v", nonStreamPayload)
	}
	// Unsupported family on a native host.
	unsupported := chatRequestBodyWithBase(glmNativeHostZAI, t, &model.InternalLLMRequest{
		Model:    "glm-4.5",
		Stream:   &stream,
		Messages: userMessages(),
		Tools: []model.Tool{{
			Type:     "function",
			Function: model.Function{Name: "ReadFile"},
		}},
	})
	if _, ok := unsupported["tool_stream"]; ok {
		t.Fatalf("unsupported GLM family must not get tool_stream even on a native host: %#v", unsupported)
	}
}

func TestChatOutboundGLMToolStreamingHostSpoofNegatives(t *testing.T) {
	for _, host := range []string{
		glmSpoofHostSuffix,
		glmSpoofHostPrefix,
		glmSpoofPathOnly,
		glmSpoofUserinfo,
		glmSpoofInvalid,
		"",
	} {
		t.Run(host, func(t *testing.T) {
			request := streamedGLMToolRequest()
			applyGLMToolStreaming(request, host)
			if request.ToolStream != nil {
				t.Fatalf("host %q must not trigger tool_stream injection", host)
			}
		})
	}
}

func TestChatOutboundGenericGLMPreservesExplicitToolStream(t *testing.T) {
	stream := true
	for _, value := range []bool{true, false} {
		request := &model.InternalLLMRequest{
			Model:      "glm-5.2",
			Stream:     &stream,
			ToolStream: &value,
			Messages:   userMessages(),
			Tools: []model.Tool{{
				Type:     "function",
				Function: model.Function{Name: "ReadFile"},
			}},
		}
		payload := chatRequestBody(t, request)
		got, ok := payload["tool_stream"].(bool)
		if !ok || got != value {
			t.Fatalf("explicit tool_stream=%t on a generic GLM host must be preserved, got %#v", value, payload["tool_stream"])
		}
	}
}

func TestChatOutboundToolStreamingNativeThenGenericSameModelIsolation(t *testing.T) {
	// A native GLM attempt on api.z.ai auto-injects tool_stream, but a later
	// generic failover for the SAME GLM model must not inherit the synthetic
	// switch, and the shared retry request must remain untouched (ToolStream nil).
	request := streamedGLMToolRequest()

	nativePayload := chatRequestBodyWithBase(glmNativeHostZAI, t, request)
	if enabled, ok := nativePayload["tool_stream"].(bool); !ok || !enabled {
		t.Fatalf("native GLM attempt should enable tool_stream, got %#v", nativePayload["tool_stream"])
	}
	if request.ToolStream != nil {
		t.Fatalf("native projection leaked into the shared retry request: %#v", request.ToolStream)
	}

	genericPayload := chatRequestBody(t, request)
	if _, present := genericPayload["tool_stream"]; present {
		t.Fatalf("same-model generic retry must not emit a synthetic tool_stream, got %#v", genericPayload["tool_stream"])
	}
	if request.ToolStream != nil {
		t.Fatalf("generic retry must leave the shared request ToolStream nil, got %#v", request.ToolStream)
	}
}
