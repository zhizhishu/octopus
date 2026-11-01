package authropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// Adaptive-only Claude models (opus-5-5 & co.) reject {"type":"enabled","budget_tokens":N}
// and, for some, {"type":"disabled"} with a 400 such as sub2api's
// "claude-opus-5-5 requires adaptive thinking; omit thinking or use thinking.type=adaptive and
// output_config.effort". The outbound must pick the legal form from the OUTBOUND model.

const adaptiveTestThirdPartyBase = "https://relay.example.com"

func adaptiveTestBody(t *testing.T, req *model.InternalLLMRequest, baseURL string) string {
	t.Helper()
	if len(req.Messages) == 0 {
		text := "hi"
		req.Messages = []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}}
	}
	if req.MaxTokens == nil {
		maxTokens := int64(32000)
		req.MaxTokens = &maxTokens
	}
	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, baseURL, "test-key")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func adaptiveTestMembers(t *testing.T, body string) (thinking, outputConfig string) {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}
	return string(decoded["thinking"]), string(decoded["output_config"])
}

func int64Ptr(v int64) *int64 { return &v }

func TestAdaptiveOnlyConversionPaths(t *testing.T) {
	tests := []struct {
		name             string
		req              model.InternalLLMRequest
		wantThinking     string
		wantOutputConfig string
	}{
		{
			name:             "openai chat reasoning_effort on opus-5-5",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "high"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"high"}`,
		},
		{
			name:             "chat xhigh on opus-4-7 keeps xhigh",
			req:              model.InternalLLMRequest{Model: "claude-opus-4-7", ReasoningEffort: "X-High"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"xhigh"}`,
		},
		{
			name:             "chat minimal maps to low",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "minimal"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "responses reasoning.max_tokens only -> effort from budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningBudget: int64Ptr(10000)},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"high"}`,
		},
		{
			name:             "responses budget 4096 -> medium",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningBudget: int64Ptr(4096)},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"medium"}`,
		},
		{
			name:             "responses explicit effort wins over budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "low", ReasoningBudget: int64Ptr(60000)},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "unknown effort word falls back to budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "ultra", ReasoningBudget: int64Ptr(1024)},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "gemini thinkingLevel on sonnet-5-5",
			req:              model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "medium"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"medium"}`,
		},
		{
			name:             "dotted -thinking alias resolves as opus-5-5",
			req:              model.InternalLLMRequest{Model: "claude-opus-5.5-thinking", ReasoningEffort: "max"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"max"}`,
		},
		{
			name:             "off on opus-5-5 -> adaptive low",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "none"},
			wantThinking:     `{"type":"adaptive"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "off on sonnet-5-5 -> between_tools low",
			req:              model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "off"},
			wantThinking:     `{"type":"between_tools"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:         "off on haiku-5-5 -> disabled (allowed there)",
			req:          model.InternalLLMRequest{Model: "claude-haiku-5-5", ReasoningEffort: "none"},
			wantThinking: `{"type":"disabled"}`,
		},
		{
			name:         "older opus-4-5 keeps enabled+budget",
			req:          model.InternalLLMRequest{Model: "claude-opus-4-5", ReasoningEffort: "high"},
			wantThinking: `{"type":"enabled","budget_tokens":31999}`,
		},
		{
			name:         "sonnet-4-6 (manual still accepted) keeps enabled+budget",
			req:          model.InternalLLMRequest{Model: "claude-sonnet-4-6", ReasoningEffort: "medium"},
			wantThinking: `{"type":"enabled","budget_tokens":8192}`,
		},
		{
			name: "older model budget-only stays as before (no thinking)",
			req:  model.InternalLLMRequest{Model: "claude-opus-4-5", ReasoningBudget: int64Ptr(10000)},
		},
		{
			name:         "off on opus-4-5 stays disabled",
			req:          model.InternalLLMRequest{Model: "claude-opus-4-5", ReasoningEffort: "none"},
			wantThinking: `{"type":"disabled"}`,
		},
		{
			name: "no intent on opus-5-5 sends no thinking",
			req:  model.InternalLLMRequest{Model: "claude-opus-5-5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			body := adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
			thinking, outputConfig := adaptiveTestMembers(t, body)
			if thinking != tt.wantThinking || outputConfig != tt.wantOutputConfig {
				t.Fatalf("thinking=%s output_config=%s, want %s %s\nbody=%s", thinking, outputConfig, tt.wantThinking, tt.wantOutputConfig, body)
			}
			if strings.Contains(body, `"budget_tokens"`) && model.ClaudeEffortCapabilityFor(req.Model).NoManual {
				t.Fatalf("adaptive-only model got budget_tokens: %s", body)
			}
		})
	}
}

func TestAdaptiveOnlyConversionMergesStructuredOutputFormat(t *testing.T) {
	req := model.InternalLLMRequest{
		Model:           "claude-opus-5-5",
		ReasoningEffort: "medium",
		ResponseFormat:  &model.ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(`{"name":"x","schema":{"type":"object"}}`)},
	}
	_, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
	if !strings.Contains(outputConfig, `"effort":"medium"`) || !strings.Contains(outputConfig, `"format"`) {
		t.Fatalf("effort and format must coexist, got %s", outputConfig)
	}
}

func TestAdaptiveOnlyConversionClearsSampling(t *testing.T) {
	temperature := 0.3
	req := model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "high", Temperature: &temperature}
	body := adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
	if strings.Contains(body, `"temperature"`) {
		t.Fatalf("temperature must be dropped with adaptive thinking: %s", body)
	}
	req = model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "none", Temperature: &temperature}
	body = adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
	if strings.Contains(body, `"temperature"`) || !strings.Contains(body, `"between_tools"`) {
		t.Fatalf("sonnet-5-5 between_tools must drop temperature: %s", body)
	}
}

func TestNativeEnabledRewriteOnlyForNonCLICaller(t *testing.T) {
	const enabled = `{"type":"enabled","budget_tokens":10000}`

	t.Run("non-CLI caller on opus-5-5 is rewritten", func(t *testing.T) {
		req := model.InternalLLMRequest{Model: "claude-opus-5-5", AnthropicThinking: json.RawMessage(enabled)}
		req.TransformOptions.ClaudeNonCLICaller = true
		thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
		if thinking != `{"type":"adaptive"}` || outputConfig != `{"effort":"high"}` {
			t.Fatalf("thinking=%s output_config=%s", thinking, outputConfig)
		}
	})

	t.Run("non-CLI caller keeps its own effort", func(t *testing.T) {
		req := model.InternalLLMRequest{Model: "claude-opus-5-5", AnthropicThinking: json.RawMessage(enabled),
			AnthropicOutputConfig: json.RawMessage(`{"effort":"low"}`)}
		req.TransformOptions.ClaudeNonCLICaller = true
		thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
		if thinking != `{"type":"adaptive"}` || outputConfig != `{"effort":"low"}` {
			t.Fatalf("thinking=%s output_config=%s", thinking, outputConfig)
		}
	})

	t.Run("non-CLI caller on opus-4-5 untouched", func(t *testing.T) {
		req := model.InternalLLMRequest{Model: "claude-opus-4-5", AnthropicThinking: json.RawMessage(enabled)}
		req.TransformOptions.ClaudeNonCLICaller = true
		thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
		if thinking != enabled || outputConfig != "" {
			t.Fatalf("thinking=%s output_config=%s", thinking, outputConfig)
		}
	})

	t.Run("CLI-shaped caller is byte-identical", func(t *testing.T) {
		// Same request, flag unset (the relay leaves it false for a claude-cli/ UA or a
		// claude-code beta). The body must be exactly what it was before this change: the
		// caller's raw enabled thinking, no output_config synthesised, nothing else moved.
		build := func(nonCLI bool, model_ string) string {
			req := model.InternalLLMRequest{Model: model_, AnthropicThinking: json.RawMessage(enabled),
				AnthropicContextManagement: json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)}
			req.TransformOptions.ClaudeNonCLICaller = nonCLI
			return adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
		}
		cli := build(false, "claude-opus-5-5")
		// Reference: the same body for a model the change never touches, with only the model
		// name swapped back — proves no other byte moved for the CLI caller.
		reference := strings.Replace(build(false, "claude-opus-4-5"), `"claude-opus-4-5"`, `"claude-opus-5-5"`, 1)
		if cli != reference {
			t.Fatalf("CLI body changed:\n got %s\nwant %s", cli, reference)
		}
		if thinking, outputConfig := adaptiveTestMembers(t, cli); thinking != enabled || outputConfig != "" {
			t.Fatalf("CLI thinking rewritten: thinking=%s output_config=%s", thinking, outputConfig)
		}
	})
}

func TestOfficialBaseDisabledDropUnchanged(t *testing.T) {
	req := model.InternalLLMRequest{Model: "claude-opus-4-5", AnthropicThinking: json.RawMessage(`{"type":"disabled"}`)}
	if thinking, _ := adaptiveTestMembers(t, adaptiveTestBody(t, &req, "https://api.anthropic.com")); thinking != "" {
		t.Fatalf("official base must still drop disabled, got %s", thinking)
	}
	req = model.InternalLLMRequest{Model: "claude-opus-4-5", AnthropicThinking: json.RawMessage(`{"type":"disabled"}`)}
	if thinking, _ := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)); thinking != `{"type":"disabled"}` {
		t.Fatalf("third-party base must keep disabled, got %s", thinking)
	}
	// Off intent on opus-5-5 via conversion on the official base: adaptive+low survives.
	req = model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "none"}
	if thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, "https://api.anthropic.com")); thinking != `{"type":"adaptive"}` || outputConfig != `{"effort":"low"}` {
		t.Fatalf("official opus-5-5 off: thinking=%s output_config=%s", thinking, outputConfig)
	}
}
