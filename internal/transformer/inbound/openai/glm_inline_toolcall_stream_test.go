package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// streamTextChunk builds a chat-completion style text delta chunk for the given model.
func streamTextChunk(modelName string, text string) *model.InternalLLMResponse {
	return &model.InternalLLMResponse{
		ID:      "chatcmpl_glm_inline",
		Model:   modelName,
		Object:  "chat.completion.chunk",
		Created: 1,
		Choices: []model.Choice{{
			Index: 0,
			Delta: &model.Message{
				Role:    "assistant",
				Content: model.MessageContent{Content: &text},
			},
		}},
	}
}

func streamFinishChunk(modelName string, finishReason string) *model.InternalLLMResponse {
	return &model.InternalLLMResponse{
		ID:      "chatcmpl_glm_inline",
		Model:   modelName,
		Object:  "chat.completion.chunk",
		Created: 1,
		Choices: []model.Choice{{Index: 0, FinishReason: &finishReason}},
	}
}

// collectStreamOutput feeds the fragments through the inbound and returns the raw SSE text.
func collectStreamOutput(t *testing.T, inbound *ResponseInbound, chunks []*model.InternalLLMResponse) string {
	t.Helper()

	var rawOutput strings.Builder
	for chunkIndex, chunk := range chunks {
		emitted, err := inbound.TransformStream(context.Background(), chunk)
		if err != nil {
			t.Fatalf("TransformStream chunk %d returned error: %v", chunkIndex, err)
		}
		rawOutput.Write(emitted)
	}
	return rawOutput.String()
}

// TestResponseInboundRecoversGLMInlineToolCallSplitAcrossChunks locks the core
// contract: a GLM upstream that streams a tool call as literal text markup, split
// across chunks, must reach the client as a real function_call — never as visible
// tag soup.
func TestResponseInboundRecoversGLMInlineToolCallSplitAcrossChunks(t *testing.T) {
	inbound := &ResponseInbound{}

	rawOutput := collectStreamOutput(t, inbound, []*model.InternalLLMResponse{
		streamTextChunk("glm-4.6", "Let me look. "),
		streamTextChunk("glm-4.6", "<tool_call>read_file"),
		streamTextChunk("glm-4.6", "<arg_key>path</arg_key>"),
		streamTextChunk("glm-4.6", "<arg_value>main.go</arg_value>"),
		streamTextChunk("glm-4.6", "</tool_call>"),
		streamFinishChunk("glm-4.6", "tool_calls"),
		{Object: "[DONE]"},
	})

	for _, leakedMarker := range []string{"<tool_call>", "<arg_key>", "<arg_value>", "</tool_call>"} {
		if strings.Contains(rawOutput, leakedMarker) {
			t.Fatalf("inline tool call markup %q leaked to the client stream:\n%s", leakedMarker, rawOutput)
		}
	}
	if !strings.Contains(rawOutput, "function_call") {
		t.Fatalf("expected a recovered function_call item in the stream:\n%s", rawOutput)
	}
	if !strings.Contains(rawOutput, "read_file") {
		t.Fatalf("expected the recovered tool name in the stream:\n%s", rawOutput)
	}
	if !strings.Contains(rawOutput, "Let me look.") {
		t.Fatalf("expected the prose preceding the markup to survive:\n%s", rawOutput)
	}
}

// TestResponseInboundLeavesNonGLMTextUntouched guards the model gate: a claude or
// codex stream must keep byte-for-byte passthrough even if its text happens to
// contain the same markup, because those models never emit inline tool calls and
// the text is legitimate content (for example, a discussion of this very bug).
func TestResponseInboundLeavesNonGLMTextUntouched(t *testing.T) {
	inbound := &ResponseInbound{}

	rawOutput := collectStreamOutput(t, inbound, []*model.InternalLLMResponse{
		streamTextChunk("claude-opus-5", "The GLM markup looks like <tool_call>x</tool_call> in practice."),
		streamFinishChunk("claude-opus-5", "stop"),
		{Object: "[DONE]"},
	})

	if !strings.Contains(rawOutput, "tool_call") {
		t.Fatalf("expected non-GLM text to pass through verbatim, markup was stripped:\n%s", rawOutput)
	}
	if strings.Contains(rawOutput, "\"type\":\"function_call\"") {
		t.Fatalf("non-GLM text must never be converted into a function_call item:\n%s", rawOutput)
	}
}

// TestResponseInboundFlushesUnterminatedGLMMarkupAtStreamEnd pins the failure mode
// for a truncated upstream: markup that never closes must still be delivered as
// text, because swallowing the tail of the answer is worse than showing raw markup.
func TestResponseInboundFlushesUnterminatedGLMMarkupAtStreamEnd(t *testing.T) {
	inbound := &ResponseInbound{}

	rawOutput := collectStreamOutput(t, inbound, []*model.InternalLLMResponse{
		streamTextChunk("glm-4.6", "Answer begins. "),
		streamTextChunk("glm-4.6", "<tool_call>read_file<arg_key>path</arg_key>"),
		streamFinishChunk("glm-4.6", "stop"),
		{Object: "[DONE]"},
	})

	if !strings.Contains(rawOutput, "read_file") {
		t.Fatalf("expected unterminated markup to be flushed rather than swallowed:\n%s", rawOutput)
	}
	if !strings.Contains(rawOutput, "Answer begins.") {
		t.Fatalf("expected the leading prose to reach the client:\n%s", rawOutput)
	}
}

// TestResponseInboundKeepsPlainGLMTextOnPassthrough verifies the gate does not
// disturb ordinary GLM answers that contain no markup at all.
func TestResponseInboundKeepsPlainGLMTextOnPassthrough(t *testing.T) {
	inbound := &ResponseInbound{}

	rawOutput := collectStreamOutput(t, inbound, []*model.InternalLLMResponse{
		streamTextChunk("glm-4.6", "Hello, "),
		streamTextChunk("glm-4.6", "this is a plain answer."),
		streamFinishChunk("glm-4.6", "stop"),
		{Object: "[DONE]"},
	})

	if !strings.Contains(rawOutput, "Hello, ") || !strings.Contains(rawOutput, "this is a plain answer.") {
		t.Fatalf("expected plain GLM text to stream through unchanged:\n%s", rawOutput)
	}
	if strings.Contains(rawOutput, "\"type\":\"function_call\"") {
		t.Fatalf("plain text must not synthesize a function_call item:\n%s", rawOutput)
	}
}

// TestResponseInboundReleasesGLMBufferPastSizeCeiling locks the starvation guard:
// an upstream that opens a marker and then never closes it must not withhold the
// answer indefinitely, so the buffer is released as plain text past the ceiling.
func TestResponseInboundReleasesGLMBufferPastSizeCeiling(t *testing.T) {
	inbound := &ResponseInbound{}

	chunks := []*model.InternalLLMResponse{
		streamTextChunk("glm-4.6", "<tool_call>never_closed"),
	}
	// Each filler chunk is well under the ceiling on its own; together they push the
	// withheld run past it.
	filler := strings.Repeat("x", 10000)
	for chunkIndex := 0; chunkIndex < 12; chunkIndex++ {
		chunks = append(chunks, streamTextChunk("glm-4.6", filler))
	}

	rawOutput := collectStreamOutput(t, inbound, chunks)

	if !strings.Contains(rawOutput, "never_closed") {
		t.Fatalf("expected the oversized withheld run to be released before the stream ends:\n%s", rawOutput[:min(len(rawOutput), 400)])
	}
	if inbound.glmInlineBuffer.Len() > maxGLMInlineBufferBytes {
		t.Fatalf("expected the buffer to be drained past the ceiling, still holding %d bytes", inbound.glmInlineBuffer.Len())
	}
}
