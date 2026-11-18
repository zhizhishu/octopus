package openai

import (
	"context"
	"encoding/json"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"io"
	"strings"
	"testing"
)

func TestGPT62OfficialChatWireAndRestore(t *testing.T) {
	for _, id := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		t.Run(id, func(t *testing.T) {
			sys, user := "behave", "hello"
			limit := int64(512)
			topLogs := int64(4)
			temp, p := 0.7, 0.9
			logprobs := true
			oldCache := "24h"
			source := &model.InternalLLMRequest{
				Model: id, MaxTokens: &limit, Temperature: &temp, TopP: &p, TopLogprobs: &topLogs, Logprobs: &logprobs,
				ReasoningEffort: "none", PromptCacheRetention: &oldCache,
				Messages: []model.Message{{Role: "system", Content: model.MessageContent{Content: &sys}}, {Role: "user", Content: model.MessageContent{Content: &user}}},
			}
			body := chatBodyWithBase(t, source, "https://api.openai.com/v1")
			for _, field := range []string{"max_tokens", "temperature", "top_p", "logprobs", "top_logprobs", "prompt_cache_retention"} {
				if _, ok := body[field]; ok {
					t.Errorf("%s unexpectedly present in %v", field, body)
				}
			}
			if body["max_completion_tokens"] != float64(limit) || body["reasoning_effort"] != "low" {
				t.Fatalf("bad modern Chat fields %#v", body)
			}
			cached, ok := body["prompt_cache_options"].(map[string]any)
			if !ok || cached["ttl"] != "30m" {
				t.Fatalf("cache migration missing: %#v", body)
			}
			roles := body["messages"].([]any)
			if roles[0].(map[string]any)["role"] != "developer" {
				t.Fatalf("system not promoted: %#v", roles[0])
			}
			if source.TopP == nil || source.Logprobs == nil || source.TopLogprobs == nil || source.ReasoningEffort != "none" || source.PromptCacheRetention == nil || len(source.PromptCacheOptions) > 0 {
				t.Fatalf("Chat projection leaked into shared request across attempts: %#v", source)
			}
		})
	}
}

func TestGPT62OfficialChatToolsNeedResponses(t *testing.T) {
	text := "use tool"
	source := &model.InternalLLMRequest{Model: "gpt-6.2-astra",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
		Tools:    []model.Tool{{Type: "function", Function: model.Function{Name: "lookup"}}},
	}
	_, err := (&ChatOutbound{}).TransformRequest(context.Background(), source, "https://api.openai.com/v1", "dummy")
	if err == nil || !strings.Contains(err.Error(), "Responses-type") {
		t.Fatalf("expected actionable upstream protocol error; got %v", err)
	}
}

func TestGPT62ThirdPartyChatKeepsProviderContract(t *testing.T) {
	text := "hi"
	temp, p := 0.7, 0.9
	max := int64(123)
	body := chatBodyWithBase(t, &model.InternalLLMRequest{
		Model: "gpt-6.2-sol", MaxTokens: &max, Temperature: &temp, TopP: &p, ReasoningEffort: "high",
		Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
	}, "https://third-party.example/v1")
	if body["temperature"] != temp || body["top_p"] != p || body["max_tokens"] != float64(max) {
		t.Fatalf("third-party compatibility changed: %#v", body)
	}
}

func TestGPT62OfficialResponsesModernFields(t *testing.T) {
	for _, id := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		t.Run(id, func(t *testing.T) {
			text := "hello"
			temp, p := 0.6, 0.7
			cache := "24h"
			request := &model.InternalLLMRequest{Model: id,
				Messages:    []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
				Temperature: &temp, TopP: &p, ReasoningEffort: "none", PromptCacheRetention: &cache,
				Include: []string{"reasoning.encrypted_content", "message.output_text.logprobs"},
			}
			outbound, err := (&ResponseOutbound{}).TransformRequest(context.Background(), request, "https://api.openai.com/v1", "dummy")
			if err != nil {
				t.Fatal(err)
			}
			bytes, err := io.ReadAll(outbound.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err = json.Unmarshal(bytes, &payload); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"temperature", "top_p", "prompt_cache_retention"} {
				if _, ok := payload[key]; ok {
					t.Fatalf("%s present: %s", key, bytes)
				}
			}
			reason := payload["reasoning"].(map[string]any)
			if reason["effort"] != "low" {
				t.Fatalf("effort not normalized %v", reason)
			}
			opts := payload["prompt_cache_options"].(map[string]any)
			if opts["ttl"] != "30m" {
				t.Fatalf("cache migration: %#v", payload)
			}
			include := payload["include"].([]any)
			if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
				t.Fatalf("include not normalized: %v", include)
			}
			if request.ReasoningEffort != "none" || len(request.Include) != 2 {
				t.Fatal("raw input changed")
			}
		})
	}
}

func TestGPT62ThirdPartyResponsesRawOptionsStayUntouched(t *testing.T) {
	text := "hello"
	temp, p := 0.6, 0.7
	options := json.RawMessage(`{"ttl":"12h"}`)
	request := &model.InternalLLMRequest{Model: "gpt-6.2-sol", Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}}, Temperature: &temp, TopP: &p, ReasoningEffort: "none", PromptCacheOptions: options, Include: []string{"message.output_text.logprobs"}}
	out, err := (&ResponseOutbound{}).TransformRequest(context.Background(), request, "https://third-party.example/v1", "dummy")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["temperature"] != temp || payload["top_p"] != p || payload["reasoning"].(map[string]any)["effort"] != "none" {
		t.Fatalf("third-party wire altered: %s", body)
	}
	if payload["prompt_cache_options"].(map[string]any)["ttl"] != "12h" {
		t.Fatalf("opaque cache options lost: %s", body)
	}
}
