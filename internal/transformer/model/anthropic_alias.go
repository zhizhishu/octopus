package model

import "strings"

const AnthropicOneMillionBeta = "context-1m-2025-08-07"

// Beta sets mirror the real Claude CLI (claude-cli/2.1.168) anthropic-beta header
// captured in .codex-runtime/claude-cli-shape-capture/.../capture.summary.safe.json;
// the order matches the wire exactly so AnyRouter shape checks see an authentic
// Claude Code request. Do not drop thinking-token-count / mid-conversation-system:
// the real CLI sends both.
var anthropicClaudeCodeBaseBetas = []string{
	"claude-code-20250219",
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"advisor-tool-2026-03-01",
	"effort-2025-11-24",
	"structured-outputs-2025-12-15",
}

// One-million variant inserts context-1m-2025-08-07 in the same wire position the
// real CLI uses (after mid-conversation-system, before advisor-tool).
var anthropicClaudeCodeOneMillionBetas = []string{
	"claude-code-20250219",
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	AnthropicOneMillionBeta,
	"advisor-tool-2026-03-01",
	"effort-2025-11-24",
	"structured-outputs-2025-12-15",
}

func AnthropicClaudeCodeBetas(wantsOneMillion bool) []string {
	source := anthropicClaudeCodeBaseBetas
	if wantsOneMillion {
		source = anthropicClaudeCodeOneMillionBetas
	}
	result := make([]string, len(source))
	copy(result, source)
	return result
}

// NormalizeAnthropicModelAlias maps client-facing Claude/Claude Code shortcuts
// to the provider model id that should be sent upstream. In particular, current
// AnyRouter exposes/logs claude-opus-4-8, while Claude Code users commonly pass
// opus[1m] and older configs may still point at claude-opus-4-7.
func NormalizeAnthropicModelAlias(modelName string) string {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return trimmed
	}
	lower := strings.ToLower(trimmed)
	noOneMillionSuffix := strings.TrimSpace(strings.ReplaceAll(lower, "[1m]", ""))

	switch noOneMillionSuffix {
	case "opus", "claude-opus", "claude-opus-4-7", "claude-opus-4.7", "claude-opus-4-8", "claude-opus-4.8":
		return "claude-opus-4-8"
	default:
		if strings.Contains(lower, "[1m]") && strings.HasPrefix(noOneMillionSuffix, "claude-") {
			return noOneMillionSuffix
		}
		return trimmed
	}
}

func AnthropicModelAliasCandidates(modelName string) []string {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return nil
	}
	normalized := NormalizeAnthropicModelAlias(trimmed)
	lower := strings.ToLower(trimmed)
	wantsOneMillion := AnthropicModelWantsOneMillionBeta(trimmed)

	candidates := make([]string, 0, 4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, trimmed) {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		candidates = append(candidates, value)
	}

	// Prefer the explicit full model+[1m] spelling for route/group lookup when
	// users or fetched catalogs only expose the old Claude Code shortcut.
	if wantsOneMillion && (strings.Contains(lower, "opus") || strings.Contains(lower, "claude-opus-4-7") || strings.Contains(lower, "claude-opus-4-8")) {
		if strings.EqualFold(trimmed, "opus[1m]") || strings.Contains(lower, "claude-opus-4-7") {
			add("claude-opus-4-8[1m]")
		}
	}
	add(normalized)
	if wantsOneMillion && strings.Contains(lower, "claude-opus") {
		add("opus[1m]")
	}
	return candidates
}

func AnthropicModelWantsOneMillionBeta(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	if !strings.Contains(lower, "[1m]") {
		return false
	}
	return strings.Contains(lower, "claude") || strings.Contains(lower, "opus")
}

func AnthropicRequestWantsOneMillionBeta(req *InternalLLMRequest) bool {
	if req == nil {
		return false
	}
	return req.TransformOptions.AnthropicOneMillionBeta || AnthropicModelWantsOneMillionBeta(req.Model)
}
