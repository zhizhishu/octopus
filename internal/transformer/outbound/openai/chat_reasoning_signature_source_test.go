package openai

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestChatDropsProviderTaggedReasoningSignature pins FIX A7: a provider-tagged
// (foreign) ReasoningSignature — Anthropic redacted, Gemini thoughtSignature, OpenAI
// encrypted_content — must never be serialized as the chat "reasoning_signature". A
// bare untagged signature (DeepSeek V4 / Anthropic thinking) is preserved as before.
func TestChatDropsProviderTaggedReasoningSignature(t *testing.T) {
	content := "answer"
	rawBody := func(sig string) string {
		t.Helper()
		s := sig
		httpReq, err := (&ChatOutbound{}).TransformRequest(context.Background(), &model.InternalLLMRequest{
			Model: "deepseek-reasoner",
			Messages: []model.Message{{
				Role:               "assistant",
				Content:            model.MessageContent{Content: &content},
				ReasoningSignature: &s,
			}},
		}, "https://api.example.com/v1", "key")
		if err != nil {
			t.Fatalf("TransformRequest: %v", err)
		}
		body, err := io.ReadAll(httpReq.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return string(body)
	}

	for _, tc := range []struct {
		name string
		sig  string
	}{
		{"gemini_tagged", model.TagGeminiThoughtSignature("g-raw")},
		{"redacted_tagged", model.EncodeRedactedThinkingSignature("REDACTED")},
		{"openai_enc_tagged", model.TagOpenAIEncryptedContent("enc-raw")},
	} {
		t.Run(tc.name+"_dropped", func(t *testing.T) {
			if body := rawBody(tc.sig); strings.Contains(body, "reasoning_signature") {
				t.Fatalf("provider-tagged signature must not be serialized, got %s", body)
			}
		})
	}

	t.Run("bare_preserved", func(t *testing.T) {
		if body := rawBody("bare-thinking-sig"); !strings.Contains(body, `"reasoning_signature":"bare-thinking-sig"`) {
			t.Fatalf("bare signature must be preserved, got %s", body)
		}
	})
}
