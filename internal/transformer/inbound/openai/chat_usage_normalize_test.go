package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// anthropicSemanticUsage mirrors what outbound/authropic/messages.go convertAnthropicUsage
// produces for Anthropic usage {input_tokens:57, cache_read_input_tokens:20000,
// cache_creation_input_tokens:3000}: prompt_tokens excludes both cache figures (Anthropic
// semantics), the cache read lives in prompt_tokens_details.cached_tokens, the cache write in
// CacheCreationInputTokens, and AnthropicUsage marks the shape.
func anthropicSemanticUsage() *model.Usage {
	return &model.Usage{
		PromptTokens:             57,
		CompletionTokens:         100,
		TotalTokens:              57 + 100 + 20000 + 3000,
		CacheCreationInputTokens: 3000,
		AnthropicUsage:           true,
		PromptTokensDetails:      &model.PromptTokensDetails{CachedTokens: 20000},
	}
}

// openAISemanticUsage is the already-normalized shape a real OpenAI upstream sends:
// prompt_tokens already includes the cached tokens (23057 of which 20000 were cached).
func openAISemanticUsage() *model.Usage {
	return &model.Usage{
		PromptTokens:        23057,
		CompletionTokens:    100,
		TotalTokens:         23157,
		PromptTokensDetails: &model.PromptTokensDetails{CachedTokens: 20000},
	}
}

type chatWireUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func decodeChatWireUsage(t *testing.T, body []byte) chatWireUsage {
	t.Helper()
	var got struct {
		Usage *chatWireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode chat body: %v\n%s", err, body)
	}
	if got.Usage == nil {
		t.Fatalf("chat body carries no usage: %s", body)
	}
	return *got.Usage
}

func cachedTokensOf(u chatWireUsage) int64 {
	if u.PromptTokensDetails == nil {
		return -1
	}
	return u.PromptTokensDetails.CachedTokens
}

// TestChatTransformResponseIncludesAnthropicCacheInPromptTokens locks the OpenAI chat
// contract: prompt_tokens includes cached tokens. Anthropic-style usage must therefore be
// renormalized (57 + 20000 + 3000 = 23057) on the client-facing copy, while cached_tokens
// keeps reporting the cache read (20000) and i.storedResponse keeps the raw values so billing
// (relay/metrics.go usageCacheStats) does not count the cache twice.
func TestChatTransformResponseIncludesAnthropicCacheInPromptTokens(t *testing.T) {
	in := &ChatInbound{}
	resp := &model.InternalLLMResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Created: 1,
		Model:   "claude-x",
		Choices: []model.Choice{{
			Index:   0,
			Message: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr("hi")}},
		}},
		Usage: anthropicSemanticUsage(),
	}

	body, err := in.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}

	got := decodeChatWireUsage(t, body)
	if got.PromptTokens != 23057 {
		t.Fatalf("want prompt_tokens=23057 (input+cache_read+cache_creation), got %d\nbody=%s", got.PromptTokens, body)
	}
	if cached := cachedTokensOf(got); cached != 20000 {
		t.Fatalf("want prompt_tokens_details.cached_tokens=20000 preserved, got %d\nbody=%s", cached, body)
	}
	if got.CompletionTokens != 100 {
		t.Fatalf("want completion_tokens=100 untouched, got %d", got.CompletionTokens)
	}

	// Billing must keep the upstream-native usage.
	if in.storedResponse == nil || in.storedResponse.Usage == nil {
		t.Fatal("storedResponse usage lost")
	}
	stored := in.storedResponse.Usage
	if stored.PromptTokens != 57 {
		t.Fatalf("storedResponse.Usage.PromptTokens mutated: want 57, got %d", stored.PromptTokens)
	}
	if !stored.AnthropicUsage || stored.CacheCreationInputTokens != 3000 {
		t.Fatalf("storedResponse.Usage lost Anthropic markers: %#v", *stored)
	}
	if stored.PromptTokensDetails == nil || stored.PromptTokensDetails.CachedTokens != 20000 {
		t.Fatalf("storedResponse.Usage cached tokens mutated: %#v", *stored)
	}
	// The caller's own struct must not be touched either.
	if resp.Usage.PromptTokens != 57 {
		t.Fatalf("input response usage mutated: want 57, got %d", resp.Usage.PromptTokens)
	}
}

// TestChatTransformResponseKeepsOpenAISemanticUsage is the anti-regression guard: usage that
// already follows OpenAI semantics (no AnthropicUsage / SeparateCacheInputTokens marker) must
// pass through byte-identical, or a real OpenAI prompt_tokens would be inflated by its cache.
func TestChatTransformResponseKeepsOpenAISemanticUsage(t *testing.T) {
	in := &ChatInbound{}
	resp := &model.InternalLLMResponse{
		ID:      "chatcmpl-2",
		Object:  "chat.completion",
		Created: 1,
		Model:   "gpt-x",
		Choices: []model.Choice{{
			Index:   0,
			Message: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr("hi")}},
		}},
		Usage: openAISemanticUsage(),
	}

	body, err := in.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}

	got := decodeChatWireUsage(t, body)
	if got.PromptTokens != 23057 {
		t.Fatalf("OpenAI-semantics prompt_tokens must not be inflated: want 23057, got %d\nbody=%s", got.PromptTokens, body)
	}
	if cached := cachedTokensOf(got); cached != 20000 {
		t.Fatalf("want cached_tokens=20000 untouched, got %d", cached)
	}
	if in.storedResponse == nil || in.storedResponse.Usage == nil || in.storedResponse.Usage.PromptTokens != 23057 {
		t.Fatalf("storedResponse.Usage changed unexpectedly: %#v", in.storedResponse)
	}
}

// TestChatTransformResponseNormalizesSeparateCacheInputTokens covers the OpenAI-compatible
// proxy marker (no AnthropicUsage flag, but input_tokens still excludes cache).
func TestChatTransformResponseNormalizesSeparateCacheInputTokens(t *testing.T) {
	in := &ChatInbound{}
	usage := anthropicSemanticUsage()
	usage.AnthropicUsage = false
	usage.SeparateCacheInputTokens = true
	resp := &model.InternalLLMResponse{
		ID:     "chatcmpl-3",
		Object: "chat.completion",
		Choices: []model.Choice{{
			Index:   0,
			Message: &model.Message{Role: "assistant", Content: model.MessageContent{Content: ptr("hi")}},
		}},
		Usage: usage,
	}

	body, err := in.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	if got := decodeChatWireUsage(t, body); got.PromptTokens != 23057 {
		t.Fatalf("want prompt_tokens=23057 for SeparateCacheInputTokens usage, got %d\nbody=%s", got.PromptTokens, body)
	}
	if usage.PromptTokens != 57 {
		t.Fatalf("source usage mutated: want 57, got %d", usage.PromptTokens)
	}
}

// TestChatTransformStreamNormalizesUsageChunks checks every usage-bearing stream chunk (not
// just the last one) on the wire, while the chunks kept for aggregation keep raw numbers.
func TestChatTransformStreamNormalizesUsageChunks(t *testing.T) {
	in := &ChatInbound{}

	// usage rides on a mid-stream chunk here on purpose, not only the terminal one.
	chunks := []*model.InternalLLMResponse{
		{
			ID:      "chatcmpl-s1",
			Object:  "chat.completion.chunk",
			Model:   "claude-x",
			Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant"}}},
			Usage:   anthropicSemanticUsage(),
		},
		{
			ID:      "chatcmpl-s2",
			Object:  "chat.completion.chunk",
			Model:   "claude-x",
			Choices: []model.Choice{{Index: 0, Delta: &model.Message{}, FinishReason: ptr("stop")}},
			Usage:   anthropicSemanticUsage(),
		},
	}

	for idx, chunk := range chunks {
		body, err := in.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("TransformStream[%d]: %v", idx, err)
		}
		payload := strings.TrimSuffix(strings.TrimPrefix(string(body), "data: "), "\n\n")
		got := decodeChatWireUsage(t, []byte(payload))
		if got.PromptTokens != 23057 {
			t.Fatalf("chunk[%d] want prompt_tokens=23057 on the wire, got %d\nframe=%s", idx, got.PromptTokens, payload)
		}
		if cached := cachedTokensOf(got); cached != 20000 {
			t.Fatalf("chunk[%d] want cached_tokens=20000, got %d", idx, cached)
		}
	}

	// Stored chunks feed GetInternalResponse / billing and must stay raw.
	if len(in.streamChunks) != len(chunks) {
		t.Fatalf("want %d stored chunks, got %d", len(chunks), len(in.streamChunks))
	}
	for idx, stored := range in.streamChunks {
		if stored.Usage == nil || stored.Usage.PromptTokens != 57 {
			t.Fatalf("stored chunk[%d] usage mutated: %#v", idx, stored.Usage)
		}
		if !stored.Usage.AnthropicUsage || stored.Usage.CacheCreationInputTokens != 3000 {
			t.Fatalf("stored chunk[%d] lost Anthropic markers: %#v", idx, stored.Usage)
		}
	}
}

// TestChatTransformStreamKeepsOpenAISemanticUsage guards the stream path against inflating a
// genuine OpenAI prompt_tokens.
func TestChatTransformStreamKeepsOpenAISemanticUsage(t *testing.T) {
	in := &ChatInbound{}
	chunk := &model.InternalLLMResponse{
		ID:      "chatcmpl-s3",
		Object:  "chat.completion.chunk",
		Model:   "gpt-x",
		Choices: []model.Choice{{Index: 0, Delta: &model.Message{}, FinishReason: ptr("stop")}},
		Usage:   openAISemanticUsage(),
	}

	body, err := in.TransformStream(context.Background(), chunk)
	if err != nil {
		t.Fatalf("TransformStream: %v", err)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(string(body), "data: "), "\n\n")
	if got := decodeChatWireUsage(t, []byte(payload)); got.PromptTokens != 23057 {
		t.Fatalf("OpenAI-semantics chunk prompt_tokens must stay 23057, got %d\nframe=%s", got.PromptTokens, payload)
	}
}
