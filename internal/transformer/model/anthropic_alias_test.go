package model

import "testing"

func TestNormalizeAnthropicModelAlias(t *testing.T) {
	tests := map[string]string{
		"opus[1m]":               "claude-opus-4-8",
		"claude-opus-4-7":        "claude-opus-4-8",
		"claude-opus-4-7[1m]":    "claude-opus-4-8",
		"claude-opus-4-8[1m]":    "claude-opus-4-8",
		"claude-sonnet-4-5[1m]":  "claude-sonnet-4-5",
		"claude-sonnet-4-5":      "claude-sonnet-4-5",
		"custom-claude-opus-4-7": "custom-claude-opus-4-7",
	}
	for input, want := range tests {
		if got := NormalizeAnthropicModelAlias(input); got != want {
			t.Fatalf("NormalizeAnthropicModelAlias(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAnthropicModelAliasCandidatesPreferFullOneMillionAlias(t *testing.T) {
	tests := map[string][]string{
		"opus[1m]":               {"claude-opus-4-8[1m]", "claude-opus-4-8"},
		"claude-opus-4-8[1m]":    {"claude-opus-4-8", "opus[1m]"},
		"claude-opus-4-7[1m]":    {"claude-opus-4-8[1m]", "claude-opus-4-8", "opus[1m]"},
		"claude-opus-4-7":        {"claude-opus-4-8"},
		"claude-sonnet-4-5[1m]":  {"claude-sonnet-4-5"},
		"claude-sonnet-4-5":      nil,
		"custom-claude-opus-4-7": nil,
	}
	for input, want := range tests {
		got := AnthropicModelAliasCandidates(input)
		if len(got) != len(want) {
			t.Fatalf("AnthropicModelAliasCandidates(%q) = %#v, want %#v", input, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("AnthropicModelAliasCandidates(%q) = %#v, want %#v", input, got, want)
			}
		}
	}
}

func TestAnthropicRequestWantsOneMillionBetaSurvivesNormalizedModel(t *testing.T) {
	req := &InternalLLMRequest{Model: "claude-opus-4-8"}
	if AnthropicRequestWantsOneMillionBeta(req) {
		t.Fatalf("plain claude-opus-4-8 should not imply 1m beta")
	}
	req.TransformOptions.AnthropicOneMillionBeta = true
	if !AnthropicRequestWantsOneMillionBeta(req) {
		t.Fatalf("explicit transform option should keep 1m beta after model normalization")
	}
}

func TestAnthropicClaudeCodeBetasMirrorCurrentCLIShape(t *testing.T) {
	plain := AnthropicClaudeCodeBetas(false)
	oneMillion := AnthropicClaudeCodeBetas(true)
	for _, beta := range []string{"claude-code-20250219", "context-management-2025-06-27", "effort-2025-11-24"} {
		if !containsString(plain, beta) || !containsString(oneMillion, beta) {
			t.Fatalf("expected beta %q in both beta sets, plain=%#v oneMillion=%#v", beta, plain, oneMillion)
		}
	}
	if containsString(plain, AnthropicOneMillionBeta) {
		t.Fatalf("plain beta set should not include %q: %#v", AnthropicOneMillionBeta, plain)
	}
	if !containsString(oneMillion, AnthropicOneMillionBeta) {
		t.Fatalf("1m beta set should include %q: %#v", AnthropicOneMillionBeta, oneMillion)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
