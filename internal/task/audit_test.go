package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestScheduledTargetKeyKeepsChannelAndModelApart(t *testing.T) {
	left := scheduledTargetKey(1, "claude-opus-4-8")
	right := scheduledTargetKey(2, "claude-opus-4-8")
	if left == right {
		t.Fatal("different channels must not share a target key")
	}
}

// 任务名的拼写是运维核对历史任务名的锚点，别悄悄改。
func TestTaskModelAuditQuickNameStable(t *testing.T) {
	if TaskModelAuditQuick != "model_audit_quick" {
		t.Fatalf("TaskModelAuditQuick constant changed: %q", TaskModelAuditQuick)
	}
}

// 抽检重新接线后，"默认不花钱"由这一组用例证明：
//   - 开关关着（或读不出来）：零入队、零上游请求、不写快照；
//   - 开关开着但额度用完：同样零入队；
//   - 开关开着且额度够：入队，且题目是认模型的那几项（不是旧的连通/改写/截断）。

// auditEnqueueRecorder 是可注入的入队替身：只记录，不建任务、不发请求。
type auditEnqueueRecorder struct {
	mu    sync.Mutex
	calls []op.ModelAuditEnqueueInput
}

func (r *auditEnqueueRecorder) enqueue(_ context.Context, in op.ModelAuditEnqueueInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, in)
	return int64(len(r.calls)), nil
}

func (r *auditEnqueueRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *auditEnqueueRecorder) only(t *testing.T) op.ModelAuditEnqueueInput {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 enqueue, got %d", len(r.calls))
	}
	return r.calls[0]
}

func stubAuditEnqueue(t *testing.T) *auditEnqueueRecorder {
	t.Helper()
	rec := &auditEnqueueRecorder{}
	previous := enqueueAuditFunc
	enqueueAuditFunc = rec.enqueue
	t.Cleanup(func() { enqueueAuditFunc = previous })
	return rec
}

// auditUpstreamProbe 是一个"会数请求"的假上游：用例断言它一次都没被访问过。
type auditUpstreamProbe struct {
	mu sync.Mutex
	n  int
}

func (p *auditUpstreamProbe) hits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func newAuditUpstreamProbe(t *testing.T) (*auditUpstreamProbe, string) {
	t.Helper()
	p := &auditUpstreamProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.n++
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_audit","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return p, srv.URL
}

// seedAuditChannel 建一个启用渠道（Anthropic 类型 → 抽检会多问签名/系统提示词）。
func seedAuditChannel(t *testing.T, name, modelName, baseURL string) model.Channel {
	t.Helper()
	ctx := context.Background()
	channel := model.Channel{
		Name:           name,
		Type:           outbound.OutboundTypeAnthropic,
		Enabled:        true,
		Model:          modelName,
		SelectedModels: []string{modelName},
		BaseUrls:       []model.BaseUrl{{URL: baseURL}},
		Keys:           []model.ChannelKey{{Enabled: true, ChannelKey: "audit-sampling-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	return channel
}

func setAuditSetting(t *testing.T, key model.SettingKey, value string) {
	t.Helper()
	if err := op.SettingSetString(key, value); err != nil {
		t.Fatalf("set %s=%q: %v", key, value, err)
	}
}

// exhaustAuditBudget 把当日额度全部占掉：之后任何一次预扣都会失败。
func exhaustAuditBudget(t *testing.T, planned int) {
	t.Helper()
	setAuditSetting(t, model.SettingKeyModelAuditDailyRequestLimit, "1")
	if ok, _, _ := op.ModelAuditBudgetReserve(planned, time.Now()); !ok {
		t.Fatalf("precondition: reserving %d with limit 1 should succeed", planned)
	}
}

// TestModelAuditQuickTaskSwitchOffEnqueuesNothing 抽检开关默认关：一次入队、一次上游
// 请求都不许发生，连快照都不该被写（开关关着却留下"跑过"的痕迹，界面会自己骗自己）。
func TestModelAuditQuickTaskSwitchOffEnqueuesNothing(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	seedAuditChannel(t, "audit-off-channel", "claude-opus-4-8-off", baseURL)
	rec := stubAuditEnqueue(t)

	// 确保开关是明确的 false（InitCache 的出厂值）。
	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "false")
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{})

	ModelAuditQuickTask()

	if got := rec.count(); got != 0 {
		t.Fatalf("switch off must enqueue nothing, got %d enqueue(s)", got)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("switch off must not touch upstream, got %d request(s)", got)
	}
	if snap := op.ScheduledAuditGet(); snap.LastRunUnix != 0 || len(snap.Results) != 0 {
		t.Fatalf("switch off must not write a snapshot, got %+v", snap)
	}
}

// TestModelAuditQuickTaskSwitchUnreadableEnqueuesNothing 开关读不出来按关处理：
// 会花钱的东西，读配置抖动绝不能变成"那就开始吧"。
func TestModelAuditQuickTaskSwitchUnreadableEnqueuesNothing(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	seedAuditChannel(t, "audit-unreadable-channel", "claude-opus-4-8-unreadable", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "maybe")
	t.Cleanup(func() { _ = op.SettingSetString(model.SettingKeyModelAuditEnabled, "false") })
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{})

	ModelAuditQuickTask()

	if got := rec.count(); got != 0 {
		t.Fatalf("unreadable switch must enqueue nothing, got %d enqueue(s)", got)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("unreadable switch must not touch upstream, got %d request(s)", got)
	}
}

// TestModelAuditQuickTaskBudgetExhaustedEnqueuesNothing 开关开着、额度用完：零入队，
// 并且快照里要留下"为什么没跑"这句人话。
func TestModelAuditQuickTaskBudgetExhaustedEnqueuesNothing(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	seedAuditChannel(t, "audit-broke-channel", "claude-opus-4-8-broke", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "true")
	t.Cleanup(func() { _ = op.SettingSetString(model.SettingKeyModelAuditEnabled, "false") })
	exhaustAuditBudget(t, 1)
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{})

	ModelAuditQuickTask()

	if got := rec.count(); got != 0 {
		t.Fatalf("exhausted budget must enqueue nothing, got %d enqueue(s)", got)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("exhausted budget must not touch upstream, got %d request(s)", got)
	}
	snap := op.ScheduledAuditGet()
	if len(snap.Results) != 1 || !snap.Results[0].Skipped {
		t.Fatalf("exhausted budget should leave one skipped result, got %+v", snap)
	}
	if !strings.Contains(snap.Results[0].SkipReason, "额度") {
		t.Fatalf("skip reason should say the budget is gone, got %q", snap.Results[0].SkipReason)
	}
}

// TestModelAuditQuickTaskEnqueuesModelAwareProbes 开关开着、额度够：入队而不是就地跑，
// 题目是认模型的指纹/分词器（Anthropic 再加签名/系统提示词），超时按请求量放大。
func TestModelAuditQuickTaskEnqueuesModelAwareProbes(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	channel := seedAuditChannel(t, "audit-on-channel", "claude-opus-4-8-on", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "true")
	t.Cleanup(func() { _ = op.SettingSetString(model.SettingKeyModelAuditEnabled, "false") })
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{})

	ModelAuditQuickTask()

	in := rec.only(t)
	if in.ChannelID != channel.ID {
		t.Fatalf("enqueued channel = %d, want %d", in.ChannelID, channel.ID)
	}
	if len(in.Models) != 1 || in.Models[0] != "claude-opus-4-8-on" {
		t.Fatalf("enqueued models = %v, want the request model name", in.Models)
	}
	if in.Trigger != "schedule" {
		t.Fatalf("scheduled audit trigger = %q, want schedule", in.Trigger)
	}
	probes := strings.Join(in.Probes, ",")
	for _, want := range []string{"fingerprint", "tokenizer_id", "signature", "system_prompt"} {
		if !strings.Contains(probes, want) {
			t.Fatalf("anthropic sampling audit must include probe %q, got %q", want, probes)
		}
	}
	for _, gone := range []string{"liveness", "echo_rewrite", "context_canary"} {
		if strings.Contains(probes, gone) {
			t.Fatalf("the old connectivity probes must not be used for sampling, got %q", probes)
		}
	}
	if in.TimeoutSeconds <= op.AuditDefaultTimeoutSeconds {
		t.Fatalf("queue timeout should be scaled up for request-heavy probes, got %ds", in.TimeoutSeconds)
	}
	// 入队即记冷却：否则下一次节拍会挑同一个渠道，再扣一次额度。
	if !op.ScheduledAuditOnCooldown(channel.ID, "claude-opus-4-8-on", time.Now(), auditCooldown()) {
		t.Fatal("enqueued target should be on cooldown immediately")
	}
	snap := op.ScheduledAuditGet()
	if snap.TargetCount != 1 || snap.RanCount != 1 {
		t.Fatalf("snapshot should record 1 target / 1 enqueued, got %+v", snap)
	}
	// 入队不是发请求：上游一次都不该被打到（真正的请求由队列 worker 去做）。
	if got := upstream.hits(); got != 0 {
		t.Fatalf("sampling audit must not call upstream directly, got %d request(s)", got)
	}
}

// TestAuditFollowUpSwitchOffEnqueuesNothing 跟查在抽检开关关着时一律不打
// （即便跟查开关是 true）。
func TestAuditFollowUpSwitchOffEnqueuesNothing(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	channel := seedAuditChannel(t, "followup-off-channel", "claude-opus-4-8-followup-off", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "false")
	setAuditSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "true")
	t.Cleanup(func() { _ = op.SettingSetString(model.SettingKeyModelAuditFollowUpEnabled, "false") })

	runAuditFollowUp(channel.ID, "claude-opus-4-8-followup-off", "echo_mismatch", 99)

	if got := rec.count(); got != 0 {
		t.Fatalf("follow-up must not enqueue while audit switch is off, got %d enqueue(s)", got)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("follow-up must not touch upstream while audit switch is off, got %d request(s)", got)
	}
}

// TestAuditFollowUpBudgetExhaustedEnqueuesNothing 跟查与定时抽检共用同一本账：
// 额度用完时跟查同样零入队，并把原因写进快照。
func TestAuditFollowUpBudgetExhaustedEnqueuesNothing(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	channel := seedAuditChannel(t, "followup-broke-channel", "claude-opus-4-8-followup-broke", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "true")
	setAuditSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "true")
	t.Cleanup(func() {
		_ = op.SettingSetString(model.SettingKeyModelAuditFollowUpEnabled, "false")
		_ = op.SettingSetString(model.SettingKeyModelAuditEnabled, "false")
	})
	exhaustAuditBudget(t, 1)
	op.ScheduledAuditStore(model.ScheduledAuditSnapshot{})

	runAuditFollowUp(channel.ID, "claude-opus-4-8-followup-broke", "passive_hint", 7)

	if got := rec.count(); got != 0 {
		t.Fatalf("follow-up must not enqueue when the budget is gone, got %d enqueue(s)", got)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("follow-up must not touch upstream when the budget is gone, got %d request(s)", got)
	}
	snap := op.ScheduledAuditGet()
	if len(snap.Results) != 1 || !snap.Results[0].Skipped {
		t.Fatalf("follow-up exhaustion should leave one skipped result, got %+v", snap)
	}
	if snap.Results[0].Trigger != "passive_hint" {
		t.Fatalf("follow-up should keep its trigger, got %q", snap.Results[0].Trigger)
	}
}

// TestAuditFollowUpEnqueuesWithTrigger 跟查开门时走与定时抽检同一套入队参数，
// 触发原因原样带进 job（检测历史里要能分清是回显对不上还是来源线索可疑）。
func TestAuditFollowUpEnqueuesWithTrigger(t *testing.T) {
	setupScheduledAuditTestDB(t)
	upstream, baseURL := newAuditUpstreamProbe(t)
	channel := seedAuditChannel(t, "followup-on-channel", "claude-opus-4-8-followup-on", baseURL)
	rec := stubAuditEnqueue(t)

	setAuditSetting(t, model.SettingKeyModelAuditEnabled, "true")
	setAuditSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "true")
	t.Cleanup(func() {
		_ = op.SettingSetString(model.SettingKeyModelAuditFollowUpEnabled, "false")
		_ = op.SettingSetString(model.SettingKeyModelAuditEnabled, "false")
	})

	runAuditFollowUp(channel.ID, "claude-opus-4-8-followup-on", "passive_hint", 1234)

	in := rec.only(t)
	if in.Trigger != "passive_hint" {
		t.Fatalf("follow-up trigger = %q, want passive_hint", in.Trigger)
	}
	if in.LogID != 1234 {
		t.Fatalf("follow-up must carry the triggering log id, got %d", in.LogID)
	}
	if len(in.Models) != 1 || in.Models[0] != "claude-opus-4-8-followup-on" {
		t.Fatalf("follow-up models = %v", in.Models)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("follow-up must not call upstream directly, got %d request(s)", got)
	}
}
