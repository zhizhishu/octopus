package gemini

import (
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

// P0-1: a single Gemini candidate carrying multiple thought:true parts in one
// chunk must NOT lose every thought after the first. The prior
// `reasoningContent == nil` guard dropped all but the first segment, which
// surfaced as a truncated ("4-char") reasoningContent for Gemini 2.5 thinking
// summaries that emit multiple thought parts per chunk. This test asserts the
// full concatenation is preserved for both the non-stream and stream paths.
func TestConvertGeminiMultipleThoughtPartsInOneChunkNotLost(t *testing.T) {
	// Three thought segments in one candidate, in order.
	geminiResp := &transformerModel.GeminiGenerateContentResponse{
		Candidates: []*transformerModel.GeminiCandidate{{
			Index: 0,
			Content: &transformerModel.GeminiContent{
				Role: "model",
				Parts: []*transformerModel.GeminiPart{
					{Text: "第一段", Thought: true},
					{Text: "第二段", Thought: true},
					{Text: "第三段", Thought: true},
				},
			},
		}},
	}

	// Non-stream path: choices[*].Message.ReasoningContent must carry all three.
	t.Run("non_stream", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(geminiResp, false)
		if len(resp.Choices) != 1 {
			t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
		}
		if resp.Choices[0].Message == nil {
			t.Fatalf("expected non-nil Message")
		}
		rc := resp.Choices[0].Message.ReasoningContent
		if rc == nil {
			t.Fatalf("expected non-nil ReasoningContent (all thoughts dropped)")
		}
		for _, want := range []string{"第一段", "第二段", "第三段"} {
			if !strings.Contains(*rc, want) {
				t.Fatalf("ReasoningContent=%q missing segment %q", *rc, want)
			}
		}
		// Sanity: order preserved as a single concatenation.
		if got := *rc; got != "第一段第二段第三段" {
			t.Fatalf("expected concatenated reasoning %q, got %q", "第一段第二段第三段", got)
		}
	})

	// Stream path: choices[*].Delta.ReasoningContent must carry all three too.
	t.Run("stream", func(t *testing.T) {
		resp := convertGeminiToLLMResponse(geminiResp, true)
		if len(resp.Choices) != 1 {
			t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
		}
		if resp.Choices[0].Delta == nil {
			t.Fatalf("expected non-nil Delta")
		}
		rc := resp.Choices[0].Delta.ReasoningContent
		if rc == nil {
			t.Fatalf("expected non-nil ReasoningContent (all thoughts dropped)")
		}
		for _, want := range []string{"第一段", "第二段", "第三段"} {
			if !strings.Contains(*rc, want) {
				t.Fatalf("Delta.ReasoningContent=%q missing segment %q", *rc, want)
			}
		}
		if got := *rc; got != "第一段第二段第三段" {
			t.Fatalf("expected concatenated reasoning %q, got %q", "第一段第二段第三段", got)
		}
	})
}

// P0-1 regression guard: a single thought part still works (the new builder
// path must not regress the simple case).
func TestConvertGeminiSingleThoughtPartPreserved(t *testing.T) {
	geminiResp := &transformerModel.GeminiGenerateContentResponse{
		Candidates: []*transformerModel.GeminiCandidate{{
			Index: 0,
			Content: &transformerModel.GeminiContent{
				Role: "model",
				Parts: []*transformerModel.GeminiPart{
					{Text: "only one thought", Thought: true},
					{Text: "answer text"},
				},
			},
		}},
	}
	resp := convertGeminiToLLMResponse(geminiResp, false)
	if resp.Choices[0].Message == nil || resp.Choices[0].Message.ReasoningContent == nil {
		t.Fatalf("expected non-nil ReasoningContent for single thought part")
	}
	if got := *resp.Choices[0].Message.ReasoningContent; got != "only one thought" {
		t.Fatalf("expected single thought preserved, got %q", got)
	}
	if resp.Choices[0].Message.Content.Content == nil || *resp.Choices[0].Message.Content.Content != "answer text" {
		t.Fatalf("expected answer text preserved, got %#v", resp.Choices[0].Message.Content)
	}
}

// P0-1 regression guard: an empty-thought chunk (no thought parts at all) must
// leave ReasoningContent nil (the builder is empty), so the downstream consumer
// skips the reasoning assignment entirely.
func TestConvertGeminiNoThoughtPartLeavesReasoningNil(t *testing.T) {
	geminiResp := &transformerModel.GeminiGenerateContentResponse{
		Candidates: []*transformerModel.GeminiCandidate{{
			Index: 0,
			Content: &transformerModel.GeminiContent{
				Role: "model",
				Parts: []*transformerModel.GeminiPart{
					{Text: "no thoughts here"},
				},
			},
		}},
	}
	resp := convertGeminiToLLMResponse(geminiResp, false)
	if resp.Choices[0].Message == nil {
		t.Fatalf("expected non-nil Message")
	}
	if resp.Choices[0].Message.ReasoningContent != nil {
		t.Fatalf("expected nil ReasoningContent when no thought parts, got %q", *resp.Choices[0].Message.ReasoningContent)
	}
}
