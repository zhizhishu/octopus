package model

import (
	"reflect"
	"testing"
)

// A haiku downstream that does NOT carry its own beta must SYNTHESISE the reduced
// per-model set (claude-code near the end, advisor-tool present, no
// mid-conversation-system / effort), NOT the flagship 7-set — a fixed flagship set on a
// haiku request is a per-model tell the relay rejects (2026-07-02 wire A/B).
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

// A genuine claude-cli FLAGSHIP request (opus/fable) with 1M wanted must have
// context-1m inserted into its preserved beta at slot 2 (immediately after
// claude-code) so it clears the relay's 1M gate while matching a real 1M-enabled
// CLI. Haiku (the reduced model) is left verbatim with no context-1m.
func TestClientBetaFlagshipGetsOneMillionHaikuDoesNot(t *testing.T) {
	opusClient := []string{
		"claude-code-20250219",
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07",
		"advisor-tool-2026-03-01",
		"effort-2025-11-24",
	}
	wantOpus := []string{
		"claude-code-20250219",
		AnthropicOneMillionBeta,
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07",
		"advisor-tool-2026-03-01",
		"effort-2025-11-24",
	}
	if got := BuildClaudeCodeBetaHeader("claude-opus-4-8", true, opusClient, nil); !reflect.DeepEqual(got, wantOpus) {
		t.Fatalf("opus 1M beta mismatch:\n got=%v\nwant=%v", got, wantOpus)
	}

	haikuClient := []string{
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"claude-code-20250219",
		"advisor-tool-2026-03-01",
	}
	if got := BuildClaudeCodeBetaHeader("claude-haiku-4-5-20251001", true, haikuClient, nil); !reflect.DeepEqual(got, haikuClient) {
		t.Fatalf("haiku 1M reduced set must stay verbatim (no context-1m):\n got=%v\nwant=%v", got, haikuClient)
	}
}

// Haiku never carries context-1m. This IS reachable: a channel with the 1M switch
// on (TransformOptions.AnthropicOneMillionBeta) serving a haiku request from a
// non-CLI client used to synthesise the haiku set + context-1m, which real Claude
// Code never sends. The synthesis must now be the plain haiku set, both for a
// haiku[1m] id and for the channel opt-in (with or without a lone transformer 1m).
func TestSynthesizedHaikuNeverGetsOneMillion(t *testing.T) {
	for _, existing := range [][]string{nil, {""}, {AnthropicOneMillionBeta}} {
		for _, model := range []string{"claude-haiku-4-5[1m]", "claude-haiku-4-5-20251001"} {
			got := BuildClaudeCodeBetaHeader(model, true, existing, nil)
			if !reflect.DeepEqual(got, anthropicClaudeCodeHaikuBetas) {
				t.Fatalf("%s existing=%v: haiku synthesis must be the plain haiku set:\n got=%v\nwant=%v", model, existing, got, anthropicClaudeCodeHaikuBetas)
			}
		}
	}
}
