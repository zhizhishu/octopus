package balancer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 熔断探测调度的确定性单测 ──────────────────────────────────────────────
//
// 全部假件：时钟是注入的，探测是 env 里的假实现（不发请求、不碰网络），熔断条目
// 直接塞进 globalBreaker/globalHolds。真实时钟只在"既有 RecordFailureWithStatusScoped
// 会写 time.Now()"这条路上出现——那正是要验证的既有行为，所以那些用例把注入时钟设成
// 接近真实时间。

type probeTestClock struct {
	now time.Time
}

func (c *probeTestClock) Now() time.Time { return c.now }

type probeTestEnv struct {
	t     *testing.T
	clock *probeTestClock
	cfg   CircuitProbeConfig

	routes     map[string]CircuitProbeRoute // circuitKey(channel,key,model) -> route
	channelOff map[int]bool
	report     CircuitProbeReport
	runs       []CircuitProbeClaim
	onProbe    func() // 在"探测进行中"那一刻被调用（用来验车道是否仍被占着）

	auditPending bool
	jitter       func(remaining time.Duration) (time.Duration, bool)
}

func newProbeTestEnv(t *testing.T) *probeTestEnv {
	t.Helper()
	globalBreaker = sync.Map{}
	globalHolds = sync.Map{}
	ResetRuntimeTelemetry()
	ResetCircuitProbe()

	env := &probeTestEnv{
		t:          t,
		clock:      &probeTestClock{now: time.Now()},
		cfg:        CircuitProbeConfig{Enabled: true, MaxStreak: 5},
		routes:     make(map[string]CircuitProbeRoute),
		channelOff: make(map[int]bool),
	}
	env.refresh()
	t.Cleanup(func() {
		ResetCircuitProbe()
		globalBreaker = sync.Map{}
		globalHolds = sync.Map{}
		ResetRuntimeTelemetry()
	})
	return env
}

// refresh 把当前 cfg / 假件重新灌给调度器（cfg 是快照，改了就得再灌一次）。
func (env *probeTestEnv) refresh() {
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
			// 默认：提前量取满剩余（lead=remaining ⇒ due=now），确定性且始终可行；
			// 剩余不足 10s 时窗口为空（不再新排）。
			if remaining < probeJitterMin {
				return 0, false
			}
			return remaining, true
		},
	})
}

// routeText 让接线层把该 tuple 解析成一道普通文本探测。
func (env *probeTestEnv) routeText(channelID, keyID int, model string) {
	env.routes[circuitKey(channelID, keyID, model)] = CircuitProbeRoute{Endpoint: "openai_chat"}
}

// openEntry 直接塞一条 OPEN 熔断条目（ago 是"多久之前记的失败"）。writeSeq 与真实
// 写者保持一致地从 1 开始（0 表示"从没写过"，不是真实状态）。
func (env *probeTestEnv) openEntry(channelID, keyID int, model, endpoint, capability string, tripCount int, ago time.Duration) {
	globalBreaker.Store(circuitScopedKey(channelID, keyID, model, endpoint, capability), &circuitEntry{
		State:           StateOpen,
		LastFailureTime: env.clock.now.Add(-ago),
		TripCount:       tripCount,
		writeSeq:        nextCircuitRevision(),
	})
}

// historyResults 取某渠道历史环里的 result 串（最新在前）。
func (env *probeTestEnv) historyResults(channelID int) []string {
	view := SnapshotCircuitProbe(channelID)
	results := make([]string, 0, len(view.History))
	for _, row := range view.History {
		results = append(results, row.Result)
	}
	return results
}

// runOnce 走一遍 claim → complete（假探测结果由 report 给）。
func (env *probeTestEnv) runOnce(report CircuitProbeReport) (CircuitProbeClaim, bool) {
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		return CircuitProbeClaim{}, false
	}
	env.runs = append(env.runs, claim)
	CompleteCircuitProbe(claim, report)
	return claim, true
}

// entryView 取某条条目的 per-entry 探测视图。
func (env *probeTestEnv) entryView(channelID, keyID int, model, endpoint, capability string) (CircuitProbeEntryView, bool) {
	for _, entry := range SnapshotCircuitProbe(channelID).Entries {
		if entry.KeyID == keyID && entry.Model == model && entry.Endpoint == endpoint && entry.Capability == capability {
			return entry, true
		}
	}
	return CircuitProbeEntryView{}, false
}

func TestDefaultCircuitProbeLeadStaysWithinTheWindow(t *testing.T) {
	for _, remaining := range []time.Duration{
		11 * time.Second, 12 * time.Second, 30 * time.Second, 59 * time.Second,
		60 * time.Second, 61 * time.Second, 5 * time.Minute,
	} {
		upper := remaining
		if upper > probeJitterMax {
			upper = probeJitterMax
		}
		for i := 0; i < 400; i++ {
			lead, feasible := defaultCircuitProbeJitter(remaining)
			if !feasible {
				t.Fatalf("remaining=%v: lead must be feasible", remaining)
			}
			if lead < probeJitterMin || lead > upper {
				t.Fatalf("remaining=%v: lead %v outside [%v, %v]", remaining, lead, probeJitterMin, upper)
			}
			if lead > remaining {
				t.Fatalf("remaining=%v: lead %v would start after expiry", remaining, lead)
			}
		}
	}
	// 剩余恰好 10s：窗口只剩一个点，lead 必须就是 10s（于是 due = 现在）。
	for i := 0; i < 50; i++ {
		if lead, feasible := defaultCircuitProbeJitter(probeJitterMin); !feasible || lead != probeJitterMin {
			t.Fatalf("remaining=10s: lead = %v feasible=%v, want exactly 10s", lead, feasible)
		}
	}
	for _, remaining := range []time.Duration{0, time.Second, 9 * time.Second} {
		if lead, feasible := defaultCircuitProbeJitter(remaining); feasible {
			t.Fatalf("remaining=%v: no feasible lead expected, got %v", remaining, lead)
		}
	}
}

// 提前量只在一段冷却里挑一次，due = expiry - lead，且在到点前不可领。
func TestCircuitProbeLeadChosenOnceAndDueAtExpiryMinusLead(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(11, 1, "m-a")
	env.openEntry(11, 1, "m-a", "chat", "stream", 1, 5*time.Second) // 剩 25s
	calls := 0
	env.jitter = func(remaining time.Duration) (time.Duration, bool) {
		calls++
		if remaining < probeJitterMin {
			return 0, false
		}
		return 7 * time.Second, true
	}
	env.refresh()

	// lead 7s ⇒ due = expiry - 7s = now + 18s。
	TickCircuitProbe(env.clock.now)
	entry, ok := env.entryView(11, 1, "m-a", "chat", "stream")
	if !ok || entry.Probe.State != CircuitProbeStateScheduled || entry.Probe.NextInSeconds != 18 {
		t.Fatalf("schedule = %+v (found=%v), want scheduled in 18s (expiry 25s - lead 7s)", entry.Probe, ok)
	}
	if calls != 1 {
		t.Fatalf("lead picked %d times, want exactly 1", calls)
	}

	env.clock.now = env.clock.now.Add(17 * time.Second)
	if entry, _ := env.entryView(11, 1, "m-a", "chat", "stream"); entry.Probe.NextInSeconds != 1 {
		t.Fatalf("next_in_seconds = %d, want 1 (the chosen moment must stick)", entry.Probe.NextInSeconds)
	}
	if calls != 1 {
		t.Fatalf("lead re-picked within one generation: %d calls", calls)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("a probe must not run before its due moment")
	}

	env.clock.now = env.clock.now.Add(time.Second) // 正好到 due，距离到期还有 7s
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable at its due moment")
	}
	if claim.Target.Endpoint != "chat" || claim.Target.Capability != "stream" {
		t.Fatalf("claim kept the breaker scope: %+v", claim.Target)
	}
	if claim.Route.Endpoint != "openai_chat" {
		t.Fatalf("claim route = %+v, want the wiring layer's endpoint", claim.Route)
	}
	if claim.Manual || claim.Generation == "" || claim.EntrySeq == 0 || claim.ExpiryUnixMs == 0 {
		t.Fatalf("claim = %+v, want an automatic claim carrying identity + expiry", claim)
	}
}

// 剩余 > 60s：不算到期、不可领，也**不挑提前量**（不写任何持久的下次时间）。
func TestCircuitProbeWindowOpensOnlyInsideTheLastMinute(t *testing.T) {
	env := newProbeTestEnv(t)
	calls := 0
	env.jitter = func(remaining time.Duration) (time.Duration, bool) {
		calls++
		if remaining < probeJitterMin {
			return 0, false
		}
		return remaining, true
	}
	env.routeText(60, 1, "m")
	env.openEntry(60, 1, "m", "chat", "", 3, 10*time.Second) // 120s - 10s = 110s
	env.refresh()

	entry, ok := env.entryView(60, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateScheduled || entry.Probe.NextInSeconds != 50 {
		t.Fatalf("view = %+v (found=%v), want scheduled when the window opens in 50s", entry.Probe, ok)
	}
	if calls != 0 {
		t.Fatalf("lead picked while remaining > 60s: %d calls, want 0", calls)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("nothing may be claimed while the entry still has more than 60s left")
	}

	// 进窗口（剩 55s）：这一轮才挑提前量，due = expiry - lead = 现在。
	env.clock.now = env.clock.now.Add(55 * time.Second)
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("inside the last minute the entry must become claimable")
	}
	if calls != 1 {
		t.Fatalf("lead picked %d times, want exactly 1", calls)
	}
	_ = claim
}

// 剩余恰好 10s：lead 只能是 10s ⇒ due = 现在，当轮可领。
func TestCircuitProbeExactTenSecondsSchedulesNow(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(68, 1, "m")
	env.openEntry(68, 1, "m", "chat", "", 1, 20*time.Second) // 剩 10s
	env.refresh()

	entry, ok := env.entryView(68, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateScheduled || entry.Probe.NextInSeconds != 0 {
		t.Fatalf("view = %+v (found=%v), want due now", entry.Probe, ok)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); !ok {
		t.Fatal("exactly 10s left must be claimable now")
	}
}

// 剩余 < 10s 且本段从没挑过 due：不新排，也不可领。
func TestCircuitProbeLessThanTenSecondsIsNotNewlyScheduled(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(69, 1, "m")
	env.openEntry(69, 1, "m", "chat", "", 1, 21*time.Second) // 剩 9s
	env.refresh()

	entry, ok := env.entryView(69, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateStopped || entry.Probe.NextInSeconds != 0 {
		t.Fatalf("view = %+v (found=%v), want stopped", entry.Probe, ok)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("less than 10s left must not be newly scheduled or claimed")
	}
	rejection, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 69, KeyID: 1, Model: "m", Endpoint: "chat"}, false)
	if ok || rejection.State != CircuitProbeStateStopped {
		t.Fatalf("admission = %+v (ok=%v), want stopped", rejection, ok)
	}
}

// 晚到的 tick 仍然可以领本段已经挑好的 due（只要还没过到期）；过了到期就再也不开跑。
func TestCircuitProbeLateTickHonorsTheChosenDue(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(71, 1, "m")
	env.openEntry(71, 1, "m", "chat", "", 1, 10*time.Second) // 剩 20s
	env.jitter = func(remaining time.Duration) (time.Duration, bool) {
		if remaining < probeJitterMin {
			return 0, false
		}
		return 10 * time.Second, true // lead 10s ⇒ due = expiry - 10s = now + 10s
	}
	env.refresh()

	TickCircuitProbe(env.clock.now)
	if entry, _ := env.entryView(71, 1, "m", "chat", ""); entry.Probe.NextInSeconds != 10 {
		t.Fatalf("next_in_seconds = %d, want 10", entry.Probe.NextInSeconds)
	}

	// 一口气跳过 due（15s 后才 tick）：距离到期只剩 5s，但本段挑好的 due 已经过了，可以领。
	env.clock.now = env.clock.now.Add(15 * time.Second)
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("a late tick must still honor the already chosen due before expiry")
	}
	CompleteCircuitProbe(claim, CircuitProbeReport{Outcome: "canceled"})

	// 过期之后（另开一段冷却）一律不开跑。
	env.clock.now = env.clock.now.Add(time.Hour)
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("nothing may start after expiry")
	}
}

func TestCircuitProbeDoesNotScheduleWhenCooldownIsNearlyOver(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(12, 1, "m")
	env.openEntry(12, 1, "m", "chat", "", 1, 25*time.Second) // 只剩 5s

	entry, ok := env.entryView(12, 1, "m", "chat", "")
	if !ok {
		t.Fatal("the open entry must still be visible")
	}
	if entry.Probe.State != CircuitProbeStateStopped || entry.Probe.NextInSeconds != 0 {
		t.Fatalf("near-expiry entry = %+v, want stopped", entry.Probe)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("no probe may be scheduled inside the last 10s of a cooldown")
	}
}

func TestCircuitProbeIgnoresHalfOpenExpiredAndWildcardRows(t *testing.T) {
	env := newProbeTestEnv(t)

	// 半开：真实流量的半开探测归 relay 那条路，不归这里。
	globalBreaker.Store(circuitScopedKey(13, 1, "half", "chat", ""), &circuitEntry{
		State:           StateHalfOpen,
		LastFailureTime: env.clock.now,
		TripCount:       1,
	})
	// 已过冷却：不臆造、也不抢。
	env.openEntry(13, 2, "expired", "chat", "", 1, 40*time.Second)
	// hold 通配：不探。
	globalHolds.Store(holdKey(13, 3, holdAnyModel), holdEntry{until: env.clock.now.Add(time.Minute), rule: "quota"})
	globalHolds.Store(holdKey(13, 0, holdAnyModel), holdEntry{until: env.clock.now.Add(time.Minute), rule: "suspended"})
	// 正常条目当对照。
	env.routeText(13, 4, "live")
	env.openEntry(13, 4, "live", "chat", "", 1, 0)

	view := SnapshotCircuitProbe(13)
	if len(view.Entries) != 1 || view.Entries[0].Model != "live" {
		t.Fatalf("candidate rows = %+v, want only the live entry", view.Entries)
	}
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 13, KeyID: 1, Model: "half", Endpoint: "chat"}, false); ok {
		t.Fatal("a half-open row must not be admitted")
	}
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 13, KeyID: 2, Model: "expired", Endpoint: "chat"}, false); ok {
		t.Fatal("an expired row must not be admitted")
	}
}

func TestCircuitProbeScopeGuards(t *testing.T) {
	env := newProbeTestEnv(t)
	type testCase struct {
		name       string
		endpoint   string
		capability string
		embeddings bool
		route      CircuitProbeRoute
		wantState  string
	}
	cases := []testCase{
		{name: "text", endpoint: "chat", capability: "stream", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateScheduled},
		{name: "text-no-capability", endpoint: "messages", route: CircuitProbeRoute{Endpoint: "anthropic_messages"}, wantState: CircuitProbeStateScheduled},
		{name: "image-endpoint", endpoint: "images_generations", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "video-endpoint", endpoint: "videos", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "audio-endpoint", endpoint: "audio_tts", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "image-capability", endpoint: "chat", capability: "stream+image", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "unscoped", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "unknown-endpoint", endpoint: "mystery_endpoint", route: CircuitProbeRoute{Endpoint: "openai_chat"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "embedding-default-off", endpoint: "embeddings", route: CircuitProbeRoute{Endpoint: "openai_embeddings"}, wantState: CircuitProbeStateNotProbedMedia},
		{name: "embedding-opted-in", endpoint: "embeddings", embeddings: true, route: CircuitProbeRoute{Endpoint: "openai_embeddings"}, wantState: CircuitProbeStateScheduled},
		{name: "media-route", endpoint: "chat", route: CircuitProbeRoute{Endpoint: "openai_images_generations"}, wantState: CircuitProbeStateDisabled},
		{name: "route-missing", endpoint: "chat", wantState: CircuitProbeStateDisabled},
	}
	for i, tc := range cases {
		env.t.Run(tc.name, func(t *testing.T) {
			channelID := 100 + i
			env.cfg.EmbeddingsEnabled = tc.embeddings
			env.routes = make(map[string]CircuitProbeRoute)
			if tc.route.Endpoint != "" {
				env.routes[circuitKey(channelID, 1, "m")] = tc.route
			}
			env.openEntry(channelID, 1, "m", tc.endpoint, tc.capability, 1, 0)
			env.refresh()

			entry, ok := env.entryView(channelID, 1, "m", tc.endpoint, tc.capability)
			if !ok || entry.Probe.State != tc.wantState {
				t.Fatalf("state = %q (found=%v), want %q", entry.Probe.State, ok, tc.wantState)
			}
		})
	}
}

func TestCircuitProbeChannelAndMasterToggles(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(15, 1, "m")
	env.openEntry(15, 1, "m", "chat", "", 1, 0)

	env.cfg.Enabled = false
	env.refresh()
	if entry, _ := env.entryView(15, 1, "m", "chat", ""); entry.Probe.State != CircuitProbeStateDisabled {
		t.Fatalf("master toggle off must show disabled, got %q", entry.Probe.State)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("master toggle off must stop automatic probing")
	}

	env.cfg.Enabled = true
	env.channelOff[15] = true
	env.refresh()
	if entry, _ := env.entryView(15, 1, "m", "chat", ""); entry.Probe.State != CircuitProbeStateDisabled {
		t.Fatalf("channel toggle off must show disabled, got %q", entry.Probe.State)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("channel toggle off must stop automatic probing")
	}
}

func TestCircuitProbeSkipsRealTrafficAndCountsSkipOnce(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(16, 1, "m")
	env.openEntry(16, 1, "m", "chat", "", 1, 0)
	noteCircuitProbeRealTraffic(16, 1, "m", env.clock.now.Add(-time.Minute))

	entry, _ := env.entryView(16, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateSkippedTraffic {
		t.Fatalf("state = %q, want skipped_traffic", entry.Probe.State)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("recent real traffic must keep the probe away")
	}
	// 反复 tick/读视图不得重复计数：一次决定只记一次 skip。
	TickCircuitProbe(env.clock.now)
	_ = SnapshotCircuitProbe(16)
	_ = SnapshotCircuitProbe(16)
	if got := SnapshotCircuitProbe(16).Totals.Skipped; got != 1 {
		t.Fatalf("skipped = %d, want 1 per decision/generation", got)
	}

	// 流量时间戳落灰（>10min）后重新排期。
	noteCircuitProbeRealTraffic(16, 1, "m", env.clock.now.Add(-11*time.Minute))
	entry, _ = env.entryView(16, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateScheduled {
		t.Fatalf("state = %q, want scheduled once the traffic window lapsed", entry.Probe.State)
	}
}

func TestCircuitProbeCleanupDropsStaleTrafficStamps(t *testing.T) {
	env := newProbeTestEnv(t)
	noteCircuitProbeRealTraffic(17, 1, "m", env.clock.now.Add(-16*time.Minute))
	noteCircuitProbeRealTraffic(17, 2, "m", env.clock.now.Add(-time.Minute))

	cleanupCircuitProbeTraffic(env.clock.now)

	if _, ok := probeRealTrafficAt(circuitKey(17, 1, "m")); ok {
		t.Fatal("a stamp past the retention window must be dropped")
	}
	if _, ok := probeRealTrafficAt(circuitKey(17, 2, "m")); !ok {
		t.Fatal("a fresh stamp must be kept")
	}
}

func TestCircuitProbeUpstreamWaitBlocksAutoAndManual(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(18, 1, "m")
	env.openEntry(18, 1, "m", "chat", "", 1, 0)
	ApplyCircuitProbeWait(18, 1, "m", CircuitProbeWaitCauseFallback429, 90*time.Second)

	entry, _ := env.entryView(18, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateWaitUpstream || entry.Probe.NextInSeconds != 90 {
		t.Fatalf("view = %+v, want wait_upstream in 90s", entry.Probe)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("an upstream wait must keep the automatic probe away")
	}
	target := CircuitProbeTarget{ChannelID: 18, KeyID: 1, Model: "m", Endpoint: "chat"}
	rejection, ok := CircuitProbeAdmission(env.clock.now, target, true)
	if ok {
		t.Fatal("manual probing must not bypass an upstream wait")
	}
	if rejection.State != CircuitProbeStateWaitUpstream || rejection.RemainingSeconds != 90 {
		t.Fatalf("rejection = %+v, want wait_upstream/90s", rejection)
	}
	if remaining, cause := CircuitProbeWaitRemaining(18, 1, "m"); remaining != 90*time.Second || cause != CircuitProbeWaitCauseFallback429 {
		t.Fatalf("wait = %v %q, want 90s fallback_429", remaining, cause)
	}

	// 手动队列里的目标被等待拒绝后不留垃圾，也不占车道。
	if !QueueManualCircuitProbe(target) {
		t.Fatal("the manual queue must accept an exact target")
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("a queued manual probe must respect the upstream wait")
	}

	env.clock.now = env.clock.now.Add(91 * time.Second)
	if remaining, _ := CircuitProbeWaitRemaining(18, 1, "m"); remaining != 0 {
		t.Fatalf("wait must expire, got %v", remaining)
	}
	// 此时那条条目已经过期：手动也不再接手（手动只对活条目生效）。
	if _, ok := CircuitProbeAdmission(env.clock.now, target, true); ok {
		t.Fatal("an expired entry must not be admitted even for a manual probe")
	}
}

func TestCircuitProbeLongProviderWaitIsRetained(t *testing.T) {
	newProbeTestEnv(t)
	ApplyCircuitProbeWait(19, 1, "m", CircuitProbeWaitCauseRetryAfter, 45*time.Minute)
	remaining, _ := CircuitProbeWaitRemaining(19, 1, "m")
	if remaining != 45*time.Minute {
		t.Fatalf("a long provider wait must be kept as given, got %v", remaining)
	}
	// 成因与运营商 hold 分开记：hold 还在，等待也在。
	ApplyHold(19, 1, "m", "model", time.Minute, "operator-rule")
	if tripped, _ := IsTripped(19, 1, "m"); !tripped {
		t.Fatal("the operator hold must still be a hold")
	}
	if remaining, cause := CircuitProbeWaitRemaining(19, 1, "m"); remaining == 0 || cause == "operator-rule" {
		t.Fatalf("upstream wait must stay distinct from the operator hold: %v %q", remaining, cause)
	}
}

func TestCircuitProbeManualBypassesAutomaticGuards(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(20, 1, "m")
	env.openEntry(20, 1, "m", "chat", "", 1, 5*time.Second)
	target := CircuitProbeTarget{ChannelID: 20, KeyID: 1, Model: "m", Endpoint: "chat"}

	// 自动侧全都被挡住：总开关关、渠道开关关、有真实流量、连续未恢复已到上限。
	env.cfg = CircuitProbeConfig{Enabled: false, MaxStreak: 1}
	env.channelOff[20] = true
	noteCircuitProbeRealTraffic(20, 1, "m", env.clock.now)
	incProbeStreak(circuitKey(20, 1, "m"))
	env.refresh()

	if _, ok := CircuitProbeAdmission(env.clock.now, target, false); ok {
		t.Fatal("automatic admission must be blocked by the toggles/streak/traffic")
	}
	if _, ok := CircuitProbeAdmission(env.clock.now, target, true); !ok {
		t.Fatal("manual probing must bypass toggles, traffic, streak and jitter")
	}
	if !QueueManualCircuitProbe(target) {
		t.Fatal("manual queue rejected the target")
	}
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok || !claim.Manual {
		t.Fatalf("manual claim = %+v (ok=%v), want a manual claim", claim, ok)
	}
	CompleteCircuitProbe(claim, CircuitProbeReport{Outcome: "canceled"})

	// 但手动不豁免"路由/渠道/密钥是否存在并启用"。
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 99, KeyID: 1, Model: "m", Endpoint: "chat"}, true); ok {
		t.Fatal("manual probing must not invent a target")
	}
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 20, KeyID: 1, Model: "m"}, true); ok {
		t.Fatal("manual probing must not bypass the media/unscoped exclusion")
	}
}

func TestCircuitProbeDailyCapsApplyToBothScopes(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg.GlobalDailyCap = 2
	env.cfg.ChannelDailyCap = 1
	for _, keyID := range []int{1, 2} {
		env.routeText(21, keyID, "m")
		env.openEntry(21, keyID, "m", "chat", "", 1, 0)
	}
	env.routeText(22, 1, "m")
	env.openEntry(22, 1, "m", "chat", "", 1, 0)
	env.routeText(23, 1, "m")
	env.refresh()

	claim, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})
	if !ok || claim.Target.ChannelID != 21 || claim.Target.KeyID != 1 {
		t.Fatalf("first claim = %+v (ok=%v), want channel 21 key 1", claim, ok)
	}

	claim, ok = env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "7"})
	if !ok || claim.Target.ChannelID != 22 {
		t.Fatalf("second claim = %+v (ok=%v), want channel 22 (channel 21 is capped)", claim, ok)
	}

	// 全局额度用尽（2/2）：通道 21 key 2 还没探过，也必须停手。
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("the global daily cap must stop every further probe")
	}
	if entry, _ := env.entryView(21, 2, "m", "chat", ""); entry.Probe.State != CircuitProbeStateDisabled {
		t.Fatalf("capped entry state = %q, want disabled", entry.Probe.State)
	}
	totals := SnapshotCircuitProbeGlobal().Totals
	if totals.Used != 2 || totals.Recovered != 2 {
		t.Fatalf("global totals = %+v, want used 2 recovered 2", totals)
	}

	// 手动同样受额度约束。
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 22, KeyID: 1, Model: "m", Endpoint: "chat"}, true); ok {
		t.Fatal("manual probing must respect the daily caps")
	}
	if _, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 23, KeyID: 1, Model: "m", Endpoint: "chat"}, true); ok {
		t.Fatal("manual probing must respect the global daily cap")
	}
}

func TestCircuitProbeNegativeDailyCapsAreUnlimited(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg.GlobalDailyCap = -1
	env.cfg.ChannelDailyCap = -1
	for _, keyID := range []int{1, 2, 3} {
		env.routeText(25, keyID, "m")
		env.openEntry(25, keyID, "m", "chat", "", 1, 0)
	}
	env.refresh()

	for want := 1; want <= 3; want++ {
		if _, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok"}); !ok {
			t.Fatalf("probe %d must run: negative daily caps mean unlimited", want)
		}
	}
	if got := SnapshotCircuitProbeGlobal().Totals.Used; got != 3 {
		t.Fatalf("global used = %d, want 3", got)
	}
}

func TestCircuitProbeDailyCountersRollOverAtUTC8Midnight(t *testing.T) {
	env := newProbeTestEnv(t)
	env.clock.now = time.Date(2026, 3, 1, 15, 30, 0, 0, time.UTC) // 2026-03-01 23:30 UTC+8
	env.routeText(24, 1, "m")
	env.openEntry(24, 1, "m", "chat", "", 1, 0)
	if _, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok"}); !ok {
		t.Fatal("the probe before midnight must run")
	}
	before := SnapshotCircuitProbeGlobal()
	if before.Day != "2026-03-01" || before.Totals.Used != 1 {
		t.Fatalf("global view before midnight = %+v", before)
	}

	env.clock.now = time.Date(2026, 3, 1, 16, 0, 1, 0, time.UTC) // 2026-03-02 00:00:01 UTC+8
	TickCircuitProbe(env.clock.now)

	after := SnapshotCircuitProbeGlobal()
	if after.Day != "2026-03-02" {
		t.Fatalf("day after rollover = %q, want 2026-03-02", after.Day)
	}
	if after.Totals != (CircuitProbeTotals{}) || len(after.Channels) != 0 {
		t.Fatalf("totals after rollover = %+v / %+v, want a clean day", after.Totals, after.Channels)
	}
	if view := SnapshotCircuitProbe(24); view.Day != "2026-03-02" || view.Totals != (CircuitProbeTotals{}) {
		t.Fatalf("channel view after rollover = %+v", view)
	}
}

func TestCircuitProbeStreakStopsAutomaticProbing(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg.MaxStreak = 2
	env.routeText(25, 1, "m")
	env.openEntry(25, 1, "m", "chat", "", 1, 100*time.Millisecond)
	env.refresh()

	failure := CircuitProbeReport{Attributable: true, StatusCode: http.StatusServiceUnavailable, Outcome: "upstream_error"}
	for round := 1; round <= 2; round++ {
		claim, ok := env.runOnce(failure)
		if !ok {
			t.Fatalf("round %d: an attributable probe must be claimable", round)
		}
		if claim.Target.Model != "m" {
			t.Fatalf("round %d: unexpected target %+v", round, claim.Target)
		}
		if streak := probeStreakValue(circuitKey(25, 1, "m")); streak != round {
			t.Fatalf("round %d: streak = %d", round, streak)
		}
	}

	entry, ok := env.entryView(25, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateStopped {
		t.Fatalf("view = %+v (found=%v), want stopped after the streak limit", entry, ok)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("automatic probing must stop once the streak limit is reached")
	}

	// 真实流量清零连续计数：下一段冷却可以重新探。
	noteCircuitProbeRealTraffic(25, 1, "m", env.clock.now)
	if streak := probeStreakValue(circuitKey(25, 1, "m")); streak != 0 {
		t.Fatalf("real traffic must reset the streak, got %d", streak)
	}
}

// probeOpenTripForTest 读一条 OPEN 熔断条目的 trip 次数（不存在/不 Open 直接 fail）。
func probeOpenTripForTest(t *testing.T, channelID, keyID int, model, endpoint, capability string) int {
	t.Helper()
	value, ok := globalBreaker.Load(circuitScopedKey(channelID, keyID, model, endpoint, capability))
	if !ok {
		t.Fatalf("breaker entry for %d/%d/%s is missing", channelID, keyID, model)
	}
	entry, ok := value.(*circuitEntry)
	if !ok {
		t.Fatalf("unexpected breaker entry type")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.State != StateOpen {
		t.Fatalf("breaker entry for %d/%d/%s is %v, want open", channelID, keyID, model, entry.State)
	}
	return entry.TripCount
}

// probeCooldownForTest 读一条熔断条目按当前 trip/policy 算出来的冷却时长。
func probeCooldownForTest(t *testing.T, channelID, keyID int, model, endpoint, capability string) time.Duration {
	t.Helper()
	value, ok := globalBreaker.Load(circuitScopedKey(channelID, keyID, model, endpoint, capability))
	if !ok {
		t.Fatalf("breaker entry for %d/%d/%s is missing", channelID, keyID, model)
	}
	entry, ok := value.(*circuitEntry)
	if !ok {
		t.Fatalf("unexpected breaker entry type")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return getCooldownForPolicy(entry.TripCount, entry.Policy)
}

func TestCircuitProbeConsecutiveFailuresEscalateTheBackoff(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg.MaxStreak = 5
	env.routeText(42, 1, "m")
	env.openEntry(42, 1, "m", "chat", "", 1, 100*time.Millisecond) // trip 1 → 30s
	env.refresh()

	// 500 不带 transient 策略上限，所以 trip 升级能真的把冷却拉长（30s → 60s → 120s）。
	failure := CircuitProbeReport{Attributable: true, StatusCode: http.StatusInternalServerError, Outcome: "upstream_error"}

	first, ok := env.runOnce(failure)
	if !ok || first.FromHold || first.TripCount != 1 || first.LastFailureUnixMs == 0 {
		t.Fatalf("first claim = %+v (ok=%v), want the open row snapshot", first, ok)
	}
	firstCooldown := probeCooldownForTest(t, 42, 1, "m", "chat", "")
	if firstCooldown != 60*time.Second {
		t.Fatalf("cooldown after the first probe failure = %v, want trip 2 (60s)", firstCooldown)
	}
	if got := probeOpenTripForTest(t, 42, 1, "m", "chat", ""); got != 2 {
		t.Fatalf("trip after the first probe failure = %d, want 2", got)
	}

	second, ok := env.runOnce(failure)
	if !ok || second.TripCount != 2 {
		t.Fatalf("second claim = %+v (ok=%v), want trip 2 (the entry must stay open with a fresh generation)", second, ok)
	}
	secondCooldown := probeCooldownForTest(t, 42, 1, "m", "chat", "")
	if secondCooldown <= firstCooldown {
		t.Fatalf("the second cooldown %v must be longer than the first %v", secondCooldown, firstCooldown)
	}

	views := ListChannelCircuits(42)
	if len(views) != 1 || views[0].State != "open" || views[0].TripCount != 3 || views[0].RemainingSeconds <= 60 {
		t.Fatalf("breaker after two attributable probe failures = %+v, want one open row with trip 3 and a longer cooldown", views)
	}
}

// 探测失败不替真实流量累积阈值：结算时条目已经不再是那条 OPEN（这里是真实成功把它
// 置成 Closed），就一个字节都不改。
func TestCircuitProbeFailureLeavesNonOpenRowsAlone(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(43, 1, "m")
	env.openEntry(43, 1, "m", "chat", "", 1, 5*time.Second)
	key := circuitScopedKey(43, 1, "m", "chat", "")

	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok || claim.FromHold || claim.EntrySeq == 0 {
		t.Fatalf("claim = %+v (ok=%v), want an open-row claim with identity", claim, ok)
	}
	// 探测在飞的时候，真实转发成功了：条目被置回 Closed（writeSeq 前进）。
	RecordSuccessScoped(43, 1, "m", "chat", "")

	CompleteCircuitProbe(claim, CircuitProbeReport{Attributable: true, StatusCode: http.StatusInternalServerError, Outcome: "upstream_error"})

	value, ok := globalBreaker.Load(key)
	if !ok {
		t.Fatal("the closed row must still exist")
	}
	entry := value.(*circuitEntry)
	entry.mu.Lock()
	state, failures, tripCount := entry.State, entry.ConsecutiveFailures, entry.TripCount
	entry.mu.Unlock()
	if state != StateClosed || failures != 0 || tripCount != 0 {
		t.Fatalf("a stale probe failure must not touch the row: state=%v failures=%d trip=%d", state, failures, tripCount)
	}
	if got := SnapshotCircuitProbe(43).Totals.Failed; got != 0 {
		t.Fatalf("failed counter = %d, want 0 (the settlement was stale)", got)
	}
	if results := env.historyResults(43); len(results) != 1 || results[0] != "stale" {
		t.Fatalf("history = %v, want one stale row", results)
	}
}

func TestCircuitProbeHeldRowFailureEscalatesTripWithoutTouchingTheHold(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(44, 1, "m")
	ApplyHoldScoped(44, 1, "m", "model", 10*time.Minute, "not-found", "chat", "")
	target := CircuitProbeTarget{ChannelID: 44, KeyID: 1, Model: "m", Endpoint: "chat"}

	if remaining, rule := holdRemaining(44, 1, "m"); remaining <= 9*time.Minute || rule != "not-found" {
		t.Fatalf("precondition: hold = %v %q", remaining, rule)
	}

	failure := CircuitProbeReport{Attributable: true, StatusCode: http.StatusInternalServerError, Outcome: "upstream_error"}
	wantTrip := []int{1, 2}
	wantFromHold := []bool{true, false}
	for round := 1; round <= 2; round++ {
		if !QueueManualCircuitProbe(target) {
			t.Fatalf("round %d: manual queue rejected the target", round)
		}
		claim, ok := ClaimCircuitProbe(env.clock.now)
		if !ok || !claim.Manual || claim.FromHold != wantFromHold[round-1] {
			t.Fatalf("round %d: claim = %+v (ok=%v)", round, claim, ok)
		}
		CompleteCircuitProbe(claim, failure)
		if got := probeOpenTripForTest(t, 44, 1, "m", "chat", ""); got != wantTrip[round-1] {
			t.Fatalf("round %d: breaker trip = %d, want %d", round, got, wantTrip[round-1])
		}
	}

	// hold 自己的到期时刻不动（那是人定的策略），只是它底下的冷却变长了。
	if remaining, rule := holdRemaining(44, 1, "m"); remaining <= 9*time.Minute || rule != "not-found" {
		t.Fatalf("the operator hold must not be touched by a probe failure: %v %q", remaining, rule)
	}
}

// 真实流量口径的回归：这两条是不许被探测改动的不变式。
func TestRealOpenFailureStaysClockOnly(t *testing.T) {
	newProbeTestEnv(t)
	key := circuitScopedKey(45, 1, "m", "chat", "")
	start := time.Now().Add(-5 * time.Second)
	entry := &circuitEntry{State: StateOpen, TripCount: 3, LastFailureTime: start}
	globalBreaker.Store(key, entry)

	RecordFailureWithStatusScoped(45, 1, "m", "chat", "", http.StatusInternalServerError)

	entry.mu.Lock()
	state, tripCount, lastFailure := entry.State, entry.TripCount, entry.LastFailureTime
	entry.mu.Unlock()
	if state != StateOpen {
		t.Fatalf("real traffic must keep an open row open, got %v", state)
	}
	if tripCount != 3 {
		t.Fatalf("real traffic on an open row must stay clock-only, trip became %d", tripCount)
	}
	if !lastFailure.After(start) {
		t.Fatalf("real traffic must still refresh the failure clock, got %v", lastFailure)
	}
}

func TestRealHalfOpenExpiryUnchanged(t *testing.T) {
	newProbeTestEnv(t)
	key := circuitScopedKey(46, 1, "m", "chat", "")
	// 30s 基础冷却、已过去 45s：到期后就该放一个半开探测名额进去。
	entry := &circuitEntry{State: StateOpen, TripCount: 1, LastFailureTime: time.Now().Add(-45 * time.Second)}
	globalBreaker.Store(key, entry)

	if tripped, remaining := IsTrippedScoped(46, 1, "m", "chat", ""); tripped || remaining != 0 {
		t.Fatalf("an open row past its cooldown must go half-open, got tripped=%v remaining=%v", tripped, remaining)
	}
	if tripped, _ := IsTrippedScoped(46, 1, "m", "chat", ""); !tripped {
		t.Fatal("half-open must hold every other caller until the probe reports")
	}

	RecordFailureWithStatusScoped(46, 1, "m", "chat", "", http.StatusInternalServerError)
	entry.mu.Lock()
	state, tripCount := entry.State, entry.TripCount
	entry.mu.Unlock()
	if state != StateOpen || tripCount != 2 {
		t.Fatalf("a failed half-open probe must re-open with trip+1, got state=%v trip=%d", state, tripCount)
	}
}

func TestCircuitProbeSuccessResetsOnlyTheExactTarget(t *testing.T) {
	env := newProbeTestEnv(t)
	env.openEntry(27, 1, "m-a", "chat", "", 1, 0)
	env.openEntry(27, 1, "m-b", "chat", "", 1, 0)
	env.openEntry(27, 2, "m-a", "chat", "", 1, 0)
	env.routeText(27, 1, "m-a")
	env.routeText(27, 1, "m-b")
	env.routeText(27, 2, "m-a")

	claim, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Prompt: "11 + 31 = ?", Answer: "the answer is 42", Duration: 900 * time.Millisecond})
	if !ok || claim.Target.Model != "m-a" || claim.Target.KeyID != 1 {
		t.Fatalf("claim = %+v (ok=%v), want key 1 / m-a", claim, ok)
	}
	if tripped, _ := IsTrippedScoped(27, 1, "m-a", "chat", ""); tripped {
		t.Fatal("the exact probed target must be reset")
	}
	if tripped, _ := IsTrippedScoped(27, 1, "m-b", "chat", ""); !tripped {
		t.Fatal("another model on the same key must stay open")
	}
	if tripped, _ := IsTrippedScoped(27, 2, "m-a", "chat", ""); !tripped {
		t.Fatal("the same model on another key must stay open")
	}

	view := SnapshotCircuitProbe(27)
	if view.Totals.Recovered != 1 || view.Totals.Used != 1 {
		t.Fatalf("totals = %+v, want used 1 recovered 1", view.Totals)
	}
	if len(view.History) != 1 {
		t.Fatalf("history rows = %d, want 1", len(view.History))
	}
	row := view.History[0]
	if row.Result != "recovered" || row.Model != "m-a" || row.KeyID != 1 || row.Answer != "42" || row.DurationMS != 900 {
		t.Fatalf("history row = %+v", row)
	}
	if row.Prompt != "11 + 31 = ?" {
		t.Fatalf("prompt = %q, want our own sanitized question", row.Prompt)
	}
	if _, err := time.Parse(time.RFC3339, row.Timestamp); err != nil {
		t.Fatalf("history timestamp %q is not RFC3339: %v", row.Timestamp, err)
	}
}

// 成功重置覆盖同一条 channel:key:model 的**所有作用域变体**（旧的 mixed_scope 例外已取消），
// 但别的 model、别的 key 一条都不动。
func TestCircuitProbeResetCoversEveryScopeVariant(t *testing.T) {
	env := newProbeTestEnv(t)
	env.openEntry(28, 1, "m", "chat", "", 1, 0)
	env.openEntry(28, 1, "m", "chat", "stream", 1, 0)
	env.openEntry(28, 1, "other", "chat", "", 1, 0)
	env.openEntry(28, 2, "m", "chat", "", 1, 0)
	env.routeText(28, 1, "m")
	env.routeText(28, 1, "other")
	env.routeText(28, 2, "m")

	claim, ok := env.runOnce(CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "9"})
	if !ok || claim.Target.Model != "m" || claim.Target.KeyID != 1 || claim.Target.Capability != "" {
		t.Fatalf("claim = %+v (ok=%v), want key 1 / m / first scope", claim, ok)
	}
	// 同一 key+model 的两个作用域变体全部清掉。
	if tripped, _ := IsTrippedScoped(28, 1, "m", "chat", ""); tripped {
		t.Fatal("the claimed scope must be reset")
	}
	if tripped, _ := IsTrippedScoped(28, 1, "m", "chat", "stream"); tripped {
		t.Fatal("every scope variant of the same channel+key+model must be reset")
	}
	// 别的 model / 别的 key 不受影响。
	if tripped, _ := IsTrippedScoped(28, 1, "other", "chat", ""); !tripped {
		t.Fatal("another model on the same key must stay open")
	}
	if tripped, _ := IsTrippedScoped(28, 2, "m", "chat", ""); !tripped {
		t.Fatal("the same model on another key must stay open")
	}

	view := SnapshotCircuitProbe(28)
	if view.Totals.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1", view.Totals.Recovered)
	}
	if len(view.History) == 0 || view.History[0].Result != "recovered" {
		t.Fatalf("history = %+v, want a recovered row", view.History)
	}
}

func TestCircuitProbeStaleCompletionChangesNothing(t *testing.T) {
	env := newProbeTestEnv(t)
	env.openEntry(29, 1, "m", "chat", "", 1, 5*time.Second)
	env.routeText(29, 1, "m")

	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the entry must be claimable")
	}

	// 探测期间条目换了冷却段（真实流量又记了一次失败）。
	RecordFailureWithStatusScoped(29, 1, "m", "chat", "", http.StatusServiceUnavailable)

	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})

	if tripped, remaining := IsTrippedScoped(29, 1, "m", "chat", ""); !tripped || remaining <= 0 {
		t.Fatalf("a stale success must not reset the entry, got %v %v", tripped, remaining)
	}
	view := SnapshotCircuitProbe(29)
	if view.Totals.Recovered != 0 {
		t.Fatalf("recovered = %d, want 0", view.Totals.Recovered)
	}
	if len(view.History) != 1 || view.History[0].Result != "stale" {
		t.Fatalf("history = %+v, want a stale row", view.History)
	}
}

func TestCircuitProbeCanceledProbeDoesNotRequeueInSameGeneration(t *testing.T) {
	env := newProbeTestEnv(t)
	env.openEntry(30, 1, "m", "chat", "", 1, 0)
	env.routeText(30, 1, "m")

	if _, ok := env.runOnce(CircuitProbeReport{Outcome: "canceled"}); !ok {
		t.Fatal("the first probe must be claimable")
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("a canceled probe must not be re-run inside the same cooldown generation")
	}
	entry, ok := env.entryView(30, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateStopped {
		t.Fatalf("view = %+v (found=%v), want stopped for the probed generation", entry, ok)
	}
	view := SnapshotCircuitProbe(30)
	if view.Totals.Used != 1 || view.Totals.Skipped != 1 || view.Totals.Failed != 0 || view.Totals.Recovered != 0 {
		t.Fatalf("totals = %+v, want used 1 skipped 1", view.Totals)
	}
	if len(view.History) != 1 || view.History[0].Result != "skipped:canceled" {
		t.Fatalf("history = %+v, want a skipped:canceled row", view.History)
	}
}

func TestCircuitProbeHistoryRingKeepsLatestTwenty(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(31, 1, "m")
	env.openEntry(31, 1, "m", "chat", "", 1, 5*time.Second)
	target := CircuitProbeTarget{ChannelID: 31, KeyID: 1, Model: "m", Endpoint: "chat"}

	for i := 1; i <= 22; i++ {
		if !QueueManualCircuitProbe(target) {
			t.Fatalf("round %d: manual queue rejected the target", i)
		}
		claim, ok := ClaimCircuitProbe(env.clock.now)
		if !ok || !claim.Manual {
			t.Fatalf("round %d: no manual claim (%+v, ok=%v)", i, claim, ok)
		}
		// 不归因的结论不改变冷却段：条目一直是活的，下一轮还能手动再探。
		CompleteCircuitProbe(claim, CircuitProbeReport{Outcome: "canceled", Answer: fmt.Sprintf("answer: %d", i)})
	}

	view := SnapshotCircuitProbe(31)
	if len(view.History) != probeHistoryRingSize {
		t.Fatalf("history rows = %d, want %d", len(view.History), probeHistoryRingSize)
	}
	if view.History[0].Answer != "22" || view.History[len(view.History)-1].Answer != "3" {
		t.Fatalf("history order = %q .. %q, want latest first", view.History[0].Answer, view.History[len(view.History)-1].Answer)
	}
	if view.History[0].Result != "skipped:canceled" {
		t.Fatalf("history result = %q, want the reason token", view.History[0].Result)
	}
	if view.Totals.Used != 22 || view.GlobalTotals.Used != 22 {
		t.Fatalf("totals = %+v / %+v, want used 22", view.Totals, view.GlobalTotals)
	}
}

func TestCircuitProbeHistoryStoresOnlySafeNumbers(t *testing.T) {
	if got := probeSafeAnswer("the sum is 42, because 40 + 2"); got != "42" {
		t.Fatalf("safe answer = %q, want only the first number", got)
	}
	if got := probeSafeAnswer("no digits here"); got != "" {
		t.Fatalf("safe answer = %q, want empty", got)
	}
	if got := probeSafeAnswer("1234567890123456789012345678901234567890"); len(got) != probeAnswerLimit {
		t.Fatalf("safe answer length = %d, want %d", len(got), probeAnswerLimit)
	}
	if got := probeSafePrompt("请计算 12 + 30 的结果，只回数字"); got != "请计算 12 + 30 的结果，只回数字" {
		t.Fatalf("safe prompt (cn) = %q, want the actual template", got)
	}
	if got := probeSafePrompt("7 × 8 = ?"); got != "7 × 8 = ?" {
		t.Fatalf("safe prompt (times) = %q", got)
	}
	if got := probeSafePrompt("9 − 4 等于几？"); got != "9 − 4 等于几？" {
		t.Fatalf("safe prompt (minus) = %q", got)
	}
	if got := probeSafePrompt("What is 12 + 30? answer with the number only"); got != "What is 12 + 30? answer with the number only" {
		t.Fatalf("safe prompt (en) = %q, want the actual template", got)
	}
	// 不合规字符：整条丢掉，而不是悄悄裁成半句话。
	if got := probeSafePrompt("```rm -rf /```"); got != "" {
		t.Fatalf("safe prompt = %q, want an empty result for nonconforming text", got)
	}
	if got := probeSafePrompt("ok @user #tag"); got != "" {
		t.Fatalf("safe prompt = %q, want an empty result for nonconforming text", got)
	}
	// 太长：按 rune 截断（不切半个字）。
	long := ""
	for range probePromptLimit + 30 {
		long += "加"
	}
	if got := probeSafePrompt(long); len([]rune(got)) != probePromptLimit {
		t.Fatalf("safe prompt length = %d runes, want %d", len([]rune(got)), probePromptLimit)
	}
	// 凭据即使在题面里（理论上不该发生）也会被抹掉，且抹完仍能通过校验。
	// pack-1128（用户 11-28 拍板）：题面原样保留，不再抹凭据形状。
	if got := probeSafePrompt("sk-abcdef123456 加 12 + 3 = ?"); !strings.Contains(got, "sk-abcdef123456") {
		t.Fatalf("pack-1128: safe prompt must preserve text verbatim, got %q", got)
	}
}

func TestCircuitProbeKeepsASingleGlobalLane(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(32, 1, "m")
	env.openEntry(32, 1, "m", "chat", "", 1, 0)
	env.routeText(33, 1, "m")
	env.openEntry(33, 1, "m", "chat", "", 1, 0)

	claim, ok := ClaimCircuitProbe(env.clock.Now())
	if !ok {
		t.Fatal("the first probe must be claimable")
	}
	entry, _ := env.entryView(32, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateRunning {
		t.Fatalf("in-flight entry state = %q, want running", entry.Probe.State)
	}
	if _, ok := ClaimCircuitProbe(env.clock.Now()); ok {
		t.Fatal("only one probe may be in flight at a time")
	}
	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok"})
	if _, ok := ClaimCircuitProbe(env.clock.Now()); !ok {
		t.Fatal("the lane must be free again after completion")
	}
}

func TestCircuitProbeYieldsTheLaneToAPendingAudit(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(34, 1, "m")
	env.openEntry(34, 1, "m", "chat", "", 1, 0)

	env.auditPending = true
	env.refresh()
	if _, ok := ClaimCircuitProbe(env.clock.Now()); ok {
		t.Fatal("a pending audit must take the lane ahead of automatic probing")
	}

	env.auditPending = false
	env.refresh()
	if _, ok := ClaimCircuitProbe(env.clock.Now()); !ok {
		t.Fatal("automatic probing must resume when the audit lane is free")
	}
}

func TestBeginRuntimeAttemptMarksRealTrafficForTheProbeScheduler(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(35, 1, "m")
	env.openEntry(35, 1, "m", "chat", "", 1, 0)

	release := BeginRuntimeAttempt(35, 1, "m")
	release()

	entry, ok := env.entryView(35, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateSkippedTraffic {
		t.Fatalf("real traffic must keep the probe away, got %+v (found=%v)", entry.Probe, ok)
	}
	if _, ok := ClaimCircuitProbe(env.clock.Now()); ok {
		t.Fatal("a probe must not run right after real traffic on the same tuple")
	}
}

func TestRealRuntimeFailureWaitReachesTheProbeLane(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(47, 1, "m")
	env.openEntry(47, 1, "m", "chat", "", 1, 0)

	// 真实转发撞上的上游等待：502 + 10 分钟 Retry-After。
	RecordRuntimeFailure(47, 1, "m", http.StatusBadGateway, time.Second, 10*time.Minute)

	// 探测车道拿到的是原值（不设上限）。
	if remaining, cause := CircuitProbeWaitRemaining(47, 1, "m"); remaining != 10*time.Minute || cause != CircuitProbeWaitCauseRetryAfter {
		t.Fatalf("probe wait = %v %q, want the raw 10m retry_after", remaining, cause)
	}
	// 真实流量的软冷却口径没变：502 的 Retry-After 仍按既有上限收敛。
	if got := SnapshotKeyRuntime(47, 1, "m").CooldownRemainingMs; got <= 0 || got > int64(maxTransientRetryAfterCooldown/time.Millisecond) {
		t.Fatalf("runtime cooldown = %dms, want it still bounded by %v", got, maxTransientRetryAfterCooldown)
	}

	entry, _ := env.entryView(47, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateWaitUpstream || entry.Probe.NextInSeconds != 600 {
		t.Fatalf("view = %+v, want wait_upstream in 600s", entry.Probe)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("the automatic probe must respect the real upstream wait")
	}
	rejection, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 47, KeyID: 1, Model: "m", Endpoint: "chat"}, true)
	if ok || rejection.State != CircuitProbeStateWaitUpstream || rejection.RemainingSeconds != 600 {
		t.Fatalf("manual admission = %+v (ok=%v), want wait_upstream/600s", rejection, ok)
	}
}

func TestRealRuntimeFailureFallback429Wait(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(48, 1, "m")
	env.openEntry(48, 1, "m", "chat", "", 1, 0)

	// 429 但上游没给 Retry-After（回落链路常见形态）。
	RecordRuntimeFailure(48, 1, "m", http.StatusTooManyRequests, time.Second, 0)

	// 没有头时不是拍一个固定值：用真实转发那套指数冷却（首发 5s），所以可能超过 30s。
	want := runtimeCooldownForStatus(http.StatusTooManyRequests, 1)
	if want <= 0 {
		t.Fatalf("precondition: runtime cooldown for 429 must be positive")
	}
	if remaining, cause := CircuitProbeWaitRemaining(48, 1, "m"); remaining != want || cause != CircuitProbeWaitCauseFallback429 {
		t.Fatalf("probe wait = %v %q, want %v fallback_429 (exponential runtime cooldown)", remaining, cause, want)
	}
	rejection, ok := CircuitProbeAdmission(env.clock.now, CircuitProbeTarget{ChannelID: 48, KeyID: 1, Model: "m", Endpoint: "chat"}, true)
	if ok || rejection.State != CircuitProbeStateWaitUpstream || rejection.RemainingSeconds != int(want/time.Second) {
		t.Fatalf("manual admission = %+v (ok=%v), want wait_upstream/%vs", rejection, ok, want)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("a fallback 429 without Retry-After must still stop the automatic probe")
	}
}

func TestCircuitProbeFallback429WithoutHeaderSetsAWait(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(49, 1, "m")
	env.openEntry(49, 1, "m", "chat", "", 1, 100*time.Millisecond)

	report := CircuitProbeReport{Attributable: true, StatusCode: http.StatusTooManyRequests, Outcome: "upstream_error"}
	if _, ok := env.runOnce(report); !ok {
		t.Fatal("the probe must be claimable")
	}
	// 没有头的 429 也记等待，而且走真实转发那套指数冷却（不是固定 30s）。
	wantFallback := runtimeCooldownForStatus(http.StatusTooManyRequests, 1)
	if remaining, cause := CircuitProbeWaitRemaining(49, 1, "m"); remaining != wantFallback || cause != CircuitProbeWaitCauseFallback429 {
		t.Fatalf("probe wait = %v %q, want %v fallback_429 (a 429 without a header still means wait)", remaining, cause, wantFallback)
	}
	entry, _ := env.entryView(49, 1, "m", "chat", "")
	if entry.Probe.State != CircuitProbeStateWaitUpstream || entry.Probe.NextInSeconds != int(wantFallback/time.Second) {
		t.Fatalf("view = %+v, want wait_upstream after a fallback 429", entry.Probe)
	}
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("after a fallback 429 the next probe must wait")
	}
}

func TestScopedHeldRowIsProbeableWithMetadata(t *testing.T) {
	env := newProbeTestEnv(t)

	// 普通文本作用域 + 元数据：这条 hold 行可探。
	ApplyHoldScoped(51, 1, "m", "model", 5*time.Minute, "model-not-found", "chat", "")
	env.routeText(51, 1, "m")
	entry, ok := env.entryView(51, 1, "m", "chat", "")
	if !ok || entry.Probe.State != CircuitProbeStateScheduled {
		t.Fatalf("scoped held row = %+v (found=%v), want scheduled", entry.Probe, ok)
	}
	// 运维列表里也带着这个作用域，看得懂这条 hold 是什么形状的流量触发的。
	sawHold := false
	for _, row := range ListChannelCircuits(51) {
		if row.State != "held" {
			continue
		}
		sawHold = true
		if row.Endpoint != "chat" {
			t.Fatalf("hold row = %+v, want the offered endpoint scope", row)
		}
	}
	if !sawHold {
		t.Fatal("the scoped hold must still be listed as held")
	}
	// 元数据只服务探测：真实 hold 匹配/路由判定仍然只看 channel:key:model，
	// 所以同一模型/密钥的**其它作用域**照样被这条 hold 挡住。
	if tripped, _ := IsTrippedScoped(51, 1, "m", "responses", "stream"); !tripped {
		t.Fatal("hold metadata must not narrow hold matching")
	}
	if QueueManualCircuitProbe(CircuitProbeTarget{ChannelID: 51, KeyID: 1, Model: "m", Endpoint: "responses", Capability: "stream"}) {
		t.Fatal("a hold cannot admit a manual probe in a different scope")
	}
	if tripped, _ := IsTrippedScoped(51, 2, "m", "chat", ""); tripped {
		t.Fatal("hold metadata must not widen hold matching to another key")
	}

	// 老口径 hold（没有元数据）不探。
	ApplyHold(52, 1, "m", "model", 5*time.Minute, "legacy")
	env.routeText(52, 1, "m")
	if entry, _ := env.entryView(52, 1, "m", "", ""); entry.Probe.State != CircuitProbeStateNotProbedMedia {
		t.Fatalf("legacy held row = %+v, want not_probed_media", entry.Probe)
	}

	// 通配 hold 无论有没有元数据都不是探测行。
	ApplyHoldScoped(53, 0, "", "channel", 5*time.Minute, "suspended", "chat", "")
	ApplyHoldScoped(53, 1, "", "key", 5*time.Minute, "quota", "chat", "")
	if view := SnapshotCircuitProbe(53); len(view.Entries) != 0 {
		t.Fatalf("wildcard holds must not appear as probe rows: %+v", view.Entries)
	}

	// 媒体作用域的元数据同样不探。
	ApplyHoldScoped(54, 1, "m", "model", 5*time.Minute, "image", "images_generations", "")
	if entry, _ := env.entryView(54, 1, "m", "images_generations", ""); entry.Probe.State != CircuitProbeStateNotProbedMedia {
		t.Fatalf("image-scoped held row = %+v, want not_probed_media", entry.Probe)
	}

	// 错误规则也一样：带作用域的 hold 可探，老口径的仍不探。
	withRules(t, `[{"name":"not-found","status_codes":[404],"keywords":["model_not_found"],"scope":"model","cooldown_seconds":600}]`)
	env.routeText(55, 1, "m")
	ApplyErrorRulesScoped(55, 1, "m", http.StatusNotFound, `{"error":{"code":"model_not_found"}}`, "chat", "")
	if entry, _ := env.entryView(55, 1, "m", "chat", ""); entry.Probe.State != CircuitProbeStateScheduled {
		t.Fatalf("scoped error-rule hold = %+v, want scheduled", entry.Probe)
	}
	env.routeText(56, 1, "m")
	ApplyErrorRules(56, 1, "m", http.StatusNotFound, `{"error":{"code":"model_not_found"}}`)
	if entry, _ := env.entryView(56, 1, "m", "", ""); entry.Probe.State != CircuitProbeStateNotProbedMedia {
		t.Fatalf("legacy error-rule hold = %+v, want not_probed_media", entry.Probe)
	}
}

// 车道占用必须盖住整次探测（含结算），否则第二条探测会在状态改到一半时插进来。
func TestCircuitProbeLaneStaysHeldDuringTheProbeCall(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(57, 1, "m")
	env.openEntry(57, 1, "m", "chat", "", 1, 100*time.Millisecond)

	var claimedDuringCall []bool
	env.onProbe = func() {
		_, ok := ClaimCircuitProbe(env.clock.now)
		claimedDuringCall = append(claimedDuringCall, ok)
	}
	env.report = CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"}
	env.refresh()

	if ran := RunDueCircuitProbes(context.Background()); ran != 1 {
		t.Fatalf("drained %d probes, want 1", ran)
	}
	if len(claimedDuringCall) != 1 || claimedDuringCall[0] {
		t.Fatalf("a second claim succeeded while the lane was reserved: %v", claimedDuringCall)
	}
	if len(env.runs) != 1 {
		t.Fatalf("RunProbe calls = %d, want 1 (the drain must stay serial)", len(env.runs))
	}
	if tripped, _ := IsTrippedScoped(57, 1, "m", "chat", ""); tripped {
		t.Fatal("the settled probe must have reset the exact target")
	}
}

// 升级不能把既有冷却收窄：典型的 5xx(120s 上限) 条目录得一次 503(30s 上限) 不应该反过来
// 变短。
func TestCircuitProbeEscalationKeepsAWiderCooldownCeiling(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(58, 1, "m")
	// 通用 5xx 策略（policy 零值）：trip 3 → 120s 上限；只过了 70s，剩 50s 在窗口内。
	globalBreaker.Store(circuitScopedKey(58, 1, "m", "chat", ""), &circuitEntry{
		State:           StateOpen,
		TripCount:       3,
		LastFailureTime: env.clock.now.Add(-70 * time.Second),
		writeSeq:        nextCircuitRevision(),
	})
	env.refresh()

	failure := CircuitProbeReport{Attributable: true, StatusCode: http.StatusServiceUnavailable, Outcome: "upstream_error"}
	if _, ok := env.runOnce(failure); !ok {
		t.Fatal("the probe must be claimable")
	}
	if cooldown := probeCooldownForTest(t, 58, 1, "m", "chat", ""); cooldown < 120*time.Second {
		t.Fatalf("escalation narrowed the cooldown to %v, want it kept at the earlier 120s tier", cooldown)
	}
	if tripped, remaining := IsTrippedScoped(58, 1, "m", "chat", ""); !tripped || remaining < 119*time.Second {
		t.Fatalf("the entry must stay cooling for its earlier tier, got %v %v", tripped, remaining)
	}
}

// 到期前发出、到期后才回来的探测，只要条目还是同一条未变的 OPEN，结论依然作数。
func TestCircuitProbeSettlesAfterExpiryWhenUnchanged(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(61, 1, "m")
	env.openEntry(61, 1, "m", "chat", "", 1, 18*time.Second) // 剩 12s

	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable before expiry")
	}
	env.clock.now = env.clock.now.Add(2 * time.Minute) // 到期之后才回来

	CompleteCircuitProbe(claim, CircuitProbeReport{Attributable: true, StatusCode: 0, Outcome: "timeout"})

	if got := probeOpenTripForTest(t, 61, 1, "m", "chat", ""); got != 2 {
		t.Fatalf("a probe started before expiry and finished after it must still settle, trip=%d", got)
	}
	if view := SnapshotCircuitProbe(61); view.Totals.Failed != 1 {
		t.Fatalf("failed = %d, want 1", view.Totals.Failed)
	}
}

// 成功只清“还是准入时那一条”的那一段：期间被真实写换代了就不动。
func TestCircuitProbeResetSkipsReplacementGeneration(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(62, 1, "m")
	env.openEntry(62, 1, "m", "chat", "", 1, 5*time.Second)

	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable")
	}
	// 探测在飞的时候，真实流量又记了一次失败：同一条条目被写过 ⇒ 换了代。
	RecordFailureWithStatusScoped(62, 1, "m", "chat", "", http.StatusServiceUnavailable)

	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})

	if tripped, remaining := IsTrippedScoped(62, 1, "m", "chat", ""); !tripped || remaining <= 0 {
		t.Fatalf("a replaced generation must not be reset, got %v %v", tripped, remaining)
	}
	view := SnapshotCircuitProbe(62)
	if view.Totals.Recovered != 0 {
		t.Fatalf("recovered = %d, want 0", view.Totals.Recovered)
	}
	if len(view.History) == 0 || view.History[0].Result != "stale" {
		t.Fatalf("history = %+v, want a stale row", view.History)
	}
}

// Retry-After 先于任何 stale/取消/不归因的遮挡：原始路由上一定看得到等待。
func TestCircuitProbeRetryAfterHonoredEvenWhenStale(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(66, 1, "m")
	env.openEntry(66, 1, "m", "chat", "", 1, 5*time.Second)

	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable")
	}
	RecordFailureWithStatusScoped(66, 1, "m", "chat", "", http.StatusServiceUnavailable) // 换代 ⇒ 结论会作废

	CompleteCircuitProbe(claim, CircuitProbeReport{Attributable: true, StatusCode: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute, Outcome: "upstream_error"})

	if remaining, cause := CircuitProbeWaitRemaining(66, 1, "m"); remaining != 2*time.Minute || cause != CircuitProbeWaitCauseRetryAfter {
		t.Fatalf("Retry-After must be honored before the stale mask: %v %q", remaining, cause)
	}
	// 也没有替那条（已经被真实流量换代过的）条目加 trip。
	if views := ListChannelCircuits(66); len(views) != 1 || views[0].TripCount != claim.TripCount {
		t.Fatalf("a stale settlement must not touch the breaker: %+v", views)
	}
}

// 历史里要分得出 timeout / wrong_answer / 各类 skip，而且 tick 侧的 skip 每个理由每段只记一次。
func TestCircuitProbeHistoryKeepsDistinctReasons(t *testing.T) {
	env := newProbeTestEnv(t)

	env.routeText(63, 1, "m")
	env.openEntry(63, 1, "m", "chat", "", 1, 100*time.Millisecond)
	if _, ok := env.runOnce(CircuitProbeReport{Attributable: true, StatusCode: 0, Outcome: "timeout"}); !ok {
		t.Fatal("the timeout probe must run")
	}
	if _, ok := env.runOnce(CircuitProbeReport{Attributable: true, StatusCode: http.StatusOK, Outcome: "wrong_answer"}); !ok {
		t.Fatal("the wrong-answer probe must run")
	}
	results := env.historyResults(63)
	if len(results) != 2 || results[0] != "failed:wrong_answer" || results[1] != "failed:timeout" {
		t.Fatalf("history = %v, want [failed:wrong_answer failed:timeout] (latest first)", results)
	}

	// 真实流量导致 skip：每个理由每段冷却只记一条。
	env.routeText(64, 1, "m")
	env.openEntry(64, 1, "m", "chat", "", 1, 0)
	noteCircuitProbeRealTraffic(64, 1, "m", env.clock.now)
	for i := 0; i < 3; i++ {
		TickCircuitProbe(env.clock.now)
	}
	trafficResults := env.historyResults(64)
	if len(trafficResults) != 1 || trafficResults[0] != "skipped:"+probeSkipTraffic {
		t.Fatalf("history = %v, want exactly one skipped_traffic row", trafficResults)
	}

	// 上游等待同理。
	env.routeText(65, 1, "m")
	env.openEntry(65, 1, "m", "chat", "", 1, 0)
	ApplyCircuitProbeWait(65, 1, "m", CircuitProbeWaitCauseFallback429, time.Minute)
	for i := 0; i < 3; i++ {
		TickCircuitProbe(env.clock.now)
	}
	waitResults := env.historyResults(65)
	if len(waitResults) != 1 || waitResults[0] != "skipped:"+probeSkipWait {
		t.Fatalf("history = %v, want exactly one wait_upstream row", waitResults)
	}
}

// 手动探测只对“确切的、现在正在挡路的条目”生效；陈旧请求绝不发出去。
func TestManualProbeRequiresALiveEntryAndNeverSendsStale(t *testing.T) {
	env := newProbeTestEnv(t)
	env.cfg.Enabled = false
	env.refresh()
	env.routeText(67, 1, "m")
	target := CircuitProbeTarget{ChannelID: 67, KeyID: 1, Model: "m", Endpoint: "chat"}

	if QueueManualCircuitProbe(target) {
		t.Fatal("a manual probe must not be queued without a live eligible entry")
	}

	env.openEntry(67, 1, "m", "chat", "", 1, 5*time.Second)
	if !QueueManualCircuitProbe(target) {
		t.Fatal("a live open entry must accept a manual probe")
	}
	// 入队之后条目换代（真实流量）：领的时候直接出队，绝不发出去、也不记 recovered。
	RecordFailureWithStatusScoped(67, 1, "m", "chat", "", http.StatusServiceUnavailable)
	if _, ok := ClaimCircuitProbe(env.clock.now); ok {
		t.Fatal("a stale manual request must never be sent")
	}
	if view := SnapshotCircuitProbe(67); view.Totals.Recovered != 0 || view.Totals.Used != 0 {
		t.Fatalf("a stale manual request must not count: %+v", view.Totals)
	}

	// 活条目 + 队列：能领、能结算成 recovered。
	if !QueueManualCircuitProbe(target) {
		t.Fatal("the live entry must accept a manual probe again")
	}
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok || !claim.Manual {
		t.Fatalf("manual claim = %+v (ok=%v)", claim, ok)
	}
	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})
	if view := SnapshotCircuitProbe(67); view.Totals.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1", view.Totals.Recovered)
	}
}

func TestCircuitProbeDefaultStreakIsThree(t *testing.T) {
	if DefaultCircuitProbeMaxStreak != 3 {
		t.Fatalf("default streak = %d, want 3", DefaultCircuitProbeMaxStreak)
	}
	if got := (CircuitProbeConfig{}).maxStreak(); got != 3 {
		t.Fatalf("zero config streak = %d, want the 3 fallback", got)
	}
	if got := (CircuitProbeConfig{MaxStreak: 9}).maxStreak(); got != 9 {
		t.Fatalf("configured streak = %d, want 9", got)
	}
}

// 并发领探测：单车道 + 同代去重 ⇒ 只能有一个成功。
func TestCircuitProbeConcurrentClaimsOnlyOneWins(t *testing.T) {
	env := newProbeTestEnv(t)
	for i := 0; i < 4; i++ {
		env.routeText(70+i, 1, "m")
		env.openEntry(70+i, 1, "m", "chat", "", 1, 5*time.Second)
	}
	env.refresh()

	const workers = 8
	var wg sync.WaitGroup
	oks := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, oks[slot] = ClaimCircuitProbe(env.clock.Now())
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, ok := range oks {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent claims = %d, want exactly 1 (single global lane)", wins)
	}
}

// 重置必须与 tuple 级提交锁串行：真实失败正在提交这个 tuple 时，重置要等它。
func TestCircuitProbeResetSerializesWithTupleCommitLock(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(75, 1, "m")
	env.openEntry(75, 1, "m", "chat", "", 1, 5*time.Second)
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable")
	}

	// 模拟"真实失败正在提交这个 tuple"：先占住 tuple 锁。
	unlockReal := lockCircuitTuple(75, 1, "m")
	done := make(chan bool, 1)
	go func() { done <- probeResetClaimed(claim) }()

	select {
	case <-done:
		t.Fatal("the reset must wait for the tuple commit lock")
	case <-time.After(50 * time.Millisecond):
	}
	unlockReal()
	if reset := <-done; !reset {
		t.Fatal("after the lock is released the unchanged claim must reset")
	}
}

// 旧 claim 不能清掉重建后的新一代：即使重建后的形状（trip=1）看起来一样。
func TestCircuitProbeResetRefusesRecreatedEntry(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(76, 1, "m")
	env.openEntry(76, 1, "m", "chat", "", 1, 5*time.Second)
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok {
		t.Fatal("the probe must be claimable")
	}

	// 条目被重置后又被真实失败重建（trip 正好也走到 1，内容上看不出区别）。
	ResetScoped(76, 1, "m")
	for i := 0; i < 10; i++ {
		RecordFailureWithStatusScoped(76, 1, "m", "chat", "", http.StatusServiceUnavailable)
	}
	if got := probeOpenTripForTest(t, 76, 1, "m", "chat", ""); got != 1 {
		t.Fatalf("precondition: the recreated row should be back at trip 1, got %d", got)
	}

	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})

	if tripped, _ := IsTrippedScoped(76, 1, "m", "chat", ""); !tripped {
		t.Fatal("a recreated generation must not be reset by an old claim")
	}
	if view := SnapshotCircuitProbe(76); view.Totals.Recovered != 0 {
		t.Fatalf("recovered = %d, want 0", view.Totals.Recovered)
	}
}

// hold 被延长之后（换了版本号），旧 claim 不得把它清掉。
func TestCircuitProbeHeldResetRefusesExtendedHold(t *testing.T) {
	env := newProbeTestEnv(t)
	env.routeText(77, 1, "m")
	ApplyHoldScoped(77, 1, "m", "model", 10*time.Minute, "rule-a", "chat", "")
	target := CircuitProbeTarget{ChannelID: 77, KeyID: 1, Model: "m", Endpoint: "chat"}
	if !QueueManualCircuitProbe(target) {
		t.Fatal("a live held row must accept a manual probe")
	}
	claim, ok := ClaimCircuitProbe(env.clock.now)
	if !ok || !claim.FromHold {
		t.Fatalf("claim = %+v (ok=%v), want a hold-row claim", claim, ok)
	}

	// 探测在飞时这条 hold 被延长（同一作用域，换了版本号）。
	ApplyHoldScoped(77, 1, "m", "model", 20*time.Minute, "rule-a", "chat", "")

	CompleteCircuitProbe(claim, CircuitProbeReport{Recovered: true, Outcome: "ok", Answer: "42"})

	if remaining, _ := holdRemaining(77, 1, "m"); remaining <= 10*time.Minute {
		t.Fatalf("an extended hold must not be cleared by a stale claim, remaining=%v", remaining)
	}
}
