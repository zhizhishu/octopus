package relay

import (
	"encoding/json"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestNormalizeClaudeDefaultEffortReadsLegacyValues(t *testing.T) {
	cases := map[string]string{
		"": "high", "auto": "high", "bogus": "high",
		"off": "none", "false": "none", "disabled": "none", "none": "none",
		"low": "low", "medium": "medium", "high": "high", "XHIGH": "xhigh", "max": "max",
	}
	for in, want := range cases {
		if got := normalizeClaudeDefaultEffort(in); got != want {
			t.Errorf("normalizeClaudeDefaultEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func setClaudeEffort(t *testing.T, value string) {
	t.Helper()
	if err := op.SettingSetString(dbmodel.SettingKeyClaudeCLIReasoningEffort, value); err != nil {
		t.Fatalf("set effort: %v", err)
	}
}

func TestClaudeDefaultEffortFillsPlainClientOnly(t *testing.T) {
	setupRescueDeadlineDB(t)

	// Default (high) on a plain SDK client with no thinking choice.
	ra := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.internalRequest.Model = "claude-opus-4-7"
	ra.prepareClaudePlainClientShape()
	if ra.internalRequest.ReasoningEffort != "high" || !ra.internalRequest.AdaptiveThinking || ra.claudeDefaultEffort != "high" {
		t.Fatalf("plain client: effort=%q adaptive=%v", ra.internalRequest.ReasoningEffort, ra.internalRequest.AdaptiveThinking)
	}

	// Real Claude Code (User-Agent) is never touched.
	cli := claudeShapeAttempt(t, "claude-cli/2.1.296 (external, sdk-cli)", outbound.OutboundTypeAnthropic)
	cli.internalRequest.Model = "claude-opus-4-7"
	cli.prepareClaudePlainClientShape()
	if cli.internalRequest.ReasoningEffort != "" || cli.internalRequest.AdaptiveThinking {
		t.Fatalf("claude-cli caller was changed: effort=%q", cli.internalRequest.ReasoningEffort)
	}

	// Real Claude Code (claude-code beta, any UA) is never touched.
	beta := claudeShapeAttempt(t, "node", outbound.OutboundTypeAnthropic)
	beta.internalRequest.Model = "claude-opus-4-7"
	beta.c.Request.Header.Set("Anthropic-Beta", "claude-code-20250219")
	beta.prepareClaudePlainClientShape()
	if beta.internalRequest.ReasoningEffort != "" {
		t.Fatalf("claude-code beta caller was changed")
	}

	// Client's own choice wins (thinking off stays off).
	own := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	own.internalRequest.Model = "claude-opus-4-7"
	own.internalRequest.ReasoningEffort = "low"
	own.prepareClaudePlainClientShape()
	if own.internalRequest.ReasoningEffort != "low" || own.internalRequest.AdaptiveThinking {
		t.Fatalf("client effort overwritten: %q adaptive=%v", own.internalRequest.ReasoningEffort, own.internalRequest.AdaptiveThinking)
	}

	// 不补.
	setClaudeEffort(t, "none")
	none := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	none.internalRequest.Model = "claude-opus-4-7"
	none.prepareClaudePlainClientShape()
	if none.internalRequest.ReasoningEffort != "" || none.claudeDefaultEffort != "" {
		t.Fatalf("setting none still filled %q", none.internalRequest.ReasoningEffort)
	}
}

func TestClaudeShapeKeysFollowDefaultEffortSetting(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatal(err)
	}

	// xhigh on a model without xhigh -> high, in the CLI raw shape.
	setClaudeEffort(t, "xhigh")
	ra := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.prepareClaudePlainClientShape()
	ra.ensureClaudeCLIShapeTopLevelKeys()
	if got := string(ra.internalRequest.AnthropicOutputConfig); got != `{"effort":"high"}` {
		t.Fatalf("output_config = %s, want effort high (sonnet-4-6 has no xhigh)", got)
	}
	if got := string(ra.internalRequest.AnthropicThinking); got != `{"type":"adaptive","display":"omitted"}` {
		t.Fatalf("thinking = %s", got)
	}

	setClaudeEffort(t, "max")
	ra = claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.internalRequest.Model = "claude-opus-4-7"
	ra.prepareClaudePlainClientShape()
	ra.ensureClaudeCLIShapeTopLevelKeys()
	if got := string(ra.internalRequest.AnthropicOutputConfig); got != `{"effort":"max"}` {
		t.Fatalf("output_config = %s, want effort max (no hard-coded high)", got)
	}

	// 不补: no thinking/output_config; the injected context_management never leaves (400 fix).
	setClaudeEffort(t, "none")
	ra = claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.prepareClaudePlainClientShape()
	ra.ensureClaudeCLIShapeTopLevelKeys()
	if len(ra.internalRequest.AnthropicThinking) != 0 || len(ra.internalRequest.AnthropicOutputConfig) != 0 {
		t.Fatalf("setting none must not add thinking/output_config: %s %s", ra.internalRequest.AnthropicThinking, ra.internalRequest.AnthropicOutputConfig)
	}
	if !ra.internalRequest.TransformOptions.ClaudeContextManagementInjected {
		t.Fatal("injected context_management must be marked")
	}
	body := outboundBody(t, ra)
	if strings.Contains(string(body), "context_management") || strings.Contains(string(body), `"thinking"`) {
		t.Fatalf("thinking off: outbound must carry neither thinking nor clear_thinking: %s", body)
	}

	// Client turned thinking off explicitly: same — clear_thinking never goes with disabled.
	setClaudeEffort(t, "high")
	ra = claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.internalRequest.AnthropicThinking = json.RawMessage(`{"type":"disabled"}`)
	ra.prepareClaudePlainClientShape()
	ra.ensureClaudeCLIShapeTopLevelKeys()
	body = outboundBody(t, ra)
	if strings.Contains(string(body), "context_management") || !strings.Contains(string(body), `"disabled"`) {
		t.Fatalf("client disabled thinking: %s", body)
	}
}

func TestCodexDefaultReasoningEffort(t *testing.T) {
	cases := []struct {
		name, model, client, level, want string
	}{
		{"none never fills", "gpt-5.5-codex", "", "none", ""},
		{"fills when client sent nothing", "gpt-5.5-codex", "", "high", "high"},
		{"client wins", "gpt-5.5-codex", "low", "xhigh", "low"},
		{"non reasoning model skipped", "gpt-4.1", "", "high", ""},
		{"o-series", "o4-mini", "", "medium", "medium"},
		{"prefixed", "openai/gpt-5", "", "low", "low"},
		{"invalid level", "gpt-5", "", "auto", ""},
	}
	for _, c := range cases {
		req := &model.InternalLLMRequest{Model: c.model, ReasoningEffort: c.client}
		applyCodexDefaultReasoningEffort(req, c.level)
		if req.ReasoningEffort != c.want {
			t.Errorf("%s: effort=%q, want %q", c.name, req.ReasoningEffort, c.want)
		}
	}
	// max on non-5.6 still becomes xhigh afterwards (existing rule kept).
	req := &model.InternalLLMRequest{Model: "gpt-5.5-codex"}
	applyCodexDefaultReasoningEffort(req, "max")
	normalizeCodexReasoningEffort(req)
	if req.ReasoningEffort != "xhigh" {
		t.Fatalf("max on gpt-5.5 = %q, want xhigh", req.ReasoningEffort)
	}
}
