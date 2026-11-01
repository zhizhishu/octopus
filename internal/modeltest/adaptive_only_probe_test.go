package modeltest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

const adaptiveProbeSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_probe","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// captureAnthropicThinking starts a fake third-party Anthropic upstream and records the
// thinking / output_config members of every request it receives.
func captureAnthropicThinking(t *testing.T) (*httptest.Server, *[2]string) {
	t.Helper()
	seen := &[2]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		seen[0], seen[1] = string(payload["thinking"]), string(payload["output_config"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(adaptiveProbeSSE))
	}))
	t.Cleanup(upstream.Close)
	return upstream, seen
}

func adaptiveTestChannel(url string) dbmodel.Channel {
	return dbmodel.Channel{
		Name:     "adaptive-only",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: url}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-test-not-real"}},
	}
}

// Model tests on a cloaked Anthropic channel always send an explicit "no thinking" object.
// opus-5-5 rejects {"type":"disabled"} (sub2api: "claude-opus-5-5 requires adaptive thinking"),
// so the spelling must follow the model while older models keep the byte-identical disabled.
func TestModelTestThinkingOffFollowsModel(t *testing.T) {
	ctx := setupModelTestDB(t)
	for modelName, want := range map[string][2]string{
		"claude-opus-5-5":   {`{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		"claude-sonnet-5-5": {`{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		"claude-haiku-5-5":  {`{"type":"disabled"}`, ``},
		"claude-opus-4-5":   {`{"type":"disabled"}`, ``},
	} {
		for _, endpoint := range []string{"anthropic_messages", "openai_chat"} {
			t.Run(modelName+"/"+endpoint, func(t *testing.T) {
				upstream, seen := captureAnthropicThinking(t)
				response, err := RunChannel(ctx, adaptiveTestChannel(upstream.URL), dbmodel.ModelTestRequest{
					Model:    modelName,
					Endpoint: endpoint,
				})
				if err != nil {
					t.Fatalf("run channel test: %v", err)
				}
				if response.Summary.Success != 1 {
					t.Fatalf("model test failed: %#v", response)
				}
				if seen[0] != want[0] || seen[1] != want[1] {
					t.Fatalf("thinking=%s output_config=%s, want %s %s", seen[0], seen[1], want[0], want[1])
				}
			})
		}
	}
}

// The signature probe asks for thinking. Adaptive-only models get adaptive + display omitted
// (Claude Code golden shape; signature still round-trips with empty text); other models keep
// the CLI-legal enabled+budget form.
func TestSignatureProbeThinkingFollowsModel(t *testing.T) {
	setupModelTestDB(t)
	for modelName, want := range map[string][2]string{
		"claude-opus-5-5": {`{"type":"adaptive","display":"omitted"}`, `{"effort":"high"}`},
		"claude-opus-4-7": {`{"type":"adaptive","display":"omitted"}`, `{"effort":"high"}`},
		"claude-opus-4-5": {`{"type":"enabled","budget_tokens":8000}`, ``},
	} {
		t.Run(modelName, func(t *testing.T) {
			upstream, seen := captureAnthropicThinking(t)
			sender, err := NewProbeSender(adaptiveTestChannel(upstream.URL), modelName, "anthropic_messages")
			if err != nil {
				t.Fatalf("NewProbeSender: %v", err)
			}
			if _, err := sender.Ask(context.Background(), modelverify.Ask{Prompt: "say OK", Thinking: true}); err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if seen[0] != want[0] || seen[1] != want[1] {
				t.Fatalf("thinking=%s output_config=%s, want %s %s", seen[0], seen[1], want[0], want[1])
			}
		})
	}
}
