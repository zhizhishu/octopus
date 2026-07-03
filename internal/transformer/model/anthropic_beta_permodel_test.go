package model

import (
	"reflect"
	"strings"
	"testing"
)

// A haiku downstream that does NOT carry its own beta must SYNTHESISE the reduced
// per-model set (claude-code near the end, advisor-tool present, no
// mid-conversation-system / effort), NOT the flagship 7-set — a fixed flagship set on a
// haiku request is a per-model tell AnyRouter rejects (2026-07-02 wire A/B).
func TestSynthesizedBetaIsPerModelHaiku(t *testing.T) {
	got := BuildClaudeCodeBetaHeader("claude-haiku-4-5-20251001", false, nil, nil)
	want := []string{
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"claude-code-20250219",
		"advisor-tool-2026-03-01",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("haiku synthesised beta mismatch:\n got=%v\nwant=%v", got, want)
	}
	for _, banned := range []string{"mid-conversation-system-2026-04-07", "effort-2025-11-24"} {
		if containsString(got, banned) {
			t.Fatalf("haiku set must not carry flagship-only beta %q: %v", banned, got)
		}
	}
}

// A flagship / unknown model still synthesises the full flagship set — behaviour for
// opus/sonnet/fable is unchanged by the per-model split.
func TestSynthesizedBetaFlagshipUnchanged(t *testing.T) {
	for _, m := range []string{"claude-opus-4-8", "claude-sonnet-5", "claude-fable-5", ""} {
		got := BuildClaudeCodeBetaHeader(m, false, nil, nil)
		if !reflect.DeepEqual(got, AnthropicClaudeCodeBetas(false)) {
			t.Fatalf("flagship model %q must synthesise the flagship set: got=%v", m, got)
		}
	}
}

// A genuine claude-cli's OWN beta is preserved verbatim regardless of model — the
// per-model synthesis only fires when the downstream sent no beta.
func TestClientBetaPreservedRegardlessOfModel(t *testing.T) {
	client := []string{"interleaved-thinking-2025-05-14", "claude-code-20250219", "advisor-tool-2026-03-01"}
	for _, m := range []string{"claude-haiku-4-5-20251001", "claude-opus-4-8"} {
		got := BuildClaudeCodeBetaHeader(m, false, client, nil)
		if !reflect.DeepEqual(got, client) {
			t.Fatalf("client beta must be preserved verbatim for model %q:\n got=%v\nwant=%v", m, got, client)
		}
	}
}

// Even a (practically unreachable) haiku[1m] synthesis must not leave context-1m at
// position 1 and must include it exactly once.
func TestSynthesizedHaikuOneMillionSlot(t *testing.T) {
	got := BuildClaudeCodeBetaHeader("claude-haiku-4-5[1m]", true, nil, nil)
	if len(got) == 0 || strings.EqualFold(got[0], AnthropicOneMillionBeta) {
		t.Fatalf("context-1m must not sit at position 1: %v", got)
	}
	count := 0
	for _, b := range got {
		if strings.EqualFold(b, AnthropicOneMillionBeta) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("context-1m must appear exactly once, got %d: %v", count, got)
	}
}
