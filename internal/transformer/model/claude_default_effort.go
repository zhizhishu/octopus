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

// ClaudeEffortCapability describes what a Claude model accepts for thinking.
//
// Adaptive/XHigh/Max drive the "默认思考强度" fill (unchanged). The remaining fields are the
// adaptive-only requirements the Anthropic outbound must honour when it CONVERTS a request
// (OpenAI Chat / Responses / Gemini -> Anthropic, or a non-CLI native caller), because the
// upstream answers 400 otherwise, e.g. sub2api validateClaude55Request:
// "claude-opus-5-5 requires adaptive thinking; omit thinking or use thinking.type=adaptive and
// output_config.effort".
type ClaudeEffortCapability struct {
	Adaptive bool
	XHigh    bool
	Max      bool
	// NoManual: {"type":"enabled","budget_tokens":N} is rejected; thinking must be adaptive
	// with output_config.effort (new-api supportsManual=false).
	NoManual bool
	// NoDisable: {"type":"disabled"} is rejected; the lowest representable setting is adaptive
	// at effort low (new-api supportsDisable=false).
	NoDisable bool
	// DisableAsBetweenTools: the model's "thinking off" is {"type":"between_tools"} instead of
	// disabled (sonnet-5-5), and that mode takes effort high or below only.
	DisableAsBetweenTools bool
	// OffEffortAtMostHigh: while thinking is off (disabled / between_tools) the model accepts
	// effort high or below only (opus-5*, haiku-5*, sonnet-5-5).
	OffEffortAtMostHigh bool
	// DefaultEffort is the effort the model runs at when the request names none (Anthropic
	// effort doc): medium on opus-5-5 and haiku-5*, high elsewhere; "" for non-adaptive models.
	DefaultEffort string
}

// NormalizeClaudeModelID lowercases a model id and strips provider prefixes / suffixes so the
// capability table can match by prefix: "anthropic/claude-opus-4.7" -> "claude-opus-4-7",
// "us.anthropic.claude-sonnet-4-6" -> "claude-sonnet-4-6", "claude-opus-4-7@20260101" ->
// "claude-opus-4-7", "claude-opus-4-7[1m]" -> "claude-opus-4-7", "claude-opus-5.5" ->
// "claude-opus-5-5" (OpenRouter-style dotted minor), "claude-opus-5-5-thinking" ->
// "claude-opus-5-5" (the -thinking alias suffix some relays expose; sub2api
// normalizeEffortModelID strips it the same way, so it must not hide an adaptive-only model).
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
	m = strings.TrimSuffix(m, "-thinking")
	return strings.ReplaceAll(m, ".", "-")
}

// ClaudeEffortCapabilityFor returns the thinking capability of a (normalized or raw) model id.
// Unknown and non-Claude models report the zero value: nothing is filled and nothing is
// rewritten for them (they keep the legacy enabled+budget_tokens conversion).
//
// Requirement decisions (mirror new-api relaykit/relayconvert/reasoning/claude.go
// claudeCapabilitiesFor, cross-checked with sub2api validateClaude55Request /
// sanitizeBedrockThinking; order matters because prefixes nest, e.g. claude-opus-5-5 before
// claude-opus-5):
//
//	fable-5, mythos-5          adaptive only, cannot disable          (xhigh, max)
//	mythos-preview             adaptive + manual, cannot disable      (max)
//	opus-5-5                   adaptive only, cannot disable, default medium   (xhigh, max)
//	sonnet-5-5                 adaptive only; API also accepts between_tools as "off", but
//	                           real Claude Code never sends it — CLI/-p uses adaptive+omitted
//	                           (effort <= high when off) (xhigh, max)
//	opus-5, sonnet-5, haiku-5, adaptive only, disabled allowed        (xhigh, max)
//	opus-4-8, opus-4-7           (opus-5*/haiku-5* only at effort <= high when off;
//	                              haiku-5* default medium)
//	opus-4-6, sonnet-4-6       adaptive + manual                      (max)
//	everything else            manual (enabled+budget_tokens) only
func ClaudeEffortCapabilityFor(model string) ClaudeEffortCapability {
	m := NormalizeClaudeModelID(model)
	has := func(prefixes ...string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(m, prefix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("claude-fable-5", "claude-mythos-5"):
		return ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true, NoManual: true, NoDisable: true, DefaultEffort: "high"}
	case has("claude-mythos-preview"):
		return ClaudeEffortCapability{Adaptive: true, Max: true, NoDisable: true, DefaultEffort: "high"}
	case has("claude-opus-5-5"):
		return ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true, NoManual: true, NoDisable: true, DefaultEffort: "medium"}
	case has("claude-sonnet-5-5"):
		return ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true, NoManual: true, NoDisable: true,
			DisableAsBetweenTools: true, OffEffortAtMostHigh: true, DefaultEffort: "high"}
	case has("claude-opus-5", "claude-haiku-5"):
		effort := "high"
		if strings.HasPrefix(m, "claude-haiku-5") {
			effort = "medium"
		}
		return ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true, NoManual: true, OffEffortAtMostHigh: true, DefaultEffort: effort}
	case has("claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7"):
		return ClaudeEffortCapability{Adaptive: true, XHigh: true, Max: true, NoManual: true, DefaultEffort: "high"}
	case has("claude-opus-4-6", "claude-sonnet-4-6"):
		return ClaudeEffortCapability{Adaptive: true, Max: true, DefaultEffort: "high"}
	}
	// claude-opus-4-5, claude-sonnet-4-5, claude-haiku-4-5, claude-opus-4-1 and earlier,
	// claude-3*: extended thinking only — {"type":"adaptive"} returns 400.
	return ClaudeEffortCapability{}
}

// ClaudeAdaptiveEffortFromBudget converts a manual thinking budget into the adaptive effort an
// adaptive-only model accepts: <=1024 low, <=8192 medium, otherwise high (new-api
// reasoning.EffortFromBudget), then clamped to what the model takes. A budget is a token CAP,
// not a request for the xhigh/max cost tiers, so it never maps above high even though
// EffortFromBudget's wider table would. Returns "" for a non-positive budget or a model that
// takes no adaptive effort.
func ClaudeAdaptiveEffortFromBudget(model string, budget int64) string {
	effort := EffortFromBudget(budget)
	switch effort {
	case "":
		return ""
	case "xhigh", "max":
		effort = "high"
	}
	return ClampClaudeEffort(model, effort)
}

// ClaudeThinkingOff describes how to say "no thinking" to a model, for callers that must send an
// explicit thinking object (model-test probes mirroring the CLI /-p print-mode shape). Thinking
// is the raw thinking member; OutputConfig is the raw output_config to pair with it (nil = none).
//
// Real Claude Code never sends between_tools (wire capture 2.1.316). On sonnet-5-5 / opus-5-5 /
// fable / mythos it says "off" as adaptive + display omitted + effort (medium by default, or
// low via --effort low). Probes use low — a CLI-producible value — so the body stays
// CLI-plausible without inventing a non-CLI type.
//
//	default (disabled allowed)  {"type":"disabled"}
//	sonnet-5-5 / cannot disable {"type":"adaptive","display":"omitted"} + {"effort":"low"}
func ClaudeThinkingOff(model string) (thinking, outputConfig json.RawMessage) {
	capability := ClaudeEffortCapabilityFor(model)
	switch {
	case capability.DisableAsBetweenTools, capability.NoDisable:
		return json.RawMessage(`{"type":"adaptive","display":"omitted"}`), json.RawMessage(`{"effort":"low"}`)
	default:
		return json.RawMessage(`{"type":"disabled"}`), nil
	}
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
