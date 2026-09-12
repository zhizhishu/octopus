package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestApplyThinkingToContentStream_CloseOnFinish verifies D2 fix: when reasoning
// ends without subsequent content (DeepSeek tool_calls, abrupt stop), the </think>
// tag is closed at finish_reason to prevent thinking/text mixing.
func TestApplyThinkingToContentStream_CloseOnFinish(t *testing.T) {
	tests := []struct {
		name     string
		chunks   []*model.InternalLLMResponse
		wantLast string // expected content in the last meaningful delta
	}{
		{
			name: "reasoning_then_tool_call_no_text",
			chunks: []*model.InternalLLMResponse{
				// Chunk 1: reasoning starts
				{
					Choices: []model.Choice{
						{
							Index: 0,
							Delta: &model.Message{
								ReasoningContent: ptrStr("I need to call the search tool"),
							},
						},
					},
				},
				// Chunk 2: tool_call appears with finish_reason, no content
				{
					Choices: []model.Choice{
						{
							Index: 0,
							Delta: &model.Message{
								ToolCalls: []model.ToolCall{
									{Index: 0, ID: "call_123", Type: "function"},
								},
							},
							FinishReason: ptrStr("tool_calls"),
						},
					},
				},
			},
			wantLast: "\n</think>\n",
		},
		{
			name: "reasoning_then_stop_no_text",
			chunks: []*model.InternalLLMResponse{
				// Chunk 1: reasoning
				{
					Choices: []model.Choice{
						{
							Index: 0,
							Delta: &model.Message{
								ReasoningContent: ptrStr("Thinking about this..."),
							},
						},
					},
				},
				// Chunk 2: abrupt stop with finish_reason
				{
					Choices: []model.Choice{
						{
							Index:        0,
							Delta:        &model.Message{},
							FinishReason: ptrStr("stop"),
						},
					},
				},
			},
			wantLast: "\n</think>\n",
		},
		{
			name: "reasoning_then_content_normal_close",
			chunks: []*model.InternalLLMResponse{
				// Chunk 1: reasoning
				{
					Choices: []model.Choice{
						{
							Index: 0,
							Delta: &model.Message{
								ReasoningContent: ptrStr("Let me think"),
							},
						},
					},
				},
				// Chunk 2: content appears - should close thinking here
				{
					Choices: []model.Choice{
						{
							Index: 0,
							Delta: &model.Message{
								Content: model.MessageContent{
									Content: ptrStr("Here is my answer"),
								},
							},
						},
					},
				},
				// Chunk 3: finish
				{
					Choices: []model.Choice{
						{
							Index:        0,
							Delta:        &model.Message{},
							FinishReason: ptrStr("stop"),
						},
					},
				},
			},
			wantLast: "\n</think>\nHere is my answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbound := &ChatInbound{
				thinkingToContent: true,
			}

			var lastMeaningfulContent string
			for _, chunk := range tt.chunks {
				result := applyThinkingToContentStream(chunk, inbound)
				if result != nil && len(result.Choices) > 0 {
					delta := result.Choices[0].Delta
					if delta != nil && delta.Content.Content != nil && *delta.Content.Content != "" {
						lastMeaningfulContent = *delta.Content.Content
					}
				}
			}

			if lastMeaningfulContent != tt.wantLast {
				t.Errorf("last meaningful content = %q, want %q", lastMeaningfulContent, tt.wantLast)
			}

			// Verify thinking was properly closed
			if !inbound.streamThinkingClosed[0] {
				t.Error("streamThinkingClosed[0] = false, want true (thinking should be closed)")
			}
		})
	}
}

// TestApplyThinkingToContentStream_MultipleReasoningChunks verifies that
// multiple reasoning deltas are correctly accumulated before closing.
func TestApplyThinkingToContentStream_MultipleReasoningChunks(t *testing.T) {
	inbound := &ChatInbound{
		thinkingToContent: true,
	}

	chunks := []*model.InternalLLMResponse{
		// Chunk 1: first reasoning
		{
			Choices: []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						ReasoningContent: ptrStr("Step 1: analyze"),
					},
				},
			},
		},
		// Chunk 2: more reasoning
		{
			Choices: []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						ReasoningContent: ptrStr(" Step 2: conclude"),
					},
				},
			},
		},
		// Chunk 3: finish without content
		{
			Choices: []model.Choice{
				{
					Index:        0,
					Delta:        &model.Message{},
					FinishReason: ptrStr("stop"),
				},
			},
		},
	}

	var allContent []string
	for _, chunk := range chunks {
		result := applyThinkingToContentStream(chunk, inbound)
		if result != nil && len(result.Choices) > 0 {
			delta := result.Choices[0].Delta
			if delta != nil && delta.Content.Content != nil && *delta.Content.Content != "" {
				allContent = append(allContent, *delta.Content.Content)
			}
		}
	}

	// Should have: opening, continuation, closing
	if len(allContent) < 3 {
		t.Fatalf("got %d content chunks, want at least 3 (open, continue, close)", len(allContent))
	}

	// First should open
	if allContent[0] != "<think>\nStep 1: analyze" {
		t.Errorf("first content = %q, want opening with reasoning", allContent[0])
	}

	// Middle should continue
	if allContent[1] != " Step 2: conclude" {
		t.Errorf("second content = %q, want continued reasoning", allContent[1])
	}

	// Last should close
	if allContent[2] != "\n</think>\n" {
		t.Errorf("last content = %q, want closing tag", allContent[2])
	}

	if !inbound.streamThinkingClosed[0] {
		t.Error("streamThinkingClosed[0] = false, want true")
	}
}

func ptrStr(s string) *string {
	return &s
}
