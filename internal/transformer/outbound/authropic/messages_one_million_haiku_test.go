package authropic

import (
	"context"
	"strings"
	"testing"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// Outbound transformer path: the channel 1M switch adds context-1m for opus but
// never for haiku (real Claude Code never sends it for haiku).
func TestTransformRequestOneMillionSkipsHaiku(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-haiku-4-5-20251001": false,
		"claude-opus-4-8":           true,
		"sonnet[1m]":                true,
	} {
		content := "hi"
		req := &transformerModel.InternalLLMRequest{
			Model: model,
			Messages: []transformerModel.Message{{
				Role:    "user",
				Content: transformerModel.MessageContent{Content: &content},
			}},
		}
		req.TransformOptions.AnthropicOneMillionBeta = true
		httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://upstream.example", "test-key")
		if err != nil {
			t.Fatalf("%s: TransformRequest: %v", model, err)
		}
		got := strings.Contains(httpReq.Header.Get("Anthropic-Beta"), transformerModel.AnthropicOneMillionBeta)
		if got != want {
			t.Fatalf("%s: context-1m present=%v, want %v (header %q)", model, got, want, httpReq.Header.Get("Anthropic-Beta"))
		}
	}
}
