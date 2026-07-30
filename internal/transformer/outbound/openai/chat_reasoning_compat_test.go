package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestApplyOpenAIReasoningChatCompatMaxTokensAndTemperature(t *testing.T) {
	maxTokens := int64(512)
	temp := 0.7
	sys := "you are helpful"
	user := "hi"

	for _, modelName := range []string{"o3-mini", "o1", "o4-mini", "gpt-5", "gpt-5-mini", "openai/o3"} {
		payload := chatBodyWithBase(t, &model.InternalLLMRequest{
			Model:       modelName,
			MaxTokens:   &maxTokens,
			Temperature: &temp,
			Messages: []model.Message{
				{Role: "system", Content: model.MessageContent{Content: &sys}},
				{Role: "user", Content: model.MessageContent{Content: &user}},
			},
		}, "https://api.openai.com/v1")

		if _, ok := payload["max_tokens"]; ok {
			t.Errorf("%s: max_tokens must be dropped, body=%v", modelName, payload)
		}
		if v, ok := payload["max_completion_tokens"]; !ok {
			t.Errorf("%s: expected max_completion_tokens, body=%v", modelName, payload)
		} else if n, ok := v.(float64); !ok || int64(n) != maxTokens {
			t.Errorf("%s: max_completion_tokens want %d, got %#v", modelName, maxTokens, v)
		}
		if _, ok := payload["temperature"]; ok {
			t.Errorf("%s: temperature must be dropped, body=%v", modelName, payload)
		}
	}
}

func TestApplyOpenAIReasoningChatCompatSystemToDeveloper(t *testing.T) {
	sys := "instructions"
	user := "hi"
	maxTokens := int64(64)

	// o3 / gpt-5: first system -> developer
	for _, modelName := range []string{"o3", "o4-mini", "gpt-5", "gpt-5-pro"} {
		payload := chatBodyWithBase(t, &model.InternalLLMRequest{
			Model:     modelName,
			MaxTokens: &maxTokens,
			Messages: []model.Message{
				{Role: "system", Content: model.MessageContent{Content: &sys}},
				{Role: "user", Content: model.MessageContent{Content: &user}},
			},
		}, "https://api.openai.com/v1")

		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 1 {
			t.Fatalf("%s: expected messages in body, got %#v", modelName, payload)
		}
		first, _ := msgs[0].(map[string]any)
		if first["role"] != "developer" {
			t.Errorf("%s: first system message must become developer, got %#v", modelName, first)
		}
	}

	// o1-mini / o1-preview keep system (do not promote to developer)
	for _, modelName := range []string{"o1-mini", "o1-preview", "o1-mini-2024-09-12"} {
		payload := chatBodyWithBase(t, &model.InternalLLMRequest{
			Model:     modelName,
			MaxTokens: &maxTokens,
			Messages: []model.Message{
				{Role: "system", Content: model.MessageContent{Content: &sys}},
				{Role: "user", Content: model.MessageContent{Content: &user}},
			},
		}, "https://api.openai.com/v1")

		msgs, ok := payload["messages"].([]any)
		if !ok || len(msgs) < 1 {
			t.Fatalf("%s: expected messages in body, got %#v", modelName, payload)
		}
		first, _ := msgs[0].(map[string]any)
		if first["role"] != "system" {
			t.Errorf("%s: system role must stay system, got %#v", modelName, first)
		}
	}
}

func TestNonReasoningChatStillDemotesDeveloperToSystem(t *testing.T) {
	dev := "instructions"
	user := "hi"
	payload := chatBodyWithBase(t, &model.InternalLLMRequest{
		Model: "gpt-4o",
		Messages: []model.Message{
			{Role: "developer", Content: model.MessageContent{Content: &dev}},
			{Role: "user", Content: model.MessageContent{Content: &user}},
		},
	}, "https://api.openai.com/v1")

	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) < 1 {
		t.Fatalf("expected messages in body, got %#v", payload)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("non-reasoning models must demote developer->system, got %#v", first)
	}
}

func TestStreamOptionsIncludeUsageOnlyForcedForOfficialOpenAI(t *testing.T) {
	stream := true
	user := "hi"
	baseReq := func() *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model:  "gpt-4o",
			Stream: &stream,
			Messages: []model.Message{
				{Role: "user", Content: model.MessageContent{Content: &user}},
			},
		}
	}

	// Genuine OpenAI base: inject stream_options.include_usage when missing.
	official := chatBodyWithBase(t, baseReq(), "https://api.openai.com/v1")
	so, ok := official["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("official base must inject stream_options, body=%v", official)
	}
	if so["include_usage"] != true {
		t.Fatalf("official base must force include_usage=true, got %#v", so)
	}

	// Official base also upgrades a client-provided stream_options with include_usage=false.
	falseUsage := &model.StreamOptions{IncludeUsage: false}
	reqUpgrade := baseReq()
	reqUpgrade.StreamOptions = falseUsage
	upgraded := chatBodyWithBase(t, reqUpgrade, "https://api.openai.com/v1")
	so2, ok := upgraded["stream_options"].(map[string]any)
	if !ok || so2["include_usage"] != true {
		t.Fatalf("official base must force include_usage=true on client false, got %#v", upgraded["stream_options"])
	}

	// Third-party OpenAI-compatible base: do NOT force inject stream_options.
	third := chatBodyWithBase(t, baseReq(), "https://third-party.example/v1")
	if _, ok := third["stream_options"]; ok {
		t.Fatalf("third-party base must not inject stream_options, body=%v", third)
	}

	// Third-party base: respect client-provided stream_options as-is (no force true).
	clientSO := &model.StreamOptions{IncludeUsage: false}
	reqClient := baseReq()
	reqClient.StreamOptions = clientSO
	thirdClient := chatBodyWithBase(t, reqClient, "https://compat.proxy.example/v1")
	so3, ok := thirdClient["stream_options"].(map[string]any)
	if !ok {
		// include_usage=false with omitempty may drop the whole object; either absence
		// or an object that does not force true is acceptable. Force-true is the bug.
		return
	}
	if so3["include_usage"] == true {
		t.Fatalf("third-party base must not force include_usage=true, got %#v", so3)
	}
}

func TestIsOpenAIReasoningChatModelHelpers(t *testing.T) {
	// Unit-level classification guards so the transform gate stays stable.
	yes := []string{"o1", "o1-mini", "o3-mini", "o4-mini-2025-04-16", "gpt-5", "gpt-5-mini", "openai/gpt-5-pro"}
	no := []string{"gpt-4o", "gpt-4.1", "deepseek-chat", "glm-4.6", "", "claude-sonnet-4"}
	for _, m := range yes {
		if !isOpenAIReasoningChatModel(m) {
			t.Errorf("expected reasoning chat model: %q", m)
		}
	}
	for _, m := range no {
		if isOpenAIReasoningChatModel(m) {
			t.Errorf("did not expect reasoning chat model: %q", m)
		}
	}
	if openAIReasoningUsesDeveloperRole("o1-mini") {
		t.Error("o1-mini must not use developer role")
	}
	if openAIReasoningUsesDeveloperRole("o1-preview") {
		t.Error("o1-preview must not use developer role")
	}
	if !openAIReasoningUsesDeveloperRole("o3") {
		t.Error("o3 must use developer role")
	}
	if !openAIReasoningUsesDeveloperRole("gpt-5") {
		t.Error("gpt-5 must use developer role")
	}
}

// Ensure the transform path used by CustomChatOutbound also receives the
// reasoning adaptation (same transformChatRequest entrypoint).
func TestCustomChatOutboundAppliesReasoningCompat(t *testing.T) {
	maxTokens := int64(128)
	temp := 1.0
	sys := "sys"
	user := "u"
	req, err := (&CustomChatOutbound{}).TransformRequest(context.Background(), &model.InternalLLMRequest{
		Model:       "o3-mini",
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: &sys}},
			{Role: "user", Content: model.MessageContent{Content: &user}},
		},
	}, "https://api.openai.com/v1", "key")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := payload["max_tokens"]; ok {
		t.Fatalf("custom chat path must drop max_tokens: %v", payload)
	}
	if _, ok := payload["temperature"]; ok {
		t.Fatalf("custom chat path must drop temperature: %v", payload)
	}
	if v, ok := payload["max_completion_tokens"].(float64); !ok || int64(v) != maxTokens {
		t.Fatalf("custom chat path must set max_completion_tokens=%d, got %#v", maxTokens, payload["max_completion_tokens"])
	}
}
