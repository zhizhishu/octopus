package migrate

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func new031DB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "octopus031.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, dberr := db.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.PromptRule{}, &model.Setting{}, &model.AccessPlan{}, &model.AccessRouteProfile{}, &model.AccessRouteRule{}, &model.Channel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func count031Rules(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.PromptRule{}).Count(&n).Error; err != nil {
		t.Fatalf("count prompt rules: %v", err)
	}
	return n
}

func all031Rules(t *testing.T, db *gorm.DB) []model.PromptRule {
	t.Helper()
	rules := make([]model.PromptRule, 0)
	if err := db.Order("id ASC").Find(&rules).Error; err != nil {
		t.Fatalf("read prompt rules: %v", err)
	}
	return rules
}

func find031Rule(rules []model.PromptRule, name string) *model.PromptRule {
	for i := range rules {
		if rules[i].Name == name {
			return &rules[i]
		}
	}
	return nil
}

func seed031Legacy(t *testing.T, db *gorm.DB) (planVIP, planTest, planOff model.AccessPlan) {
	t.Helper()
	set := func(k model.SettingKey, v string) {
		t.Helper()
		if err := db.Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	set(model.SettingKeyPromptOverrideSystem, "GLOBAL-TEXT")
	set(model.SettingKeyPromptOverrideMode, string(model.PromptOverrideModeBefore))
	set(model.SettingKeyPromptOverrideEnabled, "true")
	set(model.SettingKeyPromptAudience, string(model.PromptAudienceAdminOnly))

	// profile 1 被 vip/test/off 三个方案共享；profile 2 无任何方案引用（orphan）。
	if err := db.Create(&model.AccessRouteProfile{ID: 1, Name: "shared"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AccessRouteProfile{ID: 2, Name: "orphan"}).Error; err != nil {
		t.Fatal(err)
	}
	planVIP = model.AccessPlan{Slug: "vip", DisplayName: "vip", SystemPromptOverride: "PLAN-A", PromptOverrideMode: "before", PromptOverrideEnabled: true, RouteProfileID: 1}
	planTest = model.AccessPlan{Slug: "test", DisplayName: "test", SystemPromptOverride: "PLAN-B", PromptOverrideMode: "after", PromptOverrideEnabled: true, RouteProfileID: 1}
	planOff = model.AccessPlan{Slug: "off", DisplayName: "off", SystemPromptOverride: "PLAN-OFF", PromptOverrideMode: "before", PromptOverrideEnabled: false, RouteProfileID: 1}
	plans := []model.AccessPlan{planVIP, planTest, planOff}
	if err := db.Create(&plans).Error; err != nil {
		t.Fatal(err)
	}
	// GORM 只回填传入切片的 ID；把稳定自增 ID 拷回返回值，供断言具体 ID。
	planVIP.ID, planTest.ID, planOff.ID = plans[0].ID, plans[1].ID, plans[2].ID
	return planVIP, planTest, planOff
}

// 旧四层字段 → 031：全局面、方案面（含开关关闭）、路由面（共享 profile 只建一次、
// 重复规范化 RequestModel 只迁旧首条、孤儿 profile 不放大范围）、渠道面。
// 旧行不改动；跑两轮不重复；删一条规则再跑 Up 不复活。
func TestMigration031SeedsRulesFromLegacyFields(t *testing.T) {
	db := new031DB(t)
	planVIP, planTest, planOff := seed031Legacy(t, db)

	rules := []model.AccessRouteRule{
		{RouteProfileID: 1, RequestModel: "M1[1m]", SystemPromptOverride: "ROUTE-A", PromptOverrideMode: "before", PromptOverrideEnabled: true},
		// 同键重复（规范化后与 ROUTE-A 相同）：只迁旧首条，次条不失效。
		{RouteProfileID: 1, RequestModel: "m1", SystemPromptOverride: "ROUTE-DUP", PromptOverrideMode: "before", PromptOverrideEnabled: true},
		// 关闭的旧路由提示词：迁为关闭的真实规则（阻断同键次条激活）。
		// 注意：access_route_rules 有 (route_profile_id, request_model) 复合唯一索引，真实库里
		// 「完全同名」的两行不可能存在；能出现的重复只有「规范化后同名」。次条因此用 [1m] 变体
		// 表达（CleanOneMillionCapabilityModelName 去掉尾缀后与首条同键），断言不变。
		{RouteProfileID: 1, RequestModel: "m2", SystemPromptOverride: "ROUTE-OFF", PromptOverrideMode: "before", PromptOverrideEnabled: false},
		{RouteProfileID: 1, RequestModel: "m2[1m]", SystemPromptOverride: "ROUTE-BLOCKED", PromptOverrideMode: "before", PromptOverrideEnabled: true},
		// orphan profile：无方案引用 → selected 空 PlanIDs + IncludeNoPlan=false（永不命中）。
		{RouteProfileID: 2, RequestModel: "lonely", SystemPromptOverride: "ROUTE-ORPHAN", PromptOverrideMode: "after", PromptOverrideEnabled: true},
		// 空文本旧路由：不建规则也不建壳，但 seen 占位阻断次条文本（次条同样用 [1m] 变体避开唯一索引）。
		{RouteProfileID: 1, RequestModel: "empty1", SystemPromptOverride: "   "},
		{RouteProfileID: 1, RequestModel: "empty1[1m]", SystemPromptOverride: "SHOULD-NOT-APPEAR", PromptOverrideMode: "before", PromptOverrideEnabled: true},
		// legacy 未迁移模式 + 开关列 false：按"有文本=开"语义迁为启用。
		{RouteProfileID: 1, RequestModel: "m3", SystemPromptOverride: "ROUTE-LEGACY", PromptOverrideMode: "append_system", PromptOverrideEnabled: false},
	}
	if err := db.Create(&rules).Error; err != nil {
		t.Fatal(err)
	}
	channels := []model.Channel{
		{Name: "c1", SystemPromptOverride: "CHAN-A", PromptOverrideMode: "after", PromptOverrideEnabled: true},
		{Name: "c2", SystemPromptOverride: "   ", PromptOverrideMode: "before", PromptOverrideEnabled: true},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}

	if err := migratePromptRules(db); err != nil {
		t.Fatalf("migrate 031: %v", err)
	}
	first := all031Rules(t, db)
	// global 1 + plans 3 + routes 4 (m1 / m2关閉 / orphan / m3-legacy，empty1 空文本不建) + channels 1 = 9
	if len(first) != 9 {
		t.Fatalf("rule count = %d, want 9:\n%#v", len(first), first)
	}
	g := find031Rule(first, "global")
	if g == nil || g.Text != "GLOBAL-TEXT" || g.Position != model.PromptRulePositionBefore || !g.Enabled || g.Audience != model.PromptAudienceAdminOnly || g.PlanMode != model.PromptRulePlanModeAll {
		t.Fatalf("global rule: %#v", g)
	}
	p := find031Rule(first, "plan:vip")
	if p == nil || p.Text != "PLAN-A" || p.Position != model.PromptRulePositionBefore || !p.Enabled || len(p.PlanIDs) != 1 || p.PlanIDs[0] != planVIP.ID {
		t.Fatalf("plan:vip rule: %#v", p)
	}
	p = find031Rule(first, "plan:test")
	if p == nil || p.Position != model.PromptRulePositionAfter || !p.Enabled {
		t.Fatalf("plan:test rule: %#v", p)
	}
	p = find031Rule(first, "plan:off")
	if p == nil || p.Enabled != false || p.Text != "PLAN-OFF" {
		t.Fatalf("closed plan must persist as a disabled rule: %#v", p)
	}
	// 共享 profile 只建一次并在同一规则里合并全部引用方案：vip/test/off 三方案
	// 同挂 profile 1（关闭的 planOff 也参与合并），断言具体 ID 而非只看长度。
	shared := find031Rule(first, "route:shared/m1")
	if shared == nil || shared.Text != "ROUTE-A" || !shared.Enabled || len(shared.PlanIDs) != 3 {
		t.Fatalf("shared profile must merge all 3 plan ids: %#v", shared)
	}
	for _, want := range []int{planVIP.ID, planTest.ID, planOff.ID} {
		found := false
		for _, got := range shared.PlanIDs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("shared.PlanIDs must contain plan id %d: %#v", want, shared.PlanIDs)
		}
	}
	// 路由规则必须按旧模型名收敛为 selected 过滤，不能用 all 放大范围。
	if shared.ModelMode != model.PromptRuleModelModeSelected || len(shared.Models) != 1 || shared.Models[0] != "m1" {
		t.Fatalf("route rule must carry cleaned model filter: %#v", shared)
	}
	// 重复键：旧首条生效；次条（ROUTE-DUP / ROUTE-BLOCKED）一律不迁移。
	if r := find031Rule(first, "route:shared/dup"); r != nil {
		t.Fatalf("duplicate normalized request model must not become a rule: %#v", r)
	}
	// 空文本路由：不建壳；同键次条有文本也不生效（seen 占位）。
	if r := find031Rule(first, "route:shared/empty1"); r != nil {
		t.Fatalf("blank-text route must not become a rule/shell: %#v", r)
	}
	off := find031Rule(first, "route:shared/m2")
	if off == nil || off.Enabled != false || off.Text != "ROUTE-OFF" {
		t.Fatalf("closed route first-wins shell: %#v", off)
	}
	o := find031Rule(first, "route:orphan/lonely")
	if o == nil || o.PlanIDs != nil || o.IncludeNoPlan || o.Position != model.PromptRulePositionAfter {
		t.Fatalf("orphan profile must stay selected-with-empty-planids (never matches): %#v", o)
	}
	o = find031Rule(first, "route:shared/m3")
	if o == nil || !o.Enabled || o.Text != "ROUTE-LEGACY" || o.Position != model.PromptRulePositionBefore {
		t.Fatalf("legacy unmigrated mode counts as enabled+before: %#v", o)
	}
	c := find031Rule(first, "channel:c1")
	if c == nil || c.Text != "CHAN-A" || c.Position != model.PromptRulePositionAfter || !c.Enabled || len(c.ChannelIDs) != 1 || c.ChannelIDs[0] != channels[0].ID {
		t.Fatalf("channel rule: %#v", c)
	}
	if c := find031Rule(first, "channel:c2"); c != nil {
		t.Fatalf("blank-text channel must not become a rule: %#v", c)
	}
	// 旧字段只读：迁移不改旧行。
	var planRow model.AccessPlan
	if err := db.Where("id = ?", planVIP.ID).Take(&planRow).Error; err != nil || planRow.SystemPromptOverride != "PLAN-A" {
		t.Fatalf("legacy plan row mutated: %#v %v", planRow, err)
	}

	// 第二轮直跑 + 外层 runner 语义：不再插入，行级一致。
	if err := migratePromptRules(db); err != nil {
		t.Fatalf("migrate 031 again: %v", err)
	}
	second := all031Rules(t, db)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if len(second) != len(first) || string(firstJSON) != string(secondJSON) {
		t.Fatalf("second run changed the seed:\n%s\n%s", firstJSON, secondJSON)
	}
	var rec MigrationRecord
	if err := db.Where("version = ?", 31).Take(&rec).Error; err != nil || rec.Status != MigrationRecordStatusSuccess {
		t.Fatalf("record 31: %#v %v", rec, err)
	}
}

// Enabled 二值 + 受众无效值保留：全局关→规则关；plan/channel/route/全局的无效
// 受众（未知非空）原样存入不映射成 all，判定端按默认拒绝；空 audience 等价 all。
func TestMigration031DisabledAndInvalidAudiencePreserved(t *testing.T) {
	db := new031DB(t)
	set := func(k model.SettingKey, v string) {
		t.Helper()
		if err := db.Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	set(model.SettingKeyPromptOverrideSystem, "G")
	set(model.SettingKeyPromptOverrideMode, string(model.PromptOverrideModeBefore))
	set(model.SettingKeyPromptOverrideEnabled, "false")
	set(model.SettingKeyPromptAudience, "typo_audience")
	if err := db.Create(&model.AccessRouteProfile{ID: 7, Name: "p7"}).Error; err != nil {
		t.Fatal(err)
	}
	plans := []model.AccessPlan{
		{Slug: "pa", DisplayName: "pa", SystemPromptOverride: "P", PromptOverrideMode: "before", PromptOverrideEnabled: true, RouteProfileID: 7, PromptAudience: model.PromptAudience("typo_plan")},
		{Slug: "blank", DisplayName: "blank", SystemPromptOverride: "", PromptOverrideMode: "before", PromptOverrideEnabled: false, PromptAudience: model.PromptAudience("typo_blank")},
	}
	if err := db.Create(&plans).Error; err != nil {
		t.Fatal(err)
	}
	routes := []model.AccessRouteRule{
		{RouteProfileID: 7, RequestModel: "ONLY[1m]", SystemPromptOverride: "R", PromptOverrideMode: "before", PromptOverrideEnabled: true, PromptAudience: model.PromptAudience("typo_route")},
	}
	if err := db.Create(&routes).Error; err != nil {
		t.Fatal(err)
	}
	channels := []model.Channel{
		{Name: "cx", SystemPromptOverride: "C", PromptOverrideMode: "before", PromptOverrideEnabled: true, PromptAudience: model.PromptAudience("typo_channel")},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}

	if err := migratePromptRules(db); err != nil {
		t.Fatalf("migrate 031: %v", err)
	}
	rules := all031Rules(t, db)
	// global 1 + plan 1（blank 空文本不建）+ route 1 + channel 1 = 4
	if len(rules) != 4 {
		t.Fatalf("rule count = %d, want 4:\n%#v", len(rules), rules)
	}
	g := find031Rule(rules, "global")
	if g == nil || g.Enabled != false || g.Text != "G" {
		t.Fatalf("disabled global must persist as disabled: %#v", g)
	}
	if g.Audience != model.PromptAudience("typo_audience") {
		t.Fatalf("invalid global audience must be preserved as-is: %#v", g.Audience)
	}
	pa := find031Rule(rules, "plan:pa")
	if pa == nil || pa.Enabled != true || pa.Audience != model.PromptAudience("typo_plan") {
		t.Fatalf("plan rule enabled/audience: %#v", pa)
	}
	r := find031Rule(rules, "route:p7/only")
	if r == nil || r.Enabled != true || r.ModelMode != model.PromptRuleModelModeSelected || len(r.Models) != 1 || r.Models[0] != "only" {
		t.Fatalf("route rule model filter + enabled: %#v", r)
	}
	if r.Audience != model.PromptAudience("typo_route") {
		t.Fatalf("invalid route audience must be preserved as-is: %#v", r.Audience)
	}
	c := find031Rule(rules, "channel:cx")
	if c == nil || c.Enabled != true || c.Audience != model.PromptAudience("typo_channel") {
		t.Fatalf("channel rule enabled/audience: %#v", c)
	}
	if find031Rule(rules, "plan:blank") != nil {
		t.Fatalf("blank-text plan must not become a rule: %#v", find031Rule(rules, "plan:blank"))
	}
}

// 删除规则后重跑 Up 不复活（031 自身幂等，与外层 runMigrationsWithRecord 无关）。
func TestMigration031DeletedRulesStayDeleted(t *testing.T) {
	db := new031DB(t)
	if err := db.Create(&model.Setting{Key: model.SettingKeyPromptOverrideSystem, Value: "G"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Setting{Key: model.SettingKeyPromptOverrideEnabled, Value: "true"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migratePromptRules(db); err != nil {
		t.Fatal(err)
	}
	if n := count031Rules(t, db); n != 1 {
		t.Fatalf("want 1 global rule, got %d", n)
	}
	if err := db.Where("1 = 1").Delete(&model.PromptRule{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migratePromptRules(db); err != nil {
		t.Fatal(err)
	}
	if n := count031Rules(t, db); n != 0 {
		t.Fatalf("deleted rules must stay deleted after rerun, got %d", n)
	}
}

// 生成名截断：031 拼出来的名字（plan:<slug> / route:<profile>/<model> / channel:<name>）
// 写库前按 []rune 截 191；只动 name，text / 作用域 / 开关全部保留；正好 191 的不动。
func TestMigration031TruncatesGeneratedNamesTo191Runes(t *testing.T) {
	db := new031DB(t)
	longSlug := strings.Repeat("方", 240)     // 240 runes → plan:<slug> 245 runes，必须截到 191
	longProfile := strings.Repeat("档", 240)  // route:<profile>/m 必截
	longChannel := strings.Repeat("📣", 200)  // 200 runes emoji：截断不能切出半个 rune
	exactChannel := strings.Repeat("字", 183) // "channel:" + 183 = 191，正好卡边界，不得被改

	if err := db.Create(&model.AccessRouteProfile{ID: 11, Name: longProfile}).Error; err != nil {
		t.Fatal(err)
	}
	plans := []model.AccessPlan{
		{Slug: longSlug, DisplayName: "long", SystemPromptOverride: "PLAN-TEXT", PromptOverrideMode: "after", PromptOverrideEnabled: true, RouteProfileID: 11},
	}
	if err := db.Create(&plans).Error; err != nil {
		t.Fatal(err)
	}
	routes := []model.AccessRouteRule{
		{RouteProfileID: 11, RequestModel: "M[1m]", SystemPromptOverride: "ROUTE-TEXT", PromptOverrideMode: "before", PromptOverrideEnabled: true},
	}
	if err := db.Create(&routes).Error; err != nil {
		t.Fatal(err)
	}
	channels := []model.Channel{
		{Name: longChannel, SystemPromptOverride: "CHAN-LONG", PromptOverrideMode: "after", PromptOverrideEnabled: true},
		{Name: exactChannel, SystemPromptOverride: "CHAN-EXACT", PromptOverrideMode: "before", PromptOverrideEnabled: true},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}

	if err := migratePromptRules(db); err != nil {
		t.Fatalf("migrate 031: %v", err)
	}
	rules := all031Rules(t, db)

	byText := func(text string) *model.PromptRule {
		for i := range rules {
			if rules[i].Text == text {
				return &rules[i]
			}
		}
		return nil
	}
	check := func(label, text, prefix string) *model.PromptRule {
		t.Helper()
		r := byText(text)
		if r == nil {
			t.Fatalf("%s: rule with text %q not migrated: %#v", label, text, rules)
		}
		if got := utf8.RuneCountInString(r.Name); got != 191 {
			t.Fatalf("%s: name rune count = %d, want 191: %q", label, got, r.Name)
		}
		if !utf8.ValidString(r.Name) {
			t.Fatalf("%s: truncated name is not valid UTF-8（切到半个 rune）: %q", label, r.Name)
		}
		if !strings.HasPrefix(r.Name, prefix) {
			t.Fatalf("%s: name prefix lost: %q", label, r.Name)
		}
		return r
	}

	planRule := check("plan", "PLAN-TEXT", "plan:")
	routeRule := check("route", "ROUTE-TEXT", "route:")
	chanLong := check("channel", "CHAN-LONG", "channel:")
	chanExact := check("channel-exact", "CHAN-EXACT", "channel:")

	// 非名字字段原样保留（截断只动 name，不改 text / 作用域 / 开关）。
	if planRule.Position != model.PromptRulePositionAfter || !planRule.Enabled ||
		planRule.PlanMode != model.PromptRulePlanModeSelected || len(planRule.PlanIDs) != 1 || planRule.PlanIDs[0] != plans[0].ID ||
		planRule.Text != "PLAN-TEXT" {
		t.Fatalf("plan rule 非名字字段被改动: %#v", planRule)
	}
	if routeRule.Position != model.PromptRulePositionBefore || !routeRule.Enabled ||
		routeRule.ModelMode != model.PromptRuleModelModeSelected || len(routeRule.Models) != 1 || routeRule.Models[0] != "m" ||
		len(routeRule.PlanIDs) != 1 || routeRule.PlanIDs[0] != plans[0].ID || routeRule.Text != "ROUTE-TEXT" {
		t.Fatalf("route rule 非名字字段被改动: %#v", routeRule)
	}
	if chanLong.ChannelMode != model.PromptRuleChannelModeInclude || len(chanLong.ChannelIDs) != 1 ||
		chanLong.ChannelIDs[0] != channels[0].ID || chanLong.Position != model.PromptRulePositionAfter || !chanLong.Enabled {
		t.Fatalf("channel rule 非名字字段被改动: %#v", chanLong)
	}
	if want := "channel:" + exactChannel; chanExact.Name != want {
		t.Fatalf("exactly-191 name must stay untouched: got %q want %q", chanExact.Name, want)
	}
}

// 方言覆盖：031 全部走 ORM 读写，不得出现方言 SQL 字面量（沿用 021 的源码断言风格）。
func TestMigration031NoDialectSQLLiterals(t *testing.T) {
	file := parseMigrationSource(t, "031.go")
	for _, sql := range execStringLiterals(file) {
		for _, kw := range []string{"CREATE TABLE", "CREATE INDEX", "INSERT INTO", "AUTO_INCREMENT", "MODIFY COLUMN"} {
			if contains(sql, kw) {
				t.Fatalf("031.go 含方言 SQL 字面量 %q：%s", kw, sql)
			}
		}
	}
}
