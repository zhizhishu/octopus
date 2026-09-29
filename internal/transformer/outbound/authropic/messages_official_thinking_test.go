package authropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
)

// Official api.anthropic.com answers 400 for an explicit thinking type "disabled"
// on its current models ('"thinking.type.disabled" is not supported for this
// model. Use "thinking.type.adaptive" ...'), so the outbound must not send it
// there — while third-party relays still expect the byte-exact claude-cli shape.
func TestTransformRequestThinkingDisabledByBase(t *testing.T) {
	tests := []struct {
		name         string
		baseURL      string
		thinking     string
		wantDisabled bool
	}{
		{
			name:         "official drops disabled",
			baseURL:      "https://api.anthropic.com",
			thinking:     `{"type":"disabled"}`,
			wantDisabled: false,
		},
		{
			name:         "official subdomain drops disabled",
			baseURL:      "https://eu.api.anthropic.com/v1",
			thinking:     `{"type":"disabled"}`,
			wantDisabled: false,
		},
		{
			name:         "official keeps adaptive",
			baseURL:      "https://api.anthropic.com",
			thinking:     `{"type":"adaptive"}`,
			wantDisabled: false,
		},
		{
			name:         "third-party relay keeps disabled",
			baseURL:      "https://relay.example/api/provider/anthropic",
			thinking:     `{"type":"disabled"}`,
			wantDisabled: true,
		},
		{
			name:         "third-party relay keeps enabled budget",
			baseURL:      "https://relay.example/api/provider/anthropic",
			thinking:     `{"type":"enabled","budget_tokens":2048}`,
			wantDisabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &model.InternalLLMRequest{
				Model:             "claude-opus-5-5",
				AnthropicThinking: json.RawMessage(tt.thinking),
				Messages: []model.Message{{
					Role:    "user",
					Content: model.MessageContent{Content: lo.ToPtr("ping")},
				}},
			}

			httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, tt.baseURL, "sk-test")
			if err != nil {
				t.Fatalf("TransformRequest returned error: %v", err)
			}
			raw, err := io.ReadAll(httpReq.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			body := string(raw)

			sentDisabled := strings.Contains(body, `"thinking":{"type":"disabled"}`)
			if sentDisabled != tt.wantDisabled {
				t.Fatalf("thinking disabled on wire = %v, want %v; body=%s", sentDisabled, tt.wantDisabled, body)
			}
			if tt.thinking == `{"type":"adaptive"}` && !strings.Contains(body, `"thinking":{"type":"adaptive"}`) {
				t.Fatalf("adaptive thinking must survive on official bases; body=%s", body)
			}
			if tt.thinking == `{"type":"enabled","budget_tokens":2048}` && !strings.Contains(body, `"budget_tokens":2048`) {
				t.Fatalf("enabled thinking budget must survive on third-party bases; body=%s", body)
			}
		})
	}
}
