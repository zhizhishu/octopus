package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func strPtr(s string) *string { return &s }

func TestThinkingToContentNonStreamFoldsEmptyContent(t *testing.T) {
	reasoning := "I should say OK"
	resp := &model.InternalLLMResponse{
		ID:     "chatcmpl-test",
		Object: "chat.completion",
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role:             "assistant",
				ReasoningContent: &reasoning,
			},
		}},
	}
	out := applyThinkingToContentNonStream(resp, true)
	if out == nil || len(out.Choices) == 0 || out.Choices[0].Message == nil {
		t.Fatalf("expected rewritten response")
	}
	content := ""
	if out.Choices[0].Message.Content.Content != nil {
		content = *out.Choices[0].Message.Content.Content
	}
	if !strings.Contains(content, "<think>") || !strings.Contains(content, reasoning) {
		t.Fatalf("expected folded thinking in content, got %q", content)
	}
	if resp.Choices[0].Message.Content.Content != nil && *resp.Choices[0].Message.Content.Content != "" {
		t.Fatalf("original response must not be mutated")
	}
}

func TestThinkingToContentNonStreamSkipsWhenContentPresent(t *testing.T) {
	reasoning := "secret plan"
	text := "OK"
	resp := &model.InternalLLMResponse{
		Choices: []model.Choice{{
			Message: &model.Message{
				Role:             "assistant",
				Content:          model.MessageContent{Content: &text},
				ReasoningContent: &reasoning,
			},
		}},
	}
	out := applyThinkingToContentNonStream(resp, true)
	if out != resp {
		t.Fatalf("when content already present, response must be returned unchanged (no copy)")
	}
}

func TestThinkingToContentNonStreamDisabledNoop(t *testing.T) {
	reasoning := "only think"
	resp := &model.InternalLLMResponse{
		Choices: []model.Choice{{
			Message: &model.Message{Role: "assistant", ReasoningContent: &reasoning},
		}},
	}
	out := applyThinkingToContentNonStream(resp, false)
	if out != resp {
		t.Fatalf("disabled must be a no-op identity")
	}
}

func TestChatInboundTransformResponseThinkingToContent(t *testing.T) {
	inbound := &ChatInbound{thinkingToContent: true}
	reasoning := "fold me"
	resp := &model.InternalLLMResponse{
		ID: "x", Object: "chat.completion", Model: "glm-5.2",
		Choices: []model.Choice{{
			Message: &model.Message{Role: "assistant", ReasoningContent: &reasoning},
		}},
	}
	body, err := inbound.TransformResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var wire model.InternalLLMResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire.Choices[0].Message == nil || wire.Choices[0].Message.Content.Content == nil {
		t.Fatalf("wire content missing: %s", body)
	}
	if !strings.Contains(*wire.Choices[0].Message.Content.Content, "fold me") {
		t.Fatalf("wire content = %q", *wire.Choices[0].Message.Content.Content)
	}
}

func TestChatInboundTransformStreamThinkingToContent(t *testing.T) {
	inbound := &ChatInbound{thinkingToContent: true}
	plan := "plan A"
	chunk1 := &model.InternalLLMResponse{
		Object: "chat.completion.chunk",
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{ReasoningContent: &plan},
		}},
	}
	body1, err := inbound.TransformStream(context.Background(), chunk1)
	if err != nil {
		t.Fatalf("stream1: %v", err)
	}
	s1 := string(body1)
	// encoding/json escapes < as \u003c; either form is fine on the wire.
	if !(strings.Contains(s1, "<think>") || strings.Contains(s1, `\u003cthink\u003e`)) || !strings.Contains(s1, "plan A") {
		t.Fatalf("first thinking chunk should open <think>, got %s", s1)
	}
	raw1 := strings.TrimPrefix(strings.TrimSpace(s1), "data: ")
	raw1 = strings.TrimSuffix(raw1, "\n\n")
	var payload1 struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw1), &payload1); err != nil {
		t.Fatalf("unmarshal stream1: %v body=%s", err, raw1)
	}
	if payload1.Choices[0].Delta.ReasoningContent != nil {
		t.Fatalf("wire delta must clear reasoning_content")
	}

	ok := "OK"
	chunk2 := &model.InternalLLMResponse{
		Object: "chat.completion.chunk",
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{Content: model.MessageContent{Content: &ok}},
		}},
	}
	body2, err := inbound.TransformStream(context.Background(), chunk2)
	if err != nil {
		t.Fatalf("stream2: %v", err)
	}
	s2 := string(body2)
	if !(strings.Contains(s2, "</think>") || strings.Contains(s2, `\u003c/think\u003e`)) || !strings.Contains(s2, "OK") {
		t.Fatalf("content transition should close think and include OK, got %s", s2)
	}
	_ = strPtr
}
