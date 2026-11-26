package op

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// testContext 返回写路径用的背景 context；测试里不需要请求作用域取消。
func testContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// mustList 包装 PromptRuleList 给孤儿删除后的空表断言用。
func mustList(t *testing.T) []model.PromptRule {
	t.Helper()
	rules, err := PromptRuleList(testContext(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return rules
}

// 提示词规则写路径钉住三件事：toggle 的显式列写让 false 落库；reorder 重复/
// 遗漏整失败且库不动；update 按 body.id 定位且时间列沿用旧值。

func setupPromptRuleTest(t *testing.T) {
	t.Helper()
	if err := db.InitDB("sqlite", t.TempDir()+"/octopus.db", false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
}

func promptRuleFixture(t *testing.T, name string, mutate func(*model.PromptRule)) model.PromptRule {
	t.Helper()
	rule := model.PromptRule{
		Name:        name,
		Enabled:     true,
		Text:        "be terse",
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeAll,
		Audience:    model.PromptAudienceAll,
	}
	if rule.Position == "" {
		rule.Position = model.PromptRulePositionBefore
	}
	if mutate != nil {
		mutate(&rule)
	}
	if err := PromptRuleCreate(&rule, testContext(t)); err != nil {
		t.Fatalf("create rule %s: %v", name, err)
	}
	return rule
}

func loadPromptRule(t *testing.T, id int) model.PromptRule {
	t.Helper()
	var rule model.PromptRule
	if err := db.GetDB().First(&rule, id).Error; err != nil {
		t.Fatalf("reload rule %d: %v", id, err)
	}
	return rule
}

// TestPromptRuleTogglePersistsDisabled：开/关走显式列，false 不能被零值吞掉。
func TestPromptRuleTogglePersistsDisabled(t *testing.T) {
	setupPromptRuleTest(t)
	rule := promptRuleFixture(t, "toggle-false", func(r *model.PromptRule) { r.Enabled = true })

	if err := PromptRuleToggle(rule.ID, false, testContext(t)); err != nil {
		t.Fatalf("toggle off: %v", err)
	}
	if reloaded := loadPromptRule(t, rule.ID); reloaded.Enabled {
		t.Fatalf("disabled rule stayed enabled: %+v", reloaded)
	}
	if err := PromptRuleToggle(rule.ID, true, testContext(t)); err != nil {
		t.Fatalf("toggle on: %v", err)
	}
	if reloaded := loadPromptRule(t, rule.ID); !reloaded.Enabled {
		t.Fatalf("enabled rule stayed disabled: %+v", reloaded)
	}
}

// TestPromptRuleUpdateLocatesByBodyIDKeepsTimes：update 只认 body.id；向前伪造时间
// 无效：created_at 保留库值，updated_at 每次修改由服务端推进；false 不丢。
func TestPromptRuleUpdateLocatesByBodyIDKeepsTimes(t *testing.T) {
	setupPromptRuleTest(t)
	rule := promptRuleFixture(t, "locate-by-id")
	before := loadPromptRule(t, rule.ID)

	forged := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	patch := model.PromptRule{
		ID:          rule.ID,
		Name:        "renamed",
		Enabled:     false,
		Text:        "rewritten",
		Position:    model.PromptRulePositionAfter,
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeAll,
		Audience:    model.PromptAudienceAll,
		CreatedAt:   forged, // body 伪造，必须被丢弃
		UpdatedAt:   forged,
	}
	if err := PromptRuleUpdate(&patch, testContext(t)); err != nil {
		t.Fatalf("update: %v", err)
	}
	after := loadPromptRule(t, rule.ID)
	if after.Name != "renamed" || after.Enabled || after.Text != "rewritten" || after.Position != model.PromptRulePositionAfter {
		t.Fatalf("patch not applied: %+v", after)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("created_at forged: got %v want %v", after.CreatedAt, before.CreatedAt)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) || after.UpdatedAt.Equal(forged) {
		t.Fatalf("updated_at not advanced by server: got %v want after %v and not %v", after.UpdatedAt, before.UpdatedAt, forged)
	}
}

// TestPromptRuleUpdateRejectsInvalidReferences：引用不存在的 plan/channel 必须显式
// 报 ErrPromptRuleInvalid，静默丢列/静默清空都算错。
func TestPromptRuleUpdateRejectsInvalidReferences(t *testing.T) {
	setupPromptRuleTest(t)
	rule := promptRuleFixture(t, "bad-refs")
	patch := rule
	patch.PlanMode = model.PromptRulePlanModeSelected
	patch.PlanIDs = []int{rule.ID + 9999}

	err := PromptRuleUpdate(&patch, testContext(t))
	if !errors.Is(err, ErrPromptRuleInvalid) {
		t.Fatalf("want ErrPromptRuleInvalid, got %v", err)
	}
}

// TestPromptRuleReorderRejectsDuplicateAndGaps：全量重排无重复/遗漏/未知项，
// 失败要原子（一次都不写），成功则 sort 与下标一致。
func TestPromptRuleReorderRejectsDuplicateAndGaps(t *testing.T) {
	setupPromptRuleTest(t)
	first := promptRuleFixture(t, "reorder-a")
	second := promptRuleFixture(t, "reorder-b")

	ctx := testContext(t)
	if err := PromptRuleReorder([]int{first.ID, first.ID}, ctx); !errors.Is(err, ErrPromptRuleReorder) {
		t.Fatalf("duplicate: want ErrPromptRuleReorder, got %v", err)
	}
	if err := PromptRuleReorder([]int{first.ID}, ctx); !errors.Is(err, ErrPromptRuleReorder) {
		t.Fatalf("gap: want ErrPromptRuleReorder, got %v", err)
	}
	if err := PromptRuleReorder([]int{first.ID, second.ID, second.ID + 9}, ctx); !errors.Is(err, ErrPromptRuleReorder) {
		t.Fatalf("unknown id: want ErrPromptRuleReorder, got %v", err)
	}
	if err := PromptRuleReorder([]int{second.ID, first.ID}, ctx); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if got := loadPromptRule(t, first.ID); got.Sort != 1 || got.Name != "reorder-a" {
		t.Fatalf("first rule not at index 1: %+v", got)
	}
	if got := loadPromptRule(t, second.ID); got.Sort != 0 {
		t.Fatalf("second rule not at index 0: %+v", got)
	}
}

// TestPromptRuleOrphansListToggleDelete：migration 迁来的孤儿（引用已删 plan）
// 能列出、能开关、能删除。
func TestPromptRuleOrphansListToggleDelete(t *testing.T) {
	setupPromptRuleTest(t)
	orphan := model.PromptRule{
		Name:     "orphan",
		Enabled:  true,
		Text:     "legacy",
		Position: model.PromptRulePositionBefore,
		PlanMode: model.PromptRulePlanModeSelected,
		PlanIDs:  []int{987654},
	}
	if err := db.GetDB().WithContext(testContext(t)).Create(&orphan).Error; err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	listed, err := PromptRuleList(testContext(t))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != orphan.ID || listed[0].PlanIDs[0] != 987654 {
		t.Fatalf("orphan not listed: %+v", listed)
	}
	if err := PromptRuleToggle(orphan.ID, false, testContext(t)); err != nil {
		t.Fatalf("toggle orphan: %v", err)
	}
	if reloaded := loadPromptRule(t, orphan.ID); reloaded.Enabled {
		t.Fatalf("orphan toggle lost: %+v", reloaded)
	}
	if err := PromptRuleDelete(orphan.ID, testContext(t)); err != nil {
		t.Fatalf("delete orphan: %v", err)
	}
	if remaining := mustList(t); len(remaining) != 0 {
		t.Fatalf("orphan not deleted: %+v", remaining)
	}
}

// TestPromptRuleCreateRejectsUnknownAudience：migration 031 迁出的未知历史
// audience 值继续拒绝，API 只收 all/admin_only。
func TestPromptRuleCreateRejectsUnknownAudience(t *testing.T) {
	setupPromptRuleTest(t)
	rule := model.PromptRule{
		Name:        "legacy-audience",
		Enabled:     true,
		Position:    model.PromptRulePositionBefore,
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeAll,
		Audience:    "user_only",
	}
	if err := PromptRuleCreate(&rule, testContext(t)); !errors.Is(err, ErrPromptRuleInvalid) {
		t.Fatalf("want ErrPromptRuleInvalid, got %v", err)
	}
}

// TestPromptRuleModelPatterns：bare * 合法；精确 foo 与 foo* 不是重复（都圆）；
// 重复同型、多星、中间星、首星、空白 pattern 均拒绝。
func TestPromptRuleModelPatterns(t *testing.T) {
	setupPromptRuleTest(t)
	base := model.PromptRule{
		Enabled:     true,
		Position:    model.PromptRulePositionBefore,
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeSelected,
		Audience:    model.PromptAudienceAll,
	}
	ok := func(name string, models []string) {
		r := base
		r.Name = name
		r.Models = models
		if err := PromptRuleCreate(&r, testContext(t)); err != nil {
			t.Fatalf("%s %v: want ok, got %v", name, models, err)
		}
	}
	bad := func(name string, models []string) {
		r := base
		r.Name = name
		r.Models = models
		if err := PromptRuleCreate(&r, testContext(t)); !errors.Is(err, ErrPromptRuleInvalid) {
			t.Fatalf("%s %v: want ErrPromptRuleInvalid, got %v", name, models, err)
		}
	}
	ok("bare-star", []string{"*"})
	ok("exact-plus-wild", []string{"foo", "foo*"})
	bad("dup-wild", []string{"foo*", "FOO*"})
	bad("dup-exact", []string{"foo", "FOO"})
	bad("double-star", []string{"foo**"})
	bad("mid-star", []string{"fo*o"})
	bad("lead-star", []string{"*foo"})
	bad("blank-pattern", []string{"   "})
}

// TestPromptRuleNameRuneLimit：191 rune 上限按 rune 计，多字节字符不占重复字节。
func TestPromptRuleNameRuneLimit(t *testing.T) {
	setupPromptRuleTest(t)
	base := model.PromptRule{
		Enabled:     true,
		Position:    model.PromptRulePositionBefore,
		PlanMode:    model.PromptRulePlanModeAll,
		ChannelMode: model.PromptRuleChannelModeAll,
		ModelMode:   model.PromptRuleModelModeAll,
		Audience:    model.PromptAudienceAll,
	}
	r := base
	r.Name = strings.Repeat("好", 191)
	if err := PromptRuleCreate(&r, testContext(t)); err != nil {
		t.Fatalf("191 cjk runes should pass: %v", err)
	}
	r2 := base
	r2.Name = strings.Repeat("好", 192)
	if err := PromptRuleCreate(&r2, testContext(t)); !errors.Is(err, ErrPromptRuleInvalid) {
		t.Fatalf("192 cjk runes should fail, got %v", err)
	}
}
