package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func chatBodyWithBase(t *testing.T, request *model.InternalLLMRequest, baseUrl string) map[string]any {
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

// TestApplyThirdPartyChatParamCompat verifies that ANY third-party
// OpenAI-compatible chat upstream (keyed on base URL, not a per-model allowlist)
// strips the OpenAI-only residue params (prompt_cache_key / prompt_cache_retention
// / safety_identifier / store), keeps a valid tool_choice, drops a malformed one,
// and that genuine api.openai.com keeps the params.
func TestApplyThirdPartyChatParamCompat(t *testing.T) {
	store := false
	pck := "cache-key-123"
	ret := "in_memory"
	sid := "safety-1"
	auto := "auto"

	newReq := func(m string) *model.InternalLLMRequest {
		return &model.InternalLLMRequest{
			Model: m, Messages: userMessages(),
			Store: &store, PromptCacheKey: &pck, PromptCacheRetention: &ret,
			SafetyIdentifier: &sid, ToolChoice: &model.ToolChoice{ToolChoice: &auto},
		}
	}

	// Third-party upstreams (non api.openai.com) strip the residue params,
	// regardless of model name — covers DeepSeek/GLM/Qwen/MiniMax/Kimi/Grok/...
	for _, m := range []string{"deepseek-v4-pro", "glm-5.1", "qwen-max", "mimo-7b", "kimi-k2", "grok-4"} {
		body := chatBodyWithBase(t, newReq(m), "https://third-party.example/v1")
		for _, k := range []string{"store", "prompt_cache_key", "prompt_cache_retention", "safety_identifier"} {
			if _, ok := body[k]; ok {
				t.Errorf("%s (third-party) must strip residue field %q, body=%v", m, k, body)
			}
		}
		if body["tool_choice"] != "auto" {
			t.Errorf("%s: valid tool_choice=auto must survive, got %v", m, body["tool_choice"])
		}
	}

	// A malformed tool_choice is dropped (would marshal to null / non-function
	// named / empty name and be rejected upstream).
	malformed := map[string]*model.ToolChoice{
		"empty":           {},
		"nonFunctionType": {NamedToolChoice: &model.NamedToolChoice{Type: "allowed_tools"}},
		"emptyName":       {NamedToolChoice: &model.NamedToolChoice{Type: "function"}},
	}
	for name, tc := range malformed {
		b := chatBodyWithBase(t, &model.InternalLLMRequest{
			Model: "deepseek-v4-pro", Messages: userMessages(), ToolChoice: tc,
		}, "https://third-party.example/v1")
		if _, ok := b["tool_choice"]; ok {
			t.Errorf("malformed tool_choice(%s) must be dropped, body=%v", name, b)
		}
	}

	// Genuine OpenAI (api.openai.com) keeps the residue params — it supports them.
	official := chatBodyWithBase(t, newReq("gpt-4o"), "https://api.openai.com/v1")
	for _, k := range []string{"prompt_cache_key", "safety_identifier"} {
		if _, ok := official[k]; !ok {
			t.Errorf("official api.openai.com must keep %q, body=%v", k, official)
		}
	}
}
