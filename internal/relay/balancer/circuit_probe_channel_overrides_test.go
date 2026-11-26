package balancer

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// ── 渠道级探测覆写（CircuitProbeChannelOverrides）的单测 ─────────────────────
//
// 全部复用 circuit_probe_test.go 的假件（newProbeTestEnv 的注入时钟与假 RunProbe，
// 不发任何真实上游请求、不碰网络）。覆写只改"该渠道的连续上限与每日额度"两处口径，
// 其余闸门（媒体/作用域/真实流量让路/上游等待）不属于本组用例。

// configureProbeOverrides 在 newProbeTestEnv 的假件之上重灌一遍口径并附上渠道级
// 覆写回调。ConfigureCircuitProbe 是整体替换，没有"只改一个字段"的口子，所以这里
// 必须把与 env.refresh 相同的回调原样再设一遍——cb 闭包读的都是 env 字段，行为一致。
func configureProbeOverrides(env *probeTestEnv, overrides func(channelID int) CircuitProbeChannelOverrides) {
	ConfigureCircuitProbe(env.cfg, CircuitProbeCallbacks{
		ResolveTarget: func(target CircuitProbeTarget) (CircuitProbeRoute, bool) {
			route, ok := env.routes[circuitKey(target.ChannelID, target.KeyID, target.Model)]
			return route, ok
		},
		ChannelEnabled: func(channelID int) bool { return !env.channelOff[channelID] },
		AuditPending:   func() bool { return env.auditPending },
		RunProbe: func(_ context.Context, claim CircuitProbeClaim) CircuitProbeReport {
			env.runs = append(env.runs, claim)
			if env.onProbe != nil {
				env.onProbe()
			}
			return env.report
		},
		Now: env.clock.Now,
		Jitter: func(remaining time.Duration) (time.Duration, bool) {
			if env.jitter != nil {
				return env.jitter(remaining)
			}
			// 与 env.refresh 的默认实现一致：lead=remaining ⇒ due=now（确定且可行），
			// 剩余不足 10s 时窗口为空（不再新排）。
			if remaining < probeJitterMin {
				return 0, false
			}
			return remaining, true
		},
		ChannelProbeOverrides: overrides,
	})
}

// 纯函数口径：覆写只在 >0 时生效，零/负值一律回落全局；零覆写必须严格等价于原口径。
func TestCircuitProbeConfigOverrideHelpers(t *testing.T) {
	cfg := CircuitProbeConfig{MaxStreak: 7, ChannelDailyCap: 4}
	zero := CircuitProbeChannelOverrides{}

	if got := cfg.maxStreakFor(zero); got != cfg.maxStreak() {
		t.Fatalf("zero overrides maxStreakFor = %d, want %d", got, cfg.maxStreak())
	}
	if got := cfg.channelDailyCapFor(zero); got != cfg.ChannelDailyCap {
		t.Fatalf("zero overrides channelDailyCapFor = %d, want %d", got, cfg.ChannelDailyCap)
	}

	// MaxStreak 未配置（0）时走默认 3；ChannelDailyCap 未配置（0）= 不限。
	if got := (CircuitProbeConfig{}).maxStreakFor(zero); got != DefaultCircuitProbeMaxStreak {
		t.Fatalf("zero config maxStreakFor = %d, want the default %d", got, DefaultCircuitProbeMaxStreak)
	}
	if got := (CircuitProbeConfig{}).channelDailyCapFor(zero); got != 0 {
		t.Fatalf("zero config channelDailyCapFor = %d, want 0 (unlimited)", got)
	}

	// 覆写 > 0：覆盖全局。
	positive := CircuitProbeChannelOverrides{DailyCap: 9, MaxStreak: 2}
	if got := cfg.maxStreakFor(positive); got != 2 {
		t.Fatalf("maxStreakFor(positive) = %d, want 2", got)
	}
	if got := cfg.channelDailyCapFor(positive); got != 9 {
		t.Fatalf("channelDailyCapFor(positive) = %d, want 9", got)
	}

	// 覆写 0 或负值：回落全局（<=0 一律按沿用处理，负值不得把额度变成 0）。
	negative := CircuitProbeChannelOverrides{DailyCap: -3, MaxStreak: -1}
	if got := cfg.maxStreakFor(negative); got != 7 {
		t.Fatalf("maxStreakFor(negative) = %d, want 7", got)
	}
	if got := cfg.channelDailyCapFor(negative); got != 4 {
		t.Fatalf("channelDailyCapFor(negative) = %d, want 4", got)
	}

	// 混合：一个字段覆写、另一个字段回落，两个半边互不牵连。
	mixed := CircuitProbeChannelOverrides{DailyCap: 5}
	if got := cfg.maxStreakFor(mixed); got != 7 {
		t.Fatalf("maxStreakFor(mixed) = %d, want 7", got)
	}
	if got := cfg.channelDailyCapFor(mixed); got != 5 {
		t.Fatalf("channelDailyCapFor(mixed) = %d, want 5", got)
	}
	mixedStreak := CircuitProbeChannelOverrides{MaxStreak: 2}
	if got := cfg.maxStreakFor(mixedStreak); got != 2 {
		t.Fatalf("maxStreakFor(mixedStreak) = %d, want 2", got)
	}
	if got := cfg.channelDailyCapFor(mixedStreak); got != 4 {
		t.Fatalf("channelDailyCapFor(mixedStreak) = %d, want 4", got)
	}

	// 全局口径本身 <=0 时也走同一条回落链路（默认值 / 不限），覆写不改变这条链路。
	if got := (CircuitProbeConfig{MaxStreak: -2}).maxStreakFor(negative); got != DefaultCircuitProbeMaxStreak {
		t.Fatalf("negative config maxStreakFor = %d, want the default %d", got, DefaultCircuitProbeMaxStreak)
	}
	if got := (CircuitProbeConfig{ChannelDailyCap: -5}).channelDailyCapFor(zero); got != 0 {
		t.Fatalf("negative config channelDailyCapFor = %d, want 0 (unlimited)", got)
	}
}

// 渠道每日上限覆写：渠道 30 覆写 DailyCap=1（全局单渠道上限 2 不变），失败结论会
// trip+1 换出新一代冷却让条目立刻再次可领，正好把两道额度跑到头：渠道 30 第 2 次
// 探测被自己的覆写拒掉，渠道 31 能探满全局单渠道上限 2 次。GlobalDailyCap=0（不限），
// 别让全局额度抢戏。
func TestCircuitProbeChannelDailyCapOverride(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg = CircuitProbeConfig{Enabled: true, MaxStreak: 5, ChannelDailyCap: 2, GlobalDailyCap: 0}
	env.routeText(30, 1, "m")
	env.openEntry(30, 1, "m", "chat", "", 1, 5*time.Second) // 剩 25s：due=now、立即可领
	env.routeText(31, 1, "m")
	env.openEntry(31, 1, "m", "chat", "", 1, 5*time.Second)
	configureProbeOverrides(env, func(channelID int) CircuitProbeChannelOverrides {
		if channelID == 30 {
			return CircuitProbeChannelOverrides{DailyCap: 1}
		}
		return CircuitProbeChannelOverrides{}
	})
	// 可归因的失败：结算后 trip+1、冷却段换代，条目立刻再次可领，额度才会被顶到。
	env.report = CircuitProbeReport{Attributable: true, StatusCode: http.StatusServiceUnavailable, Outcome: "upstream_error"}

	if ran := RunDueCircuitProbes(context.Background()); ran != 3 {
		t.Fatalf("RunDueCircuitProbes = %d, want 3 (channel 30×1 at its override cap, channel 31×2 at the global cap)", ran)
	}
	if len(env.runs) != 3 {
		t.Fatalf("probe runs = %d, want 3", len(env.runs))
	}
	want := []struct{ channelID, keyID int }{{30, 1}, {31, 1}, {31, 1}}
	for i, w := range want {
		if env.runs[i].Target.ChannelID != w.channelID || env.runs[i].Target.KeyID != w.keyID {
			t.Fatalf("run %d = %d/%d, want %d/%d (candidates claim in channel order; 30 must hit its override cap first)",
				i, env.runs[i].Target.ChannelID, env.runs[i].Target.KeyID, w.channelID, w.keyID)
		}
	}
	if got := SnapshotCircuitProbe(30).Totals.Used; got != 1 {
		t.Fatalf("channel 30 used = %d, want 1 (the 2nd probe must be rejected by the override cap)", got)
	}
	if got := SnapshotCircuitProbe(31).Totals.Used; got != 2 {
		t.Fatalf("channel 31 used = %d, want 2 (the unoverridden channel must exhaust the global per-channel cap)", got)
	}
	// 拒绝原因必须落在额度上：30/1 的新一代条目还在挡路，只是被 capped 判成 disabled
	// （连续上限是另一档、会是 stopped，别混）。
	if entry, ok := env.entryView(30, 1, "m", "chat", ""); !ok || entry.Probe.State != CircuitProbeStateDisabled {
		t.Fatalf("channel 30 entry = %+v (found=%v), want disabled (capped)", entry, ok)
	}
	// 额度吃满之后再也无法领到新探测。
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("nothing may be claimed once both per-channel caps are exhausted")
	}
}

// 连续未恢复上限的渠道覆写：渠道 40 覆写 MaxStreak=1，1 次未恢复就停；渠道 41 没有
// 覆写，同样 1 次计数（1 < 5）必须继续可排期、可领。decide 与 admit 两侧都验。
func TestCircuitProbeChannelStreakOverride(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(40, 1, "m")
	env.openEntry(40, 1, "m", "chat", "", 1, 5*time.Second) // 剩 25s：窗口内
	env.routeText(41, 1, "m")
	env.openEntry(41, 1, "m", "chat", "", 1, 5*time.Second)
	incProbeStreak(circuitKey(40, 1, "m"))
	incProbeStreak(circuitKey(41, 1, "m"))
	if got := probeStreakValue(circuitKey(40, 1, "m")); got != 1 {
		t.Fatalf("precondition: streak = %d, want 1", got)
	}
	configureProbeOverrides(env, func(channelID int) CircuitProbeChannelOverrides {
		if channelID == 40 {
			return CircuitProbeChannelOverrides{MaxStreak: 1}
		}
		return CircuitProbeChannelOverrides{}
	})

	// decideLocked：渠道 40 的条目被标 stopped（不再排期）。
	if entry, ok := env.entryView(40, 1, "m", "chat", ""); !ok || entry.Probe.State != CircuitProbeStateStopped {
		t.Fatalf("channel 40 entry = %+v (found=%v), want stopped at streak 1 (override maxStreak=1)", entry, ok)
	}
	// admit：自动准入同样按覆写拒绝（拒绝档位是 stopped）。
	rejection, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 40, KeyID: 1, Model: "m", Endpoint: "chat"}, false)
	if ok || rejection.State != CircuitProbeStateStopped {
		t.Fatalf("channel 40 admission = %+v (ok=%v), want stopped", rejection, ok)
	}

	// 渠道 41 没有覆写：同样 1 次计数照样排上、照样可领（40 被停，车道落到 41）。
	if entry, ok := env.entryView(41, 1, "m", "chat", ""); !ok || entry.Probe.State != CircuitProbeStateScheduled {
		t.Fatalf("channel 41 entry = %+v (found=%v), want scheduled", entry, ok)
	}
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok || claim.Target.ChannelID != 41 || claim.Target.KeyID != 1 {
		t.Fatalf("claim = %+v (ok=%v), want channel 41 key 1 (40 must be stopped, 41 must still probe)", claim, ok)
	}
	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok"})
}

// 手动探测不豁免渠道覆写的每日上限：全局额度都没开（0=不限），渠道 50 的覆写
// DailyCap=1 是唯一在生效的额度——先验证额度未用尽时手动准入放行，真跑一次
// claim+complete 把 Used 顶到 1 之后，手动准入必须被拒（capped→disabled）。
func TestCircuitProbeManualHonorChannelCap(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(50, 1, "m")
	env.openEntry(50, 1, "m", "chat", "", 1, 5*time.Second)
	env.routeText(50, 2, "m")
	env.openEntry(50, 2, "m", "chat", "", 1, 5*time.Second)
	configureProbeOverrides(env, func(channelID int) CircuitProbeChannelOverrides {
		if channelID == 50 {
			return CircuitProbeChannelOverrides{DailyCap: 1}
		}
		return CircuitProbeChannelOverrides{}
	})

	// 额度未用尽：手动准入必须放行（对照，证明后面的拒绝确实来自额度）。
	target := CircuitProbeTarget{ChannelID: 50, KeyID: 2, Model: "m", Endpoint: "chat"}
	if _, ok := CircuitProbeAdmission(env.clock.now, target, true); !ok {
		t.Fatal("manual admission must pass before the override cap is spent")
	}

	// 真跑一次 claim+complete：渠道 50 当日 Used 变成 1 = 覆写上限。
	// （50/1 排序在前，先领的那条正是它。）
	if _, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"}); !ok {
		t.Fatal("the probe must be claimable")
	}
	if got := SnapshotCircuitProbe(50).Totals.Used; got != 1 {
		t.Fatalf("channel 50 used = %d, want 1", got)
	}

	// 自动车道的下一次同样被覆写挡住。
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("the channel override cap must stop further automatic probes")
	}

	// 手动不豁免渠道覆写额度：capped → disabled。
	rejection, ok := CircuitProbeAdmission(env.clock.now, target, true)
	if ok || rejection.State != CircuitProbeStateDisabled {
		t.Fatalf("manual admission = %+v (ok=%v), want disabled (capped)", rejection, ok)
	}
}
