package model

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Claude default thinking effort ("默认思考强度").
//
// Rule (operator-facing, must stay exactly what the settings page says):
//   - the client wins: any thinking / effort intent the client expressed is sent as-is;
//   - only when the client expressed none, fill adaptive thinking + output_config.effort with
//     the configured level;
//   - never fill something the target model rejects (upstream 400).
//
// The capability table mirrors new-api relaykit/relayconvert/reasoning/claude.go
// claudeCapabilitiesFor and CLIProxyAPI internal/registry/models/models.json, cross-checked
// against https://platform.claude.com/docs/en/build-with-claude/thinking-troubleshooting
// (per-model table) and .../effort (xhigh availability).

// ClaudeDefaultEffortNone is the stored value for "不补（完全跟客户端）".
const ClaudeDefaultEffortNone = "none"

// Reasons ApplyClaudeDefaultEffort did not fill anything.
const (
	ClaudeEffortSkipSettingNone   = "setting_none"
	ClaudeEffortSkipClientOwned   = "client_owned"
	ClaudeEffortSkipModel         = "model_unsupported"
	ClaudeEffortSkipSampling      = "client_sampling_params"
	ClaudeEffortSkipForcedTool    = "forced_tool_choice"
	ClaudeEffortSkipInvalidConfig = "invalid_setting"
)

// ClaudeEffortCapability describes what a Claude model accepts for adaptive thinking.
type ClaudeEffortCapability struct {
	Adaptive bool
	XHigh    bool
	Max      bool
}

// NormalizeClaudeModelID lowercases a model id and strips provider prefixes / suffixes so the
// capability table can match by prefix: "anthropic/claude-opus-4.7" -> "claude-opus-4-7",
// "us.anthropic.claude-sonnet-4-6" -> "claude-sonnet-4-6", "claude-opus-4-7@20260101" ->
// "claude-opus-4-7", "claude-opus-4-7[1m]" -> "claude-opus-4-7".
func NormalizeClaudeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "claude-"); i > 0 {
		m = m[i:]
	}
	if i := strings.IndexAny(m, "@["); i >= 0 {
		m = m[:i]
	}
	return strings.ReplaceAll(m, ".", "-")
}

// ClaudeEffortCapabilityFor returns the adaptive-thinking capability of a (normalized or raw)
// model id. Unknown and non-Claude models report Adaptive=false: nothing is filled for them.
func ClaudeEffortCapabilityFor(model string) ClaudeEffortCapability {
	m := NormalizeClaudeModelID(model)
	full := ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true}
	for _, prefix := range []string{
		"claude-fable-5", "claude-mythos-5",
		"claude-opus-5", "claude-sonnet-5", "claude-haiku-5",
		"claude-opus-4-8", "claude-opus-4-7",
	} {
		if strings.HasPrefix(m, prefix) {
			return full
		}
	}
	for _, prefix := range []string{"claude-mythos-preview", "claude-opus-4-6", "claude-sonnet-4-6"} {
		if strings.HasPrefix(m, prefix) {
			return ClaudeEffortCapability{Adaptive: true, Max: true}
		}
	}
	// claude-opus-4-5, claude-sonnet-4-5, claude-haiku-4-5, claude-opus-4-1 and earlier,
	// claude-3*: extended thinking only — {"type":"adaptive"} returns 400.
	return ClaudeEffortCapability{}
}

// ClampClaudeEffort returns the level the model accepts in place of the requested one, or ""
// when the model takes no adaptive effort. Unsupported levels move DOWN to the nearest supported
// one (CLIProxyAPI clampLevel, tie → lower), so we never silently raise the operator's cost.
func ClampClaudeEffort(model, level string) string {
	capability := ClaudeEffortCapabilityFor(model)
	if !capability.Adaptive {
		return ""
	}
	switch level {
	case "low", "medium", "high":
		return level
	case "xhigh":
		if capability.XHigh {
			return level
		}
		return "high"
	case "max":
		if capability.Max {
			return level
		}
		if capability.XHigh {
			return "xhigh"
		}
		return "high"
	default:
		return ""
	}
}

// ClaudeRequestHasThinkingIntent reports whether the client expressed any thinking or effort
// choice, in any of the shapes the inbound adapters produce. "Off" spellings count: turning
// thinking off is a choice too.
func ClaudeRequestHasThinkingIntent(req *InternalLLMRequest) bool {
	if req == nil {
		return false
	}
	if len(bytes.TrimSpace(req.AnthropicThinking)) > 0 && string(bytes.TrimSpace(req.AnthropicThinking)) != "null" {
		return true
	}
	if claudeOutputConfigHasEffort(req.AnthropicOutputConfig) {
		return true
	}
	return strings.TrimSpace(req.ReasoningEffort) != "" || req.AdaptiveThinking || req.ReasoningBudget != nil ||
		req.EnableThinking != nil || req.ThinkingBudget != nil
}

func claudeOutputConfigHasEffort(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false
	}
	effort, ok := decoded["effort"]
	return ok && string(bytes.TrimSpace(effort)) != "null"
}

// claudeRequestForcesTool reports tool_choice any/required or a named tool.
func claudeRequestForcesTool(req *InternalLLMRequest) bool {
	if req == nil || req.ToolChoice == nil {
		return false
	}
	if req.ToolChoice.NamedToolChoice != nil {
		return true
	}
	if req.ToolChoice.ToolChoice != nil {
		switch strings.ToLower(strings.TrimSpace(*req.ToolChoice.ToolChoice)) {
		case "any", "required":
			return true
		}
	}
	return false
}

// ApplyClaudeDefaultEffort fills adaptive thinking + output_config.effort on a request whose
// client expressed no thinking intent. level is the configured value (none/low/medium/high/
// xhigh/max). It returns the effort actually filled ("" when nothing was filled) and, when
// nothing was filled, the reason. Callers own the scope decision (Anthropic channel, not a real
// Claude Code caller); this function only owns the "client wins" rule and the 400 guards.
func ApplyClaudeDefaultEffort(req *InternalLLMRequest, level string) (filled string, skip string) {
	if req == nil {
		return "", ClaudeEffortSkipClientOwned
	}
	level = strings.ToLower(strings.TrimSpace(level))
	if level == ClaudeDefaultEffortNone {
		return "", ClaudeEffortSkipSettingNone
	}
	if ClaudeRequestHasThinkingIntent(req) {
		return "", ClaudeEffortSkipClientOwned
	}
	switch level {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return "", ClaudeEffortSkipInvalidConfig
	}
	effort := ClampClaudeEffort(req.Model, level)
	if effort == "" {
		return "", ClaudeEffortSkipModel
	}
	// Thinking-on rejects / drops custom sampling on several models; filling thinking would
	// either 400 or silently drop the client's own temperature/top_p. Leave such requests alone.
	if req.Temperature != nil || req.TopP != nil || req.TopK != nil {
		return "", ClaudeEffortSkipSampling
	}
	if claudeRequestForcesTool(req) {
		return "", ClaudeEffortSkipForcedTool
	}
	// Same internal shape the adaptive outbound path already renders:
	// thinking {"type":"adaptive"} + output_config {"effort":<level>}. attemptBase restores
	// both fields per attempt, so a fallback hop to a non-Claude channel never sees them.
	req.ReasoningEffort = effort
	req.AdaptiveThinking = true
	return effort, ""
}

// ClaudeThinkingAcceptsClearThinking reports whether a thinking type allows the
// clear_thinking_20251015 context-management strategy. Anthropic rejects it otherwise:
// "`clear_thinking_20251015` strategy requires `thinking` to be enabled or adaptive"
// (CLIProxyAPI claude_executor_cloaking.go claudeThinkingAcceptsClearThinking).
func ClaudeThinkingAcceptsClearThinking(thinkingType string) bool {
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}
