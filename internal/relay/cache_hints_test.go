package relay

import (
	"strings"
	"testing"

	llmmodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestApplyOpenAIAutoPromptCacheKeyStableAcrossLaterTurns(t *testing.T) {
	base := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "system", Content: textMessageContent("You are helpful.")},
			{Role: "user", Content: textMessageContent("Open the repo and inspect cache.")},
		},
	}
	extended := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "system", Content: textMessageContent("You are helpful.")},
			{Role: "user", Content: textMessageContent("Open the repo and inspect cache.")},
			{Role: "assistant", Content: textMessageContent("I inspected it.")},
			{Role: "user", Content: textMessageContent("Now improve it.")},
		},
	}

	applyOpenAIAutoPromptCacheKey(base, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "glm-5.1", true)
	applyOpenAIAutoPromptCacheKey(extended, outbound.OutboundTypeOpenAIResponse, 7, 11, 0, "glm-5.1", true)

	if base.PromptCacheKey == nil || extended.PromptCacheKey == nil {
		t.Fatalf("expected prompt cache keys to be injected")
	}
	if *base.PromptCacheKey != *extended.PromptCacheKey {
		t.Fatalf("expected stable key across later turns, got %q and %q", *base.PromptCacheKey, *extended.PromptCacheKey)
	}
	if !strings.HasPrefix(*base.PromptCacheKey, autoPromptCacheKeyPrefix) {
		t.Fatalf("unexpected key prefix: %q", *base.PromptCacheKey)
	}
}

func TestApplyOpenAIAutoPromptCacheKeyDoesNotOverwriteClientValue(t *testing.T) {
	clientKey := "client-owned-key"
	req := &llmmodel.InternalLLMRequest{
		Model:          "gpt-5.4",
		PromptCacheKey: &clientKey,
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("hello")},
		},
	}

	applyOpenAIAutoPromptCacheKey(req, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "gpt-5.4", true)

	if req.PromptCacheKey == nil || *req.PromptCacheKey != clientKey {
		t.Fatalf("client prompt_cache_key should win, got %#v", req.PromptCacheKey)
	}
}

func TestApplyOpenAIAutoPromptCacheKeySupportsCustomOpenAIChat(t *testing.T) {
	req := &llmmodel.InternalLLMRequest{
		Model: "glm-upstream",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("hello custom openai")},
		},
	}

	applyOpenAIAutoPromptCacheKey(req, outbound.OutboundTypeCustomOpenAIChat, 7, 11, 0, "glm-request", true)

	if req.PromptCacheKey == nil || !strings.HasPrefix(*req.PromptCacheKey, autoPromptCacheKeyPrefix) {
		t.Fatalf("expected prompt cache key for custom OpenAI chat, got %#v", req.PromptCacheKey)
	}
}

func TestApplyOpenAIAutoPromptCacheKeyIsolationInputs(t *testing.T) {
	reqA := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("same first message")},
		},
	}
	reqB := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("same first message")},
		},
	}
	reqC := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("same first message")},
		},
	}

	reqD := &llmmodel.InternalLLMRequest{
		Model: "upstream-gpt",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("same first message")},
		},
	}

	applyOpenAIAutoPromptCacheKey(reqA, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "glm-5.1", true)
	applyOpenAIAutoPromptCacheKey(reqB, outbound.OutboundTypeOpenAIChat, 8, 11, 0, "glm-5.1", true)
	applyOpenAIAutoPromptCacheKey(reqC, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "glm-4.5", true)
	// reqD: identical user / api-key / model / content as reqA but a DIFFERENT
	// fingerprint profile (ProfileID 2). It MUST get an isolated key, so two channels
	// presenting different device identities never share one prompt_cache_key.
	applyOpenAIAutoPromptCacheKey(reqD, outbound.OutboundTypeOpenAIChat, 7, 11, 2, "glm-5.1", true)

	if reqA.PromptCacheKey == nil || reqB.PromptCacheKey == nil || reqC.PromptCacheKey == nil || reqD.PromptCacheKey == nil {
		t.Fatalf("expected prompt cache keys to be injected")
	}
	if *reqA.PromptCacheKey == *reqB.PromptCacheKey {
		t.Fatalf("different users should get isolated cache keys")
	}
	if *reqA.PromptCacheKey == *reqC.PromptCacheKey {
		t.Fatalf("different request models should get isolated cache keys")
	}
	if *reqA.PromptCacheKey == *reqD.PromptCacheKey {
		t.Fatalf("different fingerprint profiles should get isolated cache keys")
	}
}

func TestApplyOpenAIAutoPromptCacheKeySkipsUnsafeCases(t *testing.T) {
	reqDisabled := &llmmodel.InternalLLMRequest{
		Model: "gpt-5.4",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("hello")},
		},
	}
	applyOpenAIAutoPromptCacheKey(reqDisabled, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "gpt-5.4", false)
	if reqDisabled.PromptCacheKey != nil {
		t.Fatalf("disabled setting should skip injection")
	}

	reqAnthropic := &llmmodel.InternalLLMRequest{
		Model: "claude",
		Messages: []llmmodel.Message{
			{Role: "user", Content: textMessageContent("hello")},
		},
	}
	applyOpenAIAutoPromptCacheKey(reqAnthropic, outbound.OutboundTypeAnthropic, 7, 11, 0, "claude", true)
	if reqAnthropic.PromptCacheKey != nil {
		t.Fatalf("non-OpenAI channel should skip injection")
	}

	reqWithoutAnchor := &llmmodel.InternalLLMRequest{
		Model: "gpt-5.4",
		Messages: []llmmodel.Message{
			{Role: "assistant", Content: textMessageContent("hello")},
		},
	}
	applyOpenAIAutoPromptCacheKey(reqWithoutAnchor, outbound.OutboundTypeOpenAIChat, 7, 11, 0, "gpt-5.4", true)
	if reqWithoutAnchor.PromptCacheKey != nil {
		t.Fatalf("requests without stable prefix anchors should skip injection")
	}
}

func textMessageContent(value string) llmmodel.MessageContent {
	return llmmodel.MessageContent{Content: &value}
}
