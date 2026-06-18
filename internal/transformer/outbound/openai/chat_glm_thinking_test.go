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
