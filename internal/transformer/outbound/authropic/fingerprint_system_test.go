package authropic

import (
	"strings"
	"testing"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// sysMsg builds an internal system message the way the inbound Anthropic
// transformer does: a string-system becomes one system message, an array-system
// becomes one system message per array element (each carrying the element text).
func sysMsg(text string) model.Message {
	return model.Message{Role: "system", Content: model.MessageContent{Content: stringPtr(text)}}
}

// TestSystemFingerprintInjectionVariants pins that the Claude Code fingerprint
// (billing header + agent identity) is injected as the first two system blocks for
// every client system shape — absent, single string, and multi-block array — and
// that a genuine Claude Code request (already carrying both) passes through without
// double injection. AnyRouter risk-rejects any Anthropic request whose system lacks
// the genuine agent identity, so a gap here silently breaks non-CLI clients.
func TestSystemFingerprintInjectionVariants(t *testing.T) {
	billing := claudeBillingHeaderText()

	assertFingerprint := func(t *testing.T, parts []model.Message, wantTail []string) {
		t.Helper()
		req := &model.InternalLLMRequest{Model: "claude-opus-4-8", Messages: parts}
		got := convertToAnthropicRequest(req)
		if got.System == nil {
			t.Fatalf("system is nil")
		}
		mp := got.System.MultiplePrompts
		if len(mp) != 2+len(wantTail) {
			t.Fatalf("expected billing+identity+%d tail = %d parts, got %d: %#v", len(wantTail), 2+len(wantTail), len(mp), mp)
		}
		if !strings.HasPrefix(strings.TrimSpace(mp[0].Text), claudeBillingHeaderPrefix) {
			t.Fatalf("part[0] is not the billing header: %q", mp[0].Text)
		}
		if mp[1].Text != claudeAgentIdentityText {
			t.Fatalf("part[1] is not the agent identity: %q", mp[1].Text)
		}
		for i, want := range wantTail {
			if mp[2+i].Text != want {
				t.Fatalf("tail part[%d] = %q, want %q", i, mp[2+i].Text, want)
			}
		}
		// Never inject the fingerprint twice.
		if c := countText(mp, claudeAgentIdentityText); c != 1 {
			t.Fatalf("agent identity appears %d times, want exactly 1", c)
		}
		if c := countPrefix(mp, claudeBillingHeaderPrefix); c != 1 {
			t.Fatalf("billing header appears %d times, want exactly 1", c)
		}
	}

	t.Run("no_system", func(t *testing.T) {
		assertFingerprint(t, []model.Message{
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}, nil)
	})

	t.Run("single_string_system", func(t *testing.T) {
		assertFingerprint(t, []model.Message{
			sysMsg("You are a helpful assistant."),
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}, []string{"You are a helpful assistant."})
	})

	t.Run("multi_block_array_system", func(t *testing.T) {
		// What inbound produces for system: [{text:"A"},{text:"B"}].
		assertFingerprint(t, []model.Message{
			sysMsg("A"),
			sysMsg("B"),
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}, []string{"A", "B"})
	})

	t.Run("genuine_claude_code_passthrough", func(t *testing.T) {
		// Real Claude Code already sends billing + identity first; must not double-inject.
		assertFingerprint(t, []model.Message{
			sysMsg(billing),
			sysMsg(claudeAgentIdentityText),
			sysMsg("Real CLI system prompt."),
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}, []string{"Real CLI system prompt."})
	})
}

// TestSystemFingerprintSuppressedWhenCloakOff pins that with cloak disabled
// (TransformOptions.SuppressClaudeIdentity = true, set by the relay for channel
// cloak mode "never") the synthetic billing-header + agent-identity blocks are NOT
// injected: domestic Anthropic-compatible upstreams (GLM/DeepSeek) must receive the
// request free of injected Claude identity. The client's own system blocks still pass
// through, and a request with no system yields no system field at all.
func TestSystemFingerprintSuppressedWhenCloakOff(t *testing.T) {
	suppress := func(parts []model.Message) *model.InternalLLMRequest {
		req := &model.InternalLLMRequest{Model: "glm-4.6", Messages: parts}
		req.TransformOptions.SuppressClaudeIdentity = true
		return req
	}

	t.Run("no_system_yields_nil", func(t *testing.T) {
		got := convertToAnthropicRequest(suppress([]model.Message{
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}))
		if got.System != nil {
			t.Fatalf("cloak off + no client system must yield nil system, got %#v", got.System)
		}
	})

	t.Run("client_system_passthrough_no_injection", func(t *testing.T) {
		got := convertToAnthropicRequest(suppress([]model.Message{
			sysMsg("You are a helpful assistant."),
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}))
		if got.System == nil {
			t.Fatalf("client system must pass through")
		}
		mp := got.System.MultiplePrompts
		if len(mp) != 1 || mp[0].Text != "You are a helpful assistant." {
			t.Fatalf("expected only the client system block, got %#v", mp)
		}
		if c := countPrefix(mp, claudeBillingHeaderPrefix); c != 0 {
			t.Fatalf("billing header must not be injected when cloak off, found %d", c)
		}
		if c := countText(mp, claudeAgentIdentityText); c != 0 {
			t.Fatalf("agent identity must not be injected when cloak off, found %d", c)
		}
	})

	t.Run("cloak_on_still_injects", func(t *testing.T) {
		// Sanity: the SAME messages with cloak ON (default) still get the fingerprint,
		// guarding against an accidental flip of the default behaviour.
		req := &model.InternalLLMRequest{Model: "claude-opus-4-8", Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		}}
		mp := convertToAnthropicRequest(req).System.MultiplePrompts
		if countPrefix(mp, claudeBillingHeaderPrefix) != 1 || countText(mp, claudeAgentIdentityText) != 1 {
			t.Fatalf("cloak on (default) must still inject billing+identity, got %#v", mp)
		}
	})
}

// TestClaudeBillingHeaderUsesNamedConstants pins that the billing header cc_version
// is rendered from the named fingerprint constants rather than a re-introduced inline
// literal, so a version bump is a single-constant edit. The cross-package guarantee
// that ClaudeCLIVersion equals the CLI user-agent version lives in the relay package
// (this package cannot import internal/model: cycle).
func TestClaudeBillingHeaderUsesNamedConstants(t *testing.T) {
	got := claudeBillingHeaderText()
	if want := "cc_version=" + ClaudeCLIVersion + "." + ClaudeCLIVersionSuffix + ";"; !strings.Contains(got, want) {
		t.Fatalf("billing header %q must carry %q", got, want)
	}
}

func countText(parts []anthropicModel.SystemPromptPart, text string) int {
	n := 0
	for _, p := range parts {
		if p.Text == text {
			n++
		}
	}
	return n
}

func countPrefix(parts []anthropicModel.SystemPromptPart, prefix string) int {
	n := 0
	for _, p := range parts {
		if strings.HasPrefix(strings.TrimSpace(p.Text), prefix) {
			n++
		}
	}
	return n
}

// TestFingerprintInjectionPreservesClientCacheControl pins that injecting the billing
// + agent-identity system blocks does NOT move or drop the client's prompt-cache
// breakpoint: the injected blocks carry no cache_control, and the client's
// cache_control stays on the client's own system block. This keeps UPSTREAM
// (Anthropic) prompt caching working — the cached prefix [billing, identity, system]
// is stable across requests, so the upstream recognises and reuses it (a cache hit),
// rather than the breakpoint silently shifting onto an injected block.
func TestFingerprintInjectionPreservesClientCacheControl(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model: "claude-opus-4-8",
		Messages: []model.Message{
			{
				Role:         "system",
				Content:      model.MessageContent{Content: stringPtr("big stable cacheable system prompt")},
				CacheControl: &model.CacheControl{Type: "ephemeral"},
			},
			{Role: "user", Content: model.MessageContent{Content: stringPtr("hi")}},
		},
	}
	mp := convertToAnthropicRequest(req).System.MultiplePrompts
	if len(mp) != 3 {
		t.Fatalf("expected billing + identity + client system, got %d: %#v", len(mp), mp)
	}
	if mp[0].CacheControl != nil || mp[1].CacheControl != nil {
		t.Fatalf("injected fingerprint blocks must not carry cache_control: %#v / %#v", mp[0].CacheControl, mp[1].CacheControl)
	}
	if mp[2].CacheControl == nil || mp[2].CacheControl.Type != "ephemeral" {
		t.Fatalf("client cache_control breakpoint lost after injection: %#v", mp[2].CacheControl)
	}
}
