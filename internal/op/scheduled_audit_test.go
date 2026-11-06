package op

import (
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
)

func TestScheduledAuditCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if ScheduledAuditOnCooldown(3, "claude-opus-4-8", now, 6*time.Hour) {
		t.Fatal("empty cooldown map must not block")
	}
	ScheduledAuditMarkHit(3, "claude-opus-4-8", now)
	if !ScheduledAuditOnCooldown(3, "claude-opus-4-8", now.Add(time.Hour), 6*time.Hour) {
		t.Fatal("same target should stay on cooldown")
	}
	if ScheduledAuditOnCooldown(3, "claude-opus-4-8", now.Add(7*time.Hour), 6*time.Hour) {
		t.Fatal("cooldown should expire")
	}
	if ScheduledAuditOnCooldown(4, "claude-opus-4-8", now.Add(time.Hour), 6*time.Hour) {
		t.Fatal("other channel must not inherit cooldown")
	}
}

// stashedSetting 临时改一个设置键的缓存值，用例结束时恢复（键原本不存在则删掉）。
//
// value == "" 表示把这个键从缓存里删掉，用来模拟“读设置失败”（SettingGetBool 返回 err），
// 不是“值为空串”。
func stashedSetting(t *testing.T, key model.SettingKey, value string) {
	t.Helper()
	previous, existed := settingCache.Get(key)
	t.Cleanup(func() {
		if existed {
			settingCache.Set(key, previous)
			return
		}
		settingCache.Del(key)
	})
	if value == "" {
		settingCache.Del(key)
		return
	}
	settingCache.Set(key, value)
}

// TestAuditFollowUpTriggerRequiresBothSwitchesAndAReason 钉死跟查的三件事：
//  1. 抽检总开关与跟查开关都必须**明确** true，读失败一律按关；
//  2. 回显对不上只是触发理由之一，被动来源里的 strong/medium 线索同样触发；
//  3. 本地校验/空请求/弱线索/没证据都不触发。
//
// 两条自动路径重新接线后，“默认不花钱”要在这一层能证明：开关关着时这个谓词
// 必须在任何日志上都返回空，后续才会零入队、零上游请求。
func TestAuditFollowUpTriggerRequiresBothSwitchesAndAReason(t *testing.T) {
	stashedSetting(t, model.SettingKeyModelAuditEnabled, "true")
	stashedSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "true")
	yes := true
	no := false
	mismatch := model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8", UpstreamModelMismatch: &yes}

	if got := AuditFollowUpTrigger(mismatch); got != "echo_mismatch" {
		t.Fatalf("echo mismatch with both switches on should trigger echo_mismatch, got %q", got)
	}
	if !ShouldTriggerAuditFollowUp(mismatch) {
		t.Fatal("ShouldTriggerAuditFollowUp disagrees with AuditFollowUpTrigger")
	}

	// 抽检总开关关着：跟查一并停（跟查就是抽检的一个来源）。
	stashedSetting(t, model.SettingKeyModelAuditEnabled, "false")
	if ShouldTriggerAuditFollowUp(mismatch) {
		t.Fatal("audit master switch off must stop follow-up entirely")
	}
	// 读失败 = 关，不是开。
	stashedSetting(t, model.SettingKeyModelAuditEnabled, "")
	if ShouldTriggerAuditFollowUp(mismatch) {
		t.Fatal("unreadable audit switch must be treated as off")
	}
	stashedSetting(t, model.SettingKeyModelAuditEnabled, "true")

	// 跟查开关同理：关着/读不到都不打。
	stashedSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "false")
	if ShouldTriggerAuditFollowUp(mismatch) {
		t.Fatal("follow-up switch off must not trigger")
	}
	stashedSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "")
	if ShouldTriggerAuditFollowUp(mismatch) {
		t.Fatal("unreadable follow-up switch must be treated as off")
	}
	stashedSetting(t, model.SettingKeyModelAuditFollowUpEnabled, "true")

	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8"}) {
		t.Fatal("undeclared echo must not follow up")
	}
	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8", UpstreamModelMismatch: &no}) {
		t.Fatal("matching echo must not follow up")
	}
	if ShouldTriggerAuditFollowUp(model.RelayLog{
		ChannelId:             1,
		RequestModelName:      "claude-opus-4-8",
		UpstreamModelMismatch: &yes,
		ErrorCode:             model.RelayLogErrorCodeClientEmptyRequest,
	}) {
		t.Fatal("local empty request must not follow up")
	}

	// 被动来源线索：strong / medium 触发，weak 与空证据不触发。
	withHint := func(strength string) model.RelayLog {
		return model.RelayLog{
			ChannelId:        1,
			RequestModelName: "claude-opus-4-8",
			PassiveSource: &model.RelayLogPassiveSource{
				Hints: []model.RelayLogSourceHint{{Platform: "aws_bedrock", Strength: strength, Code: "response_id_bedrock"}},
			},
		}
	}
	for _, strength := range []string{behavior.ProvenanceStrengthStrong, behavior.ProvenanceStrengthMedium} {
		if got := AuditFollowUpTrigger(withHint(strength)); got != "passive_hint" {
			t.Fatalf("passive hint with strength %q should trigger passive_hint, got %q", strength, got)
		}
	}
	if ShouldTriggerAuditFollowUp(withHint("weak")) {
		t.Fatal("weak passive hint must not spend a billable audit")
	}
	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 1, RequestModelName: "claude-opus-4-8"}) {
		t.Fatal("no passive evidence must not trigger")
	}
	// 渠道未知也不打：没有渠道就没有可打的上游。
	if ShouldTriggerAuditFollowUp(model.RelayLog{ChannelId: 0, RequestModelName: "claude-opus-4-8", PassiveSource: withHint("strong").PassiveSource}) {
		t.Fatal("missing channel id must not trigger")
	}
}

func TestScheduledAuditAppendKeepsNewestFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	prev := ScheduledAuditGet()
	t.Cleanup(func() { ScheduledAuditStore(prev) })
	ScheduledAuditStore(model.ScheduledAuditSnapshot{})
	ScheduledAuditAppend(model.ScheduledAuditResult{ChannelID: 1, Model: "a", Trigger: "echo_mismatch"}, now)
	ScheduledAuditAppend(model.ScheduledAuditResult{ChannelID: 2, Model: "b", Trigger: "echo_mismatch"}, now.Add(time.Minute))
	snap := ScheduledAuditGet()
	if len(snap.Results) != 2 || snap.Results[0].ChannelID != 2 {
		t.Fatalf("newest follow-up should be first, got %#v", snap.Results)
	}
}
