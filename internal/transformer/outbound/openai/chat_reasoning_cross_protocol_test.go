package openai

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestChatDropsProviderTaggedReasoningContent locks the "gemini cross-protocol history
// bridge" fix (bug B): when an assistant history message carries a foreign provider
// tag on its ReasoningSignature (gemini thoughtSignature / anthropic redacted / openai
// encrypted_content), the ReasoningContent next to it was produced by a different
// upstream and must NOT be serialized into the chat body's "reasoning_content" field
// — strict OpenAI-compatible upstreams (vercel/z.ai/GLM) reject it and a model may
// mis-attribute it as live reasoning. DeepSeek V4 keeps its own reasoning_content
// because DeepSeek signs it with a bare (untagged) signature, so the gate leaves it
// untouched. Mirrors the contract in TestChatDropsProviderTaggedReasoningSignature.
func TestChatDropsProviderTaggedReasoningContent(t *testing.T) {
	content := "answer text"
	thinking := "previous reasoning from a foreign upstream"
	rawBody := func(sig string) string {
		t.Helper()
		s := sig
		rc := thinking
		httpReq, err := (&ChatOutbound{}).TransformRequest(context.Background(), &model.InternalLLMRequest{
			Model: "deepseek-reasoner",
			Messages: []model.Message{{
				Role:               "assistant",
				Content:            model.MessageContent{Content: &content},
				ReasoningContent:   &rc,
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
		t.Run(tc.name+"_reasoning_content_dropped", func(t *testing.T) {
			body := rawBody(tc.sig)
			if strings.Contains(body, "reasoning_content") {
				t.Fatalf("foreign-tagged ReasoningContent must not be serialized as reasoning_content, got %s", body)
			}
			if strings.Contains(body, thinking) {
				t.Fatalf("foreign-tagged thinking text must not appear anywhere in the body, got %s", body)
			}
		})
	}

	t.Run("bare_signature_keeps_reasoning_content", func(t *testing.T) {
		// A bare untagged signature (DeepSeek V4 / Anthropic thinking) keeps its
		// reasoning_content — DeepSeek V4 multi-turn REQUIRES it on tool-call assistant
		// history. The chat save/restore loop preserves it for the genuine same-upstream
		// signature, so the body must contain reasoning_content with the thinking text.
		body := rawBody("bare-thinking-sig")
		if !strings.Contains(body, `"reasoning_content":"`+thinking+`"`) {
			t.Fatalf("bare signature must keep reasoning_content, got %s", body)
		}
	})

	t.Run("nil_signature_keeps_reasoning_content", func(t *testing.T) {
		// A nil signature means there is no foreign tag to gate on — the reasoning_content
		// stays so an inbound OpenAI response (no signature) can round-trip via chat.
		rc := thinking
		httpReq, err := (&ChatOutbound{}).TransformRequest(context.Background(), &model.InternalLLMRequest{
			Model: "deepseek-reasoner",
			Messages: []model.Message{{
				Role:             "assistant",
				Content:          model.MessageContent{Content: &content},
				ReasoningContent: &rc,
				// ReasoningSignature intentionally nil
			}},
		}, "https://api.example.com/v1", "key")
		if err != nil {
			t.Fatalf("TransformRequest: %v", err)
		}
		body, err := io.ReadAll(httpReq.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodyStr := string(body)
		if !strings.Contains(bodyStr, `"reasoning_content":"`+thinking+`"`) {
			t.Fatalf("nil signature must keep reasoning_content, got %s", bodyStr)
		}
	})
}
