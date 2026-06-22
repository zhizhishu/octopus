package model

import (
	"reflect"
	"strings"
	"testing"
)

// The outbound transformer leaves a lone context-1m prepended on the header; the
// shared helper must rebuild to the canonical wire order (context-1m in its real
// slot), NOT leave it stuck at position 1 — that stray-leading-1m is the non-CLI
// tell that let strict upstreams identify a channel test as not-real traffic.
func TestBuildClaudeCodeBetaOrderMatchesCanonicalAndDropsStray1M(t *testing.T) {
	existing := append([]string{AnthropicOneMillionBeta}, anthropicClaudeCodeBaseBetas...)
	got := BuildClaudeCodeBetaOrder(true, existing, []string{AnthropicOneMillionBeta})
	want := AnthropicClaudeCodeBetas(true)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("1M beta order must equal canonical:\n got=%v\nwant=%v", got, want)
	}
	if len(got) == 0 || strings.EqualFold(got[0], AnthropicOneMillionBeta) {
		t.Fatalf("context-1m must not sit at position 1 (non-CLI tell): %v", got)
	}
}

// Extras a real client sent that are not part of the canonical set are appended
// after, de-duplicated; context-1m is never emitted via the extras path.
func TestBuildClaudeCodeBetaOrderAppendsExtrasOnce(t *testing.T) {
	existing := []string{"custom-beta-2025-01-01", "claude-code-20250219", "custom-beta-2025-01-01"}
	got := BuildClaudeCodeBetaOrder(false, existing, []string{"another-extra-2025", AnthropicOneMillionBeta})
	want := append(AnthropicClaudeCodeBetas(false), "custom-beta-2025-01-01", "another-extra-2025")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extras handling:\n got=%v\nwant=%v", got, want)
	}
}

// metadata.user_id must serialise in the real Claude Code key order
// (device_id, account_uuid, session_id), NOT alphabetical (a Go map tell).
func TestBuildClaudeMetadataUserIDGoldenKeyOrder(t *testing.T) {
	got := BuildClaudeMetadataUserID("dev123", "sess456")
	want := `{"device_id":"dev123","account_uuid":"","session_id":"sess456"}`
	if got != want {
		t.Fatalf("metadata.user_id shape mismatch:\n got=%s\nwant=%s", got, want)
	}
	if strings.Index(got, "device_id") > strings.Index(got, "account_uuid") {
		t.Fatalf("device_id must precede account_uuid (golden order, not alphabetical): %s", got)
	}
}

// context-1m must never appear when 1M is not wanted, even if upstream/transform
// betas carry it.
func TestBuildClaudeCodeBetaOrderNo1MWhenNotWanted(t *testing.T) {
	got := BuildClaudeCodeBetaOrder(false, []string{AnthropicOneMillionBeta}, []string{AnthropicOneMillionBeta})
	for _, b := range got {
		if strings.EqualFold(b, AnthropicOneMillionBeta) {
			t.Fatalf("context-1m must not appear when 1M not wanted: %v", got)
		}
	}
	if !reflect.DeepEqual(got, AnthropicClaudeCodeBetas(false)) {
		t.Fatalf("non-1M result must equal canonical base: %v", got)
	}
}
