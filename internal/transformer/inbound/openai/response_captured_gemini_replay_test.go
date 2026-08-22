package openai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// capturedChatChunk mirrors the fields this replay needs from a real upstream chat
// completions SSE frame.
type capturedChatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta *struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// TestResponseInboundReplaysCapturedGeminiFlashStream replays a capture of the real
// antigravity-backed gemini-3.7-flash upstream through the Responses transformer and
// asserts that every character the upstream sent reaches the client.
//
// The capture is the ground truth for the "Cursor shows only a few words" report: that
// channel repeats finish_reason="stop" and a rolling usage block on every chunk, which
// used to complete the response on the very first fragment. The fixture keeps only the
// protocol envelope and the assistant prose (no ids, keys or account data).
func TestResponseInboundReplaysCapturedGeminiFlashStream(t *testing.T) {
	capturePath := filepath.Join("testdata", "gemini_flash_rolling_finish_stream.sse")
	rawCapture, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("reading capture fixture %s: %v", capturePath, err)
	}

	inbound := &ResponseInbound{}
	ctx := context.Background()

	var upstreamText strings.Builder
	var emittedEvents strings.Builder
	upstreamChunkCount := 0

	for _, line := range strings.Split(string(rawCapture), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			doneEvents, doneErr := inbound.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
			if doneErr != nil {
				t.Fatalf("[DONE] transform failed: %v", doneErr)
			}
			emittedEvents.Write(doneEvents)
			continue
		}

		var captured capturedChatChunk
		if err := json.Unmarshal([]byte(payload), &captured); err != nil {
			continue
		}
		if len(captured.Choices) == 0 {
			continue
		}
		upstreamChunkCount++

		internalResponse := &model.InternalLLMResponse{
			ID:      captured.ID,
			Model:   captured.Model,
			Created: captured.Created,
			Object:  "chat.completion.chunk",
		}
		for _, capturedChoice := range captured.Choices {
			internalChoice := model.Choice{
				Index:        capturedChoice.Index,
				FinishReason: capturedChoice.FinishReason,
			}
			if capturedChoice.Delta != nil {
				deltaMessage := &model.Message{Role: capturedChoice.Delta.Role}
				if capturedChoice.Delta.Content != nil {
					deltaMessage.Content = model.MessageContent{Content: capturedChoice.Delta.Content}
					upstreamText.WriteString(*capturedChoice.Delta.Content)
				}
				internalChoice.Delta = deltaMessage
			}
			internalResponse.Choices = append(internalResponse.Choices, internalChoice)
		}
		if captured.Usage != nil {
			internalResponse.Usage = &model.Usage{
				PromptTokens:     captured.Usage.PromptTokens,
				CompletionTokens: captured.Usage.CompletionTokens,
				TotalTokens:      captured.Usage.TotalTokens,
			}
		}

		producedEvents, transformErr := inbound.TransformStream(ctx, internalResponse)
		if transformErr != nil {
			t.Fatalf("chunk %d transform failed: %v", upstreamChunkCount, transformErr)
		}
		emittedEvents.Write(producedEvents)
	}

	if upstreamChunkCount < 10 {
		t.Fatalf("fixture only had %d chunks; the multi-chunk rolling-finish stream this guards is missing",
			upstreamChunkCount)
	}

	deliveredText := collectStreamedText(t, emittedEvents.String())
	expectedText := upstreamText.String()

	if deliveredText != expectedText {
		t.Fatalf(
			"replay lost text.\nupstream chunks: %d\nupstream chars: %d\ndelivered chars: %d\n"+
				"delivered tail: %q\nupstream tail: %q",
			upstreamChunkCount, len(expectedText), len(deliveredText),
			lastRunes(deliveredText, 80), lastRunes(expectedText, 80),
		)
	}

	t.Logf("replayed %d upstream chunks, delivered %d/%d characters intact",
		upstreamChunkCount, len(deliveredText), len(expectedText))
}

func lastRunes(text string, count int) string {
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[len(runes)-count:])
}
