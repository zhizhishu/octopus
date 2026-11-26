package handlers

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

// ── 渠道级探测覆写的映射用例 ──────────────────────────────────────────────
//
// 只测接线层这两个读数函数（渠道表 → 覆写值 / 生效上限）：不跑探测车道，
// 不发任何真实请求（假件 RunProbe 一次都不会被调到）。
// 回调接线层面（circuitProbeCallbacks 里把 ChannelProbeOverrides 指向
// circuitProbeChannelProbeOverrides）不属于本组用例。

// 渠道的 probe_daily_cap / probe_max_failures 落库之后必须按零值语义映成覆写口径，
// 而不存在的渠道必须映成零值（= 无覆写），不得臆造额度。
func TestCircuitProbeChannelProbeOverridesMapping(t *testing.T) {
	env := newCircuitProbeTestEnv(t)

	nine, two := 9, 2
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{
		ID:               env.channel.ID,
		ProbeDailyCap:    &nine,
		ProbeMaxFailures: &two,
	}, env.ctx); err != nil {
		t.Fatalf("update probe overrides: %v", err)
	}
	if got := circuitProbeChannelProbeOverrides(env.channel.ID); got != (balancer.CircuitProbeChannelOverrides{DailyCap: 9, MaxStreak: 2}) {
		t.Fatalf("circuitProbeChannelProbeOverrides = %+v, want {DailyCap:9 MaxStreak:2}", got)
	}
	if got := circuitProbeChannelProbeOverrides(999999); got != (balancer.CircuitProbeChannelOverrides{}) {
		t.Fatalf("unknown channel = %+v, want the zero value (no overrides)", got)
	}
}

// 生效的每日上限：渠道 ProbeDailyCap>0 用渠道的，否则（含显式 0 与渠道不存在）原样
// 用全局额度。0 的语义是"沿用全局"，不是 0=不限。
func TestCircuitProbeChannelDailyCapHelper(t *testing.T) {
	env := newCircuitProbeTestEnv(t)

	if got := circuitProbeChannelDailyCap(env.channel.ID, 20); got != 20 {
		t.Fatalf("cap without an override = %d, want the global 20", got)
	}

	nine := 9
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeDailyCap: &nine}, env.ctx); err != nil {
		t.Fatalf("set probe daily cap: %v", err)
	}
	if got := circuitProbeChannelDailyCap(env.channel.ID, 20); got != 9 {
		t.Fatalf("cap with an override = %d, want 9", got)
	}

	// 显式设回 0：回落全局额度（而不是被当成"不限"）。
	zero := 0
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeDailyCap: &zero}, env.ctx); err != nil {
		t.Fatalf("clear probe daily cap: %v", err)
	}
	if got := circuitProbeChannelDailyCap(env.channel.ID, 20); got != 20 {
		t.Fatalf("cap after clearing the override = %d, want the global 20", got)
	}

	// 渠道不存在（含 0 号）也必须回落全局额度。
	if got := circuitProbeChannelDailyCap(999999, 20); got != 20 {
		t.Fatalf("unknown channel cap = %d, want the global 20", got)
	}
	if got := circuitProbeChannelDailyCap(0, 20); got != 20 {
		t.Fatalf("channel 0 cap = %d, want the global 20", got)
	}
}
