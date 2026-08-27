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

// TestGPT5ChatKeepsSamplingParamsOnOfficialBase pins FIX E end-to-end: on the genuine
// OpenAI base a gpt-5-chat model keeps temperature and max_tokens (no reasoning rewrite),
// while a genuine reasoning model (gpt-5) still migrates max_tokens -> max_completion_tokens
// and drops temperature.
func TestGPT5ChatKeepsSamplingParamsOnOfficialBase(t *testing.T) {
	maxTokens := int64(100)
	temp := 0.7
	user := "hi"
	build := func(modelName string) *model.InternalLLMRequest {
		m := maxTokens
		tp := temp
		return &model.InternalLLMRequest{
			Model:       modelName,
			MaxTokens:   &m,
			Temperature: &tp,
			Messages:    []model.Message{{Role: "user", Content: model.MessageContent{Content: &user}}},
		}
	}

	for _, modelName := range []string{"gpt-5-chat", "gpt-5-chat-latest", "gpt-5.1-chat-latest"} {
		t.Run(modelName+"_keeps_sampling", func(t *testing.T) {
			payload := chatBodyWithBase(t, build(modelName), "https://api.openai.com/v1")
			if v, ok := payload["temperature"].(float64); !ok || v != temp {
				t.Fatalf("%s must keep temperature, body=%v", modelName, payload)
			}
			if v, ok := payload["max_tokens"].(float64); !ok || int64(v) != maxTokens {
				t.Fatalf("%s must keep max_tokens, body=%v", modelName, payload)
			}
			if _, ok := payload["max_completion_tokens"]; ok {
				t.Fatalf("%s must not convert to max_completion_tokens, body=%v", modelName, payload)
			}
		})
	}

	t.Run("gpt-5_still_rewritten", func(t *testing.T) {
		payload := chatBodyWithBase(t, build("gpt-5"), "https://api.openai.com/v1")
		if _, ok := payload["temperature"]; ok {
			t.Fatalf("gpt-5 must drop temperature, body=%v", payload)
		}
		if _, ok := payload["max_tokens"]; ok {
			t.Fatalf("gpt-5 must migrate off max_tokens, body=%v", payload)
		}
		if v, ok := payload["max_completion_tokens"].(float64); !ok || int64(v) != maxTokens {
			t.Fatalf("gpt-5 must use max_completion_tokens, body=%v", payload)
		}
	})
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

func TestStreamOptionsIncludeUsageForcedForAllOpenAIChatStreams(t *testing.T) {
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

	// Every streaming OpenAI-compatible chat base (official and third-party) must
	// force stream_options.include_usage=true so the upstream attaches aggregate
	// usage (cached tokens included) to a trailing chunk instead of ending the
	// stream usage-less.
	for _, base := range []string{
		"https://api.openai.com/v1",
		"https://third-party.example/v1",
		"https://compat.proxy.example/v1",
	} {
		body := chatBodyWithBase(t, baseReq(), base)
		so, ok := body["stream_options"].(map[string]any)
		if !ok || so["include_usage"] != true {
			t.Fatalf("streaming chat via %s must force stream_options.include_usage=true, body=%v", base, body)
		}
	}

	// A client-provided stream_options with include_usage=false is still upgraded
	// so upstream usage is never silently lost.
	falseUsage := &model.StreamOptions{IncludeUsage: false}
	reqUpgrade := baseReq()
	reqUpgrade.StreamOptions = falseUsage
	upgraded := chatBodyWithBase(t, reqUpgrade, "https://third-party.example/v1")
	so2, ok := upgraded["stream_options"].(map[string]any)
	if !ok || so2["include_usage"] != true {
		t.Fatalf("client include_usage=false must be upgraded to true, got %#v", upgraded["stream_options"])
	}
}

func TestIsOpenAIReasoningChatModelHelpers(t *testing.T) {
	// Unit-level classification guards so the transform gate stays stable.
	yes := []string{"o1", "o1-mini", "o3-mini", "o4-mini-2025-04-16", "gpt-5", "gpt-5-mini", "openai/gpt-5-pro"}
	// FIX E: the gpt-5-chat family are non-reasoning chat models and must classify as NO.
	no := []string{"gpt-4o", "gpt-4.1", "deepseek-chat", "glm-4.6", "", "claude-sonnet-4",
		"gpt-5-chat", "gpt-5-chat-latest", "gpt-5.1-chat-latest", "openai/gpt-5-chat-latest"}
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
