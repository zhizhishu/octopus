package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
)

// 熔断探测接线的用例集。全部**不启动 worker、不发任何真实探测请求**：RunProbe 只在
// 真正跑探测车道时才会被调到，而这一组用例一次都不跑它；出站那一侧（modeltest）由它
// 自己的用例覆盖。

const (
	// 本文件所有值都是隔离临时库里的假 fixture，不涉及任何真实凭据或上游。
	probeTestChannelKey = "sk-upstream-test-probe-key-0123456789"
	probeTestModel      = "probe-model"
	probeTripFailures   = 12 // 大于默认阈值 10，保证条目真的进 OPEN
)

type circuitProbeTestEnv struct {
	userAdminEnv
	channel model.Channel
	fake    *circuitProbeFake
}

// circuitProbeFake 记录探测车道被真正跑过几次。本套用例要求它恒为 0。
type circuitProbeFake struct {
	runs int
}

// newCircuitProbeTestEnv 复用 user_admin 那套真实路由表 + 真实 Auth/AdminOnly 中间件
// （router.RegisterAll 会把本文件的 /channel/probe* 一并注册进来），再加一个渠道。
func newCircuitProbeTestEnv(t *testing.T) *circuitProbeTestEnv {
	t.Helper()
	balancer.ResetCircuitProbe()
	t.Cleanup(balancer.ResetCircuitProbe)

	base := setupUserAdminEnv(t)
	channel := model.Channel{
		Name:    "probe-channel",
		Enabled: true,
		Model:   probeTestModel,
		Keys:    []model.ChannelKey{{Enabled: true, ChannelKey: probeTestChannelKey}},
	}
	if err := op.ChannelCreate(&channel, base.ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { balancer.ResetChannel(channel.ID) })

	env := &circuitProbeTestEnv{userAdminEnv: base, channel: channel}
	env.configureFake()
	return env
}

// configureFake 给调度器注入假回调：把目标自己的作用域原样翻成路由，没有任何真实出站。
// （接线层正是这么做的：作用域抄自熔断键，而不是去表里另找一条。）
func (e *circuitProbeTestEnv) configureFake() {
	fake := &circuitProbeFake{}
	e.fake = fake
	channelID := e.channel.ID
	balancer.ConfigureCircuitProbe(balancer.CircuitProbeConfig{}, balancer.CircuitProbeCallbacks{
		ResolveTarget: func(target balancer.CircuitProbeTarget) (balancer.CircuitProbeRoute, bool) {
			if target.ChannelID != channelID || target.KeyID <= 0 || target.Model == "" || target.Endpoint == "" {
				return balancer.CircuitProbeRoute{}, false
			}
			return balancer.CircuitProbeRoute{Endpoint: target.Endpoint, Capability: target.Capability}, true
		},
		ChannelEnabled: func(int) bool { return true },
		AuditPending:   func() bool { return false },
		RunProbe: func(ctx context.Context, claim balancer.CircuitProbeClaim) balancer.CircuitProbeReport {
			// 走到这里说明有人真的在跑探测车道 —— 用例里绝不允许。
			fake.runs++
			return balancer.CircuitProbeReport{}
		},
	})
}

func (e *circuitProbeTestEnv) keyID() int { return e.channel.Keys[0].ID }

// drainCircuitProbeWake 清空唤醒信号（前面的用例可能留过一次未消费的唤醒）。
func drainCircuitProbeWake() {
	for {
		select {
		case <-circuitProbeWake:
		default:
			return
		}
	}
}

// tripScoped 把这条 channel/key/model+作用域 打到 OPEN（用 500：它不在任何阈值抬高的
// 策略表里，所以默认阈值 10 就是判据）。
func (e *circuitProbeTestEnv) tripScoped(t *testing.T, endpoint, capability string) {
	t.Helper()
	for range probeTripFailures {
		balancer.RecordFailureWithStatusScoped(e.channel.ID, e.keyID(), probeTestModel, endpoint, capability, http.StatusInternalServerError)
	}
	tripped := false
	for _, entry := range balancer.ListChannelCircuits(e.channel.ID) {
		if entry.Model == probeTestModel && entry.Endpoint == endpoint && entry.Capability == capability && entry.State == "open" {
			tripped = true
		}
	}
	if !tripped {
		t.Fatalf("precondition: the %s/%q scope must be open", endpoint, capability)
	}
}

func (e *circuitProbeTestEnv) startProbe(t *testing.T) channelProbeStartResponse {
	t.Helper()
	return e.startProbeWith(t, nil)
}

func (e *circuitProbeTestEnv) startProbeWith(t *testing.T, scope map[string]any) channelProbeStartResponse {
	t.Helper()
	body := map[string]any{"id": e.channel.ID, "key_id": e.keyID(), "model": probeTestModel}
	for name, value := range scope {
		body[name] = value
	}
	rec, data := e.do(t, http.MethodPost, "/api/v1/channel/probe", e.token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("start probe = %d %s", rec.Code, rec.Body.String())
	}
	var got channelProbeStartResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode start response: %v (%s)", err, data)
	}
	return got
}

// ── 出厂口径与校验 ──────────────────────────────────────────────────────

func TestCircuitProbeSettingDefaultsAndValidation(t *testing.T) {
	want := []model.Setting{
		{Key: model.SettingKeyCircuitProbeEnabled, Value: "false"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "200"},
		{Key: model.SettingKeyCircuitProbeChannelDailyCap, Value: "20"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "3"},
		{Key: model.SettingKeyCircuitProbeEmbeddingsEnabled, Value: "false"},
	}
	seeded := make(map[model.SettingKey]string, len(want))
	for _, setting := range model.DefaultSettings() {
		seeded[setting.Key] = setting.Value
	}
	for _, setting := range want {
		value, ok := seeded[setting.Key]
		if !ok {
			t.Fatalf("%s is missing from the shipped defaults", setting.Key)
		}
		if value != setting.Value {
			t.Fatalf("%s default = %q, want %q", setting.Key, value, setting.Value)
		}
	}

	// 写坏值必须当场被拒（管理员保存设置走的就是 Setting.Validate）。
	for _, bad := range []model.Setting{
		{Key: model.SettingKeyCircuitProbeEnabled, Value: "on"},
		{Key: model.SettingKeyCircuitProbeEmbeddingsEnabled, Value: "1"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "-1"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "200000"},
		{Key: model.SettingKeyCircuitProbeChannelDailyCap, Value: "abc"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "0"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "101"},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("%s=%q must be rejected", bad.Key, bad.Value)
		}
	}
	for _, good := range []model.Setting{
		{Key: model.SettingKeyCircuitProbeEnabled, Value: "true"},
		{Key: model.SettingKeyCircuitProbeEmbeddingsEnabled, Value: "false"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "0"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "100000"},
		{Key: model.SettingKeyCircuitProbeChannelDailyCap, Value: "20"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "1"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "100"},
	} {
		if err := good.Validate(); err != nil {
			t.Fatalf("%s=%q must be accepted: %v", good.Key, good.Value, err)
		}
	}
}

func TestCircuitProbeConfigReflectsSettings(t *testing.T) {
	newCircuitProbeTestEnv(t) // 需要库与设置缓存

	cfg := circuitProbeConfig()
	if cfg.Enabled || cfg.EmbeddingsEnabled {
		t.Fatalf("出厂必须两个开关都关，got %+v", cfg)
	}
	if cfg.GlobalDailyCap != 200 || cfg.ChannelDailyCap != 20 || cfg.MaxStreak != 3 {
		t.Fatalf("出厂限额 = %+v，want 200/20/3", cfg)
	}

	for _, setting := range []model.Setting{
		{Key: model.SettingKeyCircuitProbeEnabled, Value: "true"},
		{Key: model.SettingKeyCircuitProbeEmbeddingsEnabled, Value: "true"},
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "55"},
		{Key: model.SettingKeyCircuitProbeChannelDailyCap, Value: "7"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "4"},
	} {
		if err := op.SettingSetString(setting.Key, setting.Value); err != nil {
			t.Fatalf("set %s: %v", setting.Key, err)
		}
	}
	cfg = circuitProbeConfig()
	if !cfg.Enabled || !cfg.EmbeddingsEnabled || cfg.GlobalDailyCap != 55 || cfg.ChannelDailyCap != 7 || cfg.MaxStreak != 4 {
		t.Fatalf("设置没被读进来: %+v", cfg)
	}
}

// 读设置失败（或库里是不合法值）时必须回到出厂额度，而不是零值——balancer 把 <=0
// 当成“不限”，一次库抖动落成 0 就等于把「每天 200 次」拗成「无上限」。
func TestCircuitProbeConfigFallsBackToShippedBudgets(t *testing.T) {
	newCircuitProbeTestEnv(t)
	// 绕过 Validate 直接写脏值：等价于旧库残留、手改库、或一次读失败。
	for _, setting := range []model.Setting{
		{Key: model.SettingKeyCircuitProbeDailyCap, Value: "not-a-number"},
		{Key: model.SettingKeyCircuitProbeChannelDailyCap, Value: "-5"},
		{Key: model.SettingKeyCircuitProbeMaxConsecutiveFailures, Value: "oops"},
		{Key: model.SettingKeyCircuitProbeEnabled, Value: "maybe"},
	} {
		if err := op.SettingSetString(setting.Key, setting.Value); err != nil {
			t.Fatalf("set %s: %v", setting.Key, err)
		}
	}

	cfg := circuitProbeConfig()
	if cfg.GlobalDailyCap != defaultCircuitProbeDailyCap || cfg.ChannelDailyCap != defaultCircuitProbeChannelDailyCap {
		t.Fatalf("budgets fell through to %+v, want %d/%d（0 会被当成“不限”）",
			cfg, defaultCircuitProbeDailyCap, defaultCircuitProbeChannelDailyCap)
	}
	if cfg.MaxStreak != defaultCircuitProbeMaxStreak {
		t.Fatalf("streak = %d, want %d", cfg.MaxStreak, defaultCircuitProbeMaxStreak)
	}
	if cfg.Enabled || cfg.EmbeddingsEnabled {
		t.Fatalf("both switches must stay off when unreadable: %+v", cfg)
	}
}

// ── 手动探测 ────────────────────────────────────────────────────────────

// 手动探测只入队：接口不自己去打上游，而且它的作用域必须是熔断表里**那一条**的，
// 不是给所有目标套一个通用 chat。
func TestStartChannelProbeQueuesTheExactScopeWithoutCallingUpstream(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "chat", "")
	env.tripScoped(t, "responses", "")
	env.tripScoped(t, "responses", "stream")

	if got := env.startProbe(t); got.State != balancer.CircuitProbeStateDisabled {
		t.Fatalf("ambiguous unscoped start = %+v, want disabled", got)
	}
	got := env.startProbeWith(t, map[string]any{"endpoint": "responses", "capability": "stream"})
	if got.State != balancer.CircuitProbeStateScheduled || got.NextInSeconds != 0 {
		t.Fatalf("accepted start = %+v, want scheduled/0", got)
	}
	if got.RemainingSeconds != nil {
		t.Fatalf("an accepted start must not carry remaining_seconds: %+v", got)
	}

	claim, ok := balancer.ClaimCircuitProbe(time.Now())
	if !ok {
		t.Fatal("the queued manual probe must be claimable")
	}
	if claim.Target.Endpoint != "responses" || claim.Target.Capability != "stream" {
		t.Fatalf("claim scope = %+v, want the exact responses/stream scope the breaker recorded", claim.Target)
	}
	if !claim.Manual {
		t.Fatal("a queued manual probe must be admitted as manual")
	}
	if env.fake.runs != 0 {
		t.Fatal("the API path must never send an upstream probe")
	}
	// 结算是假结果：不发请求、只把车道放开。
	balancer.CompleteCircuitProbe(claim, balancer.CircuitProbeReport{Outcome: "skipped"})
}

// 上游要求等待时，拒绝要带出 explicit state + 剩余秒数，且什么都不入队。
func TestStartChannelProbeReportsWaitUpstream(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	balancer.ApplyCircuitProbeWait(env.channel.ID, env.keyID(), probeTestModel,
		balancer.CircuitProbeWaitCauseRetryAfter, 90*time.Second)

	got := env.startProbe(t)
	if got.State != balancer.CircuitProbeStateWaitUpstream {
		t.Fatalf("state = %q, want %q", got.State, balancer.CircuitProbeStateWaitUpstream)
	}
	if got.RemainingSeconds == nil || *got.RemainingSeconds <= 0 || *got.RemainingSeconds > 90 {
		t.Fatalf("wait_upstream must carry a meaningful remaining_seconds, got %+v", got)
	}
	if got.NextInSeconds != 0 {
		t.Fatalf("rejection next_in_seconds = %d, want 0", got.NextInSeconds)
	}
	if _, ok := balancer.ClaimCircuitProbe(time.Now()); ok {
		t.Fatal("a rejected start must not queue anything")
	}
}

// 媒体作用域一律不探：拿一道算术题去打图片端点只会把错误记到渠道账上。
func TestStartChannelProbeRejectsMediaScope(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "images_generations", "")

	got := env.startProbe(t)
	if got.State != balancer.CircuitProbeStateNotProbedMedia {
		t.Fatalf("state = %q, want %q", got.State, balancer.CircuitProbeStateNotProbedMedia)
	}
	if got.RemainingSeconds == nil || *got.RemainingSeconds != 0 {
		t.Fatalf("media rejection remaining_seconds = %v, want 0", got.RemainingSeconds)
	}
	if _, ok := balancer.ClaimCircuitProbe(time.Now()); ok {
		t.Fatal("a media-scoped target must never be queued")
	}
}

func TestStartChannelProbeRejectsBadInput(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	for _, body := range []map[string]any{
		{"key_id": env.keyID(), "model": probeTestModel},
		{"id": env.channel.ID, "model": probeTestModel},
		{"id": env.channel.ID, "key_id": env.keyID()},
		{"id": env.channel.ID, "key_id": env.keyID(), "model": "   "},
	} {
		rec, _ := env.do(t, http.MethodPost, "/api/v1/channel/probe", env.token, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%#v -> %d, want 400", body, rec.Code)
		}
	}
	rec, _ := env.do(t, http.MethodPost, "/api/v1/channel/probe", env.token, map[string]any{
		"id": 999, "key_id": env.keyID(), "model": probeTestModel,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown channel -> %d, want 400", rec.Code)
	}
}

// 同一个 channel/key/model 同时挂着媒体作用域与普通文本作用域时，普通文本必须照样可探
// （而不是被“表里第一条”的媒体作用域带成 disabled）。这一条用**真实回调**，只不跑车道。
func TestCircuitProbeMapsEveryScopeByItsOwnTarget(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	enabled := true
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeEnabled: &enabled}, env.ctx); err != nil {
		t.Fatalf("enable probe switch: %v", err)
	}
	env.tripScoped(t, "images_generations", "")
	env.tripScoped(t, "messages", "")

	// 真实接线层：渠道/密钥/模型/作用域都按目标自己判。
	balancer.ConfigureCircuitProbe(balancer.CircuitProbeConfig{Enabled: true, MaxStreak: 5}, circuitProbeCallbacks())

	states := map[string]string{}
	for _, entry := range balancer.SnapshotCircuitProbe(env.channel.ID).Entries {
		states[entry.Endpoint] = entry.Probe.State
	}
	if states["messages"] != balancer.CircuitProbeStateScheduled {
		t.Fatalf("messages scope = %q, want scheduled（媒体兄弟条目不能把它带成 disabled）", states["messages"])
	}
	if states["images_generations"] != balancer.CircuitProbeStateNotProbedMedia {
		t.Fatalf("images scope = %q, want not_probed_media", states["images_generations"])
	}

	if route, ok := resolveCircuitProbeTarget(balancer.CircuitProbeTarget{
		ChannelID: env.channel.ID, KeyID: env.keyID(), Model: probeTestModel, Endpoint: "messages",
	}); !ok || route.Endpoint != "anthropic_messages" {
		t.Fatalf("messages resolve = %+v (ok=%v), want anthropic_messages", route, ok)
	}
	for _, endpoint := range []string{"images_generations", "responses_compact", "completions", "systemone"} {
		if route, ok := resolveCircuitProbeTarget(balancer.CircuitProbeTarget{
			ChannelID: env.channel.ID, KeyID: env.keyID(), Model: probeTestModel, Endpoint: endpoint,
		}); ok || route.Endpoint != "" {
			t.Fatalf("%s cannot resolve to an unrelated upstream endpoint: %+v (ok=%v)", endpoint, route, ok)
		}
	}
}

// 模型归属只看渠道自己的配置：没被重映射的 selected model 与映射后的上游名可以探，
// 其它名字（包括被重映射的请求名）不行。
func TestCircuitProbeModelEligibilityFollowsChannelConfig(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	channel, err := op.ChannelGet(env.channel.ID, env.ctx)
	if err != nil {
		t.Fatalf("read channel: %v", err)
	}
	if !circuitProbeServesModel(*channel, probeTestModel) {
		t.Fatalf("the channel's own selected model must be probeable: %+v", channel.SelectedModels)
	}
	if circuitProbeServesModel(*channel, "invented-model") {
		t.Fatal("an unconfigured model name must never be probeable")
	}

	channel.ModelMapping = map[string]string{"request-alias": "upstream-mapped"}
	channel.SelectedModels = []string{"request-alias"}
	if !circuitProbeServesModel(*channel, "upstream-mapped") {
		t.Fatal("the mapped upstream name is what the breaker key carries, so it must be probeable")
	}
	if circuitProbeServesModel(*channel, "request-alias") {
		t.Fatal("a request name that gets remapped is not an upstream name")
	}
}

// 健康渠道（没有 OPEN/hold 条目）不归手动探测管：不能拿它当“往上游发一次任意请求”的入口。
func TestStartChannelProbeRequiresAConcreteBreakerRow(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	got := env.startProbe(t)
	if got.State != balancer.CircuitProbeStateDisabled {
		t.Fatalf("state = %q, want %q (没有具体条目就没有可探的目标)", got.State, balancer.CircuitProbeStateDisabled)
	}
	if _, ok := balancer.ClaimCircuitProbe(time.Now()); ok {
		t.Fatal("nothing may be queued for a healthy channel")
	}
}

// 条目还在、但渠道模型配置已经不再承接这个上游模型：照样拒绝，且不排任何探测。
func TestStartChannelProbeRefusesRowsOutsideTheChannelConfig(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "chat", "")
	replacement := []string{"replacement-model"}
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, SelectedModels: &replacement}, env.ctx); err != nil {
		t.Fatalf("change channel models: %v", err)
	}
	balancer.ConfigureCircuitProbe(balancer.CircuitProbeConfig{}, circuitProbeCallbacks())

	got := env.startProbe(t)
	if got.State != balancer.CircuitProbeStateDisabled {
		t.Fatalf("state = %q, want %q", got.State, balancer.CircuitProbeStateDisabled)
	}
	if _, ok := balancer.ClaimCircuitProbe(time.Now()); ok {
		t.Fatal("nothing may be queued for a model the channel no longer serves")
	}
}

// 入队成功必须叫一声 worker：只靠 15s 空转节拍，一次冷却尾段里的短窗口很容易错过。
// 唤醒是容量 1 的缓冲信号，由同一条串行循环消费（不会多出第二个执行者）。
func TestStartChannelProbeWakesTheSerialWorker(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "chat", "")
	drainCircuitProbeWake()

	if got := env.startProbe(t); got.State != balancer.CircuitProbeStateScheduled {
		t.Fatalf("start = %+v", got)
	}
	select {
	case <-circuitProbeWake:
	default:
		t.Fatal("a successful manual enqueue must wake the worker instead of waiting for the next tick")
	}

	// 容量 1：再入队一次也只合并成一次唤醒，不会攒成一串空转。
	if got := env.startProbe(t); got.State != balancer.CircuitProbeStateScheduled {
		t.Fatalf("second start = %+v", got)
	}
	select {
	case <-circuitProbeWake:
	default:
		t.Fatal("the second enqueue must also wake the worker")
	}
	select {
	case <-circuitProbeWake:
		t.Fatal("the wake signal must stay one buffered slot (coalesced), not a queue")
	default:
	}
	if env.fake.runs != 0 {
		t.Fatal("the API path must never send an upstream probe")
	}
}

// ── 读接口 ──────────────────────────────────────────────────────────────

// /circuits/:id 仍然是一个数组、原有字段一个不少，只是每条多一个 probe。
func TestChannelCircuitsAddsProbeAndKeepsTheLegacyShape(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "chat", "stream")

	rec, data := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/channel/circuits/%d", env.channel.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("circuits = %d %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("circuits must stay an array: %v (%s)", err, data)
	}
	if len(rows) != 1 {
		t.Fatalf("circuits = %s, want exactly one open row", data)
	}
	for _, key := range []string{"key_id", "model", "endpoint", "capability", "state", "remaining_seconds", "consecutive_failures", "trip_count"} {
		if _, ok := rows[0][key]; !ok {
			t.Fatalf("legacy field %q disappeared: %s", key, data)
		}
	}
	if rows[0]["state"] != "open" || rows[0]["endpoint"] != "chat" {
		t.Fatalf("row = %s", data)
	}
	probe, ok := rows[0]["probe"].(map[string]any)
	if !ok {
		t.Fatalf("each row must carry a probe view: %s", data)
	}
	if probe["state"] != balancer.CircuitProbeStateDisabled {
		t.Fatalf("probe state = %v, want %q (自动总开关出厂关)", probe["state"], balancer.CircuitProbeStateDisabled)
	}
	if _, ok := probe["next_in_seconds"]; !ok {
		t.Fatalf("probe view must carry next_in_seconds: %v", probe)
	}
}

// /probes/:id 与 /probe-stats 的字段集按共享契约来，cap 回显生效的设置值。
func TestChannelProbeViewsFollowTheWireContract(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	env.tripScoped(t, "chat", "")
	if got := env.startProbe(t); got.State != balancer.CircuitProbeStateScheduled {
		t.Fatalf("start = %+v", got)
	}
	claim, ok := balancer.ClaimCircuitProbe(time.Now())
	if !ok {
		t.Fatal("the queued manual probe must be claimable")
	}
	balancer.CompleteCircuitProbe(claim, balancer.CircuitProbeReport{Prompt: "12 + 30 = ?", Answer: "42", Outcome: "skipped"})

	rec, data := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/channel/probes/%d", env.channel.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("probes = %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("decode probe view: %v (%s)", err, data)
	}
	if view["channel_id"] != float64(env.channel.ID) {
		t.Fatalf("channel_id = %v, want %d", view["channel_id"], env.channel.ID)
	}
	if day, _ := view["day"].(string); day == "" {
		t.Fatalf("day must be filled: %s", data)
	}
	totals, err := probeTotalsOf(t, view["totals"], 20, data)
	if err != nil {
		t.Fatal(err)
	}
	if totals["used"] != float64(1) {
		t.Fatalf("totals.used = %v, want 1", totals["used"])
	}
	probeTotalsOf(t, view["global_totals"], 200, data)

	history, ok := view["history"].([]any)
	if !ok || len(history) != 1 {
		t.Fatalf("history = %v, want one row: %s", view["history"], data)
	}
	row, _ := history[0].(map[string]any)
	for _, key := range []string{"timestamp", "model", "key_id", "prompt", "answer", "duration_ms", "result"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("history row is missing %q: %v", key, row)
		}
	}
	if row["result"] != "skipped:skipped" {
		t.Fatalf("history result = %v, want skipped:skipped", row["result"])
	}

	rec, data = env.do(t, http.MethodGet, "/api/v1/channel/probe-stats", env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe-stats = %d %s", rec.Code, rec.Body.String())
	}
	var stats map[string]any
	if err := json.Unmarshal(data, &stats); err != nil {
		t.Fatalf("decode probe stats: %v (%s)", err, data)
	}
	if day, _ := stats["day"].(string); day == "" {
		t.Fatalf("stats.day must be filled: %s", data)
	}
	probeTotalsOf(t, stats["totals"], 200, data)
	channels, ok := stats["channels"].([]any)
	if !ok || len(channels) != 1 {
		t.Fatalf("stats.channels = %v, want the one channel that probed: %s", stats["channels"], data)
	}
	channelRow, _ := channels[0].(map[string]any)
	if channelRow["channel_id"] != float64(env.channel.ID) {
		t.Fatalf("stats channel row = %v", channelRow)
	}
	if channelRow["cap"] != float64(20) || channelRow["used"] != float64(1) {
		t.Fatalf("stats channel row = %v, want cap 20 and used 1", channelRow)
	}
}

// probeTotalsOf 校验一组当日计数的字段集与 cap 回显，返回那份 map 供调用方继续断言。
func probeTotalsOf(t *testing.T, value any, wantCap float64, body json.RawMessage) (map[string]any, error) {
	t.Helper()
	totals, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("totals missing: %s", body)
	}
	for _, key := range []string{"used", "cap", "recovered", "failed", "skipped"} {
		if _, ok := totals[key]; !ok {
			return nil, fmt.Errorf("totals is missing %q: %s", key, body)
		}
	}
	if totals["cap"] != wantCap {
		return nil, fmt.Errorf("totals.cap = %v, want %v: %s", totals["cap"], wantCap, body)
	}
	return totals, nil
}

// 三条新接口与渠道其它管理接口同一套鉴权：没有管理员身份一律拒绝。
func TestChannelProbeRoutesRequireAdmin(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	plain := env.newUser(t, "probe-plain", 1)
	for _, target := range []struct {
		path string
		body any
	}{
		{"/api/v1/channel/probe", map[string]any{"id": env.channel.ID, "key_id": env.keyID(), "model": probeTestModel}},
		{fmt.Sprintf("/api/v1/channel/probes/%d", env.channel.ID), nil},
		{"/api/v1/channel/probe-stats", nil},
	} {
		method := http.MethodGet
		if target.body != nil {
			method = http.MethodPost
		}
		for _, token := range []string{"", jwtBearer(t, plain)} {
			rec, _ := env.do(t, method, target.path, token, target.body)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s %s allowed without an admin identity", method, target.path)
			}
		}
	}
	// 管理员仍然可用（否则上面的拒绝只是"路由坏了"）。
	rec, _ := env.do(t, http.MethodGet, "/api/v1/channel/probe-stats", env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin probe-stats = %d %s", rec.Code, rec.Body.String())
	}
}

// ── 渠道开关（probe_enabled）────────────────────────────────────────────

// probe_enabled 是**指针**更新：显式 false 必须真的落库，遗漏字段不动原值。
func TestChannelUpdateProbeEnabledUsesPointerSemantics(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	ctx := context.Background()

	created, err := op.ChannelGet(env.channel.ID, ctx)
	if err != nil {
		t.Fatalf("read channel: %v", err)
	}
	if created.ProbeEnabled {
		t.Fatal("a new channel must not have automatic probing enabled")
	}

	enabled := true
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeEnabled: &enabled}, ctx); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got, _ := op.ChannelGet(env.channel.ID, ctx); !got.ProbeEnabled {
		t.Fatal("explicit true was not persisted")
	}

	// 只改别的字段：probe_enabled 必须保持 true。
	priority := 3
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, Priority: &priority}, ctx); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	if got, _ := op.ChannelGet(env.channel.ID, ctx); !got.ProbeEnabled {
		t.Fatal("an unrelated update must not touch probe_enabled")
	}

	disabled := false
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeEnabled: &disabled}, ctx); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, _ := op.ChannelGet(env.channel.ID, ctx); got.ProbeEnabled {
		t.Fatal("explicit false was skipped by the zero-value rule")
	}
}

// 渠道开关只约束**自动**探测：它反映到调度器的那个回调上。
func TestCircuitProbeChannelSwitchReachesTheScheduler(t *testing.T) {
	env := newCircuitProbeTestEnv(t)
	off := false
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeEnabled: &off}, env.ctx); err != nil {
		t.Fatalf("disable probe switch: %v", err)
	}
	if circuitProbeChannelEnabled(env.channel.ID) {
		t.Fatal("probe_enabled=false must be reported to the scheduler")
	}
	on := true
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: env.channel.ID, ProbeEnabled: &on}, env.ctx); err != nil {
		t.Fatalf("enable probe switch: %v", err)
	}
	if !circuitProbeChannelEnabled(env.channel.ID) {
		t.Fatal("probe_enabled=true must be reported to the scheduler")
	}
	if circuitProbeChannelEnabled(999) {
		t.Fatal("an unknown channel must be reported as disabled")
	}
}
