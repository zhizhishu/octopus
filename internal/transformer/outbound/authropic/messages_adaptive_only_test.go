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
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"high"}`,
		},
		{
			name:             "chat xhigh on opus-4-7 keeps xhigh",
			req:              model.InternalLLMRequest{Model: "claude-opus-4-7", ReasoningEffort: "X-High"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"xhigh"}`,
		},
		{
			name:             "chat minimal maps to low",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "minimal"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "responses reasoning.max_tokens only -> effort from budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningBudget: int64Ptr(10000)},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"high"}`,
		},
		{
			name:             "responses budget 4096 -> medium",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningBudget: int64Ptr(4096)},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"medium"}`,
		},
		{
			name:             "responses explicit effort wins over budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "low", ReasoningBudget: int64Ptr(60000)},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "unknown effort word falls back to budget",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "ultra", ReasoningBudget: int64Ptr(1024)},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "gemini thinkingLevel on sonnet-5-5",
			req:              model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "medium"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"medium"}`,
		},
		{
			name:             "dotted -thinking alias resolves as opus-5-5",
			req:              model.InternalLLMRequest{Model: "claude-opus-5.5-thinking", ReasoningEffort: "max"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"max"}`,
		},
		{
			name:             "off on opus-5-5 -> adaptive low",
			req:              model.InternalLLMRequest{Model: "claude-opus-5-5", ReasoningEffort: "none"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name:             "off on sonnet-5-5 cloaked -> adaptive omitted low (-p profile)",
			req:              model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "off"},
			wantThinking:     `{"type":"adaptive","display":"omitted"}`,
			wantOutputConfig: `{"effort":"low"}`,
		},
		{
			name: "off on sonnet-5-5 cloak=never -> between_tools low",
			req: model.InternalLLMRequest{Model: "claude-sonnet-5-5", ReasoningEffort: "off",
				TransformOptions: model.TransformOptions{SuppressClaudeIdentity: true}},
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
	if strings.Contains(body, `"temperature"`) || !strings.Contains(body, `"display":"omitted"`) {
		t.Fatalf("cloaked sonnet-5-5 adaptive off must drop temperature: %s", body)
	}
	req.TransformOptions.SuppressClaudeIdentity = true
	body = adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
	if strings.Contains(body, `"temperature"`) || !strings.Contains(body, `"between_tools"`) {
		t.Fatalf("cloak=never sonnet-5-5 between_tools must drop temperature: %s", body)
	}
}

func TestNativeEnabledRewriteOnlyForNonCLICaller(t *testing.T) {
	const enabled = `{"type":"enabled","budget_tokens":10000}`

	t.Run("non-CLI caller on opus-5-5 is rewritten", func(t *testing.T) {
		req := model.InternalLLMRequest{Model: "claude-opus-5-5", AnthropicThinking: json.RawMessage(enabled)}
		req.TransformOptions.ClaudeNonCLICaller = true
		thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
		if thinking != `{"type":"adaptive","display":"omitted"}` || outputConfig != `{"effort":"high"}` {
			t.Fatalf("thinking=%s output_config=%s", thinking, outputConfig)
		}
	})

	t.Run("non-CLI caller keeps its own effort", func(t *testing.T) {
		req := model.InternalLLMRequest{Model: "claude-opus-5-5", AnthropicThinking: json.RawMessage(enabled),
			AnthropicOutputConfig: json.RawMessage(`{"effort":"low"}`)}
		req.TransformOptions.ClaudeNonCLICaller = true
		thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
		if thinking != `{"type":"adaptive","display":"omitted"}` || outputConfig != `{"effort":"low"}` {
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
	if thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, "https://api.anthropic.com")); thinking != `{"type":"adaptive","display":"omitted"}` || outputConfig != `{"effort":"low"}` {
		t.Fatalf("official opus-5-5 off: thinking=%s output_config=%s", thinking, outputConfig)
	}
}

func TestNativeDisabledRewriteOnlyForNonCLICaller(t *testing.T) {
	const disabled = `{"type":"disabled"}`
	temperature, topP := 0.2, 0.5
	tests := []struct {
		name             string
		model            string
		nonCLI           bool
		baseURL          string
		outputConfig     string
		wantThinking     string
		wantOutputConfig string
	}{
		{"non-CLI opus-5-5 -> adaptive low", "claude-opus-5-5", true, adaptiveTestThirdPartyBase, "", `{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		{"non-CLI opus-5-5 paired effort is overridden to low", "claude-opus-5-5", true, adaptiveTestThirdPartyBase, `{"effort":"max"}`, `{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		{"non-CLI fable-5 -> adaptive low", "claude-fable-5", true, adaptiveTestThirdPartyBase, "", `{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		{"non-CLI cloaked sonnet-5-5 -> adaptive omitted low", "claude-sonnet-5-5", true, adaptiveTestThirdPartyBase, `{"effort":"xhigh"}`, `{"type":"adaptive","display":"omitted"}`, `{"effort":"low"}`},
		{"non-CLI haiku-5-5 keeps disabled", "claude-haiku-5-5", true, adaptiveTestThirdPartyBase, "", disabled, ""},
		{"non-CLI opus-4-5 keeps disabled", "claude-opus-4-5", true, adaptiveTestThirdPartyBase, "", disabled, ""},
		{"CLI opus-5-5 untouched", "claude-opus-5-5", false, adaptiveTestThirdPartyBase, "", disabled, ""},
		{"CLI sonnet-5-5 untouched", "claude-sonnet-5-5", false, adaptiveTestThirdPartyBase, `{"effort":"high"}`, disabled, `{"effort":"high"}`},
		{"official base still drops disabled (non-CLI)", "claude-opus-5-5", true, "https://api.anthropic.com", "", "", ""},
		{"official base still drops disabled (CLI)", "claude-opus-5-5", false, "https://api.anthropic.com", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := model.InternalLLMRequest{Model: tt.model, AnthropicThinking: json.RawMessage(disabled), Temperature: &temperature, TopP: &topP}
			if tt.outputConfig != "" {
				req.AnthropicOutputConfig = json.RawMessage(tt.outputConfig)
			}
			req.TransformOptions.ClaudeNonCLICaller = tt.nonCLI
			body := adaptiveTestBody(t, &req, tt.baseURL)
			thinking, outputConfig := adaptiveTestMembers(t, body)
			if thinking != tt.wantThinking || outputConfig != tt.wantOutputConfig {
				t.Fatalf("thinking=%s output_config=%s, want %s %s\nbody=%s", thinking, outputConfig, tt.wantThinking, tt.wantOutputConfig, body)
			}
			rewritten := tt.wantThinking == `{"type":"adaptive","display":"omitted"}` || tt.wantThinking == `{"type":"between_tools"}`
			hasSampling := strings.Contains(body, `"temperature"`) || strings.Contains(body, `"top_p"`)
			if rewritten && hasSampling {
				t.Fatalf("rewritten off form must drop temperature/top_p: %s", body)
			}
			if !rewritten && tt.wantThinking == disabled && !hasSampling {
				t.Fatalf("untouched disabled body lost the caller's sampling params: %s", body)
			}
		})
	}
}

func TestNativeDisabledSonnetCloakNeverKeepsBetweenTools(t *testing.T) {
	const disabled = `{"type":"disabled"}`
	req := model.InternalLLMRequest{Model: "claude-sonnet-5-5", AnthropicThinking: json.RawMessage(disabled)}
	req.TransformOptions.ClaudeNonCLICaller = true
	req.TransformOptions.SuppressClaudeIdentity = true
	thinking, outputConfig := adaptiveTestMembers(t, adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase))
	if thinking != `{"type":"between_tools"}` || outputConfig != `{"effort":"low"}` {
		t.Fatalf("cloak=never sonnet-5-5: thinking=%s output_config=%s", thinking, outputConfig)
	}
}

func TestNativeDisabledCLIBodyByteIdentical(t *testing.T) {
	build := func(modelName string) string {
		temperature := 0.2
		req := model.InternalLLMRequest{Model: modelName, AnthropicThinking: json.RawMessage(`{"type":"disabled"}`), Temperature: &temperature}
		return adaptiveTestBody(t, &req, adaptiveTestThirdPartyBase)
	}
	got := build("claude-opus-5-5")
	want := strings.Replace(build("claude-opus-4-5"), `"claude-opus-4-5"`, `"claude-opus-5-5"`, 1)
	if got != want {
		t.Fatalf("CLI disabled body changed:\n got %s\nwant %s", got, want)
	}
}

// display:omitted returns thinking blocks with empty text + signature. Those must
// round-trip on the next turn (Anthropic docs; Claude Code golden shape).
func TestOmittedThinkingSignatureRoundTrips(t *testing.T) {
	empty := ""
	sig := "EosnCkYICxIMMb3LzNrMuTESTSIG"
	text := "Done."
	hi := "hi"
	again := "again"
	maxTokens := int64(1024)
	req := &model.InternalLLMRequest{
		Model:        "claude-opus-4-8",
		MaxTokens:    &maxTokens,
		RawAPIFormat: model.APIFormatAnthropicMessage,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: &hi}},
			{
				Role:               "assistant",
				Content:            model.MessageContent{Content: &text},
				ReasoningContent:   &empty,
				ReasoningSignature: &sig,
			},
			{Role: "user", Content: model.MessageContent{Content: &again}},
		},
	}
	body := adaptiveTestBody(t, req, adaptiveTestThirdPartyBase)
	wantBlock := `{"type":"thinking","thinking":"","signature":"EosnCkYICxIMMb3LzNrMuTESTSIG"}`
	if !strings.Contains(body, wantBlock) {
		t.Fatalf("empty+signature block not byte-identical (type,thinking,signature):\nwant %s\nbody %s", wantBlock, body)
	}
	// Also: signature-only (ReasoningContent nil) must round-trip.
	req.Messages[1].ReasoningContent = nil
	body = adaptiveTestBody(t, req, adaptiveTestThirdPartyBase)
	if !strings.Contains(body, `"signature":"EosnCkYICxIMMb3LzNrMuTESTSIG"`) {
		t.Fatalf("nil-text+signature thinking block dropped:\n%s", body)
	}
}

// A genuine Claude Code /v1/messages turn that carries a prior assistant thinking
// block with empty text + signature (display:omitted response shape) must leave that
// history byte-shaped on the outbound: the thinking config the CLI sent stays raw,
// and the empty+signature block is echoed (not dropped, not rewritten). This is the
// iron-rule check for the signature-only round-trip fix.
func TestCLIEmptyThinkingSignatureHistoryPassthrough(t *testing.T) {
	empty := ""
	sig := "EosnCkYICxIMMb3LzNrMuCLISIG"
	done := "Done."
	hi := "hi"
	again := "again"
	maxTokens := int64(64000)
	thinkingCfg := `{"type":"adaptive","display":"omitted"}`
	outputCfg := `{"effort":"high"}`
	req := &model.InternalLLMRequest{
		Model:                 "claude-opus-4-8",
		MaxTokens:             &maxTokens,
		RawAPIFormat:          model.APIFormatAnthropicMessage,
		AnthropicThinking:     json.RawMessage(thinkingCfg),
		AnthropicOutputConfig: json.RawMessage(outputCfg),
		// CLI-shaped caller: ClaudeNonCLICaller stays false (zero value).
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: &hi}},
			{
				Role:               "assistant",
				Content:            model.MessageContent{Content: &done},
				ReasoningContent:   &empty,
				ReasoningSignature: &sig,
			},
			{Role: "user", Content: model.MessageContent{Content: &again}},
		},
	}
	body := adaptiveTestBody(t, req, adaptiveTestThirdPartyBase)
	thinking, outputConfig := adaptiveTestMembers(t, body)
	if thinking != thinkingCfg {
		t.Fatalf("CLI thinking config rewritten: got %s want %s", thinking, thinkingCfg)
	}
	if outputConfig != outputCfg {
		t.Fatalf("CLI output_config rewritten: got %s want %s", outputConfig, outputCfg)
	}
	// Wire capture (2.1.316 multi-turn): key order type, thinking, signature; empty
	// thinking string is present (not omitted). encoding/json struct order on
	// MessageContentBlock matches that (Type → Thinking → Signature).
	wantBlock := `{"type":"thinking","thinking":"","signature":"EosnCkYICxIMMb3LzNrMuCLISIG"}`
	if !strings.Contains(body, wantBlock) {
		t.Fatalf("CLI empty+signature thinking history not byte-identical:\nwant substring %s\nbody %s", wantBlock, body)
	}
	// Same body with ReasoningContent nil (inbound path for omitted) must also round-trip.
	req.Messages[1].ReasoningContent = nil
	body2 := adaptiveTestBody(t, req, adaptiveTestThirdPartyBase)
	if !strings.Contains(body2, `"signature":"EosnCkYICxIMMb3LzNrMuCLISIG"`) {
		t.Fatalf("CLI nil-text+signature history dropped:\n%s", body2)
	}
	thinking2, _ := adaptiveTestMembers(t, body2)
	if thinking2 != thinkingCfg {
		t.Fatalf("CLI thinking config changed on nil-text path: %s", thinking2)
	}
}
