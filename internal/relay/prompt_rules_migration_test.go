package relay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	dbmigrate "github.com/zhizhishu/octopus/internal/db/migrate"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/prompt"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 迁移 golden：样本必须从真实迁移产物得来——先埋旧四层数据（含 legacy 模式与关闭值）、
// 跑 031 真实迁移、把迁移产物（prompt_rules 行）作为新管道输入，与旧四层 oracle 在
// 四协议上的出站字节逐字节比对。共享 profile 合并、关闭壳、admin_only 全覆盖。
func TestPromptRulesMigrationGoldenMatchesLegacyWire(t *testing.T) {
	setupRelayErrorDB(t)
	reset := func() {
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "false")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "")
		_ = op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore))
	}
	reset()
	t.Cleanup(reset)

	gormDB := db.GetDB()
	// setupRelayErrorDB 的 InitDB 已在空库上跑过 AfterAutoMigrate，migration 031 已经记成
	// success（当时没有旧数据，什么也没迁）。本测试要在 seed 之后真跑迁移，所以先删掉
	// 仅 version=31 的那条记录（只在本测试的临时库），删除错误必须检查。
	if err := gormDB.Where("version = ?", 31).Delete(&dbmigrate.MigrationRecord{}).Error; err != nil {
		t.Fatalf("clear migration 31 record before seeding: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyPromptOverrideSystem, "GLOBAL"); err != nil {
		t.Fatal(err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyPromptOverrideMode, string(dbmodel.PromptOverrideModeBefore)); err != nil {
		t.Fatal(err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyPromptOverrideEnabled, "true"); err != nil {
		t.Fatal(err)
	}

	// 方案 slug 不能用 vip/svip/ssvip：setupRelayErrorDB → op.InitCache 已经播种了这三个默认方案
	// （op/access_plan.go 的 accessPlanEnsureSeedDefaults），而 slug 是 unique 列。
	plan := &dbmodel.AccessPlan{Slug: "goldenvip", DisplayName: "goldenvip", Enabled: true, SystemPromptOverride: "PLAN",
		PromptOverrideMode: dbmodel.PromptOverrideModeAfter, PromptOverrideEnabled: true, RouteProfileID: 1}
	if err := gormDB.Create(plan).Error; err != nil {
		t.Fatal(err)
	}
	offPlan := &dbmodel.AccessPlan{Slug: "off", DisplayName: "off", Enabled: true, SystemPromptOverride: "PLAN-OFF",
		PromptOverrideMode: dbmodel.PromptOverrideModeAfter, PromptOverrideEnabled: false, RouteProfileID: 1}
	if err := gormDB.Create(offPlan).Error; err != nil {
		t.Fatal(err)
	}
	rule := &dbmodel.AccessRouteRule{RouteProfileID: 1, RequestModel: "M1[1m]", SystemPromptOverride: "ROUTE",
		PromptOverrideMode: dbmodel.PromptOverrideModeBefore, PromptOverrideEnabled: true}
	if err := gormDB.Create(rule).Error; err != nil {
		t.Fatal(err)
	}
	channel := &dbmodel.Channel{Name: "goldench", SystemPromptOverride: "CHAN",
		PromptOverrideMode: dbmodel.PromptOverrideModeAfter, PromptOverrideEnabled: true}
	if err := gormDB.Create(channel).Error; err != nil {
		t.Fatal(err)
	}

	// 真实迁移（031）：全部规则从迁移产物读取，不手工拼样本。
	if err := dbmigrate.MigratePromptRulesForTest(gormDB); err != nil {
		t.Fatalf("migrate prompt rules: %v", err)
	}
	migrated := make([]dbmodel.PromptRule, 0)
	if err := gormDB.Order("id ASC").Find(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if len(migrated) == 0 {
		t.Fatal("migration produced no rules")
	}
	blob, _ := json.Marshal(migrated)
	t.Logf("migrated rules: %s", blob)

	bodies := []struct {
		name string
		in   inbound.InboundType
		out  outbound.OutboundType
		body string
	}{
		{"chat", inbound.InboundTypeOpenAIChat, outbound.OutboundTypeOpenAIChat, `{"model":"req-m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`},
		{"responses", inbound.InboundTypeOpenAIResponse, outbound.OutboundTypeOpenAIResponse, `{"model":"req-m","instructions":"I","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"anthropic", inbound.InboundTypeAnthropic, outbound.OutboundTypeAnthropic, `{"model":"req-m","max_tokens":10,"system":[{"type":"text","text":"A"}],"messages":[{"role":"user","content":"hi"}]}`},
		{"gemini", inbound.InboundTypeGemini, outbound.OutboundTypeGemini, `{"model":"gemini-x","systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	}
	for _, b := range bodies {
		// oracle：迁移前的旧四层字面量（global=GLOBAL/before；plan=PLAN/after；route=ROUTE/before；channel=CHAN/after）。
		legacyRaw := legacy16ComboWire(t, b.in, b.body, b.out, nil,
			dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter, dbmodel.PromptOverrideModeBefore, dbmodel.PromptOverrideModeAfter)
		// 新管道：迁移产物规则 + 与迁移等价的 scope（route 规则按 1M 清洗后的 m1 命中）。
		scope := prompt.Scope{PlanID: plan.ID, RequestModel: "m1", ChannelID: channel.ID}
		rulesRaw, _, snap := promptRulesWire(t, b.in, b.body, b.out, nil, migrated, scope)
		if !bytes.Equal(legacyRaw, rulesRaw) {
			t.Fatalf("%s: migration golden != legacy wire\nlegacy: %s\nmigrated: %s", b.name, legacyRaw, rulesRaw)
		}
		if len(snap.Sources) != 4 {
			t.Fatalf("%s: sources = %#v (want global/plan/route/channel)", b.name, snap.Sources)
		}
	}
	// 关闭方案（PLAN-OFF）：方案壳迁为 Enabled=false，方案文本不注入；但同一 profile 的
	// 路由规则仍然命中——旧算法里方案层开关与路由层互不影响（oracle 逐层独立判定），迁移把
	// 共享 profile 合并成一条规则并把 offPlan.ID 也写进 PlanIDs，两边行为一致。global 层旧算法
	// 本就无条件注入（不依赖方案），所以这里应当是 global + route，且绝无 plan 文本。
	scopeOff := prompt.Scope{PlanID: offPlan.ID, RequestModel: "m1", ChannelID: channel.ID + 1}
	_, _, snapOff := promptRulesWire(t, inbound.InboundTypeOpenAIChat,
		`{"model":"req-m","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`,
		outbound.OutboundTypeOpenAIChat, nil, migrated, scopeOff)
	if snapOff.Mode != "before" {
		t.Fatalf("offPlan scope must still hit the unconditional global rule; got mode %q sources=%#v", snapOff.Mode, snapOff.Sources)
	}
	// 恰好两条：global + 共享 profile 的 route；方案文本（关闭）与渠道层（这里是 channel.ID+1，未命中）都不许出现。
	if len(snapOff.Sources) != 2 {
		t.Fatalf("offPlan scope must hit exactly global+route; got %#v", snapOff.Sources)
	}
	if snapOff.Sources[0] != "global" || !strings.HasPrefix(snapOff.Sources[1], "route:") {
		t.Fatalf("offPlan scope must be [global, route:...]; got %#v", snapOff.Sources)
	}
	for _, src := range snapOff.Sources {
		if strings.HasPrefix(src, "plan:") || strings.HasPrefix(src, "channel:") {
			t.Fatalf("offPlan scope must not inject plan/channel text; got %#v", snapOff.Sources)
		}
	}

	// 幂等实测：再跑一遍迁移，id 与整行内容必须逐字节不变（成功记录已写在上一轮的同一事务里）。
	if err := dbmigrate.MigratePromptRulesForTest(gormDB); err != nil {
		t.Fatalf("migrate prompt rules again: %v", err)
	}
	again := make([]dbmodel.PromptRule, 0)
	if err := gormDB.Order("id ASC").Find(&again).Error; err != nil {
		t.Fatal(err)
	}
	firstBlob, _ := json.Marshal(migrated)
	againBlob, _ := json.Marshal(again)
	if !bytes.Equal(firstBlob, againBlob) {
		t.Fatalf("second migration run changed ids/contents:\nfirst: %s\nagain: %s", firstBlob, againBlob)
	}
}
