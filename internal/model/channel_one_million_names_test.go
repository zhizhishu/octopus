package model

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 渠道模型名与请求共用同一条 [1m] 规则（见 internal/model/channel_toggles.md）。
func TestChannelModelNamesShareOneMillionRule(t *testing.T) {
	clean := map[string]string{
		"opus[1m]":                "claude-opus-4-8",
		"claude-opus-4-7[1M]":     "claude-opus-4-8",
		"fable[1m]":               "claude-fable-5",
		"claude-fable-5[1M][1m]":  "claude-fable-5",
		"sonnet[1m]":              "sonnet",
		"claude-sonnet-4-5[1m]":   "claude-sonnet-4-5",
		"claude-haiku-4-5[1m]":    "claude-haiku-4-5",
		"deepseek-flash[1m]":      "deepseek-flash",
		"claude-opus-4-8[1m]-pre": "claude-opus-4-8[1m]-pre",
		"gpt-5":                   "gpt-5",
	}
	for in, want := range clean {
		if got := CleanOneMillionCapabilityModelName(in); got != want {
			t.Fatalf("CleanOneMillionCapabilityModelName(%q) = %q, want %q", in, got, want)
		}
	}
	wants := map[string]bool{
		"sonnet[1m]":           true,
		"fable[1M]":            true,
		"claude-opus-4-8[1m]":  true,
		"claude-haiku-4-5[1m]": false,
		"deepseek-flash[1m]":   false,
		"claude-opus-4-8":      false,
	}
	for in, want := range wants {
		if got := ModelNameWantsAnthropicContext1M(in); got != want {
			t.Fatalf("ModelNameWantsAnthropicContext1M(%q) = %v, want %v", in, got, want)
		}
	}
}

// 能力位 = 类型默认 OR 勾选。
func TestSupportsCapabilityTypeDefaultOrListed(t *testing.T) {
	cases := []struct {
		typ  outbound.OutboundType
		caps []string
		want bool
	}{
		{outbound.OutboundTypeOpenAIEmbedding, nil, true},
		{outbound.OutboundTypeOpenAIEmbedding, []string{"rerank"}, true},
		{outbound.OutboundTypeOpenAIEmbedding, []string{CapabilityEmbeddings}, true},
		{outbound.OutboundTypeOpenAIChat, nil, false},
		{outbound.OutboundTypeOpenAIChat, []string{"rerank"}, false},
		{outbound.OutboundTypeOpenAIChat, []string{CapabilityEmbeddings}, true},
		{outbound.OutboundTypeAnthropic, []string{CapabilityEmbeddings}, true},
	}
	for _, c := range cases {
		ch := &Channel{Type: c.typ, Capabilities: c.caps}
		if got := ch.SupportsCapability(CapabilityEmbeddings); got != c.want {
			t.Fatalf("type %d caps %v: SupportsCapability(embeddings) = %v, want %v", c.typ, c.caps, got, c.want)
		}
	}
}
