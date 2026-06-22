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

// BuildClaudeCodeBetaOrder returns anthropic-beta values in the exact wire order a
// genuine claude-cli sends: the canonical claude-code set is authoritative (with
// context-1m-2025-08-07 in its real slot only when 1M is wanted), and any EXTRA
// betas carried on the existing header or in transform options that are not part
// of the canonical set are appended after, de-duplicated. context-1m is NEVER
// emitted as a stray leading beta (a non-CLI tell). Both the relay forward path
// and the channel/model test path MUST build the header through this single helper
// so a channel test is byte-for-byte identical to real traffic — they previously
// had divergent copies and the test left context-1m stuck at position 1.
func BuildClaudeCodeBetaOrder(wantsOneMillion bool, existing []string, transformBetas []string) []string {
	canonical := AnthropicClaudeCodeBetas(wantsOneMillion)
	inCanonical := make(map[string]bool, len(canonical))
	for _, b := range canonical {
		inCanonical[strings.ToLower(strings.TrimSpace(b))] = true
	}
	result := make([]string, 0, len(canonical)+len(existing)+len(transformBetas))
	result = append(result, canonical...)
	seenExtra := make(map[string]bool)
	addExtras := func(list []string) {
		for _, b := range list {
			b = strings.TrimSpace(b)
			if b == "" || inCanonical[strings.ToLower(b)] || strings.EqualFold(b, AnthropicOneMillionBeta) {
				continue
			}
			key := strings.ToLower(b)
			if seenExtra[key] {
				continue
			}
			seenExtra[key] = true
			result = append(result, b)
		}
	}
	addExtras(existing)
	addExtras(transformBetas)
	return result
}

// BuildClaudeMetadataUserID serialises metadata.user_id exactly as a real Claude
// Code client does: compact (no spaces — AnyRouter rejects the spaced form) with
// key order device_id, account_uuid, session_id. Both the relay forward path and
// the channel/model test path build it through this one helper so the byte shape
// is identical; a Go map marshalled to JSON sorts keys alphabetically, which is a
// non-CLI tell. deviceID must be a 64-hex string like a genuine install reports.
func BuildClaudeMetadataUserID(deviceID, sessionID string) string {
	return `{"device_id":"` + deviceID + `","account_uuid":"","session_id":"` + sessionID + `"}`
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
