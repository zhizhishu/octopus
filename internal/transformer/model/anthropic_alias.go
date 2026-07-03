package model

import "strings"

const AnthropicOneMillionBeta = "context-1m-2025-08-07"

// Beta sets mirror the real Claude CLI (claude-cli/2.1.198) anthropic-beta header
// captured on the wire from a genuine `claude -p` request (flagship model, e.g.
// claude-opus-4-8 / claude-sonnet-5); the order matches the wire exactly so AnyRouter
// shape checks see an authentic Claude Code request. 2.1.198 DROPPED advisor-tool and
// structured-outputs from the declared set that 2.1.168 carried — sending either is a
// stale-client tell — and no longer sends thinking-token-count/mid-conversation-system
// as optional: the flagship set is these seven. NOTE: real 2.1.198 varies this set by
// model AND request type (haiku/older-sonnet send a reduced set with claude-code-20250219
// last; a title/structured-output probe drops claude-code and carries structured-outputs).
// A 2026-07-02 wire A/B against AnyRouter DISPROVED the earlier assumption that its shape
// check is not per-model strict: a fixed flagship 7-set on a haiku request is rejected.
// So this canonical set is now used ONLY as the synthesis fallback for non-claude
// downstreams / the channel test; a genuine claude-cli's own per-model beta is preserved
// verbatim — see BuildClaudeCodeBetaHeader.
var anthropicClaudeCodeBaseBetas = []string{
	"claude-code-20250219",
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"effort-2025-11-24",
}

// One-million variant inserts context-1m-2025-08-07 in the exact wire position the real
// 2.1.198 CLI uses: position 2, immediately after claude-code-20250219 (captured from a
// genuine claude-opus-4-8[1m] request), NOT after mid-conversation-system as 2.1.168 did.
var anthropicClaudeCodeOneMillionBetas = []string{
	"claude-code-20250219",
	AnthropicOneMillionBeta,
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"effort-2025-11-24",
}

// Haiku (and any model the real 2.1.198 CLI sends a reduced anthropic-beta set for)
// carries a DIFFERENT set than the flagship models, captured on the wire from a genuine
// `claude -p` haiku agentic request (2026-07-02 wire A/B, _artifacts/wire-capture): it
// drops mid-conversation-system/effort, KEEPS advisor-tool-2026-03-01, and moves
// claude-code-20250219 toward the end. A fixed flagship 7-set on a haiku request was
// rejected by AnyRouter's per-model shape check, so the synthesis fallback for a haiku
// downstream must use THIS set. (A genuine claude-cli's own beta is still preserved
// verbatim by BuildClaudeCodeBetaHeader upstream of this — this only backs the
// non-claude / channel-test synthesis.)
var anthropicClaudeCodeHaikuBetas = []string{
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"claude-code-20250219",
	"advisor-tool-2026-03-01",
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

// isReducedBetaModel reports whether modelName is a Claude model the real 2.1.198 CLI
// sends the REDUCED anthropic-beta set for (currently Haiku), matched on the id
// containing "haiku" (case-insensitive). Flagship opus/sonnet/fable keep the full set.
// Deliberately conservative: only "haiku" is confirmed by wire capture, so older-sonnet
// is NOT matched here without its own capture.
func isReducedBetaModel(modelName string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(modelName)), "haiku")
}

// claudeCodeSynthesisBetas picks the canonical anthropic-beta base set to SYNTHESISE for
// a downstream that did not send its own beta (a non-claude client under cloak, or the
// channel/model test). Per-model: Haiku gets the reduced set, everything else the
// flagship set. When 1M is wanted, context-1m-2025-08-07 is inserted in its real wire
// slot (immediately after claude-code-20250219), never as a stray leading beta.
func claudeCodeSynthesisBetas(modelName string, wantsOneMillion bool) []string {
	if isReducedBetaModel(modelName) {
		base := make([]string, len(anthropicClaudeCodeHaikuBetas))
		copy(base, anthropicClaudeCodeHaikuBetas)
		if wantsOneMillion {
			return insertOneMillionAfterClaudeCode(base)
		}
		return base
	}
	return AnthropicClaudeCodeBetas(wantsOneMillion)
}

// insertOneMillionAfterClaudeCode returns betas with context-1m-2025-08-07 inserted
// immediately after claude-code-20250219 (its genuine wire slot). If claude-code is
// absent it goes after the first entry, never at position 1. If context-1m is already
// present the input is returned unchanged.
func insertOneMillionAfterClaudeCode(betas []string) []string {
	for _, b := range betas {
		if strings.EqualFold(strings.TrimSpace(b), AnthropicOneMillionBeta) {
			return betas
		}
	}
	out := make([]string, 0, len(betas)+1)
	inserted := false
	for _, b := range betas {
		out = append(out, b)
		if !inserted && strings.EqualFold(strings.TrimSpace(b), "claude-code-20250219") {
			out = append(out, AnthropicOneMillionBeta)
			inserted = true
		}
	}
	if inserted {
		return out
	}
	if len(out) == 0 {
		return []string{AnthropicOneMillionBeta}
	}
	res := make([]string, 0, len(out)+1)
	res = append(res, out[0], AnthropicOneMillionBeta)
	res = append(res, out[1:]...)
	return res
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
func BuildClaudeCodeBetaOrder(modelName string, wantsOneMillion bool, existing []string, transformBetas []string) []string {
	canonical := claudeCodeSynthesisBetas(modelName, wantsOneMillion)
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

// BuildClaudeCodeBetaHeader decides how to present the outbound anthropic-beta
// header. A genuine claude-cli DOWNSTREAM already sends its own anthropic-beta on
// the wire (the relay forward path copies it onto the outbound request via
// copyHeaders), and that set+order is the exact per-model, per-request-type shape
// AnyRouter shape-checks: the real 2.1.198 CLI varies it by model AND request type
// (a haiku agentic turn carries advisor-tool with claude-code-20250219 near the end;
// a title/structured-output probe carries structured-outputs and no claude-code at
// all). No STATIC canonical set can match that, so a wire A/B against AnyRouter showed
// a fixed flagship 7-set on a haiku request is rejected while the real per-model beta
// passes.
//
// Rule: if `existing` carries any beta OTHER THAN the auto-inserted context-1m marker,
// it came from a real claude-cli downstream — PRESERVE it verbatim (order intact),
// appending only unseen transformBetas at the tail. Otherwise (a non-claude downstream
// under cloak, or the synthetic channel/model test, which never carries a client beta)
// fall back to the canonical synthesis via BuildClaudeCodeBetaOrder. Both the relay
// forward path and the channel/model test path route through this one helper so they
// stay byte-for-byte identical.
func BuildClaudeCodeBetaHeader(modelName string, wantsOneMillion bool, existing []string, transformBetas []string) []string {
	hasClientBeta := false
	for _, b := range existing {
		b = strings.TrimSpace(b)
		if b == "" || strings.EqualFold(b, AnthropicOneMillionBeta) {
			continue
		}
		hasClientBeta = true
		break
	}
	if !hasClientBeta {
		return BuildClaudeCodeBetaOrder(modelName, wantsOneMillion, existing, transformBetas)
	}
	result := make([]string, 0, len(existing)+len(transformBetas))
	seen := make(map[string]bool)
	add := func(list []string) {
		for _, b := range list {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			key := strings.ToLower(b)
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, b)
		}
	}
	add(existing)
	add(transformBetas)
	return result
}

// StripClaudeBetaFlags removes the flags named in stripCSV (a comma-separated list)
// from betas, preserving the order of the surviving entries. stripCSV entries are
// trimmed of surrounding whitespace and empty entries are skipped; matching is exact
// (case-sensitive, on the trimmed beta value). This backs the opt-in
// SettingKeyClaudeBetaStripFlags escape hatch: when stripCSV is empty (the default),
// betas is returned unchanged so the genuine claude-cli anthropic-beta is forwarded
// verbatim. It is a pure function so both the relay forward path and the modeltest
// mirror can call it and stay byte-for-byte identical.
func StripClaudeBetaFlags(betas []string, stripCSV string) []string {
	if strings.TrimSpace(stripCSV) == "" {
		return betas
	}
	strip := make(map[string]bool)
	for _, flag := range strings.Split(stripCSV, ",") {
		flag = strings.TrimSpace(flag)
		if flag == "" {
			continue
		}
		strip[flag] = true
	}
	if len(strip) == 0 {
		return betas
	}
	result := make([]string, 0, len(betas))
	for _, beta := range betas {
		if strip[strings.TrimSpace(beta)] {
			continue
		}
		result = append(result, beta)
	}
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
