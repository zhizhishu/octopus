package authropic_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	"github.com/zhizhishu/octopus/internal/transformer/inbound/openai"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/authropic"
)

// End-to-end: a real inbound adapter decodes the client body, the Anthropic outbound encodes
// it for an adaptive-only model. Pins that every conversion path reaches the adaptive form.
func TestAdaptiveOnlyInboundToAnthropicEndToEnd(t *testing.T) {
	type inbound interface {
		TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error)
	}
	tests := []struct {
		name             string
		in               inbound
		body             string
		model            string
		wantThinking     string
		wantOutputConfig string
	}{
		{
			name:             "openai chat reasoning_effort",
			in:               &openai.ChatInbound{},
			body:             `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`,
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"high"}`,
		},
		{
			name:             "openai chat reasoning_effort none",
			in:               &openai.ChatInbound{},
			body:             `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "responses reasoning.max_tokens budget",
			in:               &openai.ResponseInbound{},
			body:             `{"model":"claude-opus-5-5","input":"hi","reasoning":{"max_tokens":2048}}`,
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"medium"}`,
		},
		{
			name:             "responses reasoning.effort",
			in:               &openai.ResponseInbound{},
			body:             `{"model":"claude-opus-5-5","input":"hi","reasoning":{"effort":"xhigh"}}`,
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"xhigh"}`,
		},
		{
			name:             "gemini thinkingLevel",
			in:               &gemini.GenerateContentInbound{},
			body:             `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`,
			model:            "claude-opus-5-5",
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:         "openai chat on opus-4-5 keeps enabled+budget",
			in:           &openai.ChatInbound{},
			body:         `{"model":"claude-opus-4-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","max_tokens":32000}`,
			wantThinking: `{"type":"enabled","budget_tokens":31999}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.model != "" {
				ctx = gemini.WithRequestOptions(ctx, tt.model, false)
			}
			req, err := tt.in.TransformRequest(ctx, []byte(tt.body))
			if err != nil {
				t.Fatalf("inbound: %v", err)
			}
			if req.MaxTokens == nil {
				maxTokens := int64(32000)
				req.MaxTokens = &maxTokens
			}
			httpReq, err := (&authropic.MessageOutbound{}).TransformRequest(context.Background(), req, "https://relay.example.com", "k")
			if err != nil {
				t.Fatalf("outbound: %v", err)
			}
			body, _ := io.ReadAll(httpReq.Body)
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(decoded["thinking"]) != tt.wantThinking || string(decoded["output_config"]) != tt.wantOutputConfig {
				t.Fatalf("thinking=%s output_config=%s want %s %s\nbody=%s", decoded["thinking"], decoded["output_config"], tt.wantThinking, tt.wantOutputConfig, body)
			}
		})
	}
}
