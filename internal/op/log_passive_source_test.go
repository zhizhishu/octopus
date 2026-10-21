package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

func TestSourceEvidenceOnlyOnAnthropicPath(t *testing.T) {
	// OpenAI 路径：即使字段齐全也必须零证据，否则会误伤非 Anthropic 流量。
	l := model.RelayLog{
		RequestEndpoint:           "chat",
		UpstreamServiceTier:       "standard",
		CacheWriteTokens:          100,
		CacheWrite5mTokens:        50,
		UpstreamSystemFingerprint: "fp_x",
	}
	if ev := computePassiveSourceEvidence(l); !ev.Empty() {
		t.Fatalf("expected empty evidence on non-anthropic path, got %+v", ev)
	}
}

func TestSourceEvidenceFullAnthropicSignals(t *testing.T) {
	l := model.RelayLog{
		RequestEndpoint:     "messages",
		UpstreamServiceTier: "standard",
		CacheWriteTokens:    1000,
		CacheWrite5mTokens:  600,
		CacheWrite1hTokens:  400,
	}
	ev := computePassiveSourceEvidence(l)
	if ev.AnthropicScore != 6 { // 1+2 (ttl split) + 3 (service_tier)
		t.Fatalf("score = %d, want 6 (%+v)", ev.AnthropicScore, ev)
	}
	if len(ev.CounterEvidence) != 0 {
		t.Fatalf("expected no counter evidence, got %v", ev.CounterEvidence)
	}
}

func TestSourceEvidenceCounterWhenServiceTierMissing(t *testing.T) {
	// 有 TTL 拆分（正向）但没 service_tier（反证）。
	l := model.RelayLog{
		RequestEndpoint:    "messages",
		CacheWriteTokens:   1000,
		CacheWrite5mTokens: 600,
	}
	ev := computePassiveSourceEvidence(l)
	// 正向 1+2=3，反证 -3，归零。
	if ev.AnthropicScore != 0 {
		t.Fatalf("score = %d, want 0 (%+v)", ev.AnthropicScore, ev)
	}
	if len(ev.CounterEvidence) != 1 || ev.CounterEvidence[0] != "service_tier_absent" {
		t.Fatalf("counter = %v, want [service_tier_absent]", ev.CounterEvidence)
	}
}

func TestSourceEvidenceNoSignalNoCounter(t *testing.T) {
	// 走 Anthropic 路径但字段全空：既无正分也不该反证（避免噪音）。
	l := model.RelayLog{RequestEndpoint: "messages"}
	ev := computePassiveSourceEvidence(l)
	if !ev.Empty() {
		t.Fatalf("expected empty evidence, got %+v", ev)
	}
}
